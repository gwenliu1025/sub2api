# Project Rules

## Release Ownership

- `gwenliu1025/sub2api` is the release source for this fork. Upstream changes
  are merged into this repository first; production artifacts must be built
  only from this repository.
- The current release target is `v0.1.151`. Its application version is
  `0.1.151`, and its only GHCR image tag is
  `ghcr.io/gwenliu1025/sub2api:0.1.151`.
- Every future release follows the same exact mapping:
  `vX.Y.Z` -> `X.Y.Z` -> `ghcr.io/gwenliu1025/sub2api:X.Y.Z`.
- Do not publish or reference `latest`, major-only, minor-only,
  architecture-specific, bootstrap, testing, health-test, or other suffix
  tags.
- The tag `X.Y.Z` is one multi-platform OCI image for `linux/amd64` and
  `linux/arm64`. Do not create `-amd64` or `-arm64` aliases.

## Docker Update Contract

- A Docker update is eligible only when both the GitHub Release `vX.Y.Z` in
  `gwenliu1025/sub2api` and the matching GHCR image
  `ghcr.io/gwenliu1025/sub2api:X.Y.Z` exist.
- The management panel must keep the two-step interaction:
  1. Prepare Image pulls and verifies the exact image without interrupting
     service.
  2. Restart To Switch recreates only `sub2api`, checks health, and restores
     the previous exact image if activation fails.
- Docker Compose files must use `image: ${SUB2API_IMAGE}`. The host updater is
  the only component allowed to change that value during a management-panel
  Docker update.
- The application must never receive Docker socket access. It communicates
  only with the root-owned updater through the configured Unix socket.

## Current Scope

- Finish and publish `v0.1.151` from this fork.
- Do not make frontend redesign changes, deploy to any server, change DNS, or
  migrate machines as part of this release task.
