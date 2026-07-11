# Docker Update Agent Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace Docker deployments' in-container binary updater with a host-managed, two-step image prepare/activate flow that performs health verification and automatic rollback without touching unrelated services.

**Architecture:** The Go application keeps GitHub release discovery but delegates Docker image preparation and activation to a root-owned Python service over an HTTP/1.1 Unix socket. The Python service accepts only numeric versions, constructs images from one configured GHCR repository, updates only `SUB2API_IMAGE`, recreates only the `sub2api` Compose service, and restores the exact previous environment file if activation fails. The Vue admin UI preserves the existing prepare-then-restart interaction and polls real health plus updater state for up to 120 seconds.

**Tech Stack:** Go 1.26, Gin, Viper, Python 3 standard library, systemd, Docker Compose v2, Vue 3, TypeScript, Pinia, Vitest, GitHub Actions, GoReleaser, GHCR

---

## File Map

**Application configuration**

- Modify `backend/internal/config/config.go`: add update mode, Unix socket, timeout, and expected image repository configuration with validation.
- Modify `backend/internal/config/config_test.go`: cover defaults, environment overrides, and invalid Docker-agent settings.
- Modify `deploy/config.example.yaml`: document the YAML equivalents.
- Modify `deploy/.env.example`: document the Docker deployment environment variables.

**Go updater integration**

- Create `backend/internal/service/update_agent_client.go`: Unix-socket HTTP client, protocol types, response limits, timeout/cancellation, and sanitized error mapping.
- Create `backend/internal/service/update_agent_client_test.go`: transport, protocol, error, timeout, and repository consistency tests.
- Modify `backend/internal/service/update_service.go`: preserve binary mode while delegating prepare, versioned rollback, activation, and status in Docker-agent mode.
- Modify `backend/internal/service/update_service_test.go`: test both deployment modes.
- Modify `backend/internal/service/wire.go`: construct the Unix-socket client from configuration.
- Regenerate `backend/cmd/server/wire_gen.go`: keep generated dependency injection in sync.

**Admin API**

- Modify `backend/internal/handler/admin/system_handler.go`: expose status, delegate restart to the agent in Docker mode, and return structured updater errors.
- Modify `backend/internal/handler/admin/system_handler_test.go`: cover prepare, activate, status, legacy restart, and error envelopes.
- Modify `backend/internal/server/routes/admin.go`: register `GET /api/v1/admin/system/update-status`.

**Host updater**

- Create `deploy/updater/updater_core.py`: validated config, command runner, atomic state/environment writes, prepare, activation, health checks, reconciliation, and rollback.
- Create `deploy/updater/sub2api_updater.py`: peer-credential-checked Unix HTTP server and background activation worker.
- Create `deploy/updater/tests/test_updater_core.py`: state machine, image validation, environment mutation, activation, rollback, idempotency, and secret-redaction tests.
- Create `deploy/updater/tests/test_sub2api_updater.py`: HTTP protocol and accepted/rejected peer tests.
- Create `deploy/updater/config.example.json`: generic root-owned configuration example.
- Create `deploy/updater/sub2api-updater.service`: systemd unit.
- Create `deploy/updater/install.sh`: deterministic root installation and configuration script.

**Container and release integration**

- Modify `deploy/docker-compose.yml`: use `${SUB2API_IMAGE}`, mount the updater socket directory read-only, and pass updater configuration.
- Modify `Dockerfile`: make source/version OCI labels explicit for manually built bootstrap images.
- Modify `deploy/Dockerfile`: keep direct Docker builds label-compatible.
- Modify `Dockerfile.goreleaser`: keep the default source label aligned with this fork; GoReleaser build flags remain authoritative.
- Create `.github/workflows/bootstrap-image.yml`: manually publish a bootstrap image that reports `0.1.149` while containing the new updater support.
- Modify `.github/workflows/backend-ci.yml`: run Python updater tests.

**Frontend**

- Modify `frontend/src/api/admin/system.ts`: add updater mode/status types, long prepare timeout, and status API.
- Modify `frontend/src/api/__tests__/admin.system.rollback.spec.ts`: cover prepare timeout, restart result, and status calls in the existing system API suite.
- Create `frontend/src/utils/updateActivation.ts`: bounded health/status polling with terminal outcomes.
- Create `frontend/src/utils/__tests__/updateActivation.spec.ts`: cover recovery after more than eight seconds, healthy, rolled-back, rollback-failed, and timeout outcomes.
- Modify `frontend/src/stores/app.ts`: retain `update_mode` from version discovery.
- Modify `frontend/src/components/common/VersionBadge.vue`: show image preparation, activate on restart, and display terminal rollback states.
- Create `frontend/src/components/common/__tests__/VersionBadge.spec.ts`: cover preparing, prepared, specific errors, activation success, automatic rollback, and rollback failure.
- Modify `frontend/src/i18n/locales/zh/misc.ts`: Chinese two-step updater messages.
- Modify `frontend/src/i18n/locales/en/misc.ts`: English two-step updater messages.

**Operational documentation**

- Create `deploy/updater/README.md`: protocol, installation, diagnostics, manual recovery, and graduation-machine commands.

### Task 1: Add the Application Update Configuration Contract

**Files:**

- Modify: `backend/internal/config/config.go`
- Modify: `backend/internal/config/config_test.go`
- Modify: `deploy/config.example.yaml`
- Modify: `deploy/.env.example`

- [ ] **Step 1: Write failing configuration tests**

Add tests with these exact expectations:

```go
func TestLoadDefaultDockerUpdateConfig(t *testing.T) {
	resetViperWithJWTSecret(t)

	cfg, err := Load()

	require.NoError(t, err)
	require.Equal(t, UpdateModeBinary, cfg.Update.Mode)
	require.Equal(t, "/run/sub2api-updater/updater.sock", cfg.Update.AgentSocket)
	require.Equal(t, 600, cfg.Update.AgentTimeoutSeconds)
	require.Equal(t, "ghcr.io/gwenliu1025/sub2api", cfg.Update.ImageRepository)
}

func TestLoadDockerUpdateConfigFromEnv(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("UPDATE_MODE", "docker_agent")
	t.Setenv("UPDATE_AGENT_SOCKET", "/run/custom/updater.sock")
	t.Setenv("UPDATE_AGENT_TIMEOUT_SECONDS", "900")
	t.Setenv("UPDATE_IMAGE_REPOSITORY", "ghcr.io/gwenliu1025/sub2api-canary")

	cfg, err := Load()

	require.NoError(t, err)
	require.Equal(t, UpdateModeDockerAgent, cfg.Update.Mode)
	require.Equal(t, "/run/custom/updater.sock", cfg.Update.AgentSocket)
	require.Equal(t, 900, cfg.Update.AgentTimeoutSeconds)
	require.Equal(t, "ghcr.io/gwenliu1025/sub2api-canary", cfg.Update.ImageRepository)
}

func TestValidateDockerUpdateConfig(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)

	tests := []struct {
		name   string
		mutate func()
		want   string
	}{
		{"mode", func() { cfg.Update.Mode = "docker" }, "update.mode"},
		{"relative socket", func() {
			cfg.Update.Mode = UpdateModeDockerAgent
			cfg.Update.AgentSocket = "updater.sock"
		}, "update.agent_socket"},
		{"timeout", func() {
			cfg.Update.Mode = UpdateModeDockerAgent
			cfg.Update.AgentSocket = "/run/sub2api-updater/updater.sock"
			cfg.Update.AgentTimeoutSeconds = 0
		}, "update.agent_timeout_seconds"},
		{"tagged repository", func() {
			cfg.Update.AgentTimeoutSeconds = 600
			cfg.Update.ImageRepository = "ghcr.io/gwenliu1025/sub2api:latest"
		}, "update.image_repository"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fresh := *cfg
			fresh.Update = cfg.Update
			tt.mutate()
			err := cfg.Validate()
			require.ErrorContains(t, err, tt.want)
			*cfg = fresh
		})
	}
}
```

- [ ] **Step 2: Run the focused tests and confirm failure**

Run:

```bash
cd backend
go test -tags=unit ./internal/config -run 'TestLoad(DefaultDockerUpdateConfig|DockerUpdateConfigFromEnv)|TestValidateDockerUpdateConfig' -v
```

Expected: compilation fails because the update constants and fields do not exist.

- [ ] **Step 3: Implement constants, fields, defaults, and validation**

Add this contract:

```go
const (
	UpdateModeBinary      = "binary"
	UpdateModeDockerAgent = "docker_agent"

	DefaultUpdateAgentSocket     = "/run/sub2api-updater/updater.sock"
	DefaultUpdateImageRepository = "ghcr.io/gwenliu1025/sub2api"
)

type UpdateConfig struct {
	Repo                string `mapstructure:"repo"`
	ProxyURL            string `mapstructure:"proxy_url"`
	Mode                string `mapstructure:"mode"`
	AgentSocket         string `mapstructure:"agent_socket"`
	AgentTimeoutSeconds int    `mapstructure:"agent_timeout_seconds"`
	ImageRepository     string `mapstructure:"image_repository"`
}
```

Set these defaults:

```go
viper.SetDefault("update.repo", DefaultUpdateRepo)
viper.SetDefault("update.mode", UpdateModeBinary)
viper.SetDefault("update.agent_socket", DefaultUpdateAgentSocket)
viper.SetDefault("update.agent_timeout_seconds", 600)
viper.SetDefault("update.image_repository", DefaultUpdateImageRepository)
```

Validation must lowercase and trim the mode, allow only `binary` and `docker_agent`, require an absolute clean socket path, limit the timeout to `1..3600`, and validate an untagged GHCR repository with:

```go
var updateImageRepositoryPattern = regexp.MustCompile(
	`^ghcr\.io/[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?/[a-z0-9](?:[a-z0-9._-]*[a-z0-9])?$`,
)
```

- [ ] **Step 4: Add deployment examples**

Add to `deploy/.env.example`:

```dotenv
SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:latest
UPDATE_MODE=binary
UPDATE_AGENT_SOCKET=/run/sub2api-updater/updater.sock
UPDATE_AGENT_TIMEOUT_SECONDS=600
UPDATE_IMAGE_REPOSITORY=ghcr.io/gwenliu1025/sub2api
```

Add to `deploy/config.example.yaml`:

```yaml
update:
  repo: "gwenliu1025/sub2api"
  proxy_url: ""
  mode: "binary"
  agent_socket: "/run/sub2api-updater/updater.sock"
  agent_timeout_seconds: 600
  image_repository: "ghcr.io/gwenliu1025/sub2api"
```

- [ ] **Step 5: Run tests and commit**

Run:

```bash
cd backend
go test -tags=unit ./internal/config -v
```

Expected: PASS.

Commit:

```bash
git add backend/internal/config/config.go backend/internal/config/config_test.go deploy/config.example.yaml deploy/.env.example
git commit -m "feat: add docker update agent configuration"
```

### Task 2: Build the Go Unix-Socket Agent Client

**Files:**

- Create: `backend/internal/service/update_agent_client.go`
- Create: `backend/internal/service/update_agent_client_test.go`

- [ ] **Step 1: Write failing protocol and transport tests**

Define tests for:

```go
func TestUnixUpdateAgentClientPrepareUsesUnixSocketAndExactVersion(t *testing.T)
func TestUnixUpdateAgentClientActivateAccepts202(t *testing.T)
func TestUnixUpdateAgentClientStatusDecodesAllFields(t *testing.T)
func TestUnixUpdateAgentClientRejectsUnexpectedTargetRepository(t *testing.T)
func TestUnixUpdateAgentClientMapsAgentErrors(t *testing.T)
func TestUnixUpdateAgentClientHonorsContextCancellation(t *testing.T)
func TestUnixUpdateAgentClientLimitsResponseBody(t *testing.T)
```

The test server must listen on a temporary Unix socket and assert:

```json
{"version":"0.1.150"}
```

for `POST /v1/prepare`. A successful prepare response must include:

```json
{
  "state":"prepared",
  "current_image":"xqian/sub2api:equivalent-cache-20260709-083433",
  "target_image":"ghcr.io/gwenliu1025/sub2api:0.1.150",
  "previous_image":"xqian/sub2api:equivalent-cache-20260709-083433",
  "message":"image prepared",
  "updated_at":"2026-07-10T08:00:00Z"
}
```

- [ ] **Step 2: Run tests and confirm failure**

Run:

```bash
cd backend
go test -tags=unit ./internal/service -run 'TestUnixUpdateAgentClient' -v
```

Expected: compilation fails because the client does not exist.

- [ ] **Step 3: Implement protocol types and interface**

Use these exact public types:

```go
type UpdateAgentState string

const (
	UpdateAgentIdle           UpdateAgentState = "idle"
	UpdateAgentPreparing      UpdateAgentState = "preparing"
	UpdateAgentPrepared       UpdateAgentState = "prepared"
	UpdateAgentActivating     UpdateAgentState = "activating"
	UpdateAgentHealthy        UpdateAgentState = "healthy"
	UpdateAgentRolledBack     UpdateAgentState = "rolled_back"
	UpdateAgentFailed         UpdateAgentState = "failed"
	UpdateAgentRollbackFailed UpdateAgentState = "rollback_failed"
)

type UpdateAgentStatus struct {
	State         UpdateAgentState `json:"state"`
	CurrentImage  string           `json:"current_image"`
	TargetImage   string           `json:"target_image"`
	PreviousImage string           `json:"previous_image"`
	Message       string           `json:"message"`
	UpdatedAt     string           `json:"updated_at"`
}

type UpdateAgentClient interface {
	Prepare(ctx context.Context, version string) (*UpdateAgentStatus, error)
	Activate(ctx context.Context) (*UpdateAgentStatus, error)
	Status(ctx context.Context) (*UpdateAgentStatus, error)
}
```

- [ ] **Step 4: Implement bounded HTTP over Unix transport**

Construct `http.Transport` with a `DialContext` that ignores the synthetic TCP address and dials:

```go
(&net.Dialer{}).DialContext(ctx, "unix", socketPath)
```

Use base URL `http://unix`, a client timeout from configuration, `io.LimitReader` capped at `1<<20`, and no shell or external process. After successful prepare, require:

```go
status.TargetImage == expectedRepository+":"+normalizedVersion
```

Map stable agent error codes to application errors:

```go
AGENT_BUSY               -> 409 UPDATE_AGENT_BUSY
INVALID_VERSION          -> 400 UPDATE_TARGET_INVALID
IMAGE_PULL_FAILED        -> 502 UPDATE_IMAGE_PULL_FAILED
IMAGE_VERIFICATION_FAILED-> 502 UPDATE_IMAGE_VERIFICATION_FAILED
NO_PREPARED_UPDATE       -> 409 UPDATE_NOT_PREPARED
ACTIVATION_IN_PROGRESS   -> 409 UPDATE_ACTIVATION_IN_PROGRESS
```

Map socket absence/refusal to `503 UPDATE_AGENT_UNAVAILABLE`, permission errors to `503 UPDATE_AGENT_PERMISSION_DENIED`, and context deadlines to `504 UPDATE_AGENT_TIMEOUT`.

- [ ] **Step 5: Run tests and commit**

Run:

```bash
cd backend
go test -tags=unit ./internal/service -run 'TestUnixUpdateAgentClient' -v
```

Expected: PASS.

Commit:

```bash
git add backend/internal/service/update_agent_client.go backend/internal/service/update_agent_client_test.go
git commit -m "feat: add unix socket update agent client"
```

### Task 3: Delegate Update Service Operations by Deployment Mode

**Files:**

- Modify: `backend/internal/service/update_service.go`
- Modify: `backend/internal/service/update_service_test.go`
- Modify: `backend/internal/service/wire.go`
- Regenerate: `backend/cmd/server/wire_gen.go`

- [ ] **Step 1: Write failing mode-specific service tests**

Add a recording `UpdateAgentClient` stub and these tests:

```go
func TestUpdateServiceDockerAgentPreparesLatestVersion(t *testing.T)
func TestUpdateServiceDockerAgentRollbackPreparesAllowedVersion(t *testing.T)
func TestUpdateServiceDockerAgentRejectsLegacyBackupRollback(t *testing.T)
func TestUpdateServiceDockerAgentActivateDelegatesToAgent(t *testing.T)
func TestUpdateServiceDockerAgentStatusDelegatesToAgent(t *testing.T)
func TestUpdateServiceBinaryModeStillAppliesReleaseAssets(t *testing.T)
```

The latest-version test must assert that `Prepare` receives `0.1.150`, while no download method is called. The versioned rollback test must preserve the existing GitHub allowlist and pass `0.1.146` to `Prepare`.

- [ ] **Step 2: Run focused tests and confirm failure**

Run:

```bash
cd backend
go test -tags=unit ./internal/service -run 'TestUpdateService(DockerAgent|BinaryMode)' -v
```

Expected: compilation fails because mode and activation methods do not exist.

- [ ] **Step 3: Extend UpdateService without changing binary behavior**

Add fields:

```go
mode        string
agentClient UpdateAgentClient
```

Extend `UpdateInfo`:

```go
UpdateMode string `json:"update_mode"`
```

Use this constructor:

```go
func NewUpdateService(
	cache UpdateCache,
	githubClient GitHubReleaseClient,
	repo, version, buildType, mode string,
	agentClient UpdateAgentClient,
) *UpdateService
```

Add:

```go
func (s *UpdateService) UsesDockerAgent() bool
func (s *UpdateService) ActivatePreparedUpdate(ctx context.Context) (*UpdateAgentStatus, error)
func (s *UpdateService) GetUpdateStatus(ctx context.Context) (*UpdateAgentStatus, error)
```

`PerformUpdate` must call `agentClient.Prepare(ctx, info.LatestVersion)` in Docker-agent mode and retain `applyReleaseAssets` in binary mode. `RollbackToVersion` must perform the existing allowlist lookup before choosing agent prepare or binary asset replacement. `Rollback` must return:

```go
infraerrors.BadRequest(
	"LEGACY_ROLLBACK_UNAVAILABLE",
	"local binary rollback is unavailable in Docker update mode",
)
```

in Docker-agent mode.

- [ ] **Step 4: Wire the configured client**

In `ProvideUpdateService`, create the client only for `docker_agent`:

```go
timeout := time.Duration(cfg.Update.AgentTimeoutSeconds) * time.Second
agentClient := NewUnixUpdateAgentClient(
	cfg.Update.AgentSocket,
	cfg.Update.ImageRepository,
	timeout,
)
```

Pass `nil` in binary mode. Regenerate Wire:

```bash
cd backend
go generate ./cmd/server
```

Expected: `backend/cmd/server/wire_gen.go` calls the updated provider without manual edits.

- [ ] **Step 5: Run service tests and commit**

Run:

```bash
cd backend
go test -tags=unit ./internal/service -run 'UpdateService|UnixUpdateAgentClient' -v
```

Expected: PASS.

Commit:

```bash
git add backend/internal/service/update_service.go backend/internal/service/update_service_test.go backend/internal/service/wire.go backend/cmd/server/wire_gen.go
git commit -m "feat: delegate docker updates to host agent"
```

### Task 4: Expose Activation and Status Through the Admin API

**Files:**

- Modify: `backend/internal/handler/admin/system_handler.go`
- Modify: `backend/internal/handler/admin/system_handler_test.go`
- Modify: `backend/internal/server/routes/admin.go`

- [ ] **Step 1: Write failing handler tests**

Extend the handler stub with:

```go
usesDockerAgent bool
activateStatus  *service.UpdateAgentStatus
activateErr     error
status          *service.UpdateAgentStatus
statusErr       error
activateCalls   int
statusCalls     int
```

Add tests:

```go
func TestSystemHandlerRestartDockerModeActivatesPreparedImage(t *testing.T)
func TestSystemHandlerRestartBinaryModeKeepsLegacyRestartPath(t *testing.T)
func TestSystemHandlerGetUpdateStatusReturnsAgentState(t *testing.T)
func TestSystemHandlerGetUpdateStatusMapsAgentError(t *testing.T)
func TestSystemHandlerPerformUpdateReturnsSpecificAgentError(t *testing.T)
```

The Docker restart test must assert `activateCalls == 1` and that process restart scheduling is not invoked. Introduce an injectable package variable:

```go
var restartServiceAsync = sysutil.RestartServiceAsync
```

so the binary-mode test can record the legacy call.

- [ ] **Step 2: Run tests and confirm failure**

Run:

```bash
cd backend
go test -tags=unit ./internal/handler/admin -run 'TestSystemHandler(Restart|GetUpdateStatus|PerformUpdateReturnsSpecific)' -v
```

Expected: compilation fails because the interface and status route do not exist.

- [ ] **Step 3: Implement handler branching and structured errors**

Extend `systemUpdateService` with:

```go
UsesDockerAgent() bool
ActivatePreparedUpdate(context.Context) (*service.UpdateAgentStatus, error)
GetUpdateStatus(context.Context) (*service.UpdateAgentStatus, error)
```

For update, rollback, restart, and status errors, return the original `ApplicationError` through the existing idempotency helper or `response.ErrorFrom`; do not call `response.Error(c, http.StatusInternalServerError, err.Error())`.

In Docker mode, restart returns:

```go
gin.H{
	"message":      "Image activation initiated",
	"update_mode":  "docker_agent",
	"status":       status,
	"operation_id": lock.OperationID(),
}
```

In binary mode, preserve the delayed `RestartServiceAsync` behavior and return `update_mode: "binary"`.

Add:

```go
func (h *SystemHandler) GetUpdateStatus(c *gin.Context) {
	status, err := h.updateSvc.GetUpdateStatus(c.Request.Context())
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, status)
}
```

- [ ] **Step 4: Register the status route**

Add:

```go
system.GET("/update-status", h.Admin.System.GetUpdateStatus)
```

- [ ] **Step 5: Run handler tests and commit**

Run:

```bash
cd backend
go test -tags=unit ./internal/handler/admin -run 'SystemHandler' -v
```

Expected: PASS.

Commit:

```bash
git add backend/internal/handler/admin/system_handler.go backend/internal/handler/admin/system_handler_test.go backend/internal/server/routes/admin.go
git commit -m "feat: expose docker update activation status"
```

### Task 5: Implement the Python Updater Core and Prepare Phase

**Files:**

- Create: `deploy/updater/updater_core.py`
- Create: `deploy/updater/tests/test_updater_core.py`

- [ ] **Step 1: Write failing config, state, validation, and prepare tests**

Use `unittest`, `tempfile.TemporaryDirectory`, and a fake argument-array command runner. Create methods named:

```text
test_version_accepts_numeric_dot_components
test_version_rejects_prefix_suffix_digest_and_shell_text
test_target_image_uses_only_configured_repository
test_prepare_pulls_exact_image_and_validates_labels_and_architecture
test_prepare_failure_returns_sanitized_error
test_prepare_does_not_change_environment_file
test_prepare_same_target_is_idempotent
test_prepare_different_target_replaces_pending_target
test_busy_operation_is_rejected
test_state_file_contains_no_environment_contents
```

The validation test must iterate over these rejected values and assert `UpdaterError.code == "INVALID_VERSION"`:

```python
["v0.1.150", "0.1.150-rc1", "0.1.150@sha256:abc", "../0.1.150", "0.1.150;id", ""]
```

The prepare test must assert the complete fake-runner call list:

```python
[
    (["docker", "pull", "ghcr.io/gwenliu1025/sub2api:0.1.150"], 600),
    (["docker", "image", "inspect", "ghcr.io/gwenliu1025/sub2api:0.1.150"], 600),
    (["docker", "inspect", "sub2api"], 30),
]
```

Use exact image metadata:

```python
{
    "id": "sha256:target",
    "architecture": "amd64",
    "source": "https://github.com/gwenliu1025/sub2api",
    "version": "0.1.150",
}
```

- [ ] **Step 2: Run tests and confirm failure**

Run:

```bash
python3 -m unittest deploy/updater/tests/test_updater_core.py -v
```

Expected: import fails because `updater_core.py` does not exist.

- [ ] **Step 3: Implement validated configuration and state**

Use immutable configuration fields:

```python
@dataclasses.dataclass(frozen=True)
class UpdaterConfig:
    socket_path: str
    socket_gid: int
    allowed_uids: tuple[int, ...]
    image_repository: str
    image_source: str
    compose_directory: str
    compose_file: str
    environment_file: str
    service_name: str
    container_name: str
    expected_architecture: str
    health_url: str
    prepare_timeout_seconds: int
    activation_timeout_seconds: int
    poll_interval_seconds: float
    state_file: str
```

Use only these persisted state fields:

```python
@dataclasses.dataclass
class UpdateState:
    state: str = "idle"
    current_image: str = ""
    target_image: str = ""
    previous_image: str = ""
    message: str = ""
    updated_at: str = ""
```

Validate versions with:

```python
VERSION_PATTERN = re.compile(r"^[0-9]+(?:\.[0-9]+)+$")
```

Atomic state writes must create a same-directory temporary file, write JSON with mode `0600`, flush, `os.fsync`, `os.replace`, and fsync the parent directory.

- [ ] **Step 4: Implement command execution and prepare**

The production runner must use:

```python
subprocess.run(
    args,
    check=False,
    capture_output=True,
    text=True,
    timeout=timeout,
    shell=False,
)
```

Prepare must execute only fixed argument arrays equivalent to:

```text
docker pull ghcr.io/gwenliu1025/sub2api:0.1.150
docker image inspect ghcr.io/gwenliu1025/sub2api:0.1.150
docker inspect sub2api
```

It must verify exact architecture, exact source label, exact version label, record current and target image names, and leave the environment file byte-for-byte unchanged.

- [ ] **Step 5: Run prepare tests and commit**

Run:

```bash
python3 -m unittest deploy/updater/tests/test_updater_core.py -v
```

Expected: all prepare tests PASS.

Commit:

```bash
git add deploy/updater/updater_core.py deploy/updater/tests/test_updater_core.py
git commit -m "feat: add docker image prepare state machine"
```

### Task 6: Implement Activation, Health Verification, and Automatic Rollback

**Files:**

- Modify: `deploy/updater/updater_core.py`
- Modify: `deploy/updater/tests/test_updater_core.py`

- [ ] **Step 1: Write failing environment and activation tests**

Create activation test methods named:

```text
test_atomic_environment_update_replaces_only_sub2api_image
test_atomic_environment_update_appends_missing_image_key
test_environment_update_preserves_mode_owner_and_unrelated_bytes
test_activation_recreates_only_sub2api_and_becomes_healthy
test_activation_failure_restores_exact_environment_and_rolls_back
test_activation_and_rollback_failure_marks_rollback_failed
test_activation_rejects_without_prepared_target
test_stale_activating_state_reconciles_to_healthy
test_stale_activating_state_reconciles_to_rolled_back
test_error_state_does_not_include_environment_secrets
```

Use this exact secret-bearing fixture:

```python
original_env = (
    b"POSTGRES_PASSWORD=do-not-leak\n"
    b"JWT_SECRET=also-do-not-leak\n"
    b"SUB2API_IMAGE=xqian/sub2api:equivalent-cache-20260709-083433\n"
)
```

After successful activation, assert the environment contains the target image while both secret lines remain byte-identical. After rollback, assert the complete file equals `original_env`.

The command assertion must equal:

```python
[
    "docker", "compose",
    "--project-directory", "/home/ubuntu/sub2api",
    "-f", "/home/ubuntu/sub2api/docker-compose.yml",
    "--env-file", "/home/ubuntu/sub2api/.env",
    "up", "-d", "--no-deps", "--force-recreate", "sub2api",
]
```

- [ ] **Step 2: Run activation tests and confirm failure**

Run:

```bash
python3 -m unittest deploy/updater/tests/test_updater_core.py -v
```

Expected: new activation tests FAIL because activation is not implemented.

- [ ] **Step 3: Implement exact environment replacement**

`replace_sub2api_image` must:

1. Read and retain the exact original bytes.
2. Replace only a line whose key is exactly `SUB2API_IMAGE`.
3. Append the key with the file's existing newline convention if absent.
4. Write a sibling temporary file.
5. Apply the original mode, UID, and GID.
6. Flush, fsync, replace, and fsync the parent directory.
7. Return the original bytes for rollback.

No environment value other than `SUB2API_IMAGE` may be parsed into state or logs.

- [ ] **Step 4: Implement activation and health checks**

Activation must:

1. Require `prepared`.
2. Set `activating` before returning control to the HTTP layer.
3. Update `.env`.
4. run the exact Compose command above;
5. compare the running container image ID with the prepared target image ID;
6. poll Docker health until `healthy`;
7. require HTTP `200` from `http://127.0.0.1:8080/health`;
8. mark `healthy`.

On failure it must restore the exact original bytes, recreate only `sub2api`, verify the previous image ID plus Docker/HTTP health, and mark `rolled_back`. If rollback verification fails, mark `rollback_failed`.

- [ ] **Step 5: Implement stale-state reconciliation**

On startup:

- `preparing` becomes `failed` because no environment mutation occurred.
- `activating` becomes `healthy` if the target image is running and healthy.
- `activating` becomes `rolled_back` if the previous image is running and healthy.
- otherwise it becomes `failed` with a fixed sanitized message.

- [ ] **Step 6: Run tests and commit**

Run:

```bash
python3 -m unittest deploy/updater/tests/test_updater_core.py -v
```

Expected: PASS.

Commit:

```bash
git add deploy/updater/updater_core.py deploy/updater/tests/test_updater_core.py
git commit -m "feat: activate images with automatic rollback"
```

### Task 7: Add the Peer-Checked Unix HTTP Service and Installer

**Files:**

- Create: `deploy/updater/sub2api_updater.py`
- Create: `deploy/updater/tests/test_sub2api_updater.py`
- Create: `deploy/updater/config.example.json`
- Create: `deploy/updater/sub2api-updater.service`
- Create: `deploy/updater/install.sh`
- Create: `deploy/updater/README.md`

- [ ] **Step 1: Write failing HTTP and peer tests**

Create HTTP test methods named:

```text
test_health_returns_protocol_version
test_prepare_requires_json_and_numeric_version
test_prepare_returns_prepared_status
test_activate_returns_202_before_worker_runs
test_status_returns_persisted_state
test_unknown_route_returns_404
test_request_body_over_4096_bytes_is_rejected
test_allowed_peer_uid_is_accepted
test_disallowed_peer_uid_is_closed_without_dispatch
```

The health test must assert HTTP `200` and:

```json
{"ready":true,"protocol_version":"v1"}
```

The activation test must use a blocking fake worker and assert the HTTP response is already `202` with state `activating` before the worker's release event is set.

- [ ] **Step 2: Run tests and confirm failure**

Run:

```bash
python3 -m unittest deploy/updater/tests/test_sub2api_updater.py -v
```

Expected: import fails because the HTTP service does not exist.

- [ ] **Step 3: Implement Unix HTTP server**

Use `socketserver.UnixStreamServer` plus `BaseHTTPRequestHandler`. Override `get_request` and validate:

```python
pid, uid, gid = struct.unpack("3i", conn.getsockopt(
    socket.SOL_SOCKET,
    socket.SO_PEERCRED,
    struct.calcsize("3i"),
))
```

Permit only configured UIDs. Create the socket at `/run/sub2api-updater/updater.sock`, set group `1000`, and mode `0660`. Implement:

```text
GET  /v1/health
POST /v1/prepare
POST /v1/activate
GET  /v1/status
```

`POST /v1/activate` must set state to `activating`, create one daemon worker thread, and return HTTP `202` before Compose recreation begins.

- [ ] **Step 4: Add exact graduation-machine configuration example**

`config.example.json` must contain:

```json
{
  "socket_path": "/run/sub2api-updater/updater.sock",
  "socket_gid": 1000,
  "allowed_uids": [0, 1000],
  "image_repository": "ghcr.io/gwenliu1025/sub2api",
  "image_source": "https://github.com/gwenliu1025/sub2api",
  "compose_directory": "/home/ubuntu/sub2api",
  "compose_file": "/home/ubuntu/sub2api/docker-compose.yml",
  "environment_file": "/home/ubuntu/sub2api/.env",
  "service_name": "sub2api",
  "container_name": "sub2api",
  "expected_architecture": "amd64",
  "health_url": "http://127.0.0.1:8080/health",
  "prepare_timeout_seconds": 600,
  "activation_timeout_seconds": 120,
  "poll_interval_seconds": 2,
  "state_file": "/var/lib/sub2api-updater/state.json"
}
```

- [ ] **Step 5: Add systemd unit and installer**

The service must run:

```ini
ExecStart=/usr/bin/python3 /opt/sub2api-updater/sub2api_updater.py --config /etc/sub2api-updater/config.json
```

with `User=root`, `Group=root`, `Restart=on-failure`, `RuntimeDirectory=sub2api-updater`, `StateDirectory=sub2api-updater`, `UMask=0007`, `NoNewPrivileges=true`, `PrivateTmp=true`, kernel/control-group protections, and journald output.

`install.sh` must require root, validate Python and Docker Compose, install both Python files to `/opt/sub2api-updater`, write `/etc/sub2api-updater/config.json` through Python's `json` module, install the unit, run `systemctl daemon-reload`, enable/start the service, and verify the Unix-socket health endpoint.

- [ ] **Step 6: Run tests and syntax checks**

Run:

```bash
python3 -m unittest discover -s deploy/updater/tests -v
python3 -m py_compile deploy/updater/updater_core.py deploy/updater/sub2api_updater.py
bash -n deploy/updater/install.sh
```

Expected: PASS with no output from `py_compile` or `bash -n`.

- [ ] **Step 7: Commit**

```bash
git add deploy/updater/sub2api_updater.py deploy/updater/tests/test_sub2api_updater.py deploy/updater/config.example.json deploy/updater/sub2api-updater.service deploy/updater/install.sh deploy/updater/README.md
git commit -m "feat: package host docker update service"
```

### Task 8: Integrate Compose, OCI Labels, Bootstrap Publishing, and CI

**Files:**

- Modify: `deploy/docker-compose.yml`
- Modify: `Dockerfile`
- Modify: `deploy/Dockerfile`
- Modify: `Dockerfile.goreleaser`
- Create: `.github/workflows/bootstrap-image.yml`
- Modify: `.github/workflows/backend-ci.yml`

- [ ] **Step 1: Add a Compose render regression check**

Run before editing:

```bash
cd deploy
SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150 \
POSTGRES_PASSWORD=test REDIS_PASSWORD=test JWT_SECRET=12345678901234567890123456789012 \
docker compose config > /tmp/sub2api-compose-before.yaml
```

Expected: the current rendered image is still fixed and no updater socket is mounted.

- [ ] **Step 2: Make the application image durable**

Change:

```yaml
image: ${SUB2API_IMAGE}
```

Add:

```yaml
volumes:
  - /run/sub2api-updater:/run/sub2api-updater:ro
environment:
  - UPDATE_MODE=${UPDATE_MODE:-binary}
  - UPDATE_AGENT_SOCKET=${UPDATE_AGENT_SOCKET:-/run/sub2api-updater/updater.sock}
  - UPDATE_AGENT_TIMEOUT_SECONDS=${UPDATE_AGENT_TIMEOUT_SECONDS:-600}
  - UPDATE_IMAGE_REPOSITORY=${UPDATE_IMAGE_REPOSITORY:-ghcr.io/gwenliu1025/sub2api}
```

- [ ] **Step 3: Make direct-build OCI labels verifiable**

For `Dockerfile` and `deploy/Dockerfile`, add final-stage arguments, labels, and a bootstrap-test-only health switch:

```dockerfile
ARG VERSION=dev
ARG OCI_SOURCE=https://github.com/gwenliu1025/sub2api
ARG FORCE_UNHEALTHY_HEALTHCHECK=false
ENV FORCE_UNHEALTHY_HEALTHCHECK=${FORCE_UNHEALTHY_HEALTHCHECK}
LABEL org.opencontainers.image.source="${OCI_SOURCE}"
LABEL org.opencontainers.image.version="${VERSION}"
```

The direct-build health check must begin with:

```dockerfile
CMD if [ "${FORCE_UNHEALTHY_HEALTHCHECK}" = "true" ]; then exit 1; fi; \
    wget -q -T 5 -O /dev/null http://localhost:${SERVER_PORT:-8080}/health || exit 1
```

Set the static source label in `Dockerfile.goreleaser` to the fork; `.goreleaser.yaml` and `.goreleaser.simple.yaml` continue to override source/version through build flags. GoReleaser images never set the unhealthy switch.

- [ ] **Step 4: Add the bootstrap image workflow**

The manual workflow must accept:

```yaml
inputs:
  ref:
    required: true
    default: gwen-main-v0.1.149-custom
  reported_version:
    required: true
    default: 0.1.149
  image_tag:
    required: true
    default: bootstrap-0.1.149-update-agent
  force_unhealthy:
    required: false
    type: boolean
    default: false
```

Validate `reported_version` with `^[0-9]+(\.[0-9]+)+$` and `image_tag` with `^[A-Za-z0-9_.-]+$`, then use `docker/build-push-action` to publish:

```text
ghcr.io/gwenliu1025/sub2api:bootstrap-0.1.149-update-agent
```

with:

```text
VERSION=0.1.149
OCI_SOURCE=https://github.com/gwenliu1025/sub2api
FORCE_UNHEALTHY_HEALTHCHECK=false
```

- [ ] **Step 5: Add Python tests to CI**

Add a CI step:

```yaml
- name: Python updater tests
  run: python3 -m unittest discover -s deploy/updater/tests -v
```

- [ ] **Step 6: Verify rendered Compose and workflow syntax**

Run:

```bash
cd deploy
SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150 \
POSTGRES_PASSWORD=test REDIS_PASSWORD=test JWT_SECRET=12345678901234567890123456789012 \
UPDATE_MODE=docker_agent docker compose config > /tmp/sub2api-compose-after.yaml
grep -q 'ghcr.io/gwenliu1025/sub2api:0.1.150' /tmp/sub2api-compose-after.yaml
grep -q '/run/sub2api-updater' /tmp/sub2api-compose-after.yaml
```

Expected: all commands exit `0`.

- [ ] **Step 7: Commit**

```bash
git add deploy/docker-compose.yml Dockerfile deploy/Dockerfile Dockerfile.goreleaser .github/workflows/bootstrap-image.yml .github/workflows/backend-ci.yml
git commit -m "feat: integrate durable docker image updates"
```

### Task 9: Add Frontend API Types and Bounded Activation Polling

**Files:**

- Modify: `frontend/src/api/admin/system.ts`
- Modify: `frontend/src/api/__tests__/admin.system.rollback.spec.ts`
- Create: `frontend/src/utils/updateActivation.ts`
- Create: `frontend/src/utils/__tests__/updateActivation.spec.ts`
- Modify: `frontend/src/stores/app.ts`

- [ ] **Step 1: Write failing API and polling tests**

Add API expectations:

```ts
expect(post).toHaveBeenCalledWith('/admin/system/update', undefined, { timeout: 610_000 })
expect(get).toHaveBeenCalledWith('/admin/system/update-status')
```

Add polling tests using injected `checkHealth`, `getStatus`, `sleep`, and `now` functions. Use these exact case names:

```text
continues polling when recovery takes longer than eight seconds
returns healthy only after agent status is healthy
returns rolled_back with the agent message
returns rollback_failed with the agent message
returns timeout after 120 seconds
legacy mode reloads only after health disappears and returns
```

For the beyond-eight-seconds case, return `activating` for the first five status calls and `healthy` on the sixth, use `intervalMs: 2_000`, and assert six status calls occurred before the healthy outcome.

- [ ] **Step 2: Run tests and confirm failure**

Run:

```bash
pnpm --dir frontend exec vitest run \
  src/api/__tests__/admin.system.rollback.spec.ts \
  src/utils/__tests__/updateActivation.spec.ts
```

Expected: tests fail because the new types and polling helper do not exist.

- [ ] **Step 3: Implement API types and methods**

Add:

```ts
export type UpdateMode = 'binary' | 'docker_agent'
export type UpdateAgentState =
  | 'idle'
  | 'preparing'
  | 'prepared'
  | 'activating'
  | 'healthy'
  | 'rolled_back'
  | 'failed'
  | 'rollback_failed'

export interface UpdateAgentStatus {
  state: UpdateAgentState
  current_image: string
  target_image: string
  previous_image: string
  message: string
  updated_at: string
}
```

Add `update_mode: UpdateMode` to `VersionInfo`, include mode/status in restart results, use a `610_000` millisecond timeout for `performUpdate`, and add:

```ts
export async function getUpdateStatus(): Promise<UpdateAgentStatus> {
  const { data } = await apiClient.get<UpdateAgentStatus>('/admin/system/update-status')
  return data
}
```

- [ ] **Step 4: Implement polling helper**

Expose:

```ts
export type ActivationOutcome =
  | { state: 'healthy'; status?: UpdateAgentStatus }
  | { state: 'rolled_back'; status: UpdateAgentStatus }
  | { state: 'rollback_failed'; status: UpdateAgentStatus }
  | { state: 'failed'; status?: UpdateAgentStatus }
  | { state: 'timeout' }
```

Use defaults `timeoutMs=120_000` and `intervalMs=2_000`. Docker-agent mode must require a terminal updater status; a transient health or authenticated API failure during container recreation is retryable. Legacy mode must observe service unavailability before treating a later `200` as recovery, with a two-second grace fallback for environments where the restart is too fast to observe.

- [ ] **Step 5: Retain update mode in Pinia**

Add `updateMode = ref<UpdateMode>('binary')`, populate it from `VersionInfo`, return it from cached reads, clear it to `binary` during reset, and expose it from the store.

- [ ] **Step 6: Run tests and commit**

Run:

```bash
pnpm --dir frontend exec vitest run \
  src/api/__tests__/admin.system.rollback.spec.ts \
  src/utils/__tests__/updateActivation.spec.ts \
  src/stores/__tests__/app.spec.ts
```

Expected: PASS.

Commit:

```bash
git add frontend/src/api/admin/system.ts frontend/src/api/__tests__/admin.system.rollback.spec.ts frontend/src/utils/updateActivation.ts frontend/src/utils/__tests__/updateActivation.spec.ts frontend/src/stores/app.ts
git commit -m "feat: poll docker update activation state"
```

### Task 10: Redesign VersionBadge as the Approved Two-Step Interaction

**Files:**

- Modify: `frontend/src/components/common/VersionBadge.vue`
- Create: `frontend/src/components/common/__tests__/VersionBadge.spec.ts`
- Modify: `frontend/src/i18n/locales/zh/misc.ts`
- Modify: `frontend/src/i18n/locales/en/misc.ts`

- [ ] **Step 1: Write failing component tests**

Mock the admin system API, stores, clipboard, and activation helper. Use these exact case names:

```text
shows image preparing while the prepare request is pending
shows the prepared version and enables restart after prepare succeeds
shows the sanitized prepare error without truncating it
reloads after a healthy activation
shows automatic rollback instead of reloading
shows high severity recovery guidance when rollback fails
```

The prepare-error fixture must reject with:

```ts
{ message: 'Image verification failed: source label mismatch' }
```

and assert the complete message is rendered. The rollback cases must assert `window.location.reload` was not called.

Use `flushPromises()` and a deferred promise for the prepare request. Assert translation keys or rendered Chinese strings, not implementation refs.

- [ ] **Step 2: Run tests and confirm failure**

Run:

```bash
pnpm --dir frontend exec vitest run src/components/common/__tests__/VersionBadge.spec.ts
```

Expected: tests fail because the component still uses an eight-second countdown and five health probes.

- [ ] **Step 3: Implement prepare wording and state**

Replace generic update wording in Docker-agent mode:

```text
Update Now       -> Prepare Image
Updating...      -> Pulling and verifying image...
Update Complete  -> Image Prepared
Restart Required -> Restart to switch to the prepared version
```

Keep binary-mode wording unchanged. Remove the `truncate` class from specific errors and use wrapping text.

- [ ] **Step 4: Replace countdown with activation polling**

Remove `restartCountdown`, its interval, and `checkServiceAndReload`. After `restartService()` call:

```ts
const outcome = await waitForUpdateActivation({
  mode: appStore.updateMode,
  checkHealth: checkServiceHealth,
  getStatus: getUpdateStatus
})
```

Behavior:

- `healthy`: clear version cache and reload.
- `rolled_back`: stop restarting, keep the dropdown open, show the previous-version-restored message, and do not reload.
- `rollback_failed`: show high-severity manual recovery guidance and do not reload.
- `failed` or `timeout`: show a specific activation error and retain the current page.

Update manual rollback examples to use `gwenliu1025/sub2api` and `ghcr.io/gwenliu1025/sub2api`.

- [ ] **Step 5: Add locale strings**

Add matching keys in Chinese and English:

```text
prepareImage
preparingImage
imagePrepared
activatePrepared
activatingImage
waitingForHealth
activationSucceeded
activationRolledBack
activationRollbackFailed
activationFailed
activationTimedOut
manualRecoveryRequired
```

Chinese messages must explicitly distinguish "镜像已准备" from "已经切换版本".

- [ ] **Step 6: Run component, type, and locale tests**

Run:

```bash
pnpm --dir frontend exec vitest run \
  src/components/common/__tests__/VersionBadge.spec.ts \
  src/utils/__tests__/updateActivation.spec.ts \
  src/api/__tests__/admin.system.rollback.spec.ts \
  src/i18n/__tests__/localesNoKeyCollision.spec.ts
pnpm --dir frontend run build
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add frontend/src/components/common/VersionBadge.vue frontend/src/components/common/__tests__/VersionBadge.spec.ts frontend/src/i18n/locales/zh/misc.ts frontend/src/i18n/locales/en/misc.ts
git commit -m "feat: add two-step docker update experience"
```

### Task 11: Run Repository Verification and Publish the Images

**Files:**

- Modify only files required by verification failures attributable to this feature.

- [ ] **Step 1: Run Python verification**

```bash
python3 -m unittest discover -s deploy/updater/tests -v
python3 -m py_compile deploy/updater/updater_core.py deploy/updater/sub2api_updater.py
bash -n deploy/updater/install.sh
```

Expected: PASS.

- [ ] **Step 2: Run Go verification**

```bash
cd backend
go test -tags=unit ./internal/config ./internal/service ./internal/handler/admin
go test -tags=unit ./...
go test ./...
go vet ./...
go build ./cmd/server
```

Expected: PASS.

- [ ] **Step 3: Run frontend verification**

```bash
pnpm --dir frontend run lint:check
pnpm --dir frontend run test:run
pnpm --dir frontend run build
```

Expected: PASS.

- [ ] **Step 4: Run Compose and diff checks**

```bash
git diff --check
git status --short
cd deploy
SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150 \
POSTGRES_PASSWORD=test REDIS_PASSWORD=test JWT_SECRET=12345678901234567890123456789012 \
UPDATE_MODE=docker_agent docker compose config >/tmp/sub2api-compose-verified.yaml
```

Expected: no whitespace errors and valid Compose output.

- [ ] **Step 5: Commit verification-only corrections**

If verification required corrections, stage only the feature paths:

```bash
git add \
  backend/internal/config/config.go backend/internal/config/config_test.go \
  backend/internal/service/update_agent_client.go backend/internal/service/update_agent_client_test.go \
  backend/internal/service/update_service.go backend/internal/service/update_service_test.go backend/internal/service/wire.go \
  backend/cmd/server/wire_gen.go backend/internal/handler/admin/system_handler.go \
  backend/internal/handler/admin/system_handler_test.go backend/internal/server/routes/admin.go \
  deploy/updater deploy/docker-compose.yml deploy/.env.example deploy/config.example.yaml \
  Dockerfile deploy/Dockerfile Dockerfile.goreleaser \
  .github/workflows/bootstrap-image.yml .github/workflows/backend-ci.yml \
  frontend/src/api/admin/system.ts frontend/src/api/__tests__/admin.system.rollback.spec.ts \
  frontend/src/utils/updateActivation.ts frontend/src/utils/__tests__/updateActivation.spec.ts \
  frontend/src/stores/app.ts frontend/src/components/common/VersionBadge.vue \
  frontend/src/components/common/__tests__/VersionBadge.spec.ts \
  frontend/src/i18n/locales/zh/misc.ts frontend/src/i18n/locales/en/misc.ts
git commit -m "fix: harden docker update integration"
```

If no corrections were required, do not create an empty commit.

- [ ] **Step 6: Push the branch and publish bootstrap image**

```bash
git push origin gwen-main-v0.1.149-custom
gh workflow run bootstrap-image.yml \
  --ref gwen-main-v0.1.149-custom \
  -f ref=gwen-main-v0.1.149-custom \
  -f reported_version=0.1.149 \
  -f image_tag=bootstrap-0.1.149-update-agent
gh run watch --exit-status
```

Expected: GHCR contains `ghcr.io/gwenliu1025/sub2api:bootstrap-0.1.149-update-agent`.

- [ ] **Step 7: Tag and publish v0.1.150**

Create an annotated tag only after bootstrap deployment is healthy:

```bash
git tag -a v0.1.150 -m "v0.1.150

- Add host-managed Docker image preparation and activation
- Preserve two-step admin update interaction
- Add health-checked automatic rollback"
git push origin v0.1.150
gh run watch --exit-status
```

Expected: GitHub release `v0.1.150` and multi-architecture image `ghcr.io/gwenliu1025/sub2api:0.1.150`.

### Task 12: Bootstrap and Validate the Graduation Machine

**Files on host:**

- `/home/ubuntu/sub2api/docker-compose.yml`
- `/home/ubuntu/sub2api/.env`
- `/opt/sub2api-updater/updater_core.py`
- `/opt/sub2api-updater/sub2api_updater.py`
- `/etc/sub2api-updater/config.json`
- `/etc/systemd/system/sub2api-updater.service`
- `/var/lib/sub2api-updater/state.json`
- `/run/sub2api-updater/updater.sock`

- [ ] **Step 1: Record pre-change state and backups**

Create `/root/sub2api-update-backup-20260710` and store:

```text
docker-compose.yml
.env
container-ids-before.txt
sub2api-image-before.txt
sub2api.dump
```

Produce the database dump without expanding credentials into the command line:

```bash
docker exec sub2api-postgres sh -c \
  'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc' \
  > /root/sub2api-update-backup-20260710/sub2api.dump
```

Expected: non-empty dump and unchanged running containers.

- [ ] **Step 2: Install the updater without switching images**

Run the repository installer with exact machine values:

```bash
sudo deploy/updater/install.sh \
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
  --image-source https://github.com/gwenliu1025/sub2api
```

Verify socket ownership/mode and `/v1/health` over the Unix socket.

- [ ] **Step 3: Update Compose and environment without recreating containers**

Set:

```dotenv
SUB2API_IMAGE=xqian/sub2api:equivalent-cache-20260709-083433
UPDATE_MODE=docker_agent
UPDATE_AGENT_SOCKET=/run/sub2api-updater/updater.sock
UPDATE_AGENT_TIMEOUT_SECONDS=600
UPDATE_IMAGE_REPOSITORY=ghcr.io/gwenliu1025/sub2api
```

Apply only the Compose file and `.env` changes. Do not run `docker compose up` yet. Confirm every container ID still matches `container-ids-before.txt`.

- [ ] **Step 4: Deploy the bootstrap application only**

Pull and switch only `sub2api`:

```bash
docker pull ghcr.io/gwenliu1025/sub2api:bootstrap-0.1.149-update-agent
SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:bootstrap-0.1.149-update-agent \
docker compose --project-directory /home/ubuntu/sub2api \
  -f /home/ubuntu/sub2api/docker-compose.yml \
  --env-file /home/ubuntu/sub2api/.env \
  up -d --no-deps --force-recreate sub2api
```

Persist the bootstrap tag in `.env`, then verify:

```text
Docker health = healthy
http://127.0.0.1:8080/health = 200
reported application version = 0.1.149
Sub2API can GET /v1/health through the mounted Unix socket
all non-sub2api container IDs unchanged
```

- [ ] **Step 5: Exercise prepare with no downtime**

From the authenticated management panel, click the first-step update action. While it runs, continuously probe the public application and direct health endpoint.

Expected:

```text
target image ghcr.io/gwenliu1025/sub2api:0.1.150 is pulled
agent state = prepared
SUB2API_IMAGE still points to the bootstrap image
sub2api container ID is unchanged
health probes remain 200
```

- [ ] **Step 6: Exercise restart and successful activation**

Click the second-step restart action.

Expected:

```text
only sub2api container ID changes
SUB2API_IMAGE becomes ghcr.io/gwenliu1025/sub2api:0.1.150
Docker health becomes healthy within 120 seconds
host HTTP health returns 200
application reports 0.1.150
agent state = healthy
all unrelated container IDs remain unchanged
```

- [ ] **Step 7: Prove automatic rollback with a controlled health failure**

Publish a label-valid numeric test image whose Docker health intentionally fails:

```bash
gh workflow run bootstrap-image.yml \
  --ref gwen-main-v0.1.149-custom \
  -f ref=gwen-main-v0.1.149-custom \
  -f reported_version=0.1.150.999 \
  -f image_tag=0.1.150.999 \
  -f force_unhealthy=true
gh run watch --exit-status
```

Prepare and activate it only through the root-side Unix socket:

```bash
curl --unix-socket /run/sub2api-updater/updater.sock \
  -H 'Content-Type: application/json' \
  -d '{"version":"0.1.150.999"}' \
  http://localhost/v1/prepare
curl --unix-socket /run/sub2api-updater/updater.sock \
  -X POST http://localhost/v1/activate
```

Do not add arbitrary image selection to the application API.

Expected:

```text
activation detects failed Docker/HTTP health
exact previous .env bytes are restored
sub2api is recreated on ghcr.io/gwenliu1025/sub2api:0.1.150
agent state = rolled_back
application returns 200 and still reports 0.1.150
all unrelated container IDs remain unchanged
```

Remove the controlled test tag after evidence is captured.

```bash
test_version_id="$(
  gh api --paginate /users/gwenliu1025/packages/container/sub2api/versions \
    --jq '.[] | select(.metadata.container.tags | index("0.1.150.999")) | .id' |
  head -n 1
)"
test -n "$test_version_id"
gh api --method DELETE \
  "/users/gwenliu1025/packages/container/sub2api/versions/${test_version_id}"
```

- [ ] **Step 8: Record final evidence and manual recovery**

Store sanitized command output, updater status, container ID comparisons, and health results under:

```text
/root/sub2api-update-backup-20260710/verification/
```

Confirm the manual recovery path:

```bash
cp /root/sub2api-update-backup-20260710/.env /home/ubuntu/sub2api/.env
cp /root/sub2api-update-backup-20260710/docker-compose.yml /home/ubuntu/sub2api/docker-compose.yml
docker compose --project-directory /home/ubuntu/sub2api \
  -f /home/ubuntu/sub2api/docker-compose.yml \
  --env-file /home/ubuntu/sub2api/.env \
  up -d --no-deps --force-recreate sub2api
```

Do not execute manual recovery when the successful final state is healthy.

## Plan Self-Review

**Spec coverage**

- Two-step prepare/activate interaction: Tasks 3, 4, 9, and 10.
- No Docker socket in the application: Tasks 5 through 8 use a host service and read-only Unix socket mount.
- Fixed repository/version-only requests: Tasks 2, 5, and 7.
- Durable Compose image switch: Tasks 6, 8, and 12.
- Docker and HTTP health checks: Tasks 6 and 12.
- Exact `.env` rollback: Tasks 6 and 12.
- Unrelated-service isolation: exact `--no-deps --force-recreate sub2api` commands in Tasks 6, 8, and 12.
- Peer UID validation: Task 7.
- Stable sanitized errors: Tasks 2, 4, 5, 6, and 10.
- Binary mode compatibility: Tasks 1, 3, 4, 9, and 10.
- CI, bootstrap, release, and graduation deployment: Tasks 8, 11, and 12.

**Placeholder scan**

- The plan contains no deferred implementation markers.
- All paths, endpoint names, state names, commands, versions, repositories, and graduation-machine deployment values are explicit.

**Type consistency**

- Agent states match across Python, Go, JSON, and TypeScript.
- `UpdateAgentStatus` uses the same six JSON property names in all layers.
- Application mode is consistently `binary` or `docker_agent`.
- The expected image repository is consistently `ghcr.io/gwenliu1025/sub2api`.
- Prepare timeout is 600 seconds in host config and 610 seconds in the browser request.
- Activation timeout is 120 seconds in the host service and frontend polling.
