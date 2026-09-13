# Docker digest updater integration

The notifier is intentionally a CLI, not a daemon. The updater calls it after a new image is pulled, after a service health check succeeds or fails, and after a pre-deploy or recreate error. The command is best-effort: its failure must only be logged and must never stop a deployment.

## VPS installation

Install a release binary and verify its checksum before making it executable:

```bash
sudo install -m 755 deploy-telegram-notifier-linux-amd64 /usr/local/bin/deploy-telegram-notifier
sudo install -d -m 700 /var/lib/deploy-telegram-notifier
sudo install -m 600 deploy/deploy-telegram-notifier.env.example /etc/deploy-telegram-notifier.env
sudo install -d -m 755 /etc/systemd/system/docker-digest-updater.service.d
sudo install -m 644 deploy/docker-digest-updater.override.conf /etc/systemd/system/docker-digest-updater.service.d/deploy-telegram-notifier.conf
sudoedit /etc/deploy-telegram-notifier.env
sudo systemctl daemon-reload
sudo systemctl restart docker-digest-updater.service
sudo sh -c 'set -a; . /etc/deploy-telegram-notifier.env; set +a; exec /usr/local/bin/deploy-telegram-notifier test'
```

For a user systemd service, put the same environment variables in the user service environment instead of a root-owned file and set `DEPLOY_TELEGRAM_NOTIFIER_BIN` to the installed binary.

## Required Compose labels

Each updatable service must define the same project name, expected service set, and public health URL. The URL is checked only while a deployment is awaiting its terminal success notification.

```yaml
labels:
  auto-update.notify-project: my-project
  auto-update.notify-services: api,web
  auto-update.notify-health-url: https://app.example.com/healthz
  auto-update.notify-app-url: https://app.example.com
```

The project image workflow must add the OCI labels listed in the main README. The notifier groups API and web only when their `io.github.deploy-notifier.run-id` values match, so a rolling publication of mobile Docker tags cannot generate a premature success message.

## Operational behavior

- `pull`, `migration`, `recreate`, and health-check failures send one immediate, deduplicated message with a short safe reason.
- A deployment still incomplete after five minutes sends one delayed notification; after ten minutes it sends the existing incomplete-release failure. Set `DEPLOY_NOTIFIER_DELAY_THRESHOLD` to change the first threshold.
- A successful notification is sent once all expected services for one run are healthy and the public health URL returns `200`. A release that had emitted a failure first is reported as recovered.
- Republishing a revision that was already successfully online after a newer revision is reported as a rollback.
- `history --project my-project` lists the local 30-day release history.
- The existing updater invokes `pending` only after a deployment failure, allowing readiness recovery without turning the notifier into uptime monitoring.

## Safe notification tests

Load the protected environment and run a synthetic scenario. These commands only write isolated `deploy-notifier-test-*` entries to the notifier state; they do not use Docker or modify a deployed application.

```bash
sudo sh -c 'set -a; . /etc/deploy-telegram-notifier.env; set +a; exec /usr/local/bin/deploy-telegram-notifier test --scenario failure'
sudo sh -c 'set -a; . /etc/deploy-telegram-notifier.env; set +a; exec /usr/local/bin/deploy-telegram-notifier test --scenario recovery'
sudo sh -c 'set -a; . /etc/deploy-telegram-notifier.env; set +a; exec /usr/local/bin/deploy-telegram-notifier test --scenario delayed'
sudo sh -c 'set -a; . /etc/deploy-telegram-notifier.env; set +a; exec /usr/local/bin/deploy-telegram-notifier test --scenario rollback'
```
- `sweep` reports an incomplete release after ten minutes and removes state older than thirty days.
