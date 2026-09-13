# Deploy Telegram Notifier

`deploy-telegram-notifier` is a small, static Go CLI for deployment events produced by a Docker Compose updater. It has no listening port, database, container, or runtime dependency beyond the Linux binary.

It receives Docker image inspection JSON on standard input, groups the services that belong to the same GitHub Actions run, checks the public health URL once all services are healthy, and sends one Telegram message per completed release. Failures are sent immediately and deduplicated.

## Configuration

Create `/etc/deploy-telegram-notifier.env` with permissions `0600`:

```dotenv
TELEGRAM_BOT_TOKEN=123456:replace-with-the-bot-token
TELEGRAM_CHAT_ID=-1001234567890
# TELEGRAM_MESSAGE_THREAD_ID=42
# DEPLOY_NOTIFIER_STATE_DIR=/var/lib/deploy-telegram-notifier
```

The notifier expects these OCI labels in each deployed image:

```text
io.github.deploy-notifier.repository
io.github.deploy-notifier.branch
io.github.deploy-notifier.build-number
io.github.deploy-notifier.run-id
io.github.deploy-notifier.pipeline-url
io.github.deploy-notifier.commit
io.github.deploy-notifier.author
org.opencontainers.image.revision
```

## Commands

```bash
deploy-telegram-notifier test
docker image inspect example/image:tag | deploy-telegram-notifier event \
  --project worth-split --service api --status succeeded --stage healthcheck \
  --expected-services api,web --health-url https://example.com/healthz
deploy-telegram-notifier sweep
docker image inspect example/image:tag | deploy-telegram-notifier pending --project worth-split
```

`event` accepts `started`, `succeeded`, and `failed`. A failure event may use `--previous-online` when the old container was kept running. `sweep` reports a release that remains incomplete for ten minutes.
`pending` is used by the updater to retry only releases that previously failed their final readiness check.

## Build and release

```bash
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o deploy-telegram-notifier ./cmd/deploy-telegram-notifier
```

The release workflow publishes Linux `amd64` and `arm64` binaries and checksums when a `v*` tag is pushed.
