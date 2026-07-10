#!/usr/bin/env python3

import argparse
import dataclasses
import datetime
import json
import logging
import math
import os
import posixpath
import re
import socket
import socketserver
import stat
import struct
import threading
from http.server import BaseHTTPRequestHandler
from pathlib import Path
from urllib.parse import urlsplit

try:
    from .updater_core import (
        UpdateState,
        UpdaterConfig,
        UpdaterCore,
        UpdaterError,
    )
except ImportError:
    from updater_core import (
        UpdateState,
        UpdaterConfig,
        UpdaterCore,
        UpdaterError,
    )


PROTOCOL_VERSION = "v1"
MAX_REQUEST_BODY_BYTES = 4096
MAX_LINUX_ID = 2147483647
REQUEST_TIMEOUT_SECONDS = 10
MAX_CONCURRENT_REQUESTS = 16
DEFAULT_CONFIG_PATH = "/etc/sub2api-updater/config.json"

_PUBLIC_STATE_FIELDS = (
    "state",
    "current_image",
    "target_image",
    "previous_image",
    "message",
    "updated_at",
)
_ROUTE_METHODS = {
    "/v1/health": "GET",
    "/v1/prepare": "POST",
    "/v1/activate": "POST",
    "/v1/status": "GET",
}
_CONFIG_FIELDS = tuple(field.name for field in dataclasses.fields(UpdaterConfig))
_SAFE_NAME_PATTERN = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]*$")
_IMAGE_REPOSITORY_PATTERN = re.compile(
    r"^[A-Za-z0-9][A-Za-z0-9._-]*(?::[0-9]+)?"
    r"(?:/[A-Za-z0-9][A-Za-z0-9._-]*)+$"
)
_LOGGER = logging.getLogger("sub2api_updater")
_UNIX_STREAM_SERVER = getattr(
    socketserver,
    "UnixStreamServer",
    socketserver.BaseServer,
)


class RequestError(Exception):
    def __init__(self, status: int, code: str, message: str):
        self.status = status
        self.code = code
        self.message = message
        super().__init__(message)


class _ActivationResponseDeliveryAborted(Exception):
    pass


class _DuplicateConfigKey(ValueError):
    pass


def _object_without_duplicate_keys(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise _DuplicateConfigKey("duplicate configuration key")
        value[key] = item
    return value


def _reject_json_constant(_value):
    raise ValueError("non-standard JSON constant")


def _read_json_object(path):
    try:
        with Path(path).open("r", encoding="utf-8") as config_file:
            value = json.load(
                config_file,
                object_pairs_hook=_object_without_duplicate_keys,
                parse_constant=_reject_json_constant,
            )
    except (
        OSError,
        UnicodeError,
        ValueError,
    ):
        raise ValueError("updater configuration is invalid") from None
    if not isinstance(value, dict):
        raise ValueError("updater configuration must be a JSON object")
    return value


def _require_string(config, field):
    value = config[field]
    if type(value) is not str or not value:
        raise ValueError(f"{field} must be a non-empty string")
    if any(ord(character) < 32 or ord(character) == 127 for character in value):
        raise ValueError(f"{field} contains invalid characters")
    return value


def _require_absolute_path(config, field):
    value = _require_string(config, field)
    if (
        not posixpath.isabs(value)
        or value.startswith("//")
        or posixpath.normpath(value) != value
    ):
        raise ValueError(f"{field} must be an absolute path")
    return value


def _require_nonnegative_integer(config, field):
    value = config[field]
    if type(value) is not int or not 0 <= value <= MAX_LINUX_ID:
        raise ValueError(f"{field} must be a nonnegative integer")
    return value


def _require_positive_integer(config, field):
    value = config[field]
    if type(value) is not int or value <= 0:
        raise ValueError(f"{field} must be a positive integer")
    return value


def _require_positive_number(config, field):
    value = config[field]
    if (
        type(value) not in (int, float)
        or value <= 0
        or not math.isfinite(value)
    ):
        raise ValueError(f"{field} must be a positive number")
    return float(value)


def _require_safe_name(config, field):
    value = _require_string(config, field)
    if _SAFE_NAME_PATTERN.fullmatch(value) is None:
        raise ValueError(f"{field} is invalid")
    return value


def _require_url(config, field, schemes):
    value = _require_string(config, field)
    if any(character.isspace() for character in value):
        raise ValueError(f"{field} is invalid")
    try:
        parsed = urlsplit(value)
        parsed.port
    except ValueError:
        raise ValueError(f"{field} is invalid") from None
    if (
        parsed.scheme not in schemes
        or not parsed.netloc
        or parsed.hostname is None
        or parsed.username is not None
        or parsed.password is not None
        or parsed.fragment
    ):
        raise ValueError(f"{field} is invalid")
    return value


def load_config(path):
    raw_config = _read_json_object(path)
    if set(raw_config) != set(_CONFIG_FIELDS):
        raise ValueError("updater configuration keys do not match the schema")

    allowed_uids = raw_config["allowed_uids"]
    if (
        type(allowed_uids) is not list
        or not allowed_uids
        or any(
            type(uid) is not int or not 0 <= uid <= MAX_LINUX_ID
            for uid in allowed_uids
        )
        or len(set(allowed_uids)) != len(allowed_uids)
    ):
        raise ValueError("allowed_uids must contain unique nonnegative integers")

    image_repository = _require_string(raw_config, "image_repository")
    if _IMAGE_REPOSITORY_PATTERN.fullmatch(image_repository) is None:
        raise ValueError("image_repository is invalid")

    values = {
        "socket_path": _require_absolute_path(raw_config, "socket_path"),
        "socket_gid": _require_nonnegative_integer(raw_config, "socket_gid"),
        "allowed_uids": tuple(allowed_uids),
        "image_repository": image_repository,
        "image_source": _require_url(
            raw_config,
            "image_source",
            {"https"},
        ),
        "compose_directory": _require_absolute_path(
            raw_config,
            "compose_directory",
        ),
        "compose_file": _require_absolute_path(raw_config, "compose_file"),
        "environment_file": _require_absolute_path(
            raw_config,
            "environment_file",
        ),
        "service_name": _require_safe_name(raw_config, "service_name"),
        "container_name": _require_safe_name(raw_config, "container_name"),
        "expected_architecture": _require_safe_name(
            raw_config,
            "expected_architecture",
        ),
        "health_url": _require_url(
            raw_config,
            "health_url",
            {"http", "https"},
        ),
        "prepare_timeout_seconds": _require_positive_integer(
            raw_config,
            "prepare_timeout_seconds",
        ),
        "activation_timeout_seconds": _require_positive_integer(
            raw_config,
            "activation_timeout_seconds",
        ),
        "poll_interval_seconds": _require_positive_number(
            raw_config,
            "poll_interval_seconds",
        ),
        "state_file": _require_absolute_path(raw_config, "state_file"),
    }
    return UpdaterConfig(**values)


def _updated_at():
    return (
        datetime.datetime.now(datetime.timezone.utc)
        .isoformat(timespec="seconds")
        .replace("+00:00", "Z")
    )


def public_state(state):
    return {
        field: getattr(state, field)
        for field in _PUBLIC_STATE_FIELDS
    }


def require_linux_unix_support():
    missing_capabilities = []
    if getattr(socket, "AF_UNIX", None) is None:
        missing_capabilities.append("AF_UNIX")
    if getattr(socket, "SO_PEERCRED", None) is None:
        missing_capabilities.append("SO_PEERCRED")
    if getattr(socketserver, "UnixStreamServer", None) is None:
        missing_capabilities.append("UnixStreamServer")
    if missing_capabilities:
        raise RuntimeError(
            "Linux Unix socket capabilities are required: "
            + ", ".join(missing_capabilities)
        )


def read_linux_peer_credentials(connection):
    credential_size = struct.calcsize("3i")
    packed_credentials = connection.getsockopt(
        socket.SOL_SOCKET,
        socket.SO_PEERCRED,
        credential_size,
    )
    return struct.unpack("3i", packed_credentials)


def remove_stale_socket(socket_path):
    path = os.fspath(socket_path)
    try:
        metadata = os.lstat(path)
    except FileNotFoundError:
        return
    except OSError:
        raise RuntimeError("unable to inspect updater socket path") from None
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISSOCK(metadata.st_mode):
        raise RuntimeError(
            "existing updater socket path is not a safe Unix socket"
        )
    try:
        os.unlink(path)
    except OSError:
        raise RuntimeError("unable to remove stale updater socket") from None


def _socket_identity(socket_path):
    metadata = os.lstat(socket_path)
    if stat.S_ISLNK(metadata.st_mode) or not stat.S_ISSOCK(metadata.st_mode):
        raise RuntimeError("bound updater socket path is not a Unix socket")
    return metadata.st_dev, metadata.st_ino


def _remove_owned_socket(socket_path, identity):
    if identity is None:
        return
    try:
        metadata = os.lstat(socket_path)
    except FileNotFoundError:
        return
    except OSError:
        return
    if (
        stat.S_ISSOCK(metadata.st_mode)
        and not stat.S_ISLNK(metadata.st_mode)
        and (metadata.st_dev, metadata.st_ino) == identity
    ):
        try:
            os.unlink(socket_path)
        except OSError:
            pass


class ActivationDispatch:
    def __init__(
        self,
        application,
        state,
        response_gate,
        cancel_event,
    ):
        self.state = state
        self._application = application
        self._response_gate = response_gate
        self._cancel_event = cancel_event
        self._decision = None
        self._failure_claimed = False
        self._lock = threading.Lock()

    def release(self):
        with self._lock:
            if self._decision is not None:
                return
            try:
                self._response_gate.set()
            except Exception:
                self._decision = "cancel"
                self._cancel_event.set()
                raise
            self._decision = "run"

    def cancel(self, message):
        gate_failed = False
        should_persist = False
        with self._lock:
            if self._decision == "run":
                return
            if self._decision is None:
                self._decision = "cancel"
                self._cancel_event.set()
                try:
                    self._response_gate.set()
                except Exception:
                    gate_failed = True
            if not self._failure_claimed:
                self._failure_claimed = True
                should_persist = True
        if gate_failed:
            self._application.logger.error(
                "activation response gate cancellation failed"
            )
        if should_persist:
            self._application.persist_failed_state(self.state, message)

    def wait_for_worker_decision(self):
        while True:
            self._response_gate.wait(timeout=0.1)
            with self._lock:
                if self._decision is not None:
                    return self._decision
                if self._cancel_event.is_set():
                    return "cancel"

    def worker_was_cancelled(self):
        with self._lock:
            return (
                self._decision == "cancel"
                or self._cancel_event.is_set()
            )


class UpdaterApplication:
    def __init__(
        self,
        core,
        *,
        thread_factory=threading.Thread,
        gate_factory=threading.Event,
        cancel_event_factory=threading.Event,
        dispatch_factory=ActivationDispatch,
        logger=None,
    ):
        self.core = core
        self.thread_factory = thread_factory
        self.gate_factory = gate_factory
        self.cancel_event_factory = cancel_event_factory
        self.dispatch_factory = dispatch_factory
        self.logger = logger or _LOGGER
        self._worker_lock = threading.Lock()
        self._activation_worker_thread = None

    def prepare(self, version):
        return self.core.prepare(version)

    def status(self):
        return self.core.load_state()

    def begin_activation(self):
        with self._worker_lock:
            if (
                self._activation_worker_thread is not None
                and self._activation_worker_thread.is_alive()
            ):
                raise UpdaterError(
                    "ACTIVATION_IN_PROGRESS",
                    "image activation is already in progress",
                )

            activating_state = self.core.begin_activation()
            cancel_event = None
            response_gate = None
            dispatch = None
            try:
                cancel_event = self.cancel_event_factory()
                response_gate = self.gate_factory()
                dispatch = self.dispatch_factory(
                    self,
                    activating_state,
                    response_gate,
                    cancel_event,
                )
                worker = self.thread_factory(
                    target=self._run_activation_worker,
                    args=(
                        dispatch,
                        activating_state,
                    ),
                    name="sub2api-activation",
                    daemon=True,
                )
                if not worker.daemon:
                    worker.daemon = True
                self._activation_worker_thread = worker
                worker.start()
            except Exception:
                self._activation_worker_thread = None
                try:
                    if dispatch is not None:
                        dispatch.cancel(
                            "activation worker failed to start"
                        )
                    else:
                        if cancel_event is not None:
                            cancel_event.set()
                        if response_gate is not None:
                            try:
                                response_gate.set()
                            except Exception:
                                self.logger.error(
                                    (
                                        "activation response gate "
                                        "cancellation failed"
                                    )
                                )
                        self.persist_failed_state(
                            activating_state,
                            "activation worker failed to start",
                        )
                except UpdaterError:
                    raise
                raise UpdaterError(
                    "ACTIVATION_START_FAILED",
                    "activation worker failed to start",
                ) from None

        return dispatch

    def _run_activation_worker(
        self,
        dispatch,
        activating_state,
    ):
        try:
            try:
                decision = dispatch.wait_for_worker_decision()
                if decision != "run":
                    return
                terminal_state = self.core.run_activation()
                terminal_is_incomplete = terminal_state.state in {
                    "preparing",
                    "activating",
                }
            except Exception:
                if dispatch.worker_was_cancelled():
                    return
                self.logger.error("activation worker failed")
                self._reconcile_worker_failure(activating_state)
                return
            if terminal_is_incomplete:
                self._reconcile_worker_failure(activating_state)
        finally:
            with self._worker_lock:
                if self._activation_worker_thread is threading.current_thread():
                    self._activation_worker_thread = None

    def _reconcile_worker_failure(self, activating_state):
        try:
            reconciled_state = self.core.reconcile_state()
            reconciliation_is_incomplete = reconciled_state.state in {
                "preparing",
                "activating",
            }
        except Exception:
            self.logger.error(
                "activation worker state reconciliation failed"
            )
            self.persist_failed_state(
                activating_state,
                "activation worker failed and state reconciliation failed",
            )
            return
        if reconciliation_is_incomplete:
            self.persist_failed_state(
                activating_state,
                "activation worker failed and state reconciliation failed",
            )

    def persist_failed_state(self, source_state, message):
        failed_state = UpdateState(
            state="failed",
            current_image=source_state.current_image,
            target_image=source_state.target_image,
            previous_image=source_state.previous_image,
            message=message,
            updated_at=_updated_at(),
        )
        try:
            self.core.save_state(failed_state)
        except Exception:
            self.logger.error("failed to persist activation failure state")
            try:
                reconciled_state = self.core.reconcile_state()
                if reconciled_state.state in {"preparing", "activating"}:
                    raise RuntimeError
            except Exception:
                self.logger.error(
                    "activation failure state reconciliation failed"
                )
                raise UpdaterError(
                    "ACTIVATION_STATE_RECOVERY_FAILED",
                    (
                        "activation failure state could not be persisted "
                        "or reconciled"
                    ),
                ) from None
            return reconciled_state
        return failed_state


def _updater_error_status(error_code):
    if error_code == "INVALID_VERSION":
        return 400
    if error_code in {
        "AGENT_BUSY",
        "NO_PREPARED_UPDATE",
        "ACTIVATION_IN_PROGRESS",
    }:
        return 409
    if error_code in {
        "IMAGE_PULL_FAILED",
        "IMAGE_VERIFICATION_FAILED",
    }:
        return 502
    return 500


def _json_without_duplicate_keys(pairs):
    value = {}
    for key, item in pairs:
        if key in value:
            raise ValueError("duplicate JSON key")
        value[key] = item
    return value


class UpdaterRequestHandler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "sub2api-updater"
    sys_version = ""

    def log_message(self, _format, *_args):
        return

    def __getattr__(self, name):
        if name.startswith("do_"):
            return self._dispatch_request
        raise AttributeError(name)

    def do_GET(self):
        self._dispatch_request()

    def do_POST(self):
        self._dispatch_request()

    def _dispatch_request(self):
        try:
            self._reject_oversized_declared_body()
            required_method = _ROUTE_METHODS.get(self.path)
            if required_method is None:
                raise RequestError(404, "NOT_FOUND", "route not found")
            if self.command != required_method:
                raise RequestError(
                    405,
                    "METHOD_NOT_ALLOWED",
                    "method not allowed",
                )
            if self.path == "/v1/health":
                self._send_json(
                    200,
                    {
                        "ready": True,
                        "protocol_version": PROTOCOL_VERSION,
                    },
                )
                return
            if self.path == "/v1/status":
                self._send_json(
                    200,
                    public_state(self.server.application.status()),
                )
                return
            if self.path == "/v1/prepare":
                self._handle_prepare()
                return
            if self.path == "/v1/activate":
                self._handle_activate()
                return
        except _ActivationResponseDeliveryAborted:
            self.close_connection = True
            return
        except RequestError as error:
            self._send_request_error(error)
        except UpdaterError as error:
            self._send_updater_error(error)
        except (BrokenPipeError, ConnectionResetError):
            return
        except Exception:
            self.server.application.logger.error(
                "updater HTTP handler failed"
            )
            self._send_internal_error()

    def _handle_prepare(self):
        content_types = self.headers.get_all("Content-Type") or []
        if (
            len(content_types) != 1
            or content_types[0].strip().lower() != "application/json"
        ):
            raise RequestError(
                415,
                "UNSUPPORTED_MEDIA_TYPE",
                "Content-Type must be application/json",
            )
        body = self._read_request_body(require_length=True)
        try:
            decoded_body = body.decode("utf-8")
            payload = json.loads(
                decoded_body,
                object_pairs_hook=_json_without_duplicate_keys,
                parse_constant=_reject_json_constant,
            )
        except (UnicodeError, json.JSONDecodeError, ValueError):
            raise RequestError(
                400,
                "INVALID_REQUEST",
                "request body must be valid JSON",
            ) from None
        if (
            not isinstance(payload, dict)
            or set(payload) != {"version"}
            or type(payload["version"]) is not str
        ):
            raise RequestError(
                400,
                "INVALID_REQUEST",
                "request body must contain only a string version",
            )
        state = self.server.application.prepare(payload["version"])
        self._send_json(200, public_state(state))

    def _handle_activate(self):
        body = self._read_request_body(require_length=False)
        if body:
            raise RequestError(
                400,
                "INVALID_REQUEST",
                "activation request body must be empty",
            )
        dispatch = self.server.application.begin_activation()
        try:
            self._send_json(202, public_state(dispatch.state))
        except Exception:
            self.server.application.logger.error(
                "activation response delivery failed"
            )
            try:
                dispatch.cancel("activation response could not be delivered")
            except Exception:
                self.server.application.logger.error(
                    "activation response failure recovery failed"
                )
            self.close_connection = True
            raise _ActivationResponseDeliveryAborted from None
        try:
            dispatch.release()
        except Exception:
            self.server.application.logger.error(
                "activation response gate release failed"
            )
            try:
                dispatch.cancel(
                    "activation response gate could not be released"
                )
            except Exception:
                self.server.application.logger.error(
                    "activation response failure recovery failed"
                )
            self.close_connection = True
            return

    def _reject_oversized_declared_body(self):
        self._declared_body_length(require_length=False)

    def _declared_body_length(self, require_length):
        transfer_encodings = self.headers.get_all("Transfer-Encoding") or []
        if transfer_encodings:
            raise RequestError(
                400,
                "INVALID_REQUEST",
                "transfer encoding is not supported",
            )
        content_lengths = self.headers.get_all("Content-Length") or []
        if not content_lengths:
            if require_length:
                raise RequestError(
                    400,
                    "INVALID_REQUEST",
                    "Content-Length is required",
                )
            return 0
        if len(content_lengths) != 1:
            raise RequestError(
                400,
                "INVALID_REQUEST",
                "Content-Length is invalid",
            )
        content_length_text = content_lengths[0].strip()
        if re.fullmatch(r"[0-9]+", content_length_text) is None:
            raise RequestError(
                400,
                "INVALID_REQUEST",
                "Content-Length is invalid",
            )
        normalized_length = content_length_text.lstrip("0") or "0"
        maximum_length = str(MAX_REQUEST_BODY_BYTES)
        if (
            len(normalized_length) > len(maximum_length)
            or (
                len(normalized_length) == len(maximum_length)
                and normalized_length > maximum_length
            )
        ):
            raise RequestError(
                413,
                "REQUEST_BODY_TOO_LARGE",
                "request body exceeds 4096 bytes",
            )
        return int(normalized_length)

    def _read_request_body(self, require_length):
        content_length = self._declared_body_length(require_length)
        if content_length == 0:
            return b""
        body = self.rfile.read(content_length)
        if len(body) != content_length:
            raise RequestError(
                400,
                "INVALID_REQUEST",
                "request body length does not match Content-Length",
            )
        return body

    def _send_request_error(self, error):
        self._send_json(
            error.status,
            {"code": error.code, "message": error.message},
        )

    def _send_updater_error(self, error):
        self._send_json(
            _updater_error_status(error.code),
            {"code": error.code, "message": error.message},
        )

    def _send_internal_error(self):
        try:
            self._send_json(
                500,
                {
                    "code": "INTERNAL_ERROR",
                    "message": "update agent request failed",
                },
            )
        except (BrokenPipeError, ConnectionResetError):
            pass

    def _send_json(self, status, payload):
        serialized = json.dumps(
            payload,
            ensure_ascii=True,
            separators=(",", ":"),
        ).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(serialized)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(serialized)
        self.wfile.flush()
        self.close_connection = True


class PeerCheckedUnixHTTPServer(
    socketserver.ThreadingMixIn,
    _UNIX_STREAM_SERVER,
):
    daemon_threads = True
    block_on_close = True
    allow_reuse_address = False

    def __init__(
        self,
        socket_path,
        application,
        allowed_uids,
        socket_gid,
        *,
        peer_credential_getter=read_linux_peer_credentials,
    ):
        self.application = application
        self.allowed_uids = frozenset(allowed_uids)
        self.peer_credential_getter = peer_credential_getter
        self.socket_path = os.fspath(socket_path)
        self._owned_socket_identity = None
        self._request_slots = threading.BoundedSemaphore(
            MAX_CONCURRENT_REQUESTS
        )
        remove_stale_socket(self.socket_path)
        try:
            super().__init__(self.socket_path, UpdaterRequestHandler)
            self._owned_socket_identity = _socket_identity(self.socket_path)
            os.chown(self.socket_path, -1, socket_gid)
            os.chmod(self.socket_path, 0o660)
            socket_metadata = os.lstat(self.socket_path)
            if (
                stat.S_ISLNK(socket_metadata.st_mode)
                or not stat.S_ISSOCK(socket_metadata.st_mode)
                or (
                    socket_metadata.st_dev,
                    socket_metadata.st_ino,
                )
                != self._owned_socket_identity
                or socket_metadata.st_gid != socket_gid
                or stat.S_IMODE(socket_metadata.st_mode) != 0o660
            ):
                raise RuntimeError(
                    "unable to set updater socket ownership and mode"
                )
        except Exception:
            try:
                super().server_close()
            except Exception:
                pass
            _remove_owned_socket(
                self.socket_path,
                self._owned_socket_identity,
            )
            raise

    def get_request(self):
        connection, address = self.socket.accept()
        try:
            _pid, uid, _gid = self.peer_credential_getter(connection)
        except Exception:
            connection.close()
            raise OSError(
                "unable to read peer credentials"
            ) from None
        if uid not in self.allowed_uids:
            connection.close()
            raise ConnectionAbortedError(
                "peer uid is not allowed"
            )
        try:
            connection.settimeout(REQUEST_TIMEOUT_SECONDS)
        except Exception:
            connection.close()
            raise OSError(
                "unable to configure request timeout"
            ) from None
        return connection, address

    def process_request(self, request, client_address):
        if not self._request_slots.acquire(blocking=False):
            self.shutdown_request(request)
            return
        try:
            super().process_request(request, client_address)
        except Exception:
            try:
                self.shutdown_request(request)
            except Exception:
                pass
            self._request_slots.release()
            raise

    def process_request_thread(self, request, client_address):
        try:
            super().process_request_thread(request, client_address)
        finally:
            self._request_slots.release()

    def server_close(self):
        try:
            super().server_close()
        finally:
            _remove_owned_socket(
                self.socket_path,
                self._owned_socket_identity,
            )
            self._owned_socket_identity = None


def create_server(
    config,
    core,
    *,
    peer_credential_getter=read_linux_peer_credentials,
    thread_factory=threading.Thread,
    gate_factory=threading.Event,
    logger=None,
):
    require_linux_unix_support()
    application = UpdaterApplication(
        core,
        thread_factory=thread_factory,
        gate_factory=gate_factory,
        logger=logger,
    )
    return PeerCheckedUnixHTTPServer(
        config.socket_path,
        application,
        config.allowed_uids,
        config.socket_gid,
        peer_credential_getter=peer_credential_getter,
    )


def run_service(config_path):
    config = load_config(config_path)
    core = UpdaterCore(config)
    core.reconcile_state()
    server = create_server(config, core)
    try:
        server.serve_forever()
    finally:
        server.server_close()


def main(argv=None):
    parser = argparse.ArgumentParser(
        description="Sub2API host Docker update service",
    )
    parser.add_argument(
        "--config",
        default=DEFAULT_CONFIG_PATH,
        help="path to the root-owned updater configuration",
    )
    arguments = parser.parse_args(argv)
    logging.basicConfig(
        level=logging.INFO,
        format="%(levelname)s %(name)s: %(message)s",
    )
    try:
        run_service(arguments.config)
    except KeyboardInterrupt:
        return 0
    except (OSError, RuntimeError, UpdaterError, ValueError):
        _LOGGER.error("updater service failed")
        return 1
    except Exception:
        _LOGGER.error("updater service failed unexpectedly")
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
