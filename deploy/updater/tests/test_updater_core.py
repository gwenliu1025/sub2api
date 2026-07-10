import dataclasses
import json
import os
import stat
import subprocess
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

from deploy.updater.updater_core import (
    UpdaterConfig,
    UpdaterCore,
    UpdaterError,
    UpdateState,
    run_command,
    validate_version,
)


IMAGE_REPOSITORY = "ghcr.io/gwenliu1025/sub2api"
IMAGE_SOURCE = "https://github.com/gwenliu1025/sub2api"
CURRENT_IMAGE = "xqian/sub2api:equivalent-cache-20260709-083433"
ENVIRONMENT_CONTENTS = (
    b"POSTGRES_PASSWORD=do-not-leak\n"
    b"JWT_SECRET=also-do-not-leak\n"
    b"SUB2API_IMAGE=xqian/sub2api:equivalent-cache-20260709-083433\n"
)


def completed(stdout="", stderr="", returncode=0):
    return {
        "stdout": stdout,
        "stderr": stderr,
        "returncode": returncode,
    }


def image_inspect(
    version,
    architecture="amd64",
    source=IMAGE_SOURCE,
    label_version=None,
):
    if label_version is None:
        label_version = version
    return completed(
        stdout=json.dumps(
            [
                {
                    "Id": "sha256:target",
                    "Architecture": architecture,
                    "Config": {
                        "Labels": {
                            "org.opencontainers.image.source": source,
                            "org.opencontainers.image.version": label_version,
                        }
                    },
                }
            ]
        )
    )


def container_inspect(image=CURRENT_IMAGE):
    return completed(stdout=json.dumps([{"Config": {"Image": image}}]))


def successful_prepare_responses(version):
    return [
        completed(),
        image_inspect(version),
        container_inspect(),
    ]


class FakeRunner:
    def __init__(self, responses):
        self.responses = list(responses)
        self.calls = []

    def __call__(self, args, timeout):
        copied_args = list(args)
        self.calls.append((copied_args, timeout))
        if not self.responses:
            raise AssertionError(f"unexpected command: {copied_args!r}")
        response = self.responses.pop(0)
        if isinstance(response, BaseException):
            raise response
        return subprocess.CompletedProcess(
            copied_args,
            response["returncode"],
            stdout=response["stdout"],
            stderr=response["stderr"],
        )


class BlockingPrepareRunner:
    def __init__(self):
        self.started = threading.Event()
        self.release = threading.Event()
        self.calls = []
        self._calls_lock = threading.Lock()

    def __call__(self, args, timeout):
        copied_args = list(args)
        with self._calls_lock:
            self.calls.append((copied_args, timeout))

        target = f"{IMAGE_REPOSITORY}:0.1.150"
        if copied_args == ["docker", "pull", target]:
            self.started.set()
            if not self.release.wait(5):
                raise AssertionError("timed out waiting to release first prepare")
            return subprocess.CompletedProcess(copied_args, 0, stdout="", stderr="")
        if copied_args == ["docker", "image", "inspect", target]:
            response = image_inspect("0.1.150")
        elif copied_args == ["docker", "inspect", "sub2api"]:
            response = container_inspect()
        else:
            raise AssertionError(f"unexpected concurrent command: {copied_args!r}")
        return subprocess.CompletedProcess(
            copied_args,
            response["returncode"],
            stdout=response["stdout"],
            stderr=response["stderr"],
        )


class UpdaterCoreTest(unittest.TestCase):
    def setUp(self):
        self.temporary_directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary_directory.cleanup)
        self.directory = Path(self.temporary_directory.name)
        self.environment_file = self.directory / ".env"
        self.environment_file.write_bytes(ENVIRONMENT_CONTENTS)
        self.config = UpdaterConfig(
            socket_path="/run/sub2api-updater/updater.sock",
            socket_gid=1000,
            allowed_uids=(0, 1000),
            image_repository=IMAGE_REPOSITORY,
            image_source=IMAGE_SOURCE,
            compose_directory="/home/ubuntu/sub2api",
            compose_file="/home/ubuntu/sub2api/docker-compose.yml",
            environment_file=str(self.environment_file),
            service_name="sub2api",
            container_name="sub2api",
            expected_architecture="amd64",
            health_url="http://127.0.0.1:8080/health",
            prepare_timeout_seconds=600,
            activation_timeout_seconds=120,
            poll_interval_seconds=2.0,
            state_file=str(self.directory / "state.json"),
        )

    def new_core(self, responses, **config_changes):
        runner = FakeRunner(responses)
        config = dataclasses.replace(self.config, **config_changes)
        return UpdaterCore(config, runner=runner), runner

    def test_version_accepts_numeric_dot_components(self):
        for version in ["0.1.150", "1.2", "2026.07.10"]:
            with self.subTest(version=version):
                self.assertEqual(version, validate_version(version))

    def test_version_rejects_prefix_suffix_digest_and_shell_text(self):
        rejected_versions = [
            "v0.1.150",
            "0.1.150-rc1",
            "0.1.150@sha256:abc",
            "../0.1.150",
            "0.1.150;id",
            "",
        ]
        for version in rejected_versions:
            with self.subTest(version=version):
                with self.assertRaises(UpdaterError) as raised:
                    validate_version(version)
                self.assertEqual("INVALID_VERSION", raised.exception.code)

    def test_target_image_uses_only_configured_repository(self):
        core, runner = self.new_core([])

        self.assertEqual(
            "ghcr.io/gwenliu1025/sub2api:0.1.150",
            core.target_image("0.1.150"),
        )
        with self.assertRaises(UpdaterError) as raised:
            core.target_image("attacker.example/image:0.1.150")
        self.assertEqual("INVALID_VERSION", raised.exception.code)
        self.assertEqual([], runner.calls)

    def test_prepare_pulls_exact_image_and_validates_labels_and_architecture(self):
        core, runner = self.new_core(successful_prepare_responses("0.1.150"))

        state = core.prepare("0.1.150")

        self.assertEqual(
            [
                (
                    [
                        "docker",
                        "pull",
                        "ghcr.io/gwenliu1025/sub2api:0.1.150",
                    ],
                    600,
                ),
                (
                    [
                        "docker",
                        "image",
                        "inspect",
                        "ghcr.io/gwenliu1025/sub2api:0.1.150",
                    ],
                    600,
                ),
                (["docker", "inspect", "sub2api"], 30),
            ],
            runner.calls,
        )
        self.assertEqual("prepared", state.state)
        self.assertEqual(CURRENT_IMAGE, state.current_image)
        self.assertEqual(CURRENT_IMAGE, state.previous_image)
        self.assertEqual(
            "ghcr.io/gwenliu1025/sub2api:0.1.150",
            state.target_image,
        )
        self.assertTrue(state.updated_at)

    def test_prepare_failure_returns_sanitized_error(self):
        cases = [
            (
                "pull",
                [
                    completed(
                        stdout="POSTGRES_PASSWORD=pull-output-secret",
                        stderr="JWT_SECRET=pull-error-secret",
                        returncode=1,
                    )
                ],
                "IMAGE_PULL_FAILED",
            ),
            (
                "source-verification",
                [
                    completed(),
                    image_inspect(
                        "0.1.150",
                        source="https://attacker.example/repository",
                    ),
                ],
                "IMAGE_VERIFICATION_FAILED",
            ),
            (
                "architecture-verification",
                [
                    completed(),
                    image_inspect("0.1.150", architecture="arm64"),
                ],
                "IMAGE_VERIFICATION_FAILED",
            ),
            (
                "version-verification",
                [
                    completed(),
                    image_inspect("0.1.150", label_version="0.1.151"),
                ],
                "IMAGE_VERIFICATION_FAILED",
            ),
        ]
        forbidden = [
            "pull-output-secret",
            "pull-error-secret",
            "attacker.example",
            "POSTGRES_PASSWORD",
            "JWT_SECRET",
        ]

        for name, responses, expected_code in cases:
            with self.subTest(case=name):
                state_file = self.directory / f"state-{name}.json"
                core, _ = self.new_core(responses, state_file=str(state_file))

                with self.assertRaises(UpdaterError) as raised:
                    core.prepare("0.1.150")

                self.assertEqual(expected_code, raised.exception.code)
                persisted = core.load_state()
                self.assertEqual("failed", persisted.state)
                self.assertEqual(raised.exception.message, persisted.message)
                for secret in forbidden:
                    self.assertNotIn(secret, raised.exception.message)
                    self.assertNotIn(secret, persisted.message)

    def test_prepare_does_not_change_environment_file(self):
        original = self.environment_file.read_bytes()
        core, _ = self.new_core(successful_prepare_responses("0.1.150"))

        core.prepare("0.1.150")

        self.assertEqual(original, self.environment_file.read_bytes())

    def test_prepare_same_target_is_idempotent(self):
        core, runner = self.new_core(successful_prepare_responses("0.1.150"))
        first = core.prepare("0.1.150")
        calls_after_first_prepare = list(runner.calls)

        second = core.prepare("0.1.150")

        self.assertEqual(first, second)
        self.assertEqual(calls_after_first_prepare, runner.calls)

    def test_prepare_different_target_replaces_pending_target(self):
        responses = successful_prepare_responses(
            "0.1.150"
        ) + successful_prepare_responses("0.1.151")
        core, runner = self.new_core(responses)
        core.prepare("0.1.150")

        state = core.prepare("0.1.151")

        self.assertEqual(6, len(runner.calls))
        self.assertEqual(
            (
                [
                    "docker",
                    "pull",
                    "ghcr.io/gwenliu1025/sub2api:0.1.151",
                ],
                600,
            ),
            runner.calls[3],
        )
        self.assertEqual("prepared", state.state)
        self.assertEqual(
            "ghcr.io/gwenliu1025/sub2api:0.1.151",
            state.target_image,
        )
        self.assertEqual(CURRENT_IMAGE, state.previous_image)

    def test_busy_operation_is_rejected(self):
        core, runner = self.new_core([])
        for busy_state in ["preparing", "activating"]:
            with self.subTest(state=busy_state):
                core.save_state(UpdateState(state=busy_state))

                with self.assertRaises(UpdaterError) as raised:
                    core.prepare("0.1.150")

                self.assertEqual("AGENT_BUSY", raised.exception.code)
        self.assertEqual([], runner.calls)

    def test_concurrent_prepare_is_rejected_by_operation_lock(self):
        runner = BlockingPrepareRunner()
        core = UpdaterCore(self.config, runner=runner)
        first_result = {}
        second_result = {}
        second_done = threading.Event()

        def run_first_prepare():
            try:
                first_result["state"] = core.prepare("0.1.150")
            except BaseException as error:
                first_result["error"] = error

        def run_second_prepare():
            try:
                core.prepare("0.1.151")
            except BaseException as error:
                second_result["error"] = error
            finally:
                second_done.set()

        first_thread = threading.Thread(target=run_first_prepare)
        second_thread = threading.Thread(target=run_second_prepare)
        first_thread.start()
        try:
            self.assertTrue(runner.started.wait(2))
            Path(self.config.state_file).unlink()

            second_thread.start()
            self.assertTrue(second_done.wait(1))
            error = second_result.get("error")
            self.assertIsInstance(error, UpdaterError)
            self.assertEqual("AGENT_BUSY", error.code)
            self.assertEqual(
                [
                    (
                        [
                            "docker",
                            "pull",
                            "ghcr.io/gwenliu1025/sub2api:0.1.150",
                        ],
                        600,
                    )
                ],
                runner.calls,
            )
            if Path(self.config.state_file).exists():
                self.assertNotEqual(
                    "ghcr.io/gwenliu1025/sub2api:0.1.151",
                    core.load_state().target_image,
                )
        finally:
            runner.release.set()
            first_thread.join(2)
            second_thread.join(2)

        self.assertFalse(first_thread.is_alive())
        self.assertFalse(second_thread.is_alive())
        self.assertNotIn("error", first_result)
        self.assertEqual("prepared", first_result["state"].state)

    def test_state_file_contains_no_environment_contents(self):
        core, _ = self.new_core(successful_prepare_responses("0.1.150"))

        core.prepare("0.1.150")

        state_path = Path(self.config.state_file)
        state_bytes = state_path.read_bytes()
        state_text = state_bytes.decode("utf-8")
        self.assertNotIn("POSTGRES_PASSWORD", state_text)
        self.assertNotIn("JWT_SECRET", state_text)
        self.assertNotIn("do-not-leak", state_text)
        self.assertNotIn("also-do-not-leak", state_text)
        self.assertNotIn("sha256:target", state_text)
        self.assertEqual(
            {
                "state",
                "current_image",
                "target_image",
                "previous_image",
                "message",
                "updated_at",
            },
            set(json.loads(state_text)),
        )
        if os.name != "nt":
            self.assertEqual(0o600, stat.S_IMODE(os.stat(state_path).st_mode))

    def test_missing_state_returns_idle(self):
        core, runner = self.new_core([])

        self.assertEqual(UpdateState(), core.load_state())
        self.assertEqual([], runner.calls)

    def test_save_state_failure_returns_sanitized_error(self):
        core, _ = self.new_core([])
        raw_error = (
            f"replace failed for {self.config.state_file}: "
            "JWT_SECRET=write-error-secret"
        )

        with mock.patch(
            "deploy.updater.updater_core.os.replace",
            side_effect=OSError(raw_error),
        ):
            with self.assertRaises(UpdaterError) as raised:
                core.save_state(UpdateState(state="preparing"))

        self.assertEqual("STATE_WRITE_FAILED", raised.exception.code)
        self.assertEqual(
            "failed to persist update state",
            raised.exception.message,
        )
        self.assertNotIn(str(self.directory), raised.exception.message)
        self.assertNotIn("JWT_SECRET", raised.exception.message)
        self.assertNotIn("write-error-secret", raised.exception.message)

    def test_state_temp_file_creation_failure_is_sanitized(self):
        core, _ = self.new_core([])
        raw_error = (
            f"cannot create temp file in {self.directory}: "
            "JWT_SECRET=temp-file-secret"
        )

        with mock.patch(
            "deploy.updater.updater_core.tempfile.mkstemp",
            side_effect=OSError(raw_error),
        ):
            with self.assertRaises(UpdaterError) as raised:
                core.save_state(UpdateState(state="preparing"))

        self.assertEqual("STATE_WRITE_FAILED", raised.exception.code)
        self.assertEqual(
            "failed to persist update state",
            raised.exception.message,
        )
        self.assertNotIn(str(self.directory), raised.exception.message)
        self.assertNotIn("JWT_SECRET", raised.exception.message)
        self.assertNotIn("temp-file-secret", raised.exception.message)

    def test_prepare_state_write_failure_returns_sanitized_error(self):
        core, runner = self.new_core([])
        raw_error = (
            f"cannot replace {self.config.state_file}: "
            "POSTGRES_PASSWORD=write-error-secret"
        )

        with mock.patch(
            "deploy.updater.updater_core.os.replace",
            side_effect=OSError(raw_error),
        ):
            with self.assertRaises(UpdaterError) as raised:
                core.prepare("0.1.150")

        self.assertEqual("STATE_WRITE_FAILED", raised.exception.code)
        self.assertEqual(
            "failed to persist update state",
            raised.exception.message,
        )
        self.assertNotIn(str(self.directory), raised.exception.message)
        self.assertNotIn("POSTGRES_PASSWORD", raised.exception.message)
        self.assertNotIn("write-error-secret", raised.exception.message)
        self.assertEqual([], runner.calls)

    def test_failed_state_write_does_not_replace_original_business_error(self):
        core, _ = self.new_core(
            [
                completed(
                    stdout="POSTGRES_PASSWORD=pull-output-secret",
                    stderr="JWT_SECRET=pull-error-secret",
                    returncode=1,
                )
            ]
        )
        raw_error = (
            f"directory fsync failed for {self.directory}: "
            "JWT_SECRET=state-write-secret"
        )

        with mock.patch.object(
            core,
            "_fsync_parent_directory",
            side_effect=[None, OSError(raw_error)],
        ):
            with self.assertRaises(UpdaterError) as raised:
                core.prepare("0.1.150")

        self.assertEqual("IMAGE_PULL_FAILED", raised.exception.code)
        self.assertEqual("target image pull failed", raised.exception.message)
        state_text = Path(self.config.state_file).read_text(encoding="utf-8")
        self.assertIn('"state":"failed"', state_text)
        self.assertNotIn("pull-output-secret", state_text)
        self.assertNotIn("pull-error-secret", state_text)
        self.assertNotIn("state-write-secret", state_text)
        self.assertNotIn("POSTGRES_PASSWORD", state_text)
        self.assertNotIn("JWT_SECRET", state_text)

    def test_corrupt_state_is_rejected_without_running_commands(self):
        state_path = Path(self.config.state_file)
        state_path.write_text(
            '{"state":"prepared","unexpected":"JWT_SECRET=do-not-leak"}',
            encoding="utf-8",
        )
        core, runner = self.new_core([])

        with self.assertRaises(UpdaterError) as raised:
            core.prepare("0.1.150")

        self.assertEqual("STATE_INVALID", raised.exception.code)
        self.assertNotIn("JWT_SECRET", raised.exception.message)
        self.assertNotIn("do-not-leak", raised.exception.message)
        self.assertEqual([], runner.calls)

    def test_production_runner_uses_subprocess_without_shell(self):
        expected = subprocess.CompletedProcess(["docker", "pull", "image"], 0)
        with mock.patch(
            "deploy.updater.updater_core.subprocess.run",
            return_value=expected,
        ) as subprocess_run:
            result = run_command(["docker", "pull", "image"], 17)

        self.assertIs(expected, result)
        subprocess_run.assert_called_once_with(
            ["docker", "pull", "image"],
            check=False,
            capture_output=True,
            text=True,
            timeout=17,
            shell=False,
        )

    def test_runner_programming_errors_are_not_mapped(self):
        errors = [
            AssertionError("fake runner assertion"),
            ValueError("fake runner value error"),
        ]
        for index, expected_error in enumerate(errors):
            with self.subTest(error=type(expected_error).__name__):
                core, _ = self.new_core(
                    [expected_error],
                    state_file=str(self.directory / f"state-runner-{index}.json"),
                )

                with self.assertRaises(type(expected_error)) as raised:
                    core.prepare("0.1.150")

                self.assertIs(expected_error, raised.exception)

    def test_runner_timeout_is_mapped_without_command_output(self):
        timeout_error = subprocess.TimeoutExpired(
            ["docker", "pull", "secret-bearing-image"],
            600,
            output="POSTGRES_PASSWORD=timeout-output-secret",
            stderr="JWT_SECRET=timeout-error-secret",
        )
        core, _ = self.new_core([timeout_error])

        with self.assertRaises(UpdaterError) as raised:
            core.prepare("0.1.150")

        self.assertEqual("IMAGE_PULL_FAILED", raised.exception.code)
        self.assertEqual("target image pull failed", raised.exception.message)
        state_text = Path(self.config.state_file).read_text(encoding="utf-8")
        self.assertNotIn("secret-bearing-image", state_text)
        self.assertNotIn("timeout-output-secret", state_text)
        self.assertNotIn("timeout-error-secret", state_text)
        self.assertNotIn("POSTGRES_PASSWORD", state_text)
        self.assertNotIn("JWT_SECRET", state_text)


if __name__ == "__main__":
    unittest.main()
