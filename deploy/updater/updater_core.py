import contextlib
import dataclasses
import datetime
import http.client
import json
import os
import re
import stat
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Callable


VERSION_PATTERN = re.compile(r"^[0-9]+(?:\.[0-9]+)+$")

_PUBLIC_STATE_FIELDS = (
    "state",
    "current_image",
    "target_image",
    "previous_image",
    "message",
    "updated_at",
)
_INTERNAL_STATE_FIELDS = (
    "target_image_id",
    "previous_image_id",
)
_STATE_FIELDS = _PUBLIC_STATE_FIELDS + _INTERNAL_STATE_FIELDS
_VALID_STATES = {
    "idle",
    "preparing",
    "prepared",
    "activating",
    "healthy",
    "rolled_back",
    "failed",
    "rollback_failed",
}


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


@dataclasses.dataclass
class UpdateState:
    state: str = "idle"
    current_image: str = ""
    target_image: str = ""
    previous_image: str = ""
    message: str = ""
    updated_at: str = ""
    target_image_id: str = ""
    previous_image_id: str = ""


@dataclasses.dataclass(frozen=True)
class EnvironmentBackup:
    contents: bytes
    mode: int
    uid: int
    gid: int


class UpdaterError(Exception):
    def __init__(self, code: str, message: str):
        self.code = code
        self.message = _sanitize_message(message)
        super().__init__(self.message)


def _sanitize_message(message: str) -> str:
    sanitized = " ".join(str(message).split())
    return (sanitized or "update operation failed")[:200]


def _updated_at() -> str:
    return (
        datetime.datetime.now(datetime.timezone.utc)
        .isoformat(timespec="seconds")
        .replace("+00:00", "Z")
    )


def validate_version(version: str) -> str:
    if not isinstance(version, str) or VERSION_PATTERN.fullmatch(version) is None:
        raise UpdaterError(
            "INVALID_VERSION",
            "version must use numeric dot-separated components",
        )
    return version


def run_command(
    args: list[str],
    timeout: float,
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        args,
        check=False,
        capture_output=True,
        text=True,
        timeout=timeout,
        shell=False,
    )


CommandRunner = Callable[
    [list[str], float],
    subprocess.CompletedProcess[str],
]
HealthChecker = Callable[[str, float], bool]


def _read_environment_backup(environment_file: str | Path) -> EnvironmentBackup:
    path = Path(environment_file)
    file_descriptor = None
    try:
        path_metadata = os.lstat(path)
        if (
            not stat.S_ISREG(path_metadata.st_mode)
            or path_metadata.st_nlink != 1
        ):
            raise OSError

        flags = os.O_RDONLY
        if hasattr(os, "O_BINARY"):
            flags |= os.O_BINARY
        if os.name != "nt" and hasattr(os, "O_NOFOLLOW"):
            flags |= os.O_NOFOLLOW
        file_descriptor = os.open(path, flags)
        opened_metadata = os.fstat(file_descriptor)
        if (
            not stat.S_ISREG(opened_metadata.st_mode)
            or opened_metadata.st_nlink != 1
            or opened_metadata.st_dev != path_metadata.st_dev
            or opened_metadata.st_ino != path_metadata.st_ino
        ):
            raise OSError

        descriptor_to_close = file_descriptor
        with os.fdopen(descriptor_to_close, "rb") as source:
            file_descriptor = None
            contents = source.read()
    except OSError:
        if file_descriptor is not None:
            try:
                os.close(file_descriptor)
            except OSError:
                pass
        raise UpdaterError(
            "ENVIRONMENT_WRITE_FAILED",
            "failed to update deployment environment",
        ) from None
    return EnvironmentBackup(
        contents=contents,
        mode=stat.S_IMODE(opened_metadata.st_mode),
        uid=opened_metadata.st_uid,
        gid=opened_metadata.st_gid,
    )


def _environment_newline(contents: bytes) -> bytes:
    for index, value in enumerate(contents):
        if value == 13:
            if contents[index : index + 2] == b"\r\n":
                return b"\r\n"
            return b"\r"
        if value == 10:
            return b"\n"
    return b"\n"


def _updated_environment_contents(contents: bytes, target_image: str) -> bytes:
    try:
        encoded_target = target_image.encode("utf-8")
    except (AttributeError, UnicodeError):
        raise UpdaterError(
            "ENVIRONMENT_WRITE_FAILED",
            "failed to update deployment environment",
        ) from None
    if any(separator in encoded_target for separator in (b"\x00", b"\r", b"\n")):
        raise UpdaterError(
            "ENVIRONMENT_WRITE_FAILED",
            "failed to update deployment environment",
        )

    lines = contents.splitlines(keepends=True)
    matching_indexes = []
    for index, line in enumerate(lines):
        body = line
        if body.endswith(b"\r\n"):
            body = body[:-2]
        elif body.endswith((b"\r", b"\n")):
            body = body[:-1]
        if body.partition(b"=")[0] == b"SUB2API_IMAGE":
            matching_indexes.append(index)

    if len(matching_indexes) > 1:
        raise UpdaterError(
            "ENVIRONMENT_WRITE_FAILED",
            "failed to update deployment environment",
        )

    replacement = b"SUB2API_IMAGE=" + encoded_target
    if matching_indexes:
        index = matching_indexes[0]
        line = lines[index]
        if line.endswith(b"\r\n"):
            replacement += b"\r\n"
        elif line.endswith((b"\r", b"\n")):
            replacement += line[-1:]
        lines[index] = replacement
        return b"".join(lines)

    newline = _environment_newline(contents)
    if not contents:
        return replacement + newline
    separator = b"" if contents.endswith((b"\r", b"\n")) else newline
    return contents + separator + replacement + newline


def _fsync_directory(parent: Path) -> None:
    if os.name == "nt":
        return
    flags = os.O_RDONLY
    if hasattr(os, "O_DIRECTORY"):
        flags |= os.O_DIRECTORY
    directory_descriptor = os.open(parent, flags)
    try:
        os.fsync(directory_descriptor)
    finally:
        os.close(directory_descriptor)


def _apply_environment_metadata(
    file_descriptor: int,
    temporary_path: Path,
    backup: EnvironmentBackup,
    *,
    preserve_owner: bool,
    fchown: Callable[..., None] | None,
    fchmod: Callable[..., None] | None,
    chmod: Callable[..., None],
) -> None:
    if preserve_owner:
        if fchown is None:
            raise OSError
        fchown(file_descriptor, backup.uid, backup.gid)
    if fchmod is not None:
        fchmod(file_descriptor, backup.mode)
    else:
        chmod(temporary_path, backup.mode)


def _atomic_environment_write(
    environment_file: str | Path,
    contents: bytes,
    backup: EnvironmentBackup,
    *,
    expected_current_contents: bytes,
) -> None:
    path = Path(environment_file)
    parent = path.parent
    file_descriptor = None
    temporary_path = None
    try:
        file_descriptor, temporary_name = tempfile.mkstemp(
            dir=str(parent),
            prefix=f".{path.name}.",
            suffix=".tmp",
        )
        temporary_path = Path(temporary_name)
        descriptor_to_close = file_descriptor
        with os.fdopen(descriptor_to_close, "wb") as temporary_file:
            file_descriptor = None
            temporary_file.write(contents)
            temporary_file.flush()
            _apply_environment_metadata(
                temporary_file.fileno(),
                temporary_path,
                backup,
                preserve_owner=os.name != "nt",
                fchown=getattr(os, "fchown", None),
                fchmod=getattr(os, "fchmod", None),
                chmod=os.chmod,
            )
            os.fsync(temporary_file.fileno())
        current = _read_environment_backup(path)
        if current.contents != expected_current_contents:
            raise UpdaterError(
                "ENVIRONMENT_CONFLICT",
                "deployment environment changed during update",
            )
        os.replace(temporary_path, path)
        _fsync_directory(parent)
    except BaseException as error:
        if file_descriptor is not None:
            try:
                os.close(file_descriptor)
            except OSError:
                pass
        if temporary_path is not None:
            try:
                temporary_path.unlink()
            except OSError:
                pass
        if isinstance(error, UpdaterError):
            raise
        if isinstance(error, OSError):
            raise UpdaterError(
                "ENVIRONMENT_WRITE_FAILED",
                "failed to update deployment environment",
            ) from None
        raise


def replace_sub2api_image(
    environment_file: str | Path,
    target_image: str,
    *,
    backup: EnvironmentBackup | None = None,
) -> EnvironmentBackup:
    original = backup or _read_environment_backup(environment_file)
    updated_contents = _updated_environment_contents(
        original.contents,
        target_image,
    )
    _atomic_environment_write(
        environment_file,
        updated_contents,
        original,
        expected_current_contents=original.contents,
    )
    return original


def restore_environment(
    environment_file: str | Path,
    backup: EnvironmentBackup,
    *,
    expected_current_contents: bytes | None = None,
) -> None:
    expected_contents = expected_current_contents
    if expected_contents is None:
        expected_contents = _read_environment_backup(
            environment_file
        ).contents
    _atomic_environment_write(
        environment_file,
        backup.contents,
        backup,
        expected_current_contents=expected_contents,
    )


def _http_health_ok(url: str, timeout: float) -> bool:
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            return response.status == 200
    except (
        urllib.error.URLError,
        http.client.HTTPException,
        OSError,
    ):
        return False


class UpdaterCore:
    def __init__(
        self,
        config: UpdaterConfig,
        runner: CommandRunner = run_command,
        monotonic: Callable[[], float] = time.monotonic,
        sleep: Callable[[float], None] = time.sleep,
        health_checker: HealthChecker = _http_health_ok,
        environment_path: str | Path | None = None,
    ):
        self.config = config
        self._runner = runner
        self._monotonic = monotonic
        self._sleep = sleep
        self._health_checker = health_checker
        self._environment_path = Path(
            environment_path
            if environment_path is not None
            else self.config.environment_file
        )
        self._operation_lock = threading.Lock()

    def target_image(self, version: str) -> str:
        validated_version = validate_version(version)
        return f"{self.config.image_repository}:{validated_version}"

    def load_state(self) -> UpdateState:
        state_path = Path(self.config.state_file)
        try:
            with state_path.open("r", encoding="utf-8") as state_file:
                raw_state = json.load(state_file)
        except FileNotFoundError:
            return UpdateState()
        except (OSError, UnicodeError, json.JSONDecodeError):
            raise UpdaterError(
                "STATE_INVALID",
                "update state file is invalid",
            ) from None

        if not isinstance(raw_state, dict):
            raise UpdaterError(
                "STATE_INVALID",
                "update state file is invalid",
            )
        if not set(raw_state).issubset(_STATE_FIELDS):
            raise UpdaterError(
                "STATE_INVALID",
                "update state file is invalid",
            )

        values = {
            field: raw_state.get(field, "idle" if field == "state" else "")
            for field in _STATE_FIELDS
        }
        if any(not isinstance(value, str) for value in values.values()):
            raise UpdaterError(
                "STATE_INVALID",
                "update state file is invalid",
            )
        if values["state"] not in _VALID_STATES:
            raise UpdaterError(
                "STATE_INVALID",
                "update state file is invalid",
            )
        return UpdateState(**values)

    def save_state(self, state: UpdateState) -> None:
        state_path = Path(self.config.state_file)
        parent = state_path.parent
        file_descriptor = None
        temporary_path = None
        try:
            parent.mkdir(parents=True, exist_ok=True)
            file_descriptor, temporary_name = tempfile.mkstemp(
                dir=str(parent),
                prefix=f".{state_path.name}.",
                suffix=".tmp",
            )
            temporary_path = Path(temporary_name)
            os.chmod(temporary_path, 0o600)
            with os.fdopen(
                file_descriptor,
                "w",
                encoding="utf-8",
                newline="\n",
            ) as temporary_file:
                serialized_state = {
                    field: value
                    for field, value in dataclasses.asdict(state).items()
                    if field in _PUBLIC_STATE_FIELDS or value
                }
                json.dump(
                    serialized_state,
                    temporary_file,
                    ensure_ascii=True,
                    separators=(",", ":"),
                    sort_keys=True,
                )
                temporary_file.write("\n")
                temporary_file.flush()
                os.fsync(temporary_file.fileno())
            os.replace(temporary_path, state_path)
            self._fsync_parent_directory(parent)
        except BaseException as error:
            if file_descriptor is not None:
                try:
                    os.close(file_descriptor)
                except OSError:
                    pass
            if temporary_path is not None:
                try:
                    temporary_path.unlink()
                except OSError:
                    pass
            if isinstance(error, Exception):
                raise UpdaterError(
                    "STATE_WRITE_FAILED",
                    "failed to persist update state",
                ) from None
            raise

    def prepare(self, version: str) -> UpdateState:
        with self._operation_guard():
            return self._prepare(version)

    def _prepare(self, version: str) -> UpdateState:
        validated_version = validate_version(version)
        target_image = self.target_image(validated_version)
        existing_state = self.load_state()

        if existing_state.state in {"preparing", "activating"}:
            raise UpdaterError(
                "AGENT_BUSY",
                "another update operation is already in progress",
            )
        if (
            existing_state.state == "prepared"
            and existing_state.target_image == target_image
        ):
            return existing_state

        self.save_state(
            UpdateState(
                state="preparing",
                current_image=existing_state.current_image,
                target_image=target_image,
                previous_image=existing_state.previous_image,
                message="preparing target image",
                updated_at=_updated_at(),
            )
        )

        try:
            self._pull_image(target_image)
            self._verify_image(target_image, validated_version)
            current_image = self._inspect_current_image()
        except UpdaterError as operation_error:
            try:
                self.save_state(
                    UpdateState(
                        state="failed",
                        current_image=existing_state.current_image,
                        target_image=target_image,
                        previous_image=existing_state.previous_image,
                        message=operation_error.message,
                        updated_at=_updated_at(),
                    )
                )
            except UpdaterError as state_error:
                if state_error.code != "STATE_WRITE_FAILED":
                    raise
            raise

        prepared_state = UpdateState(
            state="prepared",
            current_image=current_image,
            target_image=target_image,
            previous_image=current_image,
            message="target image prepared",
            updated_at=_updated_at(),
        )
        self.save_state(prepared_state)
        return prepared_state

    def begin_activation(self) -> UpdateState:
        with self._operation_guard():
            return self._begin_activation()

    def _begin_activation(self) -> UpdateState:
        existing_state = self.load_state()
        if existing_state.state == "activating":
            raise UpdaterError(
                "ACTIVATION_IN_PROGRESS",
                "image activation is already in progress",
            )
        if (
            existing_state.state != "prepared"
            or not existing_state.target_image
            or not existing_state.previous_image
        ):
            raise UpdaterError(
                "NO_PREPARED_UPDATE",
                "no prepared image is available for activation",
            )

        activating_state = UpdateState(
            state="activating",
            current_image=existing_state.current_image,
            target_image=existing_state.target_image,
            previous_image=existing_state.previous_image,
            message="activation scheduled",
            updated_at=_updated_at(),
        )
        self.save_state(activating_state)
        return activating_state

    def run_activation(self) -> UpdateState:
        with self._operation_guard():
            return self._run_scheduled_activation()

    def _run_scheduled_activation(self) -> UpdateState:
        existing_state = self.load_state()
        if (
            existing_state.state != "activating"
            or not existing_state.target_image
            or not existing_state.previous_image
        ):
            raise UpdaterError(
                "NO_ACTIVATION_IN_PROGRESS",
                "no scheduled image activation is available",
            )
        return self._run_activation(existing_state)

    def activate(self) -> UpdateState:
        with self._operation_guard():
            self._begin_activation()
            return self._run_scheduled_activation()

    def _run_activation(self, activating_state: UpdateState) -> UpdateState:
        started_at = self._monotonic()
        overall_deadline = (
            started_at + self.config.activation_timeout_seconds
        )
        activation_deadline = (
            started_at + self.config.activation_timeout_seconds / 2
        )
        try:
            target_image_id, previous_image_id = (
                self._activation_preflight(
                    activating_state,
                    activation_deadline,
                )
            )
        except UpdaterError:
            return self._save_terminal_state(
                activating_state,
                state_name="failed",
                current_image=activating_state.previous_image,
                message="activation preflight failed",
            )

        activating_state = dataclasses.replace(
            activating_state,
            target_image_id=target_image_id,
            previous_image_id=previous_image_id,
            updated_at=_updated_at(),
        )
        self.save_state(activating_state)

        try:
            environment_backup = _read_environment_backup(
                self._environment_path
            )
        except UpdaterError:
            return self._save_terminal_state(
                activating_state,
                state_name="failed",
                current_image=activating_state.previous_image,
                message="activation failed before environment update",
            )

        try:
            target_environment_contents = _updated_environment_contents(
                environment_backup.contents,
                activating_state.target_image,
            )
        except UpdaterError:
            return self._save_terminal_state(
                activating_state,
                state_name="failed",
                current_image=activating_state.previous_image,
                message="activation failed before environment update",
            )

        try:
            replace_sub2api_image(
                self._environment_path,
                activating_state.target_image,
                backup=environment_backup,
            )
            self._compose_up(
                "ACTIVATION_FAILED",
                "target image activation failed",
                deadline=activation_deadline,
            )
            self._wait_for_image_health(
                target_image_id,
                "ACTIVATION_FAILED",
                "target image activation failed",
                deadline=activation_deadline,
            )
        except UpdaterError:
            return self._rollback_activation(
                activating_state,
                environment_backup,
                previous_image_id,
                overall_deadline,
                target_environment_contents,
            )

        return self._save_terminal_state(
            activating_state,
            state_name="healthy",
            current_image=activating_state.target_image,
            message="target image activated and healthy",
        )

    def _rollback_activation(
        self,
        activating_state: UpdateState,
        environment_backup: EnvironmentBackup,
        previous_image_id: str,
        rollback_deadline: float,
        expected_environment_contents: bytes,
    ) -> UpdateState:
        try:
            current_environment = _read_environment_backup(
                self._environment_path
            )
            if current_environment.contents == expected_environment_contents:
                restore_environment(
                    self._environment_path,
                    environment_backup,
                    expected_current_contents=expected_environment_contents,
                )
            elif current_environment.contents != environment_backup.contents:
                raise UpdaterError(
                    "ENVIRONMENT_CONFLICT",
                    "deployment environment changed during update",
                )
            self._compose_up(
                "ROLLBACK_FAILED",
                "automatic rollback failed",
                deadline=rollback_deadline,
            )
            self._wait_for_image_health(
                previous_image_id,
                "ROLLBACK_FAILED",
                "automatic rollback failed",
                deadline=rollback_deadline,
            )
        except UpdaterError as rollback_error:
            message = "activation failed and automatic rollback failed"
            if rollback_error.code == "ENVIRONMENT_CONFLICT":
                message = (
                    "activation failed; deployment environment changed; "
                    "manual recovery required"
                )
            return self._save_terminal_state(
                activating_state,
                state_name="rollback_failed",
                current_image="",
                message=message,
            )

        return self._save_terminal_state(
            activating_state,
            state_name="rolled_back",
            current_image=activating_state.previous_image,
            message="activation failed; previous image restored",
        )

    def reconcile_state(self) -> UpdateState:
        with self._operation_guard():
            existing_state = self.load_state()
            if existing_state.state == "preparing":
                return self._save_terminal_state(
                    existing_state,
                    state_name="failed",
                    current_image=existing_state.current_image,
                    message="interrupted image preparation requires retry",
                )
            if existing_state.state != "activating":
                return existing_state
            return self._reconcile_activation(existing_state)

    def _reconcile_activation(
        self,
        activating_state: UpdateState,
    ) -> UpdateState:
        error_code = "RECONCILIATION_FAILED"
        error_message = "unable to reconcile interrupted activation"
        if (
            not activating_state.target_image_id
            or not activating_state.previous_image_id
        ):
            return self._save_terminal_state(
                activating_state,
                state_name="failed",
                current_image="",
                message=error_message,
            )

        deadline = (
            self._monotonic() + self.config.activation_timeout_seconds
        )
        candidates = (
            (
                activating_state.target_image_id,
                activating_state.target_image,
                "healthy",
                activating_state.target_image,
                "target image activated and healthy",
            ),
            (
                activating_state.previous_image_id,
                activating_state.previous_image,
                "rolled_back",
                activating_state.previous_image,
                "activation failed; previous image restored",
            ),
        )
        observed_image = ""

        while self._monotonic() < deadline:
            try:
                container_timeout = self._remaining_inspect_timeout(
                    deadline,
                    error_code,
                    error_message,
                )
                container_image_id, health_status = (
                    self._inspect_running_container(
                        error_code,
                        error_message,
                        timeout=container_timeout,
                    )
                )
            except UpdaterError:
                container_image_id = ""
                health_status = ""

            for (
                candidate_image_id,
                image,
                state_name,
                current_image,
                message,
            ) in candidates:
                if container_image_id == candidate_image_id:
                    observed_image = image
                if (
                    health_status != "healthy"
                    or container_image_id != candidate_image_id
                ):
                    continue
                if not self._check_http_health(deadline):
                    break
                try:
                    replace_sub2api_image(
                        self._environment_path,
                        image,
                    )
                except UpdaterError:
                    return self._save_terminal_state(
                        activating_state,
                        state_name="failed",
                        current_image=image,
                        message="unable to reconcile deployment environment",
                    )
                return self._save_terminal_state(
                    activating_state,
                    state_name=state_name,
                    current_image=current_image,
                    message=message,
                )

            now = self._monotonic()
            if now >= deadline:
                break
            sleep_seconds = min(
                max(self.config.poll_interval_seconds, 0.01),
                deadline - now,
            )
            self._sleep(sleep_seconds)

        return self._save_terminal_state(
            activating_state,
            state_name="failed",
            current_image=observed_image,
            message=error_message,
        )

    def _save_terminal_state(
        self,
        source_state: UpdateState,
        *,
        state_name: str,
        current_image: str,
        message: str,
    ) -> UpdateState:
        terminal_state = UpdateState(
            state=state_name,
            current_image=current_image,
            target_image=source_state.target_image,
            previous_image=source_state.previous_image,
            message=message,
            updated_at=_updated_at(),
        )
        self.save_state(terminal_state)
        return terminal_state

    @contextlib.contextmanager
    def _operation_guard(self):
        if not self._operation_lock.acquire(blocking=False):
            raise UpdaterError(
                "AGENT_BUSY",
                "another update operation is already in progress",
            )
        try:
            yield
        finally:
            self._operation_lock.release()

    def _pull_image(self, target_image: str) -> None:
        self._run_checked(
            ["docker", "pull", target_image],
            self.config.prepare_timeout_seconds,
            "IMAGE_PULL_FAILED",
            "target image pull failed",
        )

    def _activation_preflight(
        self,
        activating_state: UpdateState,
        deadline: float,
    ) -> tuple[str, str]:
        error_code = "ACTIVATION_FAILED"
        error_message = "activation preflight failed"
        image_prefix = f"{self.config.image_repository}:"
        target_image = activating_state.target_image
        if not target_image.startswith(image_prefix):
            raise UpdaterError(error_code, error_message)
        version = target_image[len(image_prefix) :]
        try:
            validated_version = validate_version(version)
        except UpdaterError:
            raise UpdaterError(error_code, error_message) from None
        if target_image != f"{self.config.image_repository}:{validated_version}":
            raise UpdaterError(error_code, error_message)

        target_timeout = self._remaining_inspect_timeout(
            deadline,
            error_code,
            error_message,
        )
        target_image_id = self._inspect_verified_image_id(
            target_image,
            validated_version,
            timeout=target_timeout,
            error_code=error_code,
            error_message=error_message,
        )
        container_timeout = self._remaining_inspect_timeout(
            deadline,
            error_code,
            error_message,
        )
        current_container_image_id = self._inspect_container_image_id(
            error_code,
            error_message,
            timeout=container_timeout,
        )
        previous_timeout = self._remaining_inspect_timeout(
            deadline,
            error_code,
            error_message,
        )
        previous_image_id = self._inspect_image_id(
            activating_state.previous_image,
            error_code,
            error_message,
            timeout=previous_timeout,
        )
        if current_container_image_id != previous_image_id:
            raise UpdaterError(error_code, error_message)
        return target_image_id, previous_image_id

    def _compose_up(
        self,
        error_code: str,
        error_message: str,
        deadline: float | None = None,
    ) -> None:
        timeout = self.config.activation_timeout_seconds
        if deadline is not None:
            timeout = self._remaining_deadline_timeout(
                deadline,
                timeout,
                error_code,
                error_message,
            )
        self._run_checked(
            [
                "docker",
                "compose",
                "--project-directory",
                self.config.compose_directory,
                "-f",
                self.config.compose_file,
                "--env-file",
                self.config.environment_file,
                "up",
                "-d",
                "--no-deps",
                "--force-recreate",
                self.config.service_name,
            ],
            timeout,
            error_code,
            error_message,
        )

    def _wait_for_image_health(
        self,
        expected_image_id: str,
        error_code: str,
        error_message: str,
        deadline: float,
    ) -> None:
        while True:
            if self._monotonic() >= deadline:
                raise UpdaterError(error_code, error_message)
            try:
                container_timeout = self._remaining_inspect_timeout(
                    deadline,
                    error_code,
                    error_message,
                )
                container_image_id, health_status = (
                    self._inspect_running_container(
                        error_code,
                        error_message,
                        timeout=container_timeout,
                    )
                )
                if (
                    container_image_id == expected_image_id
                    and health_status == "healthy"
                    and self._check_http_health(deadline)
                ):
                    return
            except UpdaterError:
                pass

            now = self._monotonic()
            if now >= deadline:
                raise UpdaterError(error_code, error_message)
            sleep_seconds = min(
                max(self.config.poll_interval_seconds, 0.01),
                deadline - now,
            )
            self._sleep(sleep_seconds)

    def _remaining_inspect_timeout(
        self,
        deadline: float,
        error_code: str,
        error_message: str,
    ) -> float:
        return self._remaining_deadline_timeout(
            deadline,
            30.0,
            error_code,
            error_message,
        )

    def _remaining_deadline_timeout(
        self,
        deadline: float,
        maximum_timeout: float,
        error_code: str,
        error_message: str,
    ) -> float:
        remaining = deadline - self._monotonic()
        if remaining <= 0:
            raise UpdaterError(error_code, error_message)
        return min(maximum_timeout, remaining)

    def _check_http_health(self, deadline: float | None = None) -> bool:
        timeout = 5.0
        if deadline is not None:
            remaining = deadline - self._monotonic()
            if remaining <= 0:
                return False
            timeout = min(timeout, remaining)
        try:
            return self._health_checker(
                self.config.health_url,
                timeout,
            )
        except (http.client.HTTPException, OSError):
            return False

    def _inspect_image_id(
        self,
        image: str,
        error_code: str,
        error_message: str,
        timeout: float = 30,
    ) -> str:
        result = self._run_checked(
            ["docker", "image", "inspect", image],
            timeout,
            error_code,
            error_message,
        )
        metadata = self._parse_inspect_object(
            result.stdout,
            error_code,
            error_message,
        )
        image_id = metadata.get("Id")
        if not isinstance(image_id, str) or not image_id:
            raise UpdaterError(error_code, error_message)
        return image_id

    def _inspect_running_container(
        self,
        error_code: str = "RECONCILIATION_FAILED",
        error_message: str = "unable to reconcile interrupted activation",
        timeout: float = 30,
    ) -> tuple[str, str]:
        result = self._run_checked(
            ["docker", "inspect", self.config.container_name],
            timeout,
            error_code,
            error_message,
        )
        metadata = self._parse_inspect_object(
            result.stdout,
            error_code,
            error_message,
        )
        image_id = metadata.get("Image")
        state = metadata.get("State")
        health = state.get("Health") if isinstance(state, dict) else None
        health_status = (
            health.get("Status") if isinstance(health, dict) else None
        )
        if (
            not isinstance(image_id, str)
            or not image_id
            or not isinstance(health_status, str)
            or not health_status
        ):
            raise UpdaterError(error_code, error_message)
        return image_id, health_status

    def _verify_image(self, target_image: str, version: str) -> None:
        self._inspect_verified_image_id(
            target_image,
            version,
            timeout=self.config.prepare_timeout_seconds,
            error_code="IMAGE_VERIFICATION_FAILED",
            error_message="target image verification failed",
        )

    def _inspect_verified_image_id(
        self,
        target_image: str,
        version: str,
        *,
        timeout: float,
        error_code: str,
        error_message: str,
    ) -> str:
        result = self._run_checked(
            ["docker", "image", "inspect", target_image],
            timeout,
            error_code,
            error_message,
        )
        metadata = self._parse_inspect_object(
            result.stdout,
            error_code,
            error_message,
        )
        config = metadata.get("Config")
        labels = config.get("Labels") if isinstance(config, dict) else None
        image_id = metadata.get("Id")
        if (
            not isinstance(image_id, str)
            or not image_id
            or metadata.get("Architecture")
            != self.config.expected_architecture
            or not isinstance(labels, dict)
            or labels.get("org.opencontainers.image.source")
            != self.config.image_source
            or labels.get("org.opencontainers.image.version") != version
        ):
            raise UpdaterError(
                error_code,
                error_message,
            )
        return image_id

    def _inspect_container_image_id(
        self,
        error_code: str,
        error_message: str,
        timeout: float = 30,
    ) -> str:
        result = self._run_checked(
            ["docker", "inspect", self.config.container_name],
            timeout,
            error_code,
            error_message,
        )
        metadata = self._parse_inspect_object(
            result.stdout,
            error_code,
            error_message,
        )
        image_id = metadata.get("Image")
        if not isinstance(image_id, str) or not image_id:
            raise UpdaterError(error_code, error_message)
        return image_id

    def _inspect_current_image(self) -> str:
        result = self._run_checked(
            ["docker", "inspect", self.config.container_name],
            30,
            "IMAGE_VERIFICATION_FAILED",
            "current container inspection failed",
        )
        metadata = self._parse_inspect_object(result.stdout)
        config = metadata.get("Config")
        current_image = config.get("Image") if isinstance(config, dict) else None
        if not isinstance(current_image, str) or not current_image:
            raise UpdaterError(
                "IMAGE_VERIFICATION_FAILED",
                "current container inspection failed",
            )
        return current_image

    def _run_checked(
        self,
        args: list[str],
        timeout: float,
        error_code: str,
        error_message: str,
    ) -> subprocess.CompletedProcess[str]:
        try:
            result = self._runner(args, timeout)
            return_code = result.returncode
        except (subprocess.TimeoutExpired, OSError):
            raise UpdaterError(error_code, error_message) from None
        if not isinstance(return_code, int) or return_code != 0:
            raise UpdaterError(error_code, error_message)
        return result

    @staticmethod
    def _parse_inspect_object(
        output: str,
        error_code: str = "IMAGE_VERIFICATION_FAILED",
        error_message: str = "docker inspection returned invalid metadata",
    ) -> dict:
        try:
            parsed = json.loads(output)
        except (TypeError, json.JSONDecodeError):
            raise UpdaterError(error_code, error_message) from None
        if (
            not isinstance(parsed, list)
            or len(parsed) != 1
            or not isinstance(parsed[0], dict)
        ):
            raise UpdaterError(error_code, error_message)
        return parsed[0]

    @staticmethod
    def _fsync_parent_directory(parent: Path) -> None:
        _fsync_directory(parent)
