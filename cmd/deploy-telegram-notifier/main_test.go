package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T, serverURL string) config {
	t.Helper()
	return config{
		BotToken: "token", ChatID: "123", StateDir: t.TempDir(), TelegramAPIURL: serverURL,
		HTTPClient: &http.Client{Timeout: time.Second}, Now: func() time.Time { return time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC) },
	}
}

func imageJSON(runID, revision string) io.Reader {
	return strings.NewReader(`[{"Config":{"Labels":{"io.github.deploy-notifier.repository":"owner/app","io.github.deploy-notifier.branch":"main","io.github.deploy-notifier.build-number":"265","io.github.deploy-notifier.run-id":"` + runID + `","io.github.deploy-notifier.pipeline-url":"https://github.com/owner/app/actions/runs/` + runID + `","io.github.deploy-notifier.commit":"fix: <health>","io.github.deploy-notifier.author":"Ada","org.opencontainers.image.revision":"` + revision + `"}}}]`)
}

func TestSuccessfulReleaseSendsOneAggregatedNotification(t *testing.T) {
	var messages [][]byte
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		messages = append(messages, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer telegram.Close()
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer health.Close()
	cfg := testConfig(t, telegram.URL)

	for _, service := range []string{"web", "api"} {
		err := runEvent(cfg, []string{"--project", "worth-split", "--service", service, "--status", "succeeded", "--stage", "healthcheck", "--expected-services", "api,web", "--health-url", health.URL}, imageJSON("123", "abcdef123456"))
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(messages[0], &payload); err != nil {
		t.Fatal(err)
	}
	message := payload.Text
	for _, want := range []string{"Deploy Succeeded", "Build: #265", "Commit: fix: &lt;health&gt;"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message %q does not include %q", message, want)
		}
	}
	if !bytes.Contains(messages[0], []byte("View Pipeline")) {
		t.Fatal("pipeline button is missing")
	}
}

func TestFailureIsDeduplicatedAndDoesNotBlockRecovery(t *testing.T) {
	var messageCount int
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		messageCount++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer telegram.Close()
	cfg := testConfig(t, telegram.URL)
	args := []string{"--project", "worth-split", "--service", "api", "--status", "failed", "--stage", "migration", "--expected-services", "api,web", "--previous-online"}
	if err := runEvent(cfg, args, imageJSON("124", "abcdef123456")); err != nil {
		t.Fatal(err)
	}
	if err := runEvent(cfg, args, imageJSON("124", "abcdef123456")); err != nil {
		t.Fatal(err)
	}
	if messageCount != 1 {
		t.Fatalf("failure messages = %d, want 1", messageCount)
	}
}

func TestSweepReportsIncompleteRelease(t *testing.T) {
	var messages int
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		messages++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer telegram.Close()
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	cfg := testConfig(t, telegram.URL)
	cfg.Now = func() time.Time { return now }
	if err := runEvent(cfg, []string{"--project", "worth-split", "--service", "api", "--status", "started", "--stage", "pull", "--expected-services", "api,web"}, imageJSON("125", "abcdef123456")); err != nil {
		t.Fatal(err)
	}
	cfg.Now = func() time.Time { return now.Add(releaseTimeout + time.Second) }
	if err := runSweep(cfg); err != nil {
		t.Fatal(err)
	}
	if messages != 1 {
		t.Fatalf("messages = %d, want 1", messages)
	}
}

func TestPublicHealthFailureIsReported(t *testing.T) {
	var messages int
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		messages++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer telegram.Close()
	health := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer health.Close()
	cfg := testConfig(t, telegram.URL)
	cfg.HTTPClient = &http.Client{Timeout: 10 * time.Millisecond}
	for _, service := range []string{"api", "web"} {
		if err := runEvent(cfg, []string{"--project", "worth-split", "--service", service, "--status", "succeeded", "--stage", "healthcheck", "--expected-services", "api,web", "--health-url", health.URL}, imageJSON("126", "abcdef123456")); err != nil {
			t.Fatal(err)
		}
	}
	if messages != 1 {
		t.Fatalf("messages = %d, want 1", messages)
	}
}

func TestRollbackWithSameRevisionUsesDistinctRunIDs(t *testing.T) {
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer telegram.Close()
	cfg := testConfig(t, telegram.URL)
	for _, runID := range []string{"127", "128"} {
		if err := runEvent(cfg, []string{"--project", "worth-split", "--service", "api", "--status", "started", "--stage", "pull", "--expected-services", "api,web"}, imageJSON(runID, "same-sha")); err != nil {
			t.Fatal(err)
		}
	}
	st, err := loadState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Releases) != 2 {
		t.Fatalf("releases = %d, want 2", len(st.Releases))
	}
}

func TestTelegramFailureDoesNotFailTheDeploymentEvent(t *testing.T) {
	telegram := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer telegram.Close()
	cfg := testConfig(t, telegram.URL)
	for _, service := range []string{"api", "web"} {
		if err := runEvent(cfg, []string{"--project", "worth-split", "--service", service, "--status", "succeeded", "--stage", "healthcheck", "--expected-services", "api,web"}, imageJSON("129", "abcdef123456")); err != nil {
			t.Fatal(err)
		}
	}
	st, err := loadState(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.Releases["worth-split:129"].NotifiedSuccess {
		t.Fatal("delivery failure must leave the release eligible for a later retry")
	}
}

func TestStateIsWrittenAtomicallyWithPrivatePermissions(t *testing.T) {
	cfg := testConfig(t, "https://example.test")
	st := &state{Releases: map[string]*release{}}
	if err := saveState(cfg, st); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.StateDir, stateFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("permissions = %o, want 600", info.Mode().Perm())
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(contents, []byte(`"releases"`)) {
		t.Fatalf("unexpected state: %s", contents)
	}
}
