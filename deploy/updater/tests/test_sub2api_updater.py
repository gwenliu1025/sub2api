import dataclasses
import json
import os
import socket
import tempfile
import threading
import types
import unittest
from email.message import Message
from pathlib import Path
from unittest import mock

from deploy.updater import sub2api_updater
from deploy.updater.updater_core import (
    UpdateState,
    UpdaterConfig,
    UpdaterError,
    validate_version,
)


IMAGE_REPOSITORY = "ghcr.io/gwenliu1025/sub2api"
IMAGE_SOURCE = "https://github.com/gwenliu1025/sub2api"
CURRENT_IMAGE = "xqian/sub2api:equivalent-cache-20260709-083433"
TARGET_IMAGE = f"{IMAGE_REPOSITORY}:0.1.150"
PUBLIC_STATE_FIELDS = {
    "state",
    "current_image",
    "target_image",
    "previous_image",
    "message",
    "updated_at",
}


def idle_state():
    return UpdateState(
        state="idle",
        current_image=CURRENT_IMAGE,
        message="ready",
        updated_at="2026-07-10T00:00:00Z",
    )


def prepared_state():
    return UpdateState(
        state="prepared",
        current_image=CURRENT_IMAGE,
        target_image=TARGET_IMAGE,
        previous_image=CURRENT_IMAGE,
        message="target image prepared",
        updated_at="2026-07-10T00:01:00Z",
        target_image_id="sha256:must-not-be-public",
        previous_image_id="sha256:must-not-be-public-either",
    )


def activating_state():
    return UpdateState(
        state="activating",
        current_image=CURRENT_IMAGE,
        target_image=TARGET_IMAGE,
        previous_image=CURRENT_IMAGE,
        message="activation scheduled",
        updated_at="2026-07-10T00:02:00Z",
        target_image_id="sha256:must-not-be-public",
        previous_image_id="sha256:must-not-be-public-either",
    )


class FakeCore:
    def __init__(self, state=None):
        self.state = state or idle_state()
        self.prepare_calls = []
        self.begin_calls = 0
        self.run_calls = 0
        self.load_calls = 0
        self.reconcile_calls = 0
        self.saved_states = []
        self.prepare_error = None
        self.begin_error = None
        self.run_error = None
        self.reconcile_error = None
        self.run_started = threading.Event()
        self.reconcile_finished = threading.Event()

    def prepare(self, version):
        self.prepare_calls.append(version)
        validate_version(version)
        if self.prepare_error is not None:
            raise self.prepare_error
        self.state = prepared_state()
        return self.state

    def begin_activation(self):
        self.begin_calls += 1
        if self.begin_error is not None:
            raise self.begin_error
        self.state = activating_state()
        return self.state

    def run_activation(self):
        self.run_calls += 1
        self.run_started.set()
        if self.run_error is not None:
            raise self.run_error
        self.state = dataclasses.replace(
            activating_state(),
            state="healthy",
            current_image=TARGET_IMAGE,
            message="target image activated and healthy",
        )
        return self.state

    def load_state(self):
        self.load_calls += 1
        return self.state

    def reconcile_state(self):
        self.reconcile_calls += 1
        try:
            if self.reconcile_error is not None:
                raise self.reconcile_error
            self.state = dataclasses.replace(
                activating_state(),
                state="failed",
                current_image="",
                message="unable to reconcile interrupted activation",
            )
            return self.state
        finally:
            self.reconcile_finished.set()

    def save_state(self, state):
        self.state = state
        self.saved_states.append(state)


class BlockingResponseGate:
    def __init__(self):
        self.response_flushed = threading.Event()
        self.release_worker = threading.Event()

    def wait(self, timeout=None):
        return self.release_worker.wait(timeout)

    def set(self):
        self.response_flushed.set()


class FailingReleaseGate:
    def __init__(self):
        self._event = threading.Event()
        self.set_calls = 0

    def wait(self, timeout=None):
        return self._event.wait(timeout)

    def set(self):
        self.set_calls += 1
        if self.set_calls == 1:
            raise RuntimeError("gate detail must not escape")
        self._event.set()


class SignalingThenFailingGate:
    def __init__(self, worker_started):
        self._event = threading.Event()
        self._worker_started = worker_started
        self.set_calls = 0

    def wait(self, timeout=None):
        return self._event.wait(timeout)

    def set(self):
        self.set_calls += 1
        self._event.set()
        if self.set_calls == 1:
            self._worker_started.wait(1)
            raise RuntimeError("gate detail must not escape")


class FailingWaitGate:
    def wait(self, timeout=None):
        raise RuntimeError("worker secret must not be logged")

    def set(self):
        return None


class PreSignalFailingGate:
    def __init__(self):
        self._event = threading.Event()
        self.set_calls = 0

    def wait(self, timeout=None):
        return self._event.wait(timeout)

    def set(self):
        self.set_calls += 1
        if self.set_calls == 1:
            raise RuntimeError("gate detail must not escape")
        self._event.set()


class FakeConnection:
    def __init__(self, uid):
        self.uid = uid
        self.closed = False
        self.closed_event = threading.Event()
        self.dispatched = False
        self.timeout = None
        self.shutdown_calls = []

    def close(self):
        self.closed = True
        self.closed_event.set()

    def settimeout(self, timeout):
        self.timeout = timeout

    def shutdown(self, how):
        self.shutdown_calls.append(how)


class FakeListener:
    def __init__(self, accepted):
        self.accepted = list(accepted)

    def accept(self):
        if not self.accepted:
            raise OSError("no more test connections")
        item = self.accepted.pop(0)
        if isinstance(item, BaseException):
            raise item
        return item


def parse_http_response(raw_response):
    head, body = raw_response.split(b"\r\n\r\n", 1)
    lines = head.split(b"\r\n")
    status = int(lines[0].split()[1])
    headers = {}
    for line in lines[1:]:
        name, value = line.split(b":", 1)
        headers[name.decode("ascii").lower()] = value.strip().decode("ascii")
    if "content-length" in headers:
        expected_length = int(headers["content-length"])
        if len(body) != expected_length:
            raise AssertionError(
                f"response body length {len(body)} != {expected_length}"
            )
    payload = None
    if body:
        decoded_body = body.decode("utf-8")
        try:
            payload = json.loads(decoded_body)
        except json.JSONDecodeError:
            payload = decoded_body
    return status, headers, payload


def dispatch_raw_http_bytes(raw_request, application):
    client_socket, server_socket = socket.socketpair()
    server = types.SimpleNamespace(application=application)
    handler_errors = []

    def run_handler():
        try:
            sub2api_updater.UpdaterRequestHandler(
                server_socket,
                ("local", 0),
                server,
            )
        except BaseException as error:
            handler_errors.append(error)
        finally:
            server_socket.close()

    handler_thread = threading.Thread(target=run_handler, daemon=True)
    handler_thread.start()
    try:
        client_socket.settimeout(5)
        client_socket.sendall(raw_request)
        client_socket.shutdown(socket.SHUT_WR)
        chunks = []
        while True:
            chunk = client_socket.recv(65536)
            if not chunk:
                break
            chunks.append(chunk)
    finally:
        client_socket.close()
    handler_thread.join(5)
    if handler_thread.is_alive():
        raise AssertionError("HTTP handler did not finish")
    if handler_errors:
        raise handler_errors[0]
    return b"".join(chunks)


def dispatch_raw_http(raw_request, application):
    return parse_http_response(
        dispatch_raw_http_bytes(raw_request, application)
    )


def http_request(method, path, body=b"", headers=None, include_length=None):
    request_headers = {
        "Host": "localhost",
        "Connection": "close",
    }
    request_headers.update(headers or {})
    if include_length is None:
        include_length = method == "POST"
    if include_length:
        request_headers["Content-Length"] = str(len(body))
    serialized_headers = "".join(
        f"{name}: {value}\r\n" for name, value in request_headers.items()
    )
    return (
        f"{method} {path} HTTP/1.1\r\n{serialized_headers}\r\n".encode(
            "ascii"
        )
        + body
    )


def application_for(core, **kwargs):
    return sub2api_updater.UpdaterApplication(core, **kwargs)


class UpdaterHTTPTest(unittest.TestCase):
    def test_health_returns_protocol_version(self):
        status, headers, payload = dispatch_raw_http(
            http_request("GET", "/v1/health"),
            application_for(FakeCore()),
        )

        self.assertEqual(200, status)
        self.assertEqual("application/json", headers["content-type"])
        self.assertEqual(
            {"ready": True, "protocol_version": "v1"},
            payload,
        )

    def test_prepare_requires_json_and_numeric_version(self):
        cases = [
            (
                "missing content type",
                b'{"version":"0.1.150"}',
                {},
            ),
            (
                "wrong content type",
                b'{"version":"0.1.150"}',
                {"Content-Type": "text/plain"},
            ),
            (
                "non-object",
                b'["0.1.150"]',
                {"Content-Type": "application/json"},
            ),
            (
                "missing version",
                b"{}",
                {"Content-Type": "application/json"},
            ),
            (
                "extra key",
                b'{"version":"0.1.150","image":"attacker/image"}',
                {"Content-Type": "application/json"},
            ),
            (
                "duplicate key",
                b'{"version":"0.1.150","version":"0.1.151"}',
                {"Content-Type": "application/json"},
            ),
            (
                "non-string version",
                b'{"version":150}',
                {"Content-Type": "application/json"},
            ),
            (
                "invalid numeric version",
                b'{"version":"v0.1.150"}',
                {"Content-Type": "application/json"},
            ),
            (
                "invalid utf8",
                b'{"version":"\xff"}',
                {"Content-Type": "application/json"},
            ),
            (
                "invalid json",
                b'{"version":',
                {"Content-Type": "application/json"},
            ),
        ]

        for name, body, headers in cases:
            with self.subTest(name=name):
                core = FakeCore()
                status, _, payload = dispatch_raw_http(
                    http_request(
                        "POST",
                        "/v1/prepare",
                        body,
                        headers=headers,
                    ),
                    application_for(core),
                )

                self.assertIn(status, {400, 415})
                self.assertEqual({"code", "message"}, set(payload))
                self.assertNotIn(body.decode("utf-8", "ignore"), payload["message"])
                if name != "invalid numeric version":
                    self.assertEqual([], core.prepare_calls)

        self.assertEqual(
            "INVALID_VERSION",
            dispatch_raw_http(
                http_request(
                    "POST",
                    "/v1/prepare",
                    b'{"version":"v0.1.150"}',
                    headers={"Content-Type": "application/json"},
                ),
                application_for(FakeCore()),
            )[2]["code"],
        )

    def test_prepare_returns_prepared_status(self):
        core = FakeCore()
        status, headers, payload = dispatch_raw_http(
            http_request(
                "POST",
                "/v1/prepare",
                b'{"version":"0.1.150"}',
                headers={"Content-Type": "application/json"},
            ),
            application_for(core),
        )

        self.assertEqual(200, status)
        self.assertEqual("application/json", headers["content-type"])
        self.assertEqual(["0.1.150"], core.prepare_calls)
        self.assertEqual(PUBLIC_STATE_FIELDS, set(payload))
        self.assertEqual("prepared", payload["state"])
        self.assertEqual(TARGET_IMAGE, payload["target_image"])
        self.assertNotIn("target_image_id", payload)
        self.assertNotIn("previous_image_id", payload)

    def test_activate_returns_202_before_worker_runs(self):
        core = FakeCore(prepared_state())
        gate = BlockingResponseGate()
        application = application_for(core, gate_factory=lambda: gate)

        status, headers, payload = dispatch_raw_http(
            http_request("POST", "/v1/activate"),
            application,
        )

        self.assertEqual(202, status)
        self.assertEqual("application/json", headers["content-type"])
        self.assertEqual(str(len(json.dumps(payload, separators=(",", ":")))), headers["content-length"])
        self.assertEqual("activating", payload["state"])
        self.assertTrue(gate.response_flushed.wait(1))
        self.assertFalse(core.run_started.is_set())

        gate.release_worker.set()
        self.assertTrue(core.run_started.wait(2))
        self.assertEqual(1, core.run_calls)

    def test_status_returns_persisted_state(self):
        core = FakeCore(prepared_state())
        status, _, payload = dispatch_raw_http(
            http_request("GET", "/v1/status"),
            application_for(core),
        )

        self.assertEqual(200, status)
        self.assertEqual(1, core.load_calls)
        self.assertEqual(PUBLIC_STATE_FIELDS, set(payload))
        self.assertEqual("prepared", payload["state"])
        self.assertNotIn("target_image_id", payload)
        self.assertNotIn("previous_image_id", payload)

    def test_unknown_route_returns_404(self):
        for method in ("GET", "BREW"):
            with self.subTest(method=method):
                status, headers, payload = dispatch_raw_http(
                    http_request(method, "/v1/not-found"),
                    application_for(FakeCore()),
                )

                self.assertEqual(404, status)
                self.assertEqual(
                    "application/json",
                    headers["content-type"],
                )
                self.assertEqual("NOT_FOUND", payload["code"])

    def test_request_body_over_4096_bytes_is_rejected(self):
        core = FakeCore()
        body = b"x" * 4097
        status, _, payload = dispatch_raw_http(
            http_request(
                "POST",
                "/v1/prepare",
                body,
                headers={"Content-Type": "application/json"},
            ),
            application_for(core),
        )

        self.assertEqual(413, status)
        self.assertEqual("REQUEST_BODY_TOO_LARGE", payload["code"])
        self.assertEqual([], core.prepare_calls)

    def test_extremely_long_content_length_is_rejected_stably(self):
        core = FakeCore(prepared_state())
        raw_request = (
            b"POST /v1/activate HTTP/1.1\r\n"
            b"Host: localhost\r\n"
            b"Content-Length: "
            + (b"9" * 4301)
            + b"\r\n"
            b"Connection: close\r\n\r\n"
        )

        status, headers, payload = dispatch_raw_http(
            raw_request,
            application_for(core, logger=mock.Mock()),
        )

        self.assertEqual(413, status)
        self.assertEqual("application/json", headers["content-type"])
        self.assertEqual("REQUEST_BODY_TOO_LARGE", payload["code"])
        self.assertEqual(0, core.begin_calls)

    def test_activate_rejects_a_nonempty_body(self):
        core = FakeCore(prepared_state())
        status, _, payload = dispatch_raw_http(
            http_request("POST", "/v1/activate", b"{}"),
            application_for(core),
        )

        self.assertEqual(400, status)
        self.assertEqual("INVALID_REQUEST", payload["code"])
        self.assertEqual(0, core.begin_calls)

    def test_transfer_encoding_and_conflicting_lengths_are_rejected(self):
        cases = [
            (
                "transfer encoding",
                (
                    b"POST /v1/activate HTTP/1.1\r\n"
                    b"Host: localhost\r\n"
                    b"Transfer-Encoding: chunked\r\n"
                    b"Connection: close\r\n\r\n"
                    b"0\r\n\r\n"
                ),
            ),
            (
                "conflicting content lengths",
                (
                    b"POST /v1/activate HTTP/1.1\r\n"
                    b"Host: localhost\r\n"
                    b"Content-Length: 0\r\n"
                    b"Content-Length: 1\r\n"
                    b"Connection: close\r\n\r\n"
                    b"x"
                ),
            ),
        ]

        for name, request in cases:
            with self.subTest(name=name):
                core = FakeCore(prepared_state())
                status, _, payload = dispatch_raw_http(
                    request,
                    application_for(core),
                )

                self.assertEqual(400, status)
                self.assertEqual("INVALID_REQUEST", payload["code"])
                self.assertEqual(0, core.begin_calls)

    def test_unsupported_methods_return_stable_json(self):
        cases = [
            ("GET", "/v1/prepare"),
            ("POST", "/v1/status"),
            ("BREW", "/v1/health"),
            ("OPTIONS", "/v1/health"),
        ]
        for method, path in cases:
            with self.subTest(method=method, path=path):
                status, headers, payload = dispatch_raw_http(
                    http_request(method, path),
                    application_for(FakeCore()),
                )

                self.assertEqual(405, status)
                self.assertEqual(
                    "application/json",
                    headers["content-type"],
                )
                self.assertEqual(
                    {
                        "code": "METHOD_NOT_ALLOWED",
                        "message": "method not allowed",
                    },
                    payload,
                )

    def test_updater_errors_have_stable_http_mapping(self):
        cases = [
            ("INVALID_VERSION", 400),
            ("AGENT_BUSY", 409),
            ("NO_PREPARED_UPDATE", 409),
            ("ACTIVATION_IN_PROGRESS", 409),
            ("IMAGE_PULL_FAILED", 502),
            ("IMAGE_VERIFICATION_FAILED", 502),
            ("UNEXPECTED", 500),
        ]
        for error_code, expected_status in cases:
            with self.subTest(error_code=error_code):
                core = FakeCore()
                core.prepare_error = UpdaterError(
                    error_code,
                    "fixed sanitized detail",
                )
                status, _, payload = dispatch_raw_http(
                    http_request(
                        "POST",
                        "/v1/prepare",
                        b'{"version":"0.1.150"}',
                        headers={"Content-Type": "application/json"},
                    ),
                    application_for(core),
                )

                self.assertEqual(expected_status, status)
                self.assertEqual(error_code, payload["code"])
                self.assertEqual(
                    "fixed sanitized detail",
                    payload["message"],
                )

    def test_thread_start_failure_closes_activating_state(self):
        class FailingThread:
            def __init__(self, **_kwargs):
                self.daemon = True

            def start(self):
                raise RuntimeError("thread detail must not escape")

            def is_alive(self):
                return False

        core = FakeCore(prepared_state())
        status, _, payload = dispatch_raw_http(
            http_request("POST", "/v1/activate"),
            application_for(core, thread_factory=FailingThread),
        )

        self.assertEqual(500, status)
        self.assertEqual("ACTIVATION_START_FAILED", payload["code"])
        self.assertNotIn("thread detail", payload["message"])
        self.assertEqual("failed", core.state.state)
        self.assertEqual(
            "activation worker failed to start",
            core.state.message,
        )

    def test_response_failure_cancels_worker_and_closes_state(self):
        core = FakeCore(prepared_state())
        logger = mock.Mock()
        application = application_for(core, logger=logger)
        handler = object.__new__(sub2api_updater.UpdaterRequestHandler)
        handler.server = types.SimpleNamespace(application=application)
        handler.headers = Message()
        handler.headers["Content-Length"] = "0"

        with mock.patch.object(
            handler,
            "_send_json",
            side_effect=BrokenPipeError("client closed"),
        ):
            with self.assertRaises(
                sub2api_updater._ActivationResponseDeliveryAborted
            ):
                handler._handle_activate()

        self.assertFalse(core.run_started.wait(0.2))
        self.assertTrue(handler.close_connection)
        self.assertEqual("failed", core.state.state)
        self.assertEqual(
            "activation response could not be delivered",
            core.state.message,
        )
        logger.error.assert_called_once_with(
            "activation response delivery failed"
        )
        logger.exception.assert_not_called()

    def test_response_gate_release_failure_cancels_activation(self):
        core = FakeCore(prepared_state())
        gate = FailingReleaseGate()
        application = application_for(
            core,
            gate_factory=lambda: gate,
            logger=mock.Mock(),
        )
        handler = object.__new__(sub2api_updater.UpdaterRequestHandler)
        handler.server = types.SimpleNamespace(application=application)
        handler.headers = Message()
        handler.headers["Content-Length"] = "0"

        with mock.patch.object(handler, "_send_json"):
            handler._handle_activate()

        self.assertFalse(core.run_started.wait(0.2))
        self.assertEqual(1, gate.set_calls)
        self.assertEqual("failed", core.state.state)
        self.assertEqual(
            "activation response gate could not be released",
            core.state.message,
        )

    def test_partial_gate_release_never_authorizes_worker(self):
        core = FakeCore(prepared_state())
        gate = SignalingThenFailingGate(core.run_started)
        application = application_for(
            core,
            gate_factory=lambda: gate,
            logger=mock.Mock(),
        )
        handler = object.__new__(sub2api_updater.UpdaterRequestHandler)
        handler.server = types.SimpleNamespace(application=application)
        handler.headers = Message()
        handler.headers["Content-Length"] = "0"

        with mock.patch.object(handler, "_send_json"):
            handler._handle_activate()

        threading.Event().wait(0.1)
        self.assertEqual(0, core.run_calls)
        self.assertEqual("failed", core.state.state)
        self.assertEqual(
            "activation response gate could not be released",
            core.state.message,
        )

    def test_pre_signal_gate_failure_does_not_leave_worker_blocked(self):
        core = FakeCore(prepared_state())
        first_gate = PreSignalFailingGate()
        gates = iter((first_gate, threading.Event()))
        application = application_for(
            core,
            gate_factory=lambda: next(gates),
            logger=mock.Mock(),
        )
        handler = object.__new__(sub2api_updater.UpdaterRequestHandler)
        handler.server = types.SimpleNamespace(application=application)
        handler.headers = Message()
        handler.headers["Content-Length"] = "0"

        with mock.patch.object(handler, "_send_json"):
            handler._handle_activate()

        old_worker = application._activation_worker_thread
        self.assertIsNotNone(old_worker)
        self.assertEqual(0, core.run_calls)
        self.assertEqual("failed", core.state.state)
        self.assertEqual(
            "activation response gate could not be released",
            core.state.message,
        )

        old_worker.join(1)
        self.assertFalse(old_worker.is_alive())

        next_dispatch = application.begin_activation()
        try:
            self.assertEqual(2, core.begin_calls)
            self.assertEqual(0, core.run_calls)
        finally:
            next_dispatch.cancel("test cleanup")

    def test_post_202_recovery_failure_never_appends_500_response(self):
        class SaveFailingCore(FakeCore):
            def save_state(self, _state):
                raise RuntimeError("save secret must not escape")

        core = SaveFailingCore(prepared_state())
        core.reconcile_error = RuntimeError(
            "reconcile secret must not escape"
        )
        gate = SignalingThenFailingGate(core.run_started)
        logger = mock.Mock()
        application = application_for(
            core,
            gate_factory=lambda: gate,
            logger=logger,
        )

        raw_response = dispatch_raw_http_bytes(
            http_request("POST", "/v1/activate"),
            application,
        )

        self.assertEqual(1, raw_response.count(b"HTTP/1.1"))
        self.assertTrue(raw_response.startswith(b"HTTP/1.1 202 "))
        self.assertEqual(0, core.run_calls)
        expected_logs = {
            "activation response gate release failed",
            "failed to persist activation failure state",
            "activation failure state reconciliation failed",
            "activation response failure recovery failed",
        }
        actual_logs = {
            call.args[0]
            for call in logger.error.call_args_list
            if len(call.args) == 1
        }
        self.assertTrue(expected_logs.issubset(actual_logs))
        for log_call in logger.error.call_args_list:
            self.assertEqual(1, len(log_call.args))
            self.assertNotIn("secret", log_call.args[0])
        logger.exception.assert_not_called()

    def test_completed_202_send_failure_never_appends_500_response(self):
        class SaveFailingCore(FakeCore):
            def save_state(self, _state):
                raise RuntimeError("save secret must not escape")

        core = SaveFailingCore(prepared_state())
        core.reconcile_error = RuntimeError(
            "reconcile secret must not escape"
        )
        logger = mock.Mock()
        application = application_for(core, logger=logger)
        original_send_json = sub2api_updater.UpdaterRequestHandler._send_json

        def send_complete_202_then_fail(handler, status, payload):
            original_send_json(handler, status, payload)
            if status == 202:
                raise RuntimeError("delivery secret must not escape")

        with mock.patch.object(
            sub2api_updater.UpdaterRequestHandler,
            "_send_json",
            new=send_complete_202_then_fail,
        ):
            raw_response = dispatch_raw_http_bytes(
                http_request("POST", "/v1/activate"),
                application,
            )

        self.assertEqual(1, raw_response.count(b"HTTP/1.1"))
        self.assertTrue(raw_response.startswith(b"HTTP/1.1 202 "))
        self.assertEqual(0, core.run_calls)
        expected_logs = {
            "activation response delivery failed",
            "failed to persist activation failure state",
            "activation failure state reconciliation failed",
            "activation response failure recovery failed",
        }
        actual_logs = {
            call.args[0]
            for call in logger.error.call_args_list
            if len(call.args) == 1
        }
        self.assertTrue(expected_logs.issubset(actual_logs))
        for log_call in logger.error.call_args_list:
            self.assertEqual(1, len(log_call.args))
            self.assertNotIn("secret", log_call.args[0])
        logger.exception.assert_not_called()

    def test_activation_dispatch_uses_single_locked_decision(self):
        core = FakeCore(prepared_state())
        gate = BlockingResponseGate()
        application = application_for(
            core,
            gate_factory=lambda: gate,
            logger=mock.Mock(),
        )

        dispatch = application.begin_activation()
        try:
            self.assertFalse(hasattr(dispatch, "_decision_event"))
            self.assertTrue(hasattr(dispatch, "_decision"))
            self.assertTrue(hasattr(dispatch, "_lock"))
        finally:
            dispatch.cancel("test cleanup")
            gate.release_worker.set()

    def test_gate_creation_failure_closes_activating_state(self):
        def failing_gate_factory():
            raise RuntimeError("gate detail must not escape")

        core = FakeCore(prepared_state())
        status, _, payload = dispatch_raw_http(
            http_request("POST", "/v1/activate"),
            application_for(core, gate_factory=failing_gate_factory),
        )

        self.assertEqual(500, status)
        self.assertEqual("ACTIVATION_START_FAILED", payload["code"])
        self.assertNotIn("gate detail", payload["message"])
        self.assertEqual("failed", core.state.state)
        self.assertEqual(
            "activation worker failed to start",
            core.state.message,
        )

    def test_event_and_dispatch_creation_failures_close_activation(self):
        cases = [
            (
                "cancel event",
                "cancel_event_factory",
                mock.Mock(
                    side_effect=RuntimeError(
                        "event detail must not escape",
                    )
                ),
            ),
            (
                "dispatch",
                "dispatch_factory",
                mock.Mock(
                    side_effect=RuntimeError(
                        "dispatch detail must not escape",
                    )
                ),
            ),
        ]

        for name, factory_attribute, failing_factory in cases:
            with self.subTest(name=name):
                core = FakeCore(prepared_state())
                application = application_for(
                    core,
                    logger=mock.Mock(),
                )
                setattr(
                    application,
                    factory_attribute,
                    failing_factory,
                )
                status, _, payload = dispatch_raw_http(
                    http_request("POST", "/v1/activate"),
                    application,
                )

                self.assertEqual(500, status)
                self.assertEqual(
                    "ACTIVATION_START_FAILED",
                    payload["code"],
                )
                self.assertNotIn("detail", payload["message"])
                self.assertEqual("failed", core.state.state)
                self.assertEqual(0, core.run_calls)

    def test_worker_gate_wait_exception_reconciles_without_logging_detail(self):
        core = FakeCore(prepared_state())
        logger = mock.Mock()
        application = application_for(
            core,
            gate_factory=FailingWaitGate,
            logger=logger,
        )

        dispatch = application.begin_activation()
        dispatch.release()

        self.assertTrue(core.reconcile_finished.wait(2))
        self.assertEqual(1, core.reconcile_calls)
        logger.error.assert_called_with("activation worker failed")
        logger.exception.assert_not_called()

    def test_malformed_worker_and_reconcile_results_fail_closed(self):
        class MalformedCore(FakeCore):
            def run_activation(self):
                self.run_calls += 1
                self.run_started.set()
                return None

            def reconcile_state(self):
                self.reconcile_calls += 1
                self.reconcile_finished.set()
                return None

        core = MalformedCore(prepared_state())
        logger = mock.Mock()
        application = application_for(core, logger=logger)

        dispatch = application.begin_activation()
        dispatch.release()

        self.assertTrue(core.reconcile_finished.wait(2))
        for _ in range(20):
            if core.saved_states:
                break
            threading.Event().wait(0.01)
        self.assertTrue(core.saved_states)
        self.assertEqual("failed", core.saved_states[-1].state)
        self.assertEqual(
            "activation worker failed and state reconciliation failed",
            core.saved_states[-1].message,
        )
        logger.error.assert_any_call("activation worker failed")
        logger.error.assert_any_call(
            "activation worker state reconciliation failed"
        )
        logger.exception.assert_not_called()

    def test_failed_state_save_reconciles_immediately(self):
        class SaveFailingCore(FakeCore):
            def save_state(self, _state):
                raise RuntimeError("save secret must not be logged")

        core = SaveFailingCore(activating_state())
        logger = mock.Mock()
        application = application_for(core, logger=logger)

        result = application.persist_failed_state(
            activating_state(),
            "fixed activation failure",
        )

        self.assertEqual(1, core.reconcile_calls)
        self.assertEqual("failed", result.state)
        logger.error.assert_called_once_with(
            "failed to persist activation failure state"
        )
        logger.exception.assert_not_called()

    def test_failed_state_save_and_reconcile_failure_is_stable(self):
        class SaveFailingCore(FakeCore):
            def save_state(self, _state):
                raise RuntimeError("save secret must not escape")

        core = SaveFailingCore(activating_state())
        core.reconcile_error = RuntimeError(
            "reconcile secret must not escape"
        )
        logger = mock.Mock()
        application = application_for(core, logger=logger)

        with self.assertRaises(UpdaterError) as raised:
            application.persist_failed_state(
                activating_state(),
                "fixed activation failure",
            )

        self.assertEqual(
            "ACTIVATION_STATE_RECOVERY_FAILED",
            raised.exception.code,
        )
        self.assertEqual(
            "activation failure state could not be persisted or reconciled",
            raised.exception.message,
        )
        self.assertNotIn("secret", raised.exception.message)
        self.assertEqual(
            [
                mock.call("failed to persist activation failure state"),
                mock.call("activation failure state reconciliation failed"),
            ],
            logger.error.call_args_list,
        )
        logger.exception.assert_not_called()

    def test_worker_exception_reconciles_and_is_logged(self):
        core = FakeCore(prepared_state())
        core.run_error = RuntimeError("programming detail")
        logger = mock.Mock()
        application = application_for(core, logger=logger)

        dispatch = application.begin_activation()
        dispatch.release()

        self.assertTrue(core.reconcile_finished.wait(2))
        self.assertEqual(1, core.reconcile_calls)
        logger.error.assert_called_with("activation worker failed")
        logger.exception.assert_not_called()

    def test_worker_reconcile_failure_persists_fixed_sanitized_state(self):
        core = FakeCore(prepared_state())
        core.run_error = UpdaterError(
            "STATE_WRITE_FAILED",
            "secret command output",
        )
        core.reconcile_error = RuntimeError("secret environment contents")
        logger = mock.Mock()
        application = application_for(core, logger=logger)

        dispatch = application.begin_activation()
        dispatch.release()

        self.assertTrue(core.reconcile_finished.wait(2))
        for _ in range(20):
            if core.saved_states:
                break
            threading.Event().wait(0.01)
        self.assertTrue(core.saved_states)
        self.assertEqual("failed", core.saved_states[-1].state)
        self.assertEqual(
            "activation worker failed and state reconciliation failed",
            core.saved_states[-1].message,
        )
        self.assertNotIn("secret", core.saved_states[-1].message)
        logger.error.assert_any_call("activation worker failed")
        logger.error.assert_any_call(
            "activation worker state reconciliation failed"
        )
        logger.exception.assert_not_called()


class PeerCredentialServerTest(unittest.TestCase):
    def new_server(self, accepted, allowed_uids=(0, 1000)):
        server = object.__new__(
            sub2api_updater.PeerCheckedUnixHTTPServer
        )
        server.socket = FakeListener(accepted)
        server.allowed_uids = frozenset(allowed_uids)
        server.peer_credential_getter = (
            lambda connection: (1234, connection.uid, 1234)
        )
        return server

    def test_allowed_peer_uid_is_accepted(self):
        connection = FakeConnection(uid=1000)
        server = self.new_server([(connection, "peer")])

        accepted_connection, address = server.get_request()

        self.assertIs(connection, accepted_connection)
        self.assertEqual("peer", address)
        self.assertFalse(connection.closed)
        self.assertEqual(
            10,
            getattr(sub2api_updater, "REQUEST_TIMEOUT_SECONDS", None),
        )
        self.assertEqual(
            getattr(sub2api_updater, "REQUEST_TIMEOUT_SECONDS", None),
            connection.timeout,
        )

    def test_partial_request_body_times_out_and_handler_exits(self):
        client_socket, server_socket = socket.socketpair()
        server = self.new_server([])
        server.socket = FakeListener([(server_socket, "peer")])
        server.peer_credential_getter = lambda _connection: (1234, 1000, 1234)
        application = application_for(FakeCore(), logger=mock.Mock())
        handler_server = types.SimpleNamespace(application=application)
        handler_errors = []

        with mock.patch.object(
            sub2api_updater,
            "REQUEST_TIMEOUT_SECONDS",
            0.05,
            create=True,
        ):
            accepted_connection, address = server.get_request()

        def run_handler():
            try:
                sub2api_updater.UpdaterRequestHandler(
                    accepted_connection,
                    address,
                    handler_server,
                )
            except BaseException as error:
                handler_errors.append(error)

        handler_thread = threading.Thread(target=run_handler, daemon=True)
        handler_thread.start()
        try:
            client_socket.sendall(
                b"POST /v1/prepare HTTP/1.1\r\n"
                b"Host: localhost\r\n"
                b"Content-Type: application/json\r\n"
                b"Content-Length: 20\r\n"
                b"Connection: close\r\n\r\n"
                b"{"
            )
            handler_thread.join(0.5)
            handler_exited_after_timeout = not handler_thread.is_alive()
        finally:
            client_socket.close()
            handler_thread.join(1)
            accepted_connection.close()

        self.assertTrue(handler_exited_after_timeout)
        self.assertFalse(handler_thread.is_alive())
        self.assertEqual([], handler_errors)

    def test_concurrency_limit_rejects_excess_and_reuses_slot(self):
        server = object.__new__(
            sub2api_updater.PeerCheckedUnixHTTPServer
        )
        server._request_slots = threading.BoundedSemaphore(1)
        server.handle_error = mock.Mock()
        server.shutdown_request = lambda request: request.close()
        first_started = threading.Event()
        second_started = threading.Event()
        third_started = threading.Event()
        release_first = threading.Event()
        first = FakeConnection(uid=1000)
        second = FakeConnection(uid=1000)
        third = FakeConnection(uid=1000)

        def finish_request(request, _address):
            request.dispatched = True
            if request is first:
                first_started.set()
                release_first.wait(1)
            elif request is second:
                second_started.set()
            elif request is third:
                third_started.set()

        server.finish_request = finish_request

        self.assertEqual(
            16,
            getattr(sub2api_updater, "MAX_CONCURRENT_REQUESTS", None),
        )
        server.process_request(first, "first")
        self.assertTrue(first_started.wait(1))

        server.process_request(second, "second")
        self.assertTrue(second.closed_event.wait(1))
        self.assertFalse(second_started.is_set())
        self.assertFalse(second.dispatched)

        release_first.set()
        self.assertTrue(first.closed_event.wait(1))

        server.process_request(third, "third")
        self.assertTrue(third_started.wait(1))
        self.assertTrue(third.closed_event.wait(1))
        self.assertTrue(third.dispatched)

    def test_thread_start_failure_closes_request_and_releases_slot(self):
        class FailingThread:
            def __init__(self, **_kwargs):
                self.daemon = False

            def start(self):
                raise RuntimeError("thread could not start")

        server = object.__new__(
            sub2api_updater.PeerCheckedUnixHTTPServer
        )
        server._request_slots = threading.BoundedSemaphore(1)
        server.shutdown_request = lambda request: request.close()
        connection = FakeConnection(uid=1000)

        with mock.patch.object(
            sub2api_updater.socketserver.threading,
            "Thread",
            FailingThread,
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "thread could not start",
            ):
                server.process_request(connection, "peer")

        self.assertTrue(connection.closed)
        self.assertTrue(server._request_slots.acquire(blocking=False))
        server._request_slots.release()

    def test_disallowed_peer_uid_is_closed_without_dispatch(self):
        denied = FakeConnection(uid=2000)
        allowed = FakeConnection(uid=1000)
        server = self.new_server(
            [
                (denied, "denied"),
                (allowed, "allowed"),
            ]
        )

        with self.assertRaisesRegex(
            ConnectionAbortedError,
            "peer uid is not allowed",
        ):
            server.get_request()

        self.assertTrue(denied.closed)
        self.assertFalse(denied.dispatched)
        accepted_connection, address = server.get_request()
        self.assertIs(allowed, accepted_connection)
        self.assertEqual("allowed", address)

    def test_rejected_peer_does_not_wait_for_another_accept(self):
        class BlockingSecondAcceptListener:
            def __init__(self, first):
                self.first = first
                self.accept_calls = 0
                self.second_accept_started = threading.Event()
                self.release_second_accept = threading.Event()

            def accept(self):
                self.accept_calls += 1
                if self.accept_calls == 1:
                    return self.first
                self.second_accept_started.set()
                self.release_second_accept.wait(1)
                raise OSError("test listener released")

        denied = FakeConnection(uid=2000)
        listener = BlockingSecondAcceptListener((denied, "denied"))
        server = self.new_server([])
        server.socket = listener
        errors = []

        def get_one_request():
            try:
                server.get_request()
            except BaseException as error:
                errors.append(error)

        request_thread = threading.Thread(
            target=get_one_request,
            daemon=True,
        )
        request_thread.start()
        request_thread.join(0.2)
        returned_after_one_accept = not request_thread.is_alive()
        listener.release_second_accept.set()
        request_thread.join(1)

        self.assertTrue(returned_after_one_accept)
        self.assertEqual(1, listener.accept_calls)
        self.assertTrue(denied.closed)
        self.assertEqual(1, len(errors))
        self.assertIsInstance(errors[0], ConnectionAbortedError)

    def test_peer_getter_failure_closes_connection(self):
        connection = FakeConnection(uid=1000)
        allowed = FakeConnection(uid=1000)
        server = self.new_server(
            [
                (connection, "broken"),
                (allowed, "allowed"),
            ]
        )
        calls = 0

        def get_credentials(candidate):
            nonlocal calls
            calls += 1
            if calls == 1:
                raise OSError("peer credentials unavailable")
            return 1234, candidate.uid, 1234

        server.peer_credential_getter = get_credentials

        with self.assertRaisesRegex(
            OSError,
            "unable to read peer credentials",
        ):
            server.get_request()

        self.assertTrue(connection.closed)
        accepted_connection, _ = server.get_request()
        self.assertIs(allowed, accepted_connection)


class ConfigLoaderTest(unittest.TestCase):
    def setUp(self):
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        self.directory = Path(self.temporary_directory.name)
        self.config_path = self.directory / "config.json"
        self.valid_config = {
            "socket_path": "/run/sub2api-updater/updater.sock",
            "socket_gid": 1000,
            "allowed_uids": [0, 1000],
            "image_repository": IMAGE_REPOSITORY,
            "image_source": IMAGE_SOURCE,
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
            "state_file": "/var/lib/sub2api-updater/state.json",
        }

    def write_config(self, value):
        self.config_path.write_text(
            json.dumps(value),
            encoding="utf-8",
        )

    def test_load_config_accepts_exact_schema_and_converts_uids(self):
        self.write_config(self.valid_config)

        config = sub2api_updater.load_config(self.config_path)

        self.assertIsInstance(config, UpdaterConfig)
        self.assertEqual((0, 1000), config.allowed_uids)
        self.assertEqual(2.0, config.poll_interval_seconds)

    def test_load_config_rejects_unknown_missing_and_duplicate_keys(self):
        cases = {
            "unknown": {
                **self.valid_config,
                "unexpected": "value",
            },
            "missing": {
                key: value
                for key, value in self.valid_config.items()
                if key != "socket_path"
            },
        }
        for name, value in cases.items():
            with self.subTest(name=name):
                self.write_config(value)
                with self.assertRaises(ValueError):
                    sub2api_updater.load_config(self.config_path)

        duplicate_json = json.dumps(self.valid_config)[:-1] + (
            ',"socket_gid":1000}'
        )
        self.config_path.write_text(duplicate_json, encoding="utf-8")
        with self.assertRaises(ValueError):
            sub2api_updater.load_config(self.config_path)

    def test_load_config_rejects_bool_as_int(self):
        integer_fields = [
            "socket_gid",
            "prepare_timeout_seconds",
            "activation_timeout_seconds",
            "poll_interval_seconds",
        ]
        for field in integer_fields:
            with self.subTest(field=field):
                value = {**self.valid_config, field: True}
                self.write_config(value)
                with self.assertRaises(ValueError):
                    sub2api_updater.load_config(self.config_path)

        value = {**self.valid_config, "allowed_uids": [0, True]}
        self.write_config(value)
        with self.assertRaises(ValueError):
            sub2api_updater.load_config(self.config_path)

    def test_load_config_rejects_nonfinite_numbers_and_unsafe_syntax(self):
        cases = [
            ("poll_interval_seconds", float("nan")),
            ("poll_interval_seconds", float("inf")),
            ("socket_gid", 2147483648),
            ("allowed_uids", [0, 2147483648]),
            (
                "compose_file",
                "/home/ubuntu/sub2api/../other/docker-compose.yml",
            ),
            ("health_url", "http://127.0.0.1:not-a-port/health"),
        ]
        for field, invalid_value in cases:
            with self.subTest(field=field, invalid_value=invalid_value):
                value = {**self.valid_config, field: invalid_value}
                self.write_config(value)
                with self.assertRaises(ValueError):
                    sub2api_updater.load_config(self.config_path)

    def test_load_config_rejects_unsafe_values(self):
        cases = [
            ("socket_path", "relative/updater.sock"),
            ("compose_directory", "home/ubuntu/sub2api"),
            ("compose_file", "docker-compose.yml"),
            ("environment_file", ".env"),
            ("state_file", "state.json"),
            ("socket_gid", -1),
            ("allowed_uids", []),
            ("allowed_uids", [0, -1]),
            ("allowed_uids", [1000, 1000]),
            ("service_name", "--project-directory"),
            ("container_name", "sub2api;id"),
            ("expected_architecture", "../amd64"),
            ("prepare_timeout_seconds", 0),
            ("activation_timeout_seconds", 0),
            ("poll_interval_seconds", 0),
            ("image_repository", "bad repository"),
            ("image_source", "file:///root/source"),
            ("health_url", "unix:///var/run/docker.sock"),
        ]
        for field, invalid_value in cases:
            with self.subTest(field=field, invalid_value=invalid_value):
                value = {**self.valid_config, field: invalid_value}
                self.write_config(value)
                with self.assertRaises(ValueError):
                    sub2api_updater.load_config(self.config_path)

    def test_existing_non_socket_path_is_rejected(self):
        path = self.directory / "updater.sock"
        path.write_text("must not be deleted", encoding="utf-8")

        with self.assertRaises(RuntimeError):
            sub2api_updater.remove_stale_socket(path)

        self.assertEqual(
            "must not be deleted",
            path.read_text(encoding="utf-8"),
        )

    def test_symlink_socket_path_is_rejected_without_unlinking_target(self):
        path = self.directory / "updater.sock"
        target = self.directory / "target"
        target.write_text("target", encoding="utf-8")
        try:
            path.symlink_to(target)
        except (OSError, NotImplementedError) as error:
            self.skipTest(f"symlink creation unavailable: {error}")

        with self.assertRaises(RuntimeError):
            sub2api_updater.remove_stale_socket(path)

        self.assertTrue(path.is_symlink())
        self.assertEqual("target", target.read_text(encoding="utf-8"))


class StartupTest(unittest.TestCase):
    def test_startup_reconciles_before_server_accepts_requests(self):
        calls = []
        config = mock.sentinel.config
        core = mock.Mock()
        core.reconcile_state.side_effect = lambda: calls.append("reconcile")
        server = mock.Mock()
        server.serve_forever.side_effect = lambda: calls.append("serve")

        with (
            mock.patch.object(
                sub2api_updater,
                "load_config",
                return_value=config,
            ) as load_config,
            mock.patch.object(
                sub2api_updater,
                "UpdaterCore",
                return_value=core,
            ) as core_type,
            mock.patch.object(
                sub2api_updater,
                "create_server",
                return_value=server,
            ),
        ):
            sub2api_updater.run_service("/etc/test-config.json")

        load_config.assert_called_once_with("/etc/test-config.json")
        core_type.assert_called_once_with(config)
        self.assertEqual(["reconcile", "serve"], calls)

    def test_missing_linux_socket_capabilities_fail_explicitly(self):
        with (
            mock.patch.object(
                sub2api_updater.socket,
                "AF_UNIX",
                None,
                create=True,
            ),
            mock.patch.object(
                sub2api_updater.socket,
                "SO_PEERCRED",
                None,
                create=True,
            ),
            mock.patch.object(
                sub2api_updater.socketserver,
                "UnixStreamServer",
                None,
                create=True,
            ),
        ):
            with self.assertRaisesRegex(
                RuntimeError,
                "AF_UNIX.*SO_PEERCRED.*UnixStreamServer",
            ) as raised:
                sub2api_updater.require_linux_unix_support()

        self.assertIn("AF_UNIX", str(raised.exception))
        self.assertIn("SO_PEERCRED", str(raised.exception))
        self.assertIn("UnixStreamServer", str(raised.exception))

    def test_main_logs_only_fixed_startup_failure_text(self):
        logger = mock.Mock()
        with (
            mock.patch.object(
                sub2api_updater,
                "run_service",
                side_effect=OSError("startup secret"),
            ),
            mock.patch.object(sub2api_updater, "_LOGGER", logger),
        ):
            exit_code = sub2api_updater.main([])

        self.assertEqual(1, exit_code)
        logger.error.assert_called_once_with("updater service failed")
        logger.exception.assert_not_called()


class PackagingArtifactsTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.updater_directory = Path(__file__).resolve().parents[1]

    def test_config_example_has_exact_graduation_values(self):
        expected = {
            "socket_path": "/run/sub2api-updater/updater.sock",
            "socket_gid": 1000,
            "allowed_uids": [0, 1000],
            "image_repository": IMAGE_REPOSITORY,
            "image_source": IMAGE_SOURCE,
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
            "state_file": "/var/lib/sub2api-updater/state.json",
        }

        actual = json.loads(
            (self.updater_directory / "config.example.json").read_text(
                encoding="utf-8"
            )
        )

        self.assertEqual(expected, actual)

    def test_systemd_unit_has_required_runtime_and_hardening(self):
        unit = (
            self.updater_directory / "sub2api-updater.service"
        ).read_text(encoding="utf-8")
        required = [
            "After=docker.service network-online.target",
            "Wants=docker.service network-online.target",
            "ExecStart=/usr/bin/python3 /opt/sub2api-updater/sub2api_updater.py --config /etc/sub2api-updater/config.json",
            "User=root",
            "Group=root",
            "Restart=on-failure",
            "RuntimeDirectory=sub2api-updater",
            "StateDirectory=sub2api-updater",
            "UMask=0007",
            "NoNewPrivileges=true",
            "PrivateTmp=true",
            "ProtectKernelTunables=true",
            "ProtectKernelModules=true",
            "ProtectKernelLogs=true",
            "ProtectControlGroups=true",
            "LockPersonality=true",
            "RestrictRealtime=true",
            "RestrictNamespaces=true",
            "StandardOutput=journal",
            "StandardError=journal",
        ]
        for directive in required:
            with self.subTest(directive=directive):
                self.assertIn(directive, unit)
        self.assertNotIn("ProtectHome=true", unit)
        self.assertNotIn("ProtectSystem=strict", unit)

    def test_installer_has_required_flags_permissions_and_health_check(self):
        installer = (self.updater_directory / "install.sh").read_text(
            encoding="utf-8"
        )
        required = [
            "set -euo pipefail",
            "--compose-directory",
            "--compose-file",
            "--environment-file",
            "--service-name",
            "--container-name",
            "--app-uid",
            "--socket-gid",
            "--expected-architecture",
            "--health-url",
            "--image-repository",
            "--image-source",
            "/opt/sub2api-updater",
            "/etc/sub2api-updater",
            "/usr/bin/python3",
            "json.dump",
            "chmod 0600",
            "chmod 0644",
            "systemctl daemon-reload",
            "systemctl enable sub2api-updater.service",
            "systemctl restart sub2api-updater.service",
            "/v1/health",
            "protocol_version",
            "journalctl",
        ]
        for text in required:
            with self.subTest(text=text):
                self.assertIn(text, installer)
        self.assertNotIn("docker compose up", installer)
        self.assertNotIn("SUB2API_IMAGE=", installer)
        self.assertNotIn(
            "systemctl enable --now sub2api-updater.service",
            installer,
        )
        service_steps = [
            "systemctl daemon-reload",
            "systemctl enable sub2api-updater.service",
            "systemctl restart sub2api-updater.service",
            'b"GET /v1/health HTTP/1.1',
        ]
        if all(step in installer for step in service_steps):
            self.assertEqual(
                sorted(installer.index(step) for step in service_steps),
                [installer.index(step) for step in service_steps],
            )

    def test_installer_health_probe_drops_to_application_identity(self):
        installer = (self.updater_directory / "install.sh").read_text(
            encoding="utf-8"
        )

        required = [
            "APP_UID=",
            "SOCKET_GID=",
            'os.environ["APP_UID"]',
            'os.environ["SOCKET_GID"]',
            "os.setgroups([])",
            "os.setgid(socket_gid)",
            "os.setuid(app_uid)",
            'app_uid = integer(app_uid_text, "app UID", 0)',
            "allowed_uids = [0] if app_uid == 0 else [0, app_uid]",
        ]
        for text in required:
            with self.subTest(text=text):
                self.assertIn(text, installer)
        privilege_steps = [
            "os.setgroups([])",
            "os.setgid(socket_gid)",
            "os.setuid(app_uid)",
            "socket.socket(socket.AF_UNIX",
        ]
        if all(step in installer for step in privilege_steps):
            self.assertEqual(
                sorted(installer.index(step) for step in privilege_steps),
                [installer.index(step) for step in privilege_steps],
            )

    def test_installer_health_check_has_strict_total_deadline(self):
        installer = (self.updater_directory / "install.sh").read_text(
            encoding="utf-8"
        )

        self.assertGreaterEqual(
            installer.count(
                "remaining = deadline - time.monotonic()"
            ),
            4,
        )
        self.assertGreaterEqual(
            installer.count(
                "client.settimeout(min(2, remaining))"
            ),
            3,
        )
        self.assertIn("if remaining <= 0:", installer)
        self.assertIn("sleep_seconds = min(1, remaining)", installer)
        self.assertIn("time.sleep(sleep_seconds)", installer)

    def test_readme_documents_protocol_install_and_recovery(self):
        readme = (self.updater_directory / "README.md").read_text(
            encoding="utf-8"
        )
        required = [
            "GET /v1/health",
            "POST /v1/prepare",
            "POST /v1/activate",
            "GET /v1/status",
            "current_image",
            "target_image",
            "previous_image",
            "updated_at",
            "curl --unix-socket",
            "journalctl -u sub2api-updater",
            "--compose-directory /home/ubuntu/sub2api",
            "--image-repository ghcr.io/gwenliu1025/sub2api",
            "up -d --no-deps --force-recreate sub2api",
            "Postgres",
            "Redis",
            "Caddy",
            "QQ",
            "NapCat",
        ]
        for text in required:
            with self.subTest(text=text):
                self.assertIn(text, readme)


if __name__ == "__main__":
    unittest.main()
