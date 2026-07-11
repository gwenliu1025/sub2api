# Docker Update Agent Design

**Date:** 2026-07-10
**Status:** Implemented; release policy aligned for `v0.1.150`
**Release owner:** `gwenliu1025/sub2api`

## Purpose

Docker deployments update through a root-owned host updater instead of
replacing the application binary inside a running container. The administrator
uses two deliberate actions:

1. **Prepare Image** pulls and verifies
   `ghcr.io/gwenliu1025/sub2api:X.Y.Z` without recreating a container.
2. **Restart To Switch** recreates only `sub2api`, checks Docker and HTTP
   health, and automatically restores the previous exact image if activation
   fails.

The application never receives Docker socket access. It reaches the updater
only through a Unix-domain socket.

## Version And Artifact Contract

The fork is the artifact source. Upstream releases are not deployed directly.
Each published version has exactly these identifiers:

```text
GitHub Release: vX.Y.Z
Application:    X.Y.Z
GHCR image:     ghcr.io/gwenliu1025/sub2api:X.Y.Z
```

The GHCR tag is a multi-platform OCI image for `linux/amd64` and
`linux/arm64`. No `latest`, bootstrap, test, health-test, or
architecture-specific image tags are part of this contract.

A management-panel Docker update proceeds only when both artifacts exist in
the fork:

- GitHub Release `vX.Y.Z`;
- GHCR image `ghcr.io/gwenliu1025/sub2api:X.Y.Z` with matching source and
  version OCI metadata.

## Compose Contract

All Docker Compose variants use:

```yaml
services:
  sub2api:
    image: ${SUB2API_IMAGE}
```

`SUB2API_IMAGE` always contains an exact release tag. During activation, the
host updater atomically replaces only that environment value and executes
Compose for only the `sub2api` service.

## Current Release Scope

`v0.1.150` is being prepared and published from this fork only. This work does
not deploy to a host, move existing production workloads, change DNS, or
merge a later upstream version. Those are separate decisions after the
release assets have been verified.
