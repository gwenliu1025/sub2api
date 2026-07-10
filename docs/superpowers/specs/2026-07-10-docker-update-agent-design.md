# Docker Update Agent Design

**Date:** 2026-07-10  
**Status:** Approved for implementation  
**Target:** XiaoQianAPI Sub2API fork and graduation-machine deployment

## 1. Goal

Replace the unreliable in-container binary replacement path for Docker deployments with a host-managed image update flow while preserving the existing two-step administrator interaction:

1. **Prepare update:** pull and verify the target GHCR image without interrupting service.
2. **Restart and switch:** recreate only the `sub2api` container, verify health, and automatically restore the previous image if activation fails.

The update must survive later container recreation. PostgreSQL, Redis, Caddy, Mihomo, CPA, Kiro, and other services must remain untouched.

## 2. Current Problem

The current admin update endpoint downloads a GitHub Release archive and replaces the running executable beside `/app/sub2api`. Docker starts the process as UID/GID `1000`, while `/app` is owned by `root:root` and is not writable by that user. The update therefore fails while creating `/app/.sub2api-update-*`.

Even if `/app` were made writable, replacing a binary in the container writable layer would not update the configured Docker image. A later `docker compose up --force-recreate` would restore the binary embedded in the old image.

`UPDATE_REPO` correctly controls the GitHub repository used for release metadata and binary assets, but it does not make the admin update endpoint pull or activate a Docker image.

## 3. Non-Goals

- Do not mount `/var/run/docker.sock` into the Sub2API container.
- Do not expose a TCP administration endpoint on the host.
- Do not accept arbitrary image repositories, Compose paths, service names, or shell commands from the application.
- Do not rebuild or restart database, cache, proxy, mail, QQ, CPA, Kiro, or Caddy services.
- Do not add a general-purpose deployment platform.
- Do not promise a fixed ten-second image download time; only the activation window is expected to be a few seconds under normal conditions.

## 4. Considered Approaches

### 4.1 Host systemd agent over a Unix socket

Run a narrowly scoped updater as root on the host. Sub2API communicates through a Unix-domain socket mounted read-only into the application container.

This is the selected approach because Docker privileges remain outside the application container, requests are limited to a small protocol, and the host can perform durable Compose image changes and rollback.

### 4.2 Docker socket mounted into Sub2API

This is rejected. Access to the Docker socket is effectively host-root access and would substantially increase the impact of an application compromise.

### 4.3 Continue replacing the binary in the container

This is rejected as the primary Docker strategy. It can be fast but is not durable across container recreation. The legacy binary updater remains available only for non-Docker installations.

## 5. Architecture

### 5.1 Sub2API application

The existing update service gains a deployment mode:

- `binary`: existing GitHub Release archive behavior.
- `docker_agent`: delegate image preparation and activation to the host updater.

Configuration:

```text
UPDATE_MODE=binary|docker_agent
UPDATE_AGENT_SOCKET=/run/sub2api-updater/updater.sock
UPDATE_AGENT_TIMEOUT_SECONDS=600
UPDATE_IMAGE_REPOSITORY=ghcr.io/gwenliu1025/sub2api
```

Defaults preserve existing installations:

- `UPDATE_MODE=binary`
- `UPDATE_AGENT_SOCKET=/run/sub2api-updater/updater.sock`
- `UPDATE_AGENT_TIMEOUT_SECONDS=600`
- `UPDATE_IMAGE_REPOSITORY=ghcr.io/gwenliu1025/sub2api`

In `docker_agent` mode:

- Version discovery and allowed rollback-version discovery continue to use `UPDATE_REPO`.
- `PerformUpdate` asks the agent to prepare the latest release version.
- Versioned rollback asks the agent to prepare the selected older release version.
- The restart endpoint asks the agent to activate the prepared image instead of exiting the current process.
- Legacy local `.backup` rollback remains available only in `binary` mode.

### 5.2 Host updater

The host updater is a Python 3 standard-library service installed under:

```text
/opt/sub2api-updater/sub2api_updater.py
/etc/sub2api-updater/config.json
/etc/systemd/system/sub2api-updater.service
/var/lib/sub2api-updater/state.json
/run/sub2api-updater/updater.sock
```

Python is selected because Ubuntu 24.04 already provides it, the service can remain dependency-free, and deployment does not require a second compiled release artifact.

The updater:

- runs as root under systemd;
- listens only on a Unix-domain socket;
- creates the socket with mode `0660` and configured group ID `1000`;
- validates Linux peer credentials and permits only UID `0` or configured application UID `1000`;
- invokes subprocesses with argument arrays and never through a shell;
- logs to journald;
- serializes operations with a process lock;
- atomically persists non-secret state.

### 5.3 Compose integration

The application image becomes variable-driven:

```yaml
services:
  sub2api:
    image: ${SUB2API_IMAGE}
    volumes:
      - /run/sub2api-updater:/run/sub2api-updater:ro
```

The host `.env` initially records the exact image currently in use:

```text
SUB2API_IMAGE=xqian/sub2api:equivalent-cache-20260709-083433
UPDATE_MODE=docker_agent
UPDATE_AGENT_SOCKET=/run/sub2api-updater/updater.sock
UPDATE_IMAGE_REPOSITORY=ghcr.io/gwenliu1025/sub2api
```

Installing the updater therefore does not itself switch the running version.

## 6. Agent Protocol

The protocol is HTTP/1.1 over the Unix socket. Responses are JSON. There is no TCP listener.

### 6.1 Health

```text
GET /v1/health
```

Returns agent readiness and protocol version.

### 6.2 Prepare

```text
POST /v1/prepare
Content-Type: application/json

{"version":"0.1.150"}
```

The request contains only a version. Repository, Compose directory, service name, environment file, health URL, and allowed peer IDs come exclusively from the root-owned agent configuration.

The operation is synchronous and may take up to the configured application timeout. It:

1. validates a numeric release version;
2. constructs the image from the configured fixed repository;
3. runs `docker pull` for the exact tag;
4. inspects the pulled image;
5. verifies architecture, source label, and version label;
6. records the actual current container image and the prepared target image;
7. returns `prepared`.

It does not modify `.env` and does not recreate a container.

Preparing the same target twice is idempotent. Preparing a different target replaces the prior pending target only when no activation is running.

### 6.3 Activate

```text
POST /v1/activate
```

The agent validates that a prepared target exists, schedules activation in a background worker, and returns `202 Accepted` before the current application container is replaced.

### 6.4 Status

```text
GET /v1/status
```

Returns:

```json
{
  "state": "idle|preparing|prepared|activating|healthy|rolled_back|failed|rollback_failed",
  "current_image": "string",
  "target_image": "string",
  "previous_image": "string",
  "message": "string",
  "updated_at": "RFC3339 timestamp"
}
```

The state file contains no credentials, tokens, Compose environment contents, or command output that may contain secrets.

## 7. State Machine

```text
idle/healthy/rolled_back/failed
  -> preparing
  -> prepared
  -> activating
     -> healthy
     -> rolled_back
     -> rollback_failed
```

Rules:

- Only one prepare or activation operation may run at a time.
- `activate` is valid only from `prepared`.
- A process restart reloads state from disk.
- A stale `preparing` or `activating` state is reconciled against the actual container and configured image before accepting another operation.
- Status errors are explicit and never collapsed to `internal error` in the admin UI.

## 8. Activation and Automatic Rollback

Activation performs:

1. read and retain the exact existing `.env` bytes;
2. atomically replace or append `SUB2API_IMAGE=<target>`;
3. run:

   ```text
   docker compose --project-directory <compose-dir> up -d --no-deps --force-recreate sub2api
   ```

4. verify the running container image equals the target;
5. wait for Docker health status `healthy`;
6. verify the host HTTP health URL returns `200`;
7. mark the operation `healthy`.

The default activation timeout is 120 seconds.

If any activation check fails:

1. atomically restore the exact previous `.env` bytes;
2. recreate only `sub2api`;
3. verify Docker and HTTP health for the previous image;
4. mark the operation `rolled_back`.

If restoration also fails, mark `rollback_failed`, preserve diagnostics in journald, and leave all unrelated services untouched.

## 9. Admin Interface

The visible interaction remains two-step.

### 9.1 Update

- Button text and loading state indicate image preparation.
- The request remains pending while the image is pulled and verified.
- Success displays the prepared version and enables the restart action.
- Pull, validation, disk-space, socket, or agent errors display their sanitized specific message.

### 9.2 Restart

- The restart request returns before activation starts.
- The page polls service health for up to 120 seconds instead of relying on a fixed eight-second countdown.
- After service recovery, the page fetches agent status through a new authenticated backend status endpoint.
- `healthy` reloads the page and displays the new version.
- `rolled_back` reports that activation failed and the previous version was restored.
- `rollback_failed` displays a high-severity recovery message.

### 9.3 Versioned rollback

Rollback keeps the same two-step model:

1. choose an allowed older GitHub release and prepare its GHCR image;
2. restart to activate it with the same health checks and automatic restoration.

## 10. Security Model

- Docker Socket is never mounted into Sub2API.
- The updater has no network listener.
- Unix-socket peer UID validation is mandatory.
- Agent configuration is root-owned and not writable by UID `1000`.
- Only `ghcr.io/gwenliu1025/sub2api:<validated-version>` may be pulled.
- Versions must contain numeric dot-separated components only for the initial implementation.
- The service name is fixed to `sub2api`.
- Compose and environment paths are fixed in root-owned configuration.
- No request value is interpolated into a shell command.
- Image labels must identify the configured GitHub source and exact version.
- `.env` writes are atomic and retain mode and ownership.
- Agent state and logs must not include environment-file contents.

## 11. Error Handling

The backend maps agent failures into explicit administrator-facing categories:

- agent unavailable;
- permission denied on Unix socket;
- invalid target version;
- image pull failed;
- image verification failed;
- no prepared update;
- activation already running;
- activation timed out and rolled back;
- activation failed and rollback failed.

Unexpected internal details remain in application or journald logs, while the UI receives a stable sanitized message and operation state.

## 12. Testing

### 12.1 Python updater tests

Use `unittest`, temporary directories, a fake command runner, and a temporary Unix socket.

Required cases:

- peer UID accepted and rejected;
- version validation;
- exact image construction;
- pull success and failure;
- image source/version/architecture validation;
- prepare does not mutate `.env`;
- atomic environment update;
- activation success;
- activation failure followed by successful rollback;
- activation and rollback both failing;
- operation locking and idempotent prepare;
- state reload and stale-state reconciliation;
- no secret environment content in state or error payloads.

### 12.2 Go backend tests

- default binary mode remains unchanged;
- Docker-agent mode prepares latest version;
- versioned rollback prepares the selected version;
- restart delegates to agent activation and does not call process exit;
- agent errors map to explicit API responses;
- status endpoint maps all agent states;
- Unix-socket HTTP transport timeout and cancellation.

### 12.3 Frontend tests

- preparing state;
- prepared-success state;
- health polling beyond eight seconds;
- successful activation and reload;
- automatic rollback message;
- rollback-failed message;
- specific prepare errors are displayed.

### 12.4 Deployment verification

On the graduation machine:

- agent health and socket permissions;
- Sub2API can access the socket as UID `1000`;
- prepare pulls the release without traffic interruption;
- activation recreates only `sub2api`;
- local application, Caddy, and direct-IP health return `200`;
- reported application version matches the target;
- Postgres, Redis, Caddy, Mihomo, CPA, Kiro, and other container IDs remain unchanged;
- a controlled bad-target test proves automatic rollback.

## 13. Release and Bootstrap

The first agent-capable fork release will use version `v0.1.150`.

Bootstrap sequence:

1. back up the database, Compose file, and `.env`;
2. install and start the host updater;
3. change Compose to `${SUB2API_IMAGE}` and mount the Unix socket;
4. set `.env` to the current local image so no version switch occurs;
5. manually deploy a bootstrap image containing Docker-agent support but reporting version `0.1.149`;
6. publish the full fork release `v0.1.150` with GHCR images;
7. use the management-panel two-step update to move from the bootstrap image to `v0.1.150`;
8. verify health, version, container isolation, and rollback behavior.

Production DNS remains unchanged during graduation-machine validation.

## 14. Operational Rollback

Independent of the management panel, the operator can restore the saved Compose and `.env` files and recreate only `sub2api` with the previously recorded image.

The agent installation can be disabled with:

```text
systemctl disable --now sub2api-updater
```

The application can then be returned to `UPDATE_MODE=binary` or to a manually managed fixed image without changing database state.
