# Sub2API Host Docker Updater

This service is a root-owned host agent for updating only the Sub2API
container. It listens on a Unix domain socket, never on TCP. Linux
`SO_PEERCRED` identifies each caller, and only UIDs listed in the root-owned
configuration are dispatched to the HTTP handler. The installed socket is
owned by the configured GID with mode `0660`.

The agent verifies the configured GHCR image source, version label,
architecture, and image identity before activation. State is persisted under
`/var/lib/sub2api-updater` so interrupted work can be reconciled at startup.

## API

The HTTP/1.1 API is available only through
`/run/sub2api-updater/updater.sock`:

- `GET /v1/health` returns readiness and protocol version.
- `POST /v1/prepare` accepts exactly `{"version":"0.1.150"}` with
  `Content-Type: application/json`.
- `POST /v1/activate` requires an empty body and returns `202` after the
  activating state and response have been flushed; a daemon worker then
  performs activation.
- `GET /v1/status` returns the last persisted public state.

Every state response has exactly six fields: `state`, `current_image`,
`target_image`, `previous_image`, `message`, and `updated_at`. Internal Docker
image IDs are persisted only when required by the core and are never returned
by the HTTP API.

The interaction is deliberately two-step. First call prepare and wait for a
`prepared` state. Then call activate and poll status until it reaches a
terminal state such as `healthy`, `rolled_back`, `failed`, or
`rollback_failed`.

## Host Installation (Run Later)

Run from the repository checkout only when you intentionally prepare a target
host for Docker management-panel updates:

```bash
sudo ./deploy/updater/install.sh \
  --compose-directory /home/ubuntu/sub2api \
  --compose-file /home/ubuntu/sub2api/docker-compose.yml \
  --environment-file /home/ubuntu/sub2api/.env \
  --service-name sub2api \
  --container-name sub2api \
  --app-uid 1000 \
  --socket-gid 1000 \
  --expected-architecture amd64 \
  --health-url http://127.0.0.1:8080/health \
  --image-repository ghcr.io/gwenliu1025/sub2api \
  --image-source https://github.com/gwenliu1025/sub2api \
  --prepare-timeout-seconds 600 \
  --activation-timeout-seconds 120 \
  --poll-interval-seconds 2
```

The installer copies the updater into `/opt/sub2api-updater`, writes a
root-owned mode `0600` configuration under `/etc/sub2api-updater`, installs
the systemd unit, and verifies `GET /v1/health`. It does not run Docker
Compose, edit `.env`, edit the Compose file, or switch the running image.

## Requests

```bash
curl --unix-socket /run/sub2api-updater/updater.sock \
  http://localhost/v1/health

curl --unix-socket /run/sub2api-updater/updater.sock \
  http://localhost/v1/status

curl --unix-socket /run/sub2api-updater/updater.sock \
  -H 'Content-Type: application/json' \
  --data '{"version":"0.1.150"}' \
  http://localhost/v1/prepare

curl --unix-socket /run/sub2api-updater/updater.sock \
  -X POST --data '' \
  http://localhost/v1/activate
```

## Diagnostics

```bash
systemctl status sub2api-updater.service
journalctl -u sub2api-updater.service -n 100 --no-pager
ls -l /run/sub2api-updater/updater.sock
cat /var/lib/sub2api-updater/state.json
```

A caller rejected before HTTP dispatch usually has the wrong UID or lacks
permission through the configured socket GID. A missing socket usually means
the service failed configuration validation or lacks Linux `AF_UNIX`,
`SO_PEERCRED`, or `UnixStreamServer` support.

## Manual Recovery

If status is `rollback_failed`, inspect the persisted state and deployment
environment first. The only permitted Compose recovery action is:

```bash
docker compose \
  --project-directory /home/ubuntu/sub2api \
  -f /home/ubuntu/sub2api/docker-compose.yml \
  --env-file /home/ubuntu/sub2api/.env \
  up -d --no-deps --force-recreate sub2api
```

Do not recreate, restart, migrate, or otherwise touch Postgres, Redis, Caddy,
QQ, NapCat, or any other dependency. The updater and the recovery command are
scoped to `sub2api` only.
