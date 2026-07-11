# Official Docker Release Policy Design

**Date:** 2026-07-11

## Goal

Make the Docker update path predictable for administrators: every version shown
in the Sub2API management panel maps to one GitHub Release and one GHCR image
with the same upstream-style version number.

For example:

```text
GitHub Release: v0.1.150
Application version: 0.1.150
GHCR image: ghcr.io/gwenliu1025/sub2api:0.1.150
```

The management panel will continue to prepare that exact image first, then
restart only `sub2api` to activate it. No release-specific image aliases are
part of this contract.

## Version Contract

- A releasable version is exactly `vX.Y.Z`, where `X`, `Y`, and `Z` are
  decimal integers.
- The application reports the same value without the leading `v`.
- The only published GHCR tag for that release is the same application version
  without the leading `v`.
- Published tags must not use `latest`, major-only, minor-only, architecture,
  bootstrap, testing, feature, or health-test suffixes.
- The GHCR tag is a multi-platform OCI index for `linux/amd64` and
  `linux/arm64`, so users never need an architecture-specific tag.
- A Docker-mode management-panel update is eligible only when both
  `vX.Y.Z` exists as a GitHub Release in `UPDATE_REPO` and
  `ghcr.io/gwenliu1025/sub2api:X.Y.Z` exists with matching OCI source,
  version, and architecture metadata.

## Release Pipeline

The normal release workflow is the only image publishing path:

1. An annotated `vX.Y.Z` tag is pushed.
2. The workflow rejects tags outside the exact three-part numeric format.
3. GoReleaser creates the GitHub Release and its binary assets.
4. Docker Buildx builds and publishes one multi-platform GHCR image tagged
   only `X.Y.Z`, passing the same application version and OCI metadata into
   the build.
5. The release workflow fails if either the Release or image publication
   fails.

The release workflow must not create `latest`, architecture-specific, or
temporary image aliases.

## Management-Panel Update Behavior

For a published `vX.Y.Z`:

1. **Prepare image** reads `vX.Y.Z` from GitHub Release metadata, pulls
   `ghcr.io/gwenliu1025/sub2api:X.Y.Z`, and verifies its OCI metadata. This
   does not edit `.env` or recreate a container.
2. **Restart to switch** writes that exact image reference to
   `SUB2API_IMAGE`, recreates only `sub2api`, waits for Docker and HTTP
   health, and restores the exact previous `.env` content and previous image
   if activation fails.

Release assets alone are not enough in Docker mode. They identify the desired
version; the matching GHCR image is the runnable artifact.

## Future Host Migration

An existing deployment that does not yet include the host updater cannot use
the management-panel Docker workflow. When a target host is intentionally
prepared later, its first migration is a one-time operator action:

1. Install the root-owned host updater and add its Compose socket mount and
   environment settings without recreating any container.
2. Pull the official `ghcr.io/gwenliu1025/sub2api:0.1.150` image.
3. Recreate only `sub2api` directly onto that official image and verify it is
   healthy.

After that migration, all future versions use only the two management-panel
steps. No bootstrap image is published or deployed.

## Removed Approach

The earlier bootstrap image, suffix tags, `latest` aliases, and deliberately
unhealthy test images are removed. Automatic rollback remains covered by the
updater's local unit tests and by normal activation failure handling; it does
not require publishing a non-release image.

## Verification

- Shell tests reject aliases and accept only a matching `vX.Y.Z` release tag
  and `X.Y.Z` image tag.
- Workflow static checks confirm the image publisher receives exactly one
  GHCR tag and builds both supported architectures.
- Existing updater, Go, and frontend tests continue to pass.
- Before any future host deployment, inspect the official OCI image to confirm
  its source label, version label, and `amd64` manifest match `v0.1.150`.
