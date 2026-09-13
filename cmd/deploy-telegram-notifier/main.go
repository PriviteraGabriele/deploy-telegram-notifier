package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	defaultStateDir = "/var/lib/deploy-telegram-notifier"
	stateFileName   = "state.json"
	stateMaxAge     = 30 * 24 * time.Hour
	releaseTimeout  = 10 * time.Minute
	delayThreshold  = 5 * time.Minute
)

type config struct {
	BotToken       string
	ChatID         string
	ThreadID       string
	StateDir       string
	AppURL         string
	DelayThreshold time.Duration
	TelegramAPIURL string
	HTTPClient     *http.Client
	Now            func() time.Time
}

type metadata struct {
	Repository  string
	Branch      string
	BuildNumber string
	RunID       string
	PipelineURL string
	Commit      string
	Author      string
	Revision    string
}

type event struct {
	Project          string
	Service          string
	Status           string
	Stage            string
	ExpectedServices []string
	HealthURL        string
	AppURL           string
	PreviousOnline   bool
	Reason           string
	ReleaseFallback  string
	Metadata         metadata
	At               time.Time
}

type serviceState struct {
	Status string    `json:"status"`
	Stage  string    `json:"stage"`
	At     time.Time `json:"at"`
}

type release struct {
	Project           string                  `json:"project"`
	ReleaseID         string                  `json:"releaseId"`
	Metadata          metadata                `json:"metadata"`
	ExpectedServices  []string                `json:"expectedServices"`
	HealthURL         string                  `json:"healthUrl,omitempty"`
	AppURL            string                  `json:"appUrl,omitempty"`
	StartedAt         time.Time               `json:"startedAt"`
	UpdatedAt         time.Time               `json:"updatedAt"`
	Services          map[string]serviceState `json:"services"`
	NotifiedSuccess   bool                    `json:"notifiedSuccess"`
	NotifiedDelayed   bool                    `json:"notifiedDelayed"`
	NotifiedFailures  map[string]bool         `json:"notifiedFailures"`
	PreviousRevision  string                  `json:"previousRevision,omitempty"`
	PreviousReleaseID string                  `json:"previousReleaseId,omitempty"`
	Rollback          bool                    `json:"rollback"`
	LastFailureStage  string                  `json:"lastFailureStage,omitempty"`
	LastFailureAt     time.Time               `json:"lastFailureAt,omitempty"`
}

type state struct {
	Releases map[string]*release `json:"releases"`
}

type notification struct {
	Kind       string
	ReleaseKey string
	FailureKey string
	Service    string
	Stage      string
	Reason     string
	PreviousOK bool
}

type dockerImage struct {
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func main() {
	if len(os.Args) < 2 {
		fatal("usage: deploy-telegram-notifier <event|pending|sweep|history|test>")
	}
	cfg, err := loadConfig()
	if err != nil {
		fatal(err.Error())
	}

	switch os.Args[1] {
	case "event":
		err = runEvent(cfg, os.Args[2:], os.Stdin)
	case "pending":
		err = runPending(cfg, os.Args[2:], os.Stdin)
	case "sweep":
		err = runSweep(cfg)
	case "history":
		err = runHistory(cfg, os.Args[2:])
	case "test":
		err = runTest(cfg, os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fatal(err.Error())
	}
}

func fatal(message string) {
	fmt.Fprintln(os.Stderr, "deploy-telegram-notifier:", message)
	os.Exit(1)
}

func loadConfig() (config, error) {
	stateDir := os.Getenv("DEPLOY_NOTIFIER_STATE_DIR")
	if stateDir == "" {
		stateDir = defaultStateDir
	}
	delay := delayThreshold
	if value := os.Getenv("DEPLOY_NOTIFIER_DELAY_THRESHOLD"); value != "" {
		parsed, err := time.ParseDuration(value)
		if err != nil || parsed <= 0 {
			return config{}, fmt.Errorf("DEPLOY_NOTIFIER_DELAY_THRESHOLD must be a positive duration")
		}
		delay = parsed
	}
	cfg := config{
		BotToken:       os.Getenv("TELEGRAM_BOT_TOKEN"),
		ChatID:         os.Getenv("TELEGRAM_CHAT_ID"),
		ThreadID:       os.Getenv("TELEGRAM_MESSAGE_THREAD_ID"),
		StateDir:       stateDir,
		AppURL:         os.Getenv("DEPLOY_NOTIFIER_APP_URL"),
		DelayThreshold: delay,
		TelegramAPIURL: os.Getenv("TELEGRAM_API_BASE_URL"),
		HTTPClient:     &http.Client{Timeout: 10 * time.Second},
		Now:            func() time.Time { return time.Now().UTC() },
	}
	if cfg.TelegramAPIURL == "" {
		cfg.TelegramAPIURL = "https://api.telegram.org"
	}
	if cfg.BotToken == "" || cfg.ChatID == "" {
		return config{}, fmt.Errorf("TELEGRAM_BOT_TOKEN and TELEGRAM_CHAT_ID are required")
	}
	if _, err := url.ParseRequestURI(cfg.TelegramAPIURL); err != nil {
		return config{}, fmt.Errorf("TELEGRAM_API_BASE_URL is invalid: %w", err)
	}
	if cfg.AppURL != "" && !validHTTPURL(cfg.AppURL) {
		return config{}, fmt.Errorf("DEPLOY_NOTIFIER_APP_URL is invalid")
	}
	return cfg, nil
}

func runEvent(cfg config, args []string, input io.Reader) error {
	flags := flag.NewFlagSet("event", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", "", "project name")
	service := flags.String("service", "", "service name")
	status := flags.String("status", "", "started, succeeded, or failed")
	stage := flags.String("stage", "", "deployment stage")
	expected := flags.String("expected-services", "", "comma-separated service names")
	healthURL := flags.String("health-url", "", "public health URL")
	appURL := flags.String("app-url", "", "public application URL")
	previousOnline := flags.Bool("previous-online", false, "whether the prior release is still online")
	fallback := flags.String("release-fallback", "", "release identifier when image metadata is unavailable")
	reason := flags.String("reason", "", "safe, short failure reason")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *project == "" || *service == "" {
		return fmt.Errorf("--project and --service are required")
	}
	if *status != "started" && *status != "succeeded" && *status != "failed" {
		return fmt.Errorf("--status must be started, succeeded, or failed")
	}
	meta, err := readMetadata(input)
	if err != nil {
		return fmt.Errorf("read image metadata: %w", err)
	}
	services := splitCSV(*expected)
	if len(services) == 0 {
		services = []string{*service}
	}
	return process(cfg, event{
		Project: *project, Service: *service, Status: *status, Stage: *stage,
		ExpectedServices: services, HealthURL: *healthURL, AppURL: *appURL, PreviousOnline: *previousOnline,
		ReleaseFallback: *fallback, Reason: *reason, Metadata: meta, At: cfg.Now(),
	})
}

func runSweep(cfg config) error {
	st, err := loadState(cfg)
	if err != nil {
		return err
	}
	now := cfg.Now()
	cleanup(st, now)
	var notifications []notification
	for key, rel := range st.Releases {
		if rel.NotifiedSuccess {
			continue
		}
		if now.Sub(rel.StartedAt) >= cfg.DelayThreshold && !rel.NotifiedDelayed {
			notifications = append(notifications, notification{Kind: "delayed", ReleaseKey: key})
		}
		if now.Sub(rel.StartedAt) >= releaseTimeout {
			failureKey := "incomplete"
			if !rel.NotifiedFailures[failureKey] {
				notifications = append(notifications, notification{Kind: "failure", ReleaseKey: key, FailureKey: failureKey, Stage: "release incomplete", Reason: "not all services reached a healthy state before the timeout"})
			}
		}
	}
	return persistAndSend(cfg, st, notifications)
}

func runHistory(cfg config, args []string) error {
	flags := flag.NewFlagSet("history", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", "", "optional project name")
	limit := flags.Int("limit", 20, "maximum number of releases")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *limit <= 0 {
		return fmt.Errorf("--limit must be positive")
	}
	st, err := loadState(cfg)
	if err != nil {
		return err
	}
	items := make([]*release, 0, len(st.Releases))
	for _, rel := range st.Releases {
		if *project == "" || rel.Project == *project {
			items = append(items, rel)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].UpdatedAt.After(items[j].UpdatedAt) })
	fmt.Fprintln(os.Stdout, "TIME\tSTATUS\tPROJECT\tBUILD\tREVISION\tDURATION")
	for index, rel := range items {
		if index >= *limit {
			break
		}
		fmt.Fprintf(os.Stdout, "%s\t%s\t%s\t#%s\t%s\t%s\n",
			rel.UpdatedAt.Local().Format(time.RFC3339), historyStatus(rel), rel.Project,
			fallback(rel.Metadata.BuildNumber, "?"), shortRevision(rel.Metadata.Revision), rel.UpdatedAt.Sub(rel.StartedAt).Round(time.Second))
	}
	return nil
}

// runPending exits successfully only while a known release still needs a
// terminal notification. The updater uses it to retry a failed health check
// without turning ordinary 30-second polling into uptime monitoring.
func runPending(cfg config, args []string, input io.Reader) error {
	flags := flag.NewFlagSet("pending", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	project := flags.String("project", "", "project name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *project == "" {
		return fmt.Errorf("--project is required")
	}
	meta, err := readMetadata(input)
	if err != nil {
		return fmt.Errorf("read image metadata: %w", err)
	}
	releaseID := meta.RunID
	if releaseID == "" {
		releaseID = meta.Revision
	}
	if releaseID == "" {
		return fmt.Errorf("release metadata is unavailable")
	}
	st, err := loadState(cfg)
	if err != nil {
		return err
	}
	rel := st.Releases[*project+":"+releaseID]
	if rel == nil || rel.NotifiedSuccess {
		return fmt.Errorf("release is not pending")
	}
	return nil
}

func runTest(cfg config, args []string) error {
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	scenario := flags.String("scenario", "", "failure, recovery, delayed, or rollback")
	appURL := flags.String("app-url", "", "optional public application URL for the test button")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *appURL != "" {
		if !validHTTPURL(*appURL) {
			return fmt.Errorf("--app-url is invalid")
		}
		cfg.AppURL = *appURL
	}
	if *scenario == "" {
		message := "<b>🟢 Deploy Telegram Notifier test</b>\n\nThe Telegram delivery configuration is working."
		if err := sendTelegram(cfg, message, "", cfg.AppURL); err != nil {
			return err
		}
		fmt.Println("test notification sent")
		return nil
	}

	now := cfg.Now()
	runID := fmt.Sprintf("test-%s-%d", *scenario, now.UnixNano())
	project := "deploy-notifier-test-" + *scenario
	meta := metadata{Repository: "example/deploy-notifier", Branch: "test", BuildNumber: "0", RunID: runID, PipelineURL: "https://github.com/example/deploy-notifier/actions", Commit: "synthetic notification test", Author: "Deploy Telegram Notifier", Revision: "test000"}
	eventFor := func(service, status, stage, reason string) event {
		return event{Project: project, Service: service, Status: status, Stage: stage, Reason: reason, ExpectedServices: []string{"api", "web"}, PreviousOnline: true, Metadata: meta, At: cfg.Now()}
	}
	switch *scenario {
	case "failure":
		if err := process(cfg, eventFor("api", "failed", "migration", "synthetic migration failure")); err != nil {
			return err
		}
	case "recovery":
		if err := process(cfg, eventFor("api", "failed", "healthcheck", "synthetic healthcheck failure")); err != nil {
			return err
		}
		if err := process(cfg, eventFor("api", "succeeded", "healthcheck", "")); err != nil {
			return err
		}
		if err := process(cfg, eventFor("web", "succeeded", "healthcheck", "")); err != nil {
			return err
		}
	case "delayed":
		if err := process(cfg, eventFor("api", "started", "pull", "")); err != nil {
			return err
		}
		cfg.Now = func() time.Time { return now.Add(cfg.DelayThreshold + time.Second) }
		if err := runSweep(cfg); err != nil {
			return err
		}
	case "rollback":
		deployTestRelease := func(id, revision string) error {
			meta.RunID, meta.Revision = id, revision
			for _, service := range []string{"api", "web"} {
				if err := process(cfg, eventFor(service, "succeeded", "healthcheck", "")); err != nil {
					return err
				}
			}
			now = now.Add(time.Second)
			cfg.Now = func() time.Time { return now }
			return nil
		}
		if err := deployTestRelease(runID+"-first", "aaaaaaa"); err != nil {
			return err
		}
		if err := deployTestRelease(runID+"-second", "bbbbbbb"); err != nil {
			return err
		}
		if err := deployTestRelease(runID+"-rollback", "aaaaaaa"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("--scenario must be failure, recovery, delayed, or rollback")
	}
	fmt.Printf("%s test notification sent\n", *scenario)
	return nil
}

func process(cfg config, ev event) error {
	st, err := loadState(cfg)
	if err != nil {
		return err
	}
	cleanup(st, ev.At)
	key, rel := getRelease(st, ev)
	rel.UpdatedAt = ev.At
	mergeMetadata(&rel.Metadata, ev.Metadata)
	if ev.HealthURL != "" {
		rel.HealthURL = ev.HealthURL
	}
	if ev.AppURL != "" {
		rel.AppURL = ev.AppURL
	}
	rel.ExpectedServices = unionServices(rel.ExpectedServices, ev.ExpectedServices)
	rel.Services[ev.Service] = serviceState{Status: ev.Status, Stage: ev.Stage, At: ev.At}

	var notifications []notification
	if ev.Status == "failed" {
		failureKey := ev.Service + ":" + ev.Stage
		if !rel.NotifiedFailures[failureKey] {
			notifications = append(notifications, notification{Kind: "failure", ReleaseKey: key, FailureKey: failureKey, Service: ev.Service, Stage: ev.Stage, Reason: ev.Reason, PreviousOK: ev.PreviousOnline})
		}
	} else if ev.Status == "succeeded" && allSucceeded(rel) && !rel.NotifiedSuccess {
		if err := checkPublicHealth(cfg, rel.HealthURL); err != nil {
			failureKey := "public-health"
			if !rel.NotifiedFailures[failureKey] {
				notifications = append(notifications, notification{Kind: "failure", ReleaseKey: key, FailureKey: failureKey, Service: "web", Stage: "public health check", Reason: err.Error(), PreviousOK: false})
			}
		} else {
			kind := "success"
			if hasNotifiedFailure(rel) {
				kind = "recovered"
			} else if rel.Rollback {
				kind = "rollback"
			}
			notifications = append(notifications, notification{Kind: kind, ReleaseKey: key})
		}
	}
	return persistAndSend(cfg, st, notifications)
}

func getRelease(st *state, ev event) (string, *release) {
	releaseID := ev.Metadata.RunID
	if releaseID == "" {
		releaseID = ev.ReleaseFallback
	}
	if releaseID == "" {
		releaseID = ev.Metadata.Revision
	}
	if releaseID == "" {
		releaseID = "unknown"
	}
	key := ev.Project + ":" + releaseID
	if existing := st.Releases[key]; existing != nil {
		return key, existing
	}
	rel := &release{
		Project: ev.Project, ReleaseID: releaseID, Metadata: ev.Metadata,
		ExpectedServices: unionServices(nil, ev.ExpectedServices), HealthURL: ev.HealthURL, AppURL: ev.AppURL,
		StartedAt: ev.At, UpdatedAt: ev.At, Services: map[string]serviceState{}, NotifiedFailures: map[string]bool{},
	}
	if previous := latestSuccessful(st, ev.Project, ""); previous != nil {
		rel.PreviousRevision = previous.Metadata.Revision
		rel.PreviousReleaseID = previous.ReleaseID
		rel.Rollback = ev.Metadata.Revision != "" && ev.Metadata.Revision != previous.Metadata.Revision && hasSuccessfulRevision(st, ev.Project, ev.Metadata.Revision)
	}
	st.Releases[key] = rel
	return key, rel
}

func latestSuccessful(st *state, project, skipKey string) *release {
	var latest *release
	for key, rel := range st.Releases {
		if key == skipKey || rel.Project != project || !rel.NotifiedSuccess {
			continue
		}
		if latest == nil || rel.UpdatedAt.After(latest.UpdatedAt) {
			latest = rel
		}
	}
	return latest
}

func hasSuccessfulRevision(st *state, project, revision string) bool {
	for _, rel := range st.Releases {
		if rel.Project == project && rel.NotifiedSuccess && rel.Metadata.Revision == revision {
			return true
		}
	}
	return false
}

func hasNotifiedFailure(rel *release) bool {
	return len(rel.NotifiedFailures) > 0
}

func allSucceeded(rel *release) bool {
	for _, service := range rel.ExpectedServices {
		if rel.Services[service].Status != "succeeded" {
			return false
		}
	}
	return len(rel.ExpectedServices) > 0
}

func historyStatus(rel *release) string {
	if rel.NotifiedSuccess {
		if hasNotifiedFailure(rel) {
			return "recovered"
		}
		if rel.Rollback {
			return "rollback"
		}
		return "succeeded"
	}
	if hasNotifiedFailure(rel) {
		return "failed"
	}
	if rel.NotifiedDelayed {
		return "delayed"
	}
	return "in progress"
}

func persistAndSend(cfg config, st *state, notifications []notification) error {
	if err := saveState(cfg, st); err != nil {
		return err
	}
	for _, item := range notifications {
		rel := st.Releases[item.ReleaseKey]
		if rel == nil {
			continue
		}
		message := formatNotification(rel, item, cfg.Now())
		appURL := rel.AppURL
		if appURL == "" {
			appURL = cfg.AppURL
		}
		if err := sendTelegram(cfg, message, rel.Metadata.PipelineURL, appURL); err != nil {
			fmt.Fprintln(os.Stderr, "deploy-telegram-notifier: Telegram delivery failed:", err)
			continue
		}
		switch item.Kind {
		case "success", "recovered", "rollback":
			rel.NotifiedSuccess = true
		case "delayed":
			rel.NotifiedDelayed = true
		case "failure":
			rel.NotifiedFailures[item.FailureKey] = true
			rel.LastFailureStage = item.Stage
			rel.LastFailureAt = cfg.Now()
		}
	}
	return saveState(cfg, st)
}

func loadState(cfg config) (*state, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	contents, err := os.ReadFile(filepath.Join(cfg.StateDir, stateFileName))
	if os.IsNotExist(err) {
		return &state{Releases: map[string]*release{}}, nil
	}
	if err != nil {
		return nil, err
	}
	st := &state{}
	if err := json.Unmarshal(contents, st); err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	if st.Releases == nil {
		st.Releases = map[string]*release{}
	}
	for _, rel := range st.Releases {
		if rel.Services == nil {
			rel.Services = map[string]serviceState{}
		}
		if rel.NotifiedFailures == nil {
			rel.NotifiedFailures = map[string]bool{}
		}
	}
	return st, nil
}

func saveState(cfg config, st *state) error {
	contents, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(cfg.StateDir, ".state-*.json")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(contents); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, filepath.Join(cfg.StateDir, stateFileName))
}

func cleanup(st *state, now time.Time) {
	for key, rel := range st.Releases {
		if now.Sub(rel.UpdatedAt) > stateMaxAge {
			delete(st.Releases, key)
		}
	}
}

func readMetadata(input io.Reader) (metadata, error) {
	contents, err := io.ReadAll(input)
	if err != nil {
		return metadata{}, err
	}
	if len(bytes.TrimSpace(contents)) == 0 {
		return metadata{}, nil
	}
	var images []dockerImage
	if err := json.Unmarshal(contents, &images); err != nil {
		return metadata{}, err
	}
	if len(images) == 0 {
		return metadata{}, nil
	}
	labels := images[0].Config.Labels
	return metadata{
		Repository:  labels["io.github.deploy-notifier.repository"],
		Branch:      labels["io.github.deploy-notifier.branch"],
		BuildNumber: labels["io.github.deploy-notifier.build-number"],
		RunID:       labels["io.github.deploy-notifier.run-id"],
		PipelineURL: labels["io.github.deploy-notifier.pipeline-url"],
		Commit:      labels["io.github.deploy-notifier.commit"],
		Author:      labels["io.github.deploy-notifier.author"],
		Revision:    labels["org.opencontainers.image.revision"],
	}, nil
}

func mergeMetadata(target *metadata, incoming metadata) {
	if incoming.Repository != "" {
		target.Repository = incoming.Repository
	}
	if incoming.Branch != "" {
		target.Branch = incoming.Branch
	}
	if incoming.BuildNumber != "" {
		target.BuildNumber = incoming.BuildNumber
	}
	if incoming.RunID != "" {
		target.RunID = incoming.RunID
	}
	if incoming.PipelineURL != "" {
		target.PipelineURL = incoming.PipelineURL
	}
	if incoming.Commit != "" {
		target.Commit = incoming.Commit
	}
	if incoming.Author != "" {
		target.Author = incoming.Author
	}
	if incoming.Revision != "" {
		target.Revision = incoming.Revision
	}
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return unionServices(nil, result)
}

func unionServices(existing, incoming []string) []string {
	set := map[string]bool{}
	for _, item := range append(append([]string{}, existing...), incoming...) {
		if item != "" {
			set[item] = true
		}
	}
	result := make([]string, 0, len(set))
	for item := range set {
		result = append(result, item)
	}
	sort.Strings(result)
	return result
}

func checkPublicHealth(cfg config, healthURL string) error {
	if healthURL == "" {
		return nil
	}
	parsed, err := url.Parse(healthURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
		return fmt.Errorf("invalid public health URL")
	}
	parsed.Path = "/" + strings.TrimLeft(parsed.Path, "/")
	healthURL = parsed.String()
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		response, err := cfg.HTTPClient.Get(healthURL)
		if err == nil && response != nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
			err = fmt.Errorf("unexpected status %d", response.StatusCode)
		}
		lastErr = err
		if attempt < 4 {
			time.Sleep(2 * time.Second)
		}
	}
	return lastErr
}

func formatNotification(rel *release, item notification, now time.Time) string {
	switch item.Kind {
	case "success":
		return "<b>🟢 Deploy Succeeded</b>\n\n" + details(rel, "") + "\nDuration: " + html.EscapeString(now.Sub(rel.StartedAt).Round(time.Second).String()) + "\nServices: " + html.EscapeString(strings.Join(rel.ExpectedServices, ", "))
	case "recovered":
		return "<b>🟡 Deploy Recovered</b>\n\n" + details(rel, "") + "\nPreviously failed at: " + html.EscapeString(fallback(rel.LastFailureStage, "Unknown")) + "\nRecovery duration: " + html.EscapeString(recoveryDuration(rel, now)) + "\nTotal duration: " + html.EscapeString(now.Sub(rel.StartedAt).Round(time.Second).String()) + "\nServices: " + html.EscapeString(strings.Join(rel.ExpectedServices, ", "))
	case "rollback":
		return "<b>↩️ Rollback Succeeded</b>\n\n" + details(rel, "") + "\nFrom: " + html.EscapeString(shortRevision(rel.PreviousRevision)) + "\nTo: " + html.EscapeString(shortRevision(rel.Metadata.Revision)) + "\nDuration: " + html.EscapeString(now.Sub(rel.StartedAt).Round(time.Second).String()) + "\nServices: " + html.EscapeString(strings.Join(rel.ExpectedServices, ", "))
	case "delayed":
		return "<b>🟠 Deploy Delayed</b>\n\n" + details(rel, "") + "\nWaiting for: " + html.EscapeString(strings.Join(waitingServices(rel), ", ")) + "\nElapsed: " + html.EscapeString(now.Sub(rel.StartedAt).Round(time.Second).String())
	default:
		message := "<b>🔴 Deploy Failed</b>\n\n" + details(rel, "") + "\nService: " + html.EscapeString(item.Service) + "\nStage: " + html.EscapeString(item.Stage)
		if item.Reason != "" {
			message += "\nReason: " + html.EscapeString(truncate(item.Reason, 180))
		}
		return message + "\nPrevious release online: " + map[bool]string{true: "yes", false: "no"}[item.PreviousOK]
	}
}

func recoveryDuration(rel *release, now time.Time) string {
	if rel.LastFailureAt.IsZero() {
		return "Unknown"
	}
	return now.Sub(rel.LastFailureAt).Round(time.Second).String()
}

func waitingServices(rel *release) []string {
	var waiting []string
	for _, service := range rel.ExpectedServices {
		if rel.Services[service].Status != "succeeded" {
			waiting = append(waiting, service)
		}
	}
	if len(waiting) == 0 {
		return []string{"public health check"}
	}
	return waiting
}

func details(rel *release, _ string) string {
	meta := rel.Metadata
	return "Project: " + html.EscapeString(fallback(rel.Project, "Unknown")) +
		"\nRepo: " + html.EscapeString(fallback(meta.Repository, "Unknown")) +
		"\nBranch: " + html.EscapeString(fallback(meta.Branch, "Unknown")) +
		"\nBuild: #" + html.EscapeString(fallback(meta.BuildNumber, "Unknown")) +
		"\nCommit: " + html.EscapeString(truncate(fallback(meta.Commit, "Unavailable"), 240)) +
		"\nAuthor: " + html.EscapeString(fallback(meta.Author, "Unknown")) +
		"\nRevision: " + html.EscapeString(shortRevision(meta.Revision))
}

func fallback(value, alternative string) string {
	if value == "" {
		return alternative
	}
	return value
}

func shortRevision(value string) string {
	if len(value) > 7 {
		return value[:7]
	}
	return fallback(value, "Unknown")
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit-1]) + "…"
}

func sendTelegram(cfg config, message, pipelineURL, appURL string) error {
	type button struct {
		Text string `json:"text"`
		URL  string `json:"url"`
	}
	type keyboard struct {
		InlineKeyboard [][]button `json:"inline_keyboard"`
	}
	payload := map[string]any{"chat_id": cfg.ChatID, "text": message, "parse_mode": "HTML", "disable_web_page_preview": true}
	if cfg.ThreadID != "" {
		payload["message_thread_id"] = cfg.ThreadID
	}
	buttons := []button{}
	if validHTTPURL(pipelineURL) {
		buttons = append(buttons, button{Text: "🔍 View Pipeline", URL: pipelineURL})
	}
	if validHTTPURL(appURL) {
		buttons = append(buttons, button{Text: "🌐 Open Application", URL: appURL})
	}
	if len(buttons) > 0 {
		payload["reply_markup"] = keyboard{InlineKeyboard: [][]button{buttons}}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	request, err := http.NewRequest(http.MethodPost, strings.TrimRight(cfg.TelegramAPIURL, "/")+"/bot"+cfg.BotToken+"/sendMessage", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := cfg.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("Telegram returned HTTP %d", response.StatusCode)
	}
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return err
	}
	if !result.OK {
		return fmt.Errorf("Telegram rejected message: %s", result.Description)
	}
	return nil
}

func validHTTPURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != ""
}
