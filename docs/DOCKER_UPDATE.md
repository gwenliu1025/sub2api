# Docker Update

Sub2API Docker deployments use a root-owned host updater and two explicit
management actions.

## Artifact Contract

```text
GitHub Release: vX.Y.Z
Application:    X.Y.Z
GHCR image:     ghcr.io/gwenliu1025/sub2api:X.Y.Z
```

The fork is the artifact source. Use exact release tags only. Do not publish or
deploy floating, suffixed, bootstrap, test, health-test, or
architecture-specific tags.

## Architecture

The application talks to `sub2api-updater.service` through
`/run/sub2api-updater/updater.sock`. The app never receives Docker socket
access.

1. Prepare Image pulls and validates the target image.
2. Restart To Switch recreates only `sub2api`.
3. The updater checks Docker and HTTP health.
4. Failed activation restores the previous exact image.

## Compose

```yaml
services:
  sub2api:
    image: ${SUB2API_IMAGE}
    volumes:
      - /run/sub2api-updater:/run/sub2api-updater:ro
    environment:
      - UPDATE_REPO=${UPDATE_REPO:-gwenliu1025/sub2api}
      - UPDATE_MODE=${UPDATE_MODE:-docker_agent}
      - UPDATE_AGENT_SOCKET=${UPDATE_AGENT_SOCKET:-/run/sub2api-updater/updater.sock}
      - UPDATE_AGENT_TIMEOUT_SECONDS=${UPDATE_AGENT_TIMEOUT_SECONDS:-600}
      - UPDATE_IMAGE_REPOSITORY=${UPDATE_IMAGE_REPOSITORY:-ghcr.io/gwenliu1025/sub2api}
```

## Installation

```bash
sudo ./deploy/updater/install.sh \
  --compose-directory /home/ubuntu/sub2api \
  --compose-file /home/ubuntu/sub2api/docker-compose.yml \
  --environment-file /home/ubuntu/sub2api/.env \
  --service-name sub2api \
  --container-name sub2api \
  --app-uid 0 \
  --socket-gid 0 \
  --expected-architecture amd64 \
  --health-url http://127.0.0.1:8080/health \
  --image-repository ghcr.io/gwenliu1025/sub2api \
  --image-source https://github.com/gwenliu1025/sub2api
```

Choose `app-uid` and `socket-gid` to match the deployed container.

## Verification

```bash
systemctl is-active sub2api-updater.service
curl --unix-socket /run/sub2api-updater/updater.sock \
  http://localhost/v1/health
curl --unix-socket /run/sub2api-updater/updater.sock \
  http://localhost/v1/status
docker compose -f /home/ubuntu/sub2api/docker-compose.yml config
```

Development and manual rehearsal happen on the graduation machine first.
Production deployment requires explicit approval.
