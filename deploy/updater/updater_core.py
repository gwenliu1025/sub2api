import contextlib
import dataclasses
import datetime
import json
import os
import re
import subprocess
import tempfile
import threading
from pathlib import Path
from typing import Callable


VERSION_PATTERN = re.compile(r"^[0-9]+(?:\.[0-9]+)+$")

_STATE_FIELDS = (
    "state",
    "current_image",
    "target_image",
    "previous_image",
    "message",
    "updated_at",
)
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


class UpdaterCore:
    def __init__(
        self,
        config: UpdaterConfig,
        runner: CommandRunner = run_command,
    ):
        self.config = config
        self._runner = runner
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
                json.dump(
                    dataclasses.asdict(state),
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

    def _verify_image(self, target_image: str, version: str) -> None:
        result = self._run_checked(
            ["docker", "image", "inspect", target_image],
            self.config.prepare_timeout_seconds,
            "IMAGE_VERIFICATION_FAILED",
            "target image verification failed",
        )
        metadata = self._parse_inspect_object(result.stdout)
        config = metadata.get("Config")
        labels = config.get("Labels") if isinstance(config, dict) else None
        if (
            metadata.get("Architecture")
            != self.config.expected_architecture
            or not isinstance(labels, dict)
            or labels.get("org.opencontainers.image.source")
            != self.config.image_source
            or labels.get("org.opencontainers.image.version") != version
        ):
            raise UpdaterError(
                "IMAGE_VERIFICATION_FAILED",
                "target image verification failed",
            )

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
    def _parse_inspect_object(output: str) -> dict:
        try:
            parsed = json.loads(output)
        except (TypeError, json.JSONDecodeError):
            raise UpdaterError(
                "IMAGE_VERIFICATION_FAILED",
                "docker inspection returned invalid metadata",
            ) from None
        if (
            not isinstance(parsed, list)
            or len(parsed) != 1
            or not isinstance(parsed[0], dict)
        ):
            raise UpdaterError(
                "IMAGE_VERIFICATION_FAILED",
                "docker inspection returned invalid metadata",
            )
        return parsed[0]

    @staticmethod
    def _fsync_parent_directory(parent: Path) -> None:
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
