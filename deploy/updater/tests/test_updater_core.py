import dataclasses
import http.client
import json
import os
import stat
import subprocess
import tempfile
import threading
import unittest
from pathlib import Path
from unittest import mock

import deploy.updater.updater_core as updater_core
from deploy.updater.updater_core import (
    UpdaterConfig,
    UpdaterCore,
    UpdaterError,
    UpdateState,
    _http_health_ok,
    replace_sub2api_image,
    restore_environment,
    run_command,
    validate_version,
)


IMAGE_REPOSITORY = "ghcr.io/gwenliu1025/sub2api"
IMAGE_SOURCE = "https://github.com/gwenliu1025/sub2api"
CURRENT_IMAGE = "xqian/sub2api:equivalent-cache-20260709-083433"
TARGET_IMAGE = f"{IMAGE_REPOSITORY}:0.1.150"
TARGET_IMAGE_ID = "sha256:target"
PREVIOUS_IMAGE_ID = "sha256:previous"
original_env = (
    b"POSTGRES_PASSWORD=do-not-leak\n"
    b"JWT_SECRET=also-do-not-leak\n"
    b"SUB2API_IMAGE=xqian/sub2api:equivalent-cache-20260709-083433\n"
)
COMPOSE_COMMAND = [
    "docker",
    "compose",
    "--project-directory",
    "/home/ubuntu/sub2api",
    "-f",
    "/home/ubuntu/sub2api/docker-compose.yml",
    "--env-file",
    "/home/ubuntu/sub2api/.env",
    "up",
    "-d",
    "--no-deps",
    "--force-recreate",
    "sub2api",
]


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


def image_id_inspect(image_id):
    return completed(stdout=json.dumps([{"Id": image_id}]))


def container_inspect(image=CURRENT_IMAGE):
    return completed(stdout=json.dumps([{"Config": {"Image": image}}]))


def running_container_inspect(image_id, health_status):
    return completed(
        stdout=json.dumps(
            [
                {
                    "Image": image_id,
                    "State": {"Health": {"Status": health_status}},
                    "Config": {"Image": "must-not-be-used-for-id-comparison"},
                }
            ]
        )
    )


def successful_prepare_responses(version):
    return [
        completed(),
        image_inspect(version),
        container_inspect(),
    ]


def successful_activation_preflight_responses():
    return [
        image_inspect("0.1.150"),
        running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
        image_id_inspect(PREVIOUS_IMAGE_ID),
    ]


def activation_preflight_calls(timeout):
    return [
        (["docker", "image", "inspect", TARGET_IMAGE], timeout),
        (["docker", "inspect", "sub2api"], timeout),
        (["docker", "image", "inspect", CURRENT_IMAGE], timeout),
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


class AdvancingFakeRunner(FakeRunner):
    def __init__(self, responses, clock, advances):
        super().__init__(responses)
        self.clock = clock
        self.advances = list(advances)
        self.call_times = []

    def __call__(self, args, timeout):
        self.call_times.append(self.clock.now)
        result = super().__call__(args, timeout)
        call_index = len(self.calls) - 1
        self.clock.now += self.advances[call_index]
        return result


class FakeClock:
    def __init__(self):
        self.now = 0.0
        self.sleeps = []

    def monotonic(self):
        return self.now

    def sleep(self, seconds):
        self.sleeps.append(seconds)
        self.now += seconds


class RecordingHealthChecker:
    def __init__(self, responses):
        self.responses = list(responses)
        self.calls = []

    def __call__(self, url, timeout):
        self.calls.append((url, timeout))
        if not self.responses:
            raise AssertionError("unexpected HTTP health check")
        response = self.responses.pop(0)
        if isinstance(response, BaseException):
            raise response
        return response


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
        self.environment_file.write_bytes(original_env)
        self.config = UpdaterConfig(
            socket_path="/run/sub2api-updater/updater.sock",
            socket_gid=1000,
            allowed_uids=(0, 1000),
            image_repository=IMAGE_REPOSITORY,
            image_source=IMAGE_SOURCE,
            compose_directory="/home/ubuntu/sub2api",
            compose_file="/home/ubuntu/sub2api/docker-compose.yml",
            environment_file="/home/ubuntu/sub2api/.env",
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

    def new_activation_core(
        self,
        responses,
        health_responses=(True,),
        **config_changes,
    ):
        runner = FakeRunner(responses)
        clock = FakeClock()
        health_checker = RecordingHealthChecker(health_responses)
        config = dataclasses.replace(self.config, **config_changes)
        core = UpdaterCore(
            config,
            runner=runner,
            monotonic=clock.monotonic,
            sleep=clock.sleep,
            health_checker=health_checker,
            environment_path=self.environment_file,
        )
        return core, runner, clock, health_checker

    def save_prepared_state(self, core):
        core.save_state(
            UpdateState(
                state="prepared",
                current_image=CURRENT_IMAGE,
                target_image=TARGET_IMAGE,
                previous_image=CURRENT_IMAGE,
                message="target image prepared",
                updated_at="2026-07-10T00:00:00Z",
            )
        )

    def save_activating_state(self, core):
        core.save_state(
            UpdateState(
                state="activating",
                current_image=CURRENT_IMAGE,
                target_image=TARGET_IMAGE,
                previous_image=CURRENT_IMAGE,
                message="activation scheduled",
                updated_at="2026-07-10T00:00:00Z",
                target_image_id=TARGET_IMAGE_ID,
                previous_image_id=PREVIOUS_IMAGE_ID,
            )
        )

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

    def test_atomic_environment_update_replaces_only_sub2api_image(self):
        original = (
            b"# SUB2API_IMAGE=commented\n"
            b"X_SUB2API_IMAGE=leave-this-alone\n"
            b"SUB2API_IMAGE=old-image\n"
            b"SUB2API_IMAGE_SUFFIX=also-unchanged\n"
        )
        self.environment_file.write_bytes(original)

        backup = replace_sub2api_image(
            self.environment_file,
            TARGET_IMAGE,
        )

        self.assertEqual(original, backup.contents)
        self.assertEqual(
            (
                b"# SUB2API_IMAGE=commented\n"
                b"X_SUB2API_IMAGE=leave-this-alone\n"
                b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150\n"
                b"SUB2API_IMAGE_SUFFIX=also-unchanged\n"
            ),
            self.environment_file.read_bytes(),
        )

    def test_atomic_environment_update_appends_missing_image_key(self):
        cases = [
            (
                "lf-without-trailing-newline",
                b"FIRST=1\nSECOND=2",
                b"FIRST=1\nSECOND=2\nSUB2API_IMAGE="
                b"ghcr.io/gwenliu1025/sub2api:0.1.150\n",
            ),
            (
                "crlf-without-trailing-newline",
                b"FIRST=1\r\nSECOND=2",
                b"FIRST=1\r\nSECOND=2\r\nSUB2API_IMAGE="
                b"ghcr.io/gwenliu1025/sub2api:0.1.150\r\n",
            ),
            (
                "empty-file",
                b"",
                b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150\n",
            ),
        ]
        for name, original, expected in cases:
            with self.subTest(case=name):
                environment_file = self.directory / f"{name}.env"
                environment_file.write_bytes(original)

                backup = replace_sub2api_image(
                    environment_file,
                    TARGET_IMAGE,
                )

                self.assertEqual(original, backup.contents)
                self.assertEqual(expected, environment_file.read_bytes())

    def test_atomic_environment_update_preserves_cr_only_line_endings(self):
        original = (
            b"FIRST=1\r"
            b"SUB2API_IMAGE=old-image\r"
            b"LAST=3"
        )
        self.environment_file.write_bytes(original)

        backup = replace_sub2api_image(
            self.environment_file,
            TARGET_IMAGE,
        )

        self.assertEqual(original, backup.contents)
        self.assertEqual(
            (
                b"FIRST=1\r"
                b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150\r"
                b"LAST=3"
            ),
            self.environment_file.read_bytes(),
        )

    def test_atomic_environment_update_appends_with_cr_only_convention(self):
        cases = [
            (
                "without-trailing-newline",
                b"FIRST=1\rSECOND=2",
                b"FIRST=1\rSECOND=2\rSUB2API_IMAGE="
                b"ghcr.io/gwenliu1025/sub2api:0.1.150\r",
            ),
            (
                "with-trailing-newline",
                b"FIRST=1\rSECOND=2\r",
                b"FIRST=1\rSECOND=2\rSUB2API_IMAGE="
                b"ghcr.io/gwenliu1025/sub2api:0.1.150\r",
            ),
        ]
        for name, original, expected in cases:
            with self.subTest(case=name):
                environment_file = self.directory / f"cr-only-{name}.env"
                environment_file.write_bytes(original)

                backup = replace_sub2api_image(
                    environment_file,
                    TARGET_IMAGE,
                )

                self.assertEqual(original, backup.contents)
                self.assertEqual(expected, environment_file.read_bytes())

    def test_environment_update_preserves_mode_owner_and_unrelated_bytes(self):
        original = (
            b"POSTGRES_PASSWORD=do-not-leak\r\n"
            b"RAW_BYTES=\xff\xfe\r\n"
            b"SUB2API_IMAGE=old-image"
        )
        self.environment_file.write_bytes(original)
        os.chmod(self.environment_file, 0o640)
        original_stat = self.environment_file.stat()

        backup = replace_sub2api_image(
            self.environment_file,
            TARGET_IMAGE,
        )

        updated_stat = self.environment_file.stat()
        self.assertEqual(
            stat.S_IMODE(original_stat.st_mode),
            stat.S_IMODE(updated_stat.st_mode),
        )
        if os.name != "nt":
            self.assertEqual(original_stat.st_uid, updated_stat.st_uid)
            self.assertEqual(original_stat.st_gid, updated_stat.st_gid)
        self.assertEqual(
            (
                b"POSTGRES_PASSWORD=do-not-leak\r\n"
                b"RAW_BYTES=\xff\xfe\r\n"
                b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150"
            ),
            self.environment_file.read_bytes(),
        )

        restore_environment(self.environment_file, backup)

        restored_stat = self.environment_file.stat()
        self.assertEqual(original, self.environment_file.read_bytes())
        self.assertEqual(
            stat.S_IMODE(original_stat.st_mode),
            stat.S_IMODE(restored_stat.st_mode),
        )
        if os.name != "nt":
            self.assertEqual(original_stat.st_uid, restored_stat.st_uid)
            self.assertEqual(original_stat.st_gid, restored_stat.st_gid)

    def test_environment_metadata_sees_flushed_contents_before_file_fsync(self):
        original = (
            b"POSTGRES_PASSWORD=do-not-leak\n"
            b"SUB2API_IMAGE=old-image\n"
        )
        expected = (
            b"POSTGRES_PASSWORD=do-not-leak\n"
            b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150\n"
        )
        self.environment_file.write_bytes(original)
        observed_contents = []
        events = []
        real_apply_metadata = updater_core._apply_environment_metadata
        real_fsync = os.fsync

        def observe_metadata(file_descriptor, temporary_path, backup, **kwargs):
            position = os.lseek(file_descriptor, 0, os.SEEK_CUR)
            os.lseek(file_descriptor, 0, os.SEEK_SET)
            try:
                observed_contents.append(
                    os.read(file_descriptor, len(expected) + 1)
                )
            finally:
                os.lseek(file_descriptor, position, os.SEEK_SET)
            events.append(("metadata", file_descriptor))
            return real_apply_metadata(
                file_descriptor,
                temporary_path,
                backup,
                **kwargs,
            )

        def record_fsync(file_descriptor):
            events.append(("fsync", file_descriptor))
            return real_fsync(file_descriptor)

        with (
            mock.patch.object(
                updater_core,
                "_apply_environment_metadata",
                side_effect=observe_metadata,
            ),
            mock.patch.object(
                updater_core.os,
                "fsync",
                side_effect=record_fsync,
            ),
        ):
            replace_sub2api_image(self.environment_file, TARGET_IMAGE)

        self.assertEqual([expected], observed_contents)
        self.assertGreaterEqual(len(events), 2)
        self.assertEqual("metadata", events[0][0])
        self.assertEqual(("fsync", events[0][1]), events[1])

    @unittest.skipIf(os.name == "nt", "requires POSIX mode semantics")
    def test_environment_update_and_restore_preserve_setgid_mode(self):
        self.environment_file.write_bytes(original_env)
        os.chmod(self.environment_file, 0o2640)
        self.assertEqual(
            0o2640,
            stat.S_IMODE(self.environment_file.stat().st_mode),
        )

        backup = replace_sub2api_image(
            self.environment_file,
            TARGET_IMAGE,
        )

        self.assertEqual(
            0o2640,
            stat.S_IMODE(self.environment_file.stat().st_mode),
        )

        restore_environment(self.environment_file, backup)

        self.assertEqual(original_env, self.environment_file.read_bytes())
        self.assertEqual(
            0o2640,
            stat.S_IMODE(self.environment_file.stat().st_mode),
        )

    def test_environment_update_rejects_symlink_without_replacing_target(self):
        target_file = self.directory / "environment-target"
        target_file.write_bytes(original_env)
        environment_link = self.directory / "environment-link"
        try:
            environment_link.symlink_to(target_file)
        except OSError as error:
            self.skipTest(f"symlink creation unavailable: {error}")

        with self.assertRaises(UpdaterError) as raised:
            replace_sub2api_image(environment_link, TARGET_IMAGE)

        self.assertEqual("ENVIRONMENT_WRITE_FAILED", raised.exception.code)
        self.assertTrue(environment_link.is_symlink())
        self.assertEqual(original_env, target_file.read_bytes())

    def test_activation_rejects_symlink_environment_before_compose(self):
        target_file = self.directory / "activation-environment-target"
        target_file.write_bytes(original_env)
        environment_link = self.directory / "activation-environment-link"
        try:
            environment_link.symlink_to(target_file)
        except OSError as error:
            self.skipTest(f"symlink creation unavailable: {error}")
        runner = FakeRunner(
            successful_activation_preflight_responses()
            + [
                completed(returncode=1),
                completed(returncode=1),
            ]
        )
        clock = FakeClock()
        health_checker = RecordingHealthChecker([])
        core = UpdaterCore(
            self.config,
            runner=runner,
            monotonic=clock.monotonic,
            sleep=clock.sleep,
            health_checker=health_checker,
            environment_path=environment_link,
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual("failed", state.state)
        self.assertEqual(
            "activation failed before environment update",
            state.message,
        )
        self.assertEqual(
            [
                (["docker", "image", "inspect", TARGET_IMAGE], 30),
                (["docker", "inspect", "sub2api"], 30),
                (["docker", "image", "inspect", CURRENT_IMAGE], 30),
            ],
            runner.calls,
        )
        self.assertNotIn(COMPOSE_COMMAND, [args for args, _ in runner.calls])
        self.assertEqual([], health_checker.calls)
        self.assertTrue(environment_link.is_symlink())
        self.assertEqual(original_env, target_file.read_bytes())

    def test_environment_backup_rejects_nonregular_and_hardlinked_files(self):
        cases = [
            ("fifo", stat.S_IFIFO | 0o600, 1),
            ("hardlink", stat.S_IFREG | 0o640, 2),
        ]
        for name, mode, link_count in cases:
            with self.subTest(case=name):
                metadata = os.stat_result(
                    (mode, 0, 0, link_count, 0, 0, 0, 0, 0, 0)
                )
                with mock.patch.object(
                    updater_core.os,
                    "lstat",
                    return_value=metadata,
                ):
                    with self.assertRaises(UpdaterError) as raised:
                        updater_core._read_environment_backup(
                            self.environment_file
                        )

                self.assertEqual(
                    "ENVIRONMENT_WRITE_FAILED",
                    raised.exception.code,
                )
                self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_environment_update_rejects_external_change_before_replace(self):
        backup = updater_core._read_environment_backup(self.environment_file)
        external_environment = (
            b"POSTGRES_PASSWORD=do-not-leak\n"
            b"JWT_SECRET=externally-rotated\n"
            b"OPERATOR_NOTE=preserve-this\n"
            b"SUB2API_IMAGE=xqian/sub2api:equivalent-cache-20260709-083433\n"
        )
        real_apply_metadata = updater_core._apply_environment_metadata

        def mutate_environment(
            file_descriptor,
            temporary_path,
            environment_backup,
            **kwargs,
        ):
            result = real_apply_metadata(
                file_descriptor,
                temporary_path,
                environment_backup,
                **kwargs,
            )
            self.environment_file.write_bytes(external_environment)
            return result

        with mock.patch.object(
            updater_core,
            "_apply_environment_metadata",
            side_effect=mutate_environment,
        ):
            with self.assertRaises(UpdaterError) as raised:
                replace_sub2api_image(
                    self.environment_file,
                    TARGET_IMAGE,
                    backup=backup,
                )

        self.assertEqual("ENVIRONMENT_CONFLICT", raised.exception.code)
        self.assertEqual(
            "deployment environment changed during update",
            raised.exception.message,
        )
        self.assertEqual(
            external_environment,
            self.environment_file.read_bytes(),
        )

    def test_environment_metadata_applies_owner_before_full_mode(self):
        backup = updater_core.EnvironmentBackup(
            contents=b"",
            mode=0o2640,
            uid=123,
            gid=456,
        )
        temporary_path = self.directory / ".env.tmp"

        cases = [
            (
                "posix-fchmod",
                True,
                True,
                [
                    ("fchown", 17, 123, 456),
                    ("fchmod", 17, 0o2640),
                ],
            ),
            (
                "posix-chmod-fallback",
                True,
                False,
                [
                    ("fchown", 17, 123, 456),
                    ("chmod", temporary_path, 0o2640),
                ],
            ),
            (
                "windows-skips-owner",
                False,
                True,
                [("fchmod", 17, 0o2640)],
            ),
        ]
        for name, preserve_owner, has_fchmod, expected in cases:
            with self.subTest(case=name):
                calls = []

                def fchown(fd, uid, gid):
                    calls.append(("fchown", fd, uid, gid))

                def fchmod(fd, mode):
                    calls.append(("fchmod", fd, mode))

                def chmod(path, mode):
                    calls.append(("chmod", path, mode))

                updater_core._apply_environment_metadata(
                    17,
                    temporary_path,
                    backup,
                    preserve_owner=preserve_owner,
                    fchown=fchown,
                    fchmod=fchmod if has_fchmod else None,
                    chmod=chmod,
                )

                self.assertEqual(expected, calls)

    def test_environment_write_propagates_programming_errors(self):
        real_fdopen = updater_core.os.fdopen

        for name, failure_point, expected_error in [
            (
                "metadata-assertion",
                "metadata",
                AssertionError("metadata assertion"),
            ),
            (
                "metadata-value",
                "metadata",
                ValueError("metadata value error"),
            ),
            (
                "write-type",
                "write",
                TypeError("write type error"),
            ),
        ]:
            with self.subTest(case=name):
                self.environment_file.write_bytes(original_env)

                class FailingWriter:
                    def __init__(self, file_descriptor, mode, **kwargs):
                        self._file = real_fdopen(
                            file_descriptor,
                            mode,
                            **kwargs,
                        )

                    def __enter__(self):
                        self._file.__enter__()
                        return self

                    def __exit__(self, *args):
                        return self._file.__exit__(*args)

                    def write(self, contents):
                        raise expected_error

                    def __getattr__(self, attribute):
                        return getattr(self._file, attribute)

                if failure_point == "metadata":
                    error_patch = mock.patch.object(
                        updater_core,
                        "_apply_environment_metadata",
                        side_effect=expected_error,
                    )
                else:
                    error_patch = mock.patch.object(
                        updater_core.os,
                        "fdopen",
                        side_effect=FailingWriter,
                    )

                with error_patch:
                    try:
                        replace_sub2api_image(
                            self.environment_file,
                            TARGET_IMAGE,
                        )
                    except BaseException as actual_error:
                        self.assertIs(expected_error, actual_error)
                    else:
                        self.fail("programming error was swallowed")

                self.assertEqual(
                    original_env,
                    self.environment_file.read_bytes(),
                )
                self.assertEqual(
                    [],
                    list(self.directory.glob(".env.*.tmp")),
                )

    def test_activation_does_not_convert_environment_programming_error_to_rollback(
        self,
    ):
        expected_error = ValueError("environment metadata programming error")
        core, runner, _, health_checker = self.new_activation_core(
            successful_activation_preflight_responses()
            + [
                completed(),
                running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
            ]
        )
        self.save_prepared_state(core)

        with mock.patch.object(
            updater_core,
            "_apply_environment_metadata",
            side_effect=expected_error,
        ):
            try:
                core.activate()
            except BaseException as actual_error:
                self.assertIs(expected_error, actual_error)
            else:
                self.fail("activation converted programming error to rollback")

        self.assertEqual(activation_preflight_calls(30), runner.calls)
        self.assertEqual([], health_checker.calls)
        self.assertEqual(original_env, self.environment_file.read_bytes())
        persisted_state = core.load_state()
        self.assertEqual("activating", persisted_state.state)
        self.assertEqual(TARGET_IMAGE_ID, persisted_state.target_image_id)
        self.assertEqual(PREVIOUS_IMAGE_ID, persisted_state.previous_image_id)

    def test_duplicate_sub2api_image_keys_are_rejected_without_modification(self):
        duplicate_environment = (
            b"SUB2API_IMAGE=first\n"
            b"POSTGRES_PASSWORD=do-not-leak\n"
            b"SUB2API_IMAGE=second\n"
        )
        self.environment_file.write_bytes(duplicate_environment)

        with self.assertRaises(UpdaterError) as raised:
            replace_sub2api_image(self.environment_file, TARGET_IMAGE)

        self.assertEqual("ENVIRONMENT_WRITE_FAILED", raised.exception.code)
        self.assertEqual(
            "failed to update deployment environment",
            raised.exception.message,
        )
        self.assertEqual(
            duplicate_environment,
            self.environment_file.read_bytes(),
        )
        self.assertNotIn("do-not-leak", raised.exception.message)

    def test_activation_preflight_rejects_retagged_target_metadata(self):
        core, runner, _, health_checker = self.new_activation_core(
            [
                image_inspect(
                    "9.9.9",
                    architecture="arm64",
                    source="https://attacker.example/repository",
                ),
                image_id_inspect(TARGET_IMAGE_ID),
                running_container_inspect(TARGET_IMAGE_ID, "healthy"),
            ],
            health_responses=(True,),
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual("failed", state.state)
        self.assertEqual("activation preflight failed", state.message)
        self.assertEqual(
            [
                (
                    ["docker", "image", "inspect", TARGET_IMAGE],
                    30,
                )
            ],
            runner.calls,
        )
        self.assertNotIn(COMPOSE_COMMAND, [args for args, _ in runner.calls])
        self.assertEqual([], health_checker.calls)
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_activation_preflight_rejects_noncanonical_target_reference(self):
        cases = [
            "ghcr.io/gwenliu1025/sub2api:latest",
            "example.invalid/sub2api:0.1.150",
        ]
        for index, target_image in enumerate(cases):
            with self.subTest(target_image=target_image):
                environment_file = self.directory / f"canonical-{index}.env"
                environment_file.write_bytes(original_env)
                core, runner = self.new_core(
                    [
                        completed(returncode=1),
                        completed(returncode=1),
                    ],
                    state_file=str(
                        self.directory / f"canonical-{index}-state.json"
                    ),
                )
                core._environment_path = environment_file
                core.save_state(
                    UpdateState(
                        state="prepared",
                        current_image=CURRENT_IMAGE,
                        target_image=target_image,
                        previous_image=CURRENT_IMAGE,
                        message="target image prepared",
                        updated_at="2026-07-10T00:00:00Z",
                    )
                )

                state = core.activate()

                self.assertEqual("failed", state.state)
                self.assertEqual("activation preflight failed", state.message)
                self.assertEqual([], runner.calls)
                self.assertEqual(original_env, environment_file.read_bytes())

    def test_activation_preflight_rejects_retagged_previous_image(self):
        retagged_previous_id = "sha256:retagged-previous"
        core, runner, _, health_checker = self.new_activation_core(
            [
                image_inspect("0.1.150"),
                running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
                image_id_inspect(retagged_previous_id),
                image_id_inspect(PREVIOUS_IMAGE_ID),
                running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
                image_id_inspect(PREVIOUS_IMAGE_ID),
                running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
            ],
            health_responses=(True,),
            activation_timeout_seconds=4,
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual("failed", state.state)
        self.assertEqual("activation preflight failed", state.message)
        self.assertEqual(
            activation_preflight_calls(2.0),
            runner.calls,
        )
        self.assertNotIn(COMPOSE_COMMAND, [args for args, _ in runner.calls])
        self.assertEqual([], health_checker.calls)
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_run_activation_persists_captured_ids_before_environment_change(
        self,
    ):
        core, runner, _, health_checker = self.new_activation_core(
            successful_activation_preflight_responses(),
            health_responses=(),
        )
        self.save_prepared_state(core)
        core.begin_activation()
        simulated_crash = KeyboardInterrupt("simulated activation crash")

        with mock.patch.object(
            updater_core,
            "replace_sub2api_image",
            side_effect=simulated_crash,
        ):
            with self.assertRaises(KeyboardInterrupt) as raised:
                core.run_activation()

        self.assertIs(simulated_crash, raised.exception)
        persisted_state = core.load_state()
        self.assertEqual("activating", persisted_state.state)
        self.assertEqual(
            TARGET_IMAGE_ID,
            getattr(persisted_state, "target_image_id", None),
        )
        self.assertEqual(
            PREVIOUS_IMAGE_ID,
            getattr(persisted_state, "previous_image_id", None),
        )
        persisted_payload = json.loads(
            Path(self.config.state_file).read_text(encoding="utf-8")
        )
        self.assertEqual(TARGET_IMAGE_ID, persisted_payload["target_image_id"])
        self.assertEqual(
            PREVIOUS_IMAGE_ID,
            persisted_payload["previous_image_id"],
        )
        self.assertEqual(activation_preflight_calls(30), runner.calls)
        self.assertEqual([], health_checker.calls)
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_activation_stops_when_captured_id_state_write_fails(self):
        core, runner, _, health_checker = self.new_activation_core(
            successful_activation_preflight_responses()
            + [
                completed(),
                running_container_inspect(TARGET_IMAGE_ID, "healthy"),
            ]
        )
        self.save_prepared_state(core)
        core.begin_activation()
        state_write_error = UpdaterError(
            "STATE_WRITE_FAILED",
            "failed to persist update state",
        )

        with mock.patch.object(
            core,
            "save_state",
            side_effect=state_write_error,
        ):
            with self.assertRaises(UpdaterError) as raised:
                core.run_activation()

        self.assertIs(state_write_error, raised.exception)
        self.assertEqual(activation_preflight_calls(30), runner.calls)
        self.assertEqual([], health_checker.calls)
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_activation_and_rollback_share_overall_deadline_with_reserve(self):
        clock = FakeClock()
        runner = AdvancingFakeRunner(
            successful_activation_preflight_responses()
            + [
                completed(),
                completed(returncode=1),
                completed(),
                running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
                completed(),
                running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
            ],
            clock,
            advances=[
                0.0,
                0.0,
                0.0,
                59.0,
                1.0,
                50.0,
                8.0,
                0.0,
                0.0,
            ],
        )
        health_checker = RecordingHealthChecker([True])
        core = UpdaterCore(
            self.config,
            runner=runner,
            monotonic=clock.monotonic,
            sleep=clock.sleep,
            health_checker=health_checker,
            environment_path=self.environment_file,
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual("rolled_back", state.state)
        self.assertEqual(118.0, clock.now)
        self.assertEqual(
            [
                (["docker", "image", "inspect", TARGET_IMAGE], 30),
                (["docker", "inspect", "sub2api"], 30),
                (["docker", "image", "inspect", CURRENT_IMAGE], 30),
                (COMPOSE_COMMAND, 60),
                (["docker", "inspect", "sub2api"], 1),
                (COMPOSE_COMMAND, 60),
                (["docker", "inspect", "sub2api"], 10),
            ],
            runner.calls,
        )
        for started_at, (_, timeout) in zip(
            runner.call_times,
            runner.calls,
        ):
            self.assertGreater(timeout, 0)
            self.assertLessEqual(timeout, 120 - started_at)
        self.assertEqual(1, len(health_checker.calls))
        self.assertGreater(health_checker.calls[0][1], 0)
        self.assertLessEqual(health_checker.calls[0][1], 2)
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_activation_recreates_only_sub2api_and_becomes_healthy(self):
        responses = successful_activation_preflight_responses() + [
            completed(),
            running_container_inspect(TARGET_IMAGE_ID, "unhealthy"),
            running_container_inspect(TARGET_IMAGE_ID, "healthy"),
        ]
        core, runner, clock, health_checker = self.new_activation_core(
            responses
        )
        self.save_prepared_state(core)

        scheduled = core.begin_activation()

        self.assertEqual("activating", scheduled.state)
        self.assertEqual(scheduled, core.load_state())
        self.assertEqual([], runner.calls)
        self.assertEqual(original_env, self.environment_file.read_bytes())

        state = core.run_activation()

        self.assertEqual(
            activation_preflight_calls(30)
            + [
                (COMPOSE_COMMAND, 60),
                (["docker", "inspect", "sub2api"], 30),
                (["docker", "inspect", "sub2api"], 30),
            ],
            runner.calls,
        )
        self.assertEqual([2.0], clock.sleeps)
        self.assertEqual(1, len(health_checker.calls))
        self.assertEqual(self.config.health_url, health_checker.calls[0][0])
        self.assertLessEqual(health_checker.calls[0][1], 5.0)
        self.assertEqual("healthy", state.state)
        self.assertEqual(TARGET_IMAGE, state.current_image)
        self.assertEqual(TARGET_IMAGE, state.target_image)
        self.assertEqual(CURRENT_IMAGE, state.previous_image)
        self.assertEqual("target image activated and healthy", state.message)
        self.assertEqual("", state.target_image_id)
        self.assertEqual("", state.previous_image_id)
        self.assertEqual(
            {
                "state",
                "current_image",
                "target_image",
                "previous_image",
                "message",
                "updated_at",
            },
            set(
                json.loads(
                    Path(self.config.state_file).read_text(encoding="utf-8")
                )
            ),
        )
        self.assertEqual(
            (
                b"POSTGRES_PASSWORD=do-not-leak\n"
                b"JWT_SECRET=also-do-not-leak\n"
                b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150\n"
            ),
            self.environment_file.read_bytes(),
        )
        self.assertEqual(
            original_env.splitlines(keepends=True)[:2],
            self.environment_file.read_bytes().splitlines(keepends=True)[:2],
        )

    def test_activation_uses_configured_environment_file_for_write_and_compose(
        self,
    ):
        configured_environment = self.directory / "configured.env"
        configured_environment.write_bytes(original_env)
        config = dataclasses.replace(
            self.config,
            environment_file=str(configured_environment),
        )
        runner = FakeRunner(
            successful_activation_preflight_responses()
            + [
                completed(),
                running_container_inspect(TARGET_IMAGE_ID, "healthy"),
            ]
        )
        clock = FakeClock()
        health_checker = RecordingHealthChecker([True])
        core = UpdaterCore(
            config,
            runner=runner,
            monotonic=clock.monotonic,
            sleep=clock.sleep,
            health_checker=health_checker,
        )
        self.save_prepared_state(core)

        state = core.activate()

        expected_compose_command = list(COMPOSE_COMMAND)
        expected_compose_command[
            expected_compose_command.index("--env-file") + 1
        ] = str(configured_environment)
        self.assertEqual("healthy", state.state)
        self.assertEqual(
            activation_preflight_calls(30)
            + [
                (expected_compose_command, 60),
                (["docker", "inspect", "sub2api"], 30),
            ],
            runner.calls,
        )
        self.assertEqual(
            (
                b"POSTGRES_PASSWORD=do-not-leak\n"
                b"JWT_SECRET=also-do-not-leak\n"
                b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150\n"
            ),
            configured_environment.read_bytes(),
        )
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_activation_inspect_timeouts_are_bounded_by_remaining_deadline(self):
        clock = FakeClock()
        runner = AdvancingFakeRunner(
            successful_activation_preflight_responses()
            + [
                completed(),
                running_container_inspect(TARGET_IMAGE_ID, "healthy"),
            ],
            clock,
            advances=[1.0, 1.0, 1.0, 1.0, 0.0],
        )
        health_checker = RecordingHealthChecker([True])
        config = dataclasses.replace(
            self.config,
            activation_timeout_seconds=10,
        )
        core = UpdaterCore(
            config,
            runner=runner,
            monotonic=clock.monotonic,
            sleep=clock.sleep,
            health_checker=health_checker,
            environment_path=self.environment_file,
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual("healthy", state.state)
        expected_timeouts = [5, 4, 3, 2, 1]
        for (_, timeout), expected_timeout in zip(
            runner.calls,
            expected_timeouts,
        ):
            self.assertGreater(timeout, 0)
            self.assertLessEqual(timeout, expected_timeout)
        self.assertEqual(1, len(health_checker.calls))
        self.assertGreater(health_checker.calls[0][1], 0)
        self.assertLessEqual(health_checker.calls[0][1], 1)

    def test_activation_compose_time_counts_against_health_deadline(self):
        clock = FakeClock()
        runner = AdvancingFakeRunner(
            successful_activation_preflight_responses()
            + [
                completed(),
                running_container_inspect(TARGET_IMAGE_ID, "healthy"),
            ],
            clock,
            advances=[0.0, 0.0, 0.0, 59.0, 0.0],
        )
        health_checker = RecordingHealthChecker([True])
        core = UpdaterCore(
            self.config,
            runner=runner,
            monotonic=clock.monotonic,
            sleep=clock.sleep,
            health_checker=health_checker,
            environment_path=self.environment_file,
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual("healthy", state.state)
        self.assertEqual(
            activation_preflight_calls(30)
            + [
                (COMPOSE_COMMAND, 60),
                (["docker", "inspect", "sub2api"], 1),
            ],
            runner.calls,
        )
        self.assertEqual(
            ["docker", "inspect", "sub2api"],
            runner.calls[4][0],
        )
        self.assertEqual(1, len(health_checker.calls))
        self.assertGreater(health_checker.calls[0][1], 0)
        self.assertLessEqual(health_checker.calls[0][1], 1)

    def test_activation_reserves_rollback_time_when_compose_uses_target_budget(
        self,
    ):
        clock = FakeClock()
        runner = AdvancingFakeRunner(
            successful_activation_preflight_responses()
            + [
                completed(),
                completed(),
                running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
            ],
            clock,
            advances=[0.0, 0.0, 0.0, 60.0, 0.0, 0.0],
        )
        health_checker = RecordingHealthChecker([True])
        core = UpdaterCore(
            self.config,
            runner=runner,
            monotonic=clock.monotonic,
            sleep=clock.sleep,
            health_checker=health_checker,
            environment_path=self.environment_file,
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual("rolled_back", state.state)
        self.assertEqual(
            activation_preflight_calls(30)
            + [
                (COMPOSE_COMMAND, 60),
                (COMPOSE_COMMAND, 60),
                (["docker", "inspect", "sub2api"], 30),
            ],
            runner.calls,
        )
        self.assertEqual(
            1,
            [args for args, _ in runner.calls].count(
                ["docker", "image", "inspect", TARGET_IMAGE]
            ),
        )
        self.assertEqual([], clock.sleeps)
        self.assertEqual(1, len(health_checker.calls))
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_rollback_compose_time_counts_against_health_deadline(self):
        clock = FakeClock()
        runner = AdvancingFakeRunner(
            successful_activation_preflight_responses()
            + [
                completed(returncode=1),
                completed(),
                running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
            ],
            clock,
            advances=[0.0, 0.0, 0.0, 1.0, 8.0, 0.0],
        )
        health_checker = RecordingHealthChecker([True])
        config = dataclasses.replace(
            self.config,
            activation_timeout_seconds=10,
        )
        core = UpdaterCore(
            config,
            runner=runner,
            monotonic=clock.monotonic,
            sleep=clock.sleep,
            health_checker=health_checker,
            environment_path=self.environment_file,
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual("rolled_back", state.state)
        self.assertEqual(
            activation_preflight_calls(5)
            + [
                (COMPOSE_COMMAND, 5),
                (COMPOSE_COMMAND, 9),
                (["docker", "inspect", "sub2api"], 1),
            ],
            runner.calls,
        )
        self.assertEqual(1, len(health_checker.calls))
        self.assertGreater(health_checker.calls[0][1], 0)
        self.assertLessEqual(health_checker.calls[0][1], 1)
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_http_health_timeout_does_not_exceed_sub_10ms_deadline(self):
        core, _, clock, health_checker = self.new_activation_core(
            [],
            health_responses=(True,),
        )

        result = core._check_http_health(deadline=0.005)

        self.assertTrue(result)
        self.assertEqual(0.0, clock.now)
        self.assertEqual(1, len(health_checker.calls))
        timeout = health_checker.calls[0][1]
        self.assertGreater(timeout, 0)
        self.assertLessEqual(timeout, 0.005)

    def test_http_health_without_deadline_uses_five_second_timeout(self):
        core, _, _, health_checker = self.new_activation_core(
            [],
            health_responses=(True,),
        )

        result = core._check_http_health()

        self.assertTrue(result)
        self.assertEqual(
            [(self.config.health_url, 5.0)],
            health_checker.calls,
        )

    def test_activation_http_exception_triggers_automatic_rollback(self):
        responses = successful_activation_preflight_responses() + [
            completed(),
            running_container_inspect(TARGET_IMAGE_ID, "healthy"),
            completed(),
            running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
        ]
        core, runner, clock, health_checker = self.new_activation_core(
            responses,
            health_responses=(
                http.client.BadStatusLine("invalid health response"),
                True,
            ),
            activation_timeout_seconds=2,
        )
        self.save_prepared_state(core)

        try:
            state = core.activate()
        except http.client.HTTPException as error:
            self.fail(f"HTTPException escaped activation: {error!r}")

        self.assertEqual("rolled_back", state.state)
        self.assertEqual("rolled_back", core.load_state().state)
        self.assertEqual(
            activation_preflight_calls(1)
            + [
                (COMPOSE_COMMAND, 1),
                (["docker", "inspect", "sub2api"], 1),
                (COMPOSE_COMMAND, 1),
                (["docker", "inspect", "sub2api"], 1),
            ],
            runner.calls,
        )
        self.assertEqual([1.0], clock.sleeps)
        self.assertEqual(2, len(health_checker.calls))
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_default_http_health_checker_treats_http_exception_as_unhealthy(self):
        with mock.patch(
            "deploy.updater.updater_core.urllib.request.urlopen",
            side_effect=http.client.HTTPException("invalid response"),
        ):
            try:
                result = _http_health_ok(self.config.health_url, 5.0)
            except http.client.HTTPException as error:
                self.fail(f"HTTPException escaped health checker: {error!r}")

        self.assertFalse(result)

    def test_default_http_health_checker_propagates_programming_errors(self):
        expected_error = ValueError("invalid health check configuration")
        with mock.patch(
            "deploy.updater.updater_core.urllib.request.urlopen",
            side_effect=expected_error,
        ):
            with self.assertRaises(ValueError) as raised:
                _http_health_ok(self.config.health_url, 5.0)

        self.assertIs(expected_error, raised.exception)

    def test_activation_retries_transient_container_inspect_failures(self):
        responses = successful_activation_preflight_responses() + [
            completed(),
            completed(returncode=1),
            running_container_inspect(TARGET_IMAGE_ID, "healthy"),
        ]
        core, runner, clock, health_checker = self.new_activation_core(
            responses,
            activation_timeout_seconds=8,
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual("healthy", state.state)
        self.assertEqual(
            activation_preflight_calls(4)
            + [
                (COMPOSE_COMMAND, 4),
                (["docker", "inspect", "sub2api"], 4),
                (["docker", "inspect", "sub2api"], 2),
            ],
            runner.calls,
        )
        self.assertEqual([2.0], clock.sleeps)
        self.assertEqual(1, len(health_checker.calls))
        self.assertGreater(health_checker.calls[0][1], 0)
        self.assertLessEqual(health_checker.calls[0][1], 4)

    def test_activation_waits_until_deadline_before_rolling_back_inspect_failures(self):
        responses = successful_activation_preflight_responses() + [
            completed(),
            completed(returncode=1),
            completed(),
            running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
        ]
        core, runner, clock, health_checker = self.new_activation_core(
            responses,
            activation_timeout_seconds=4,
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual("rolled_back", state.state)
        self.assertEqual(
            activation_preflight_calls(2)
            + [
                (COMPOSE_COMMAND, 2),
                (["docker", "inspect", "sub2api"], 2),
                (COMPOSE_COMMAND, 2),
                (["docker", "inspect", "sub2api"], 2),
            ],
            runner.calls,
        )
        self.assertEqual([2.0], clock.sleeps)
        self.assertEqual(1, len(health_checker.calls))
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_activation_polling_does_not_swallow_runner_programming_errors(self):
        errors = [
            AssertionError("activation runner assertion"),
            ValueError("activation runner value error"),
        ]
        for index, expected_error in enumerate(errors):
            with self.subTest(error=type(expected_error).__name__):
                environment_file = self.directory / f"programming-{index}.env"
                environment_file.write_bytes(original_env)
                runner = FakeRunner(
                    successful_activation_preflight_responses()
                    + [completed(), expected_error]
                )
                clock = FakeClock()
                health_checker = RecordingHealthChecker([])
                config = dataclasses.replace(
                    self.config,
                    state_file=str(
                        self.directory / f"programming-{index}-state.json"
                    ),
                )
                core = UpdaterCore(
                    config,
                    runner=runner,
                    monotonic=clock.monotonic,
                    sleep=clock.sleep,
                    health_checker=health_checker,
                    environment_path=environment_file,
                )
                self.save_prepared_state(core)

                with self.assertRaises(type(expected_error)) as raised:
                    core.activate()

                self.assertIs(expected_error, raised.exception)
                self.assertEqual([], clock.sleeps)
                self.assertEqual([], health_checker.calls)

    def test_activation_health_checker_programming_error_propagates(self):
        expected_error = ValueError("health checker programming error")
        core, _, clock, _ = self.new_activation_core(
            successful_activation_preflight_responses()
            + [
                completed(),
                running_container_inspect(TARGET_IMAGE_ID, "healthy"),
            ],
            health_responses=(expected_error,),
        )
        self.save_prepared_state(core)

        with self.assertRaises(ValueError) as raised:
            core.activate()

        self.assertIs(expected_error, raised.exception)
        self.assertEqual([], clock.sleeps)

    def test_activation_failure_restores_exact_environment_and_rolls_back(self):
        responses = successful_activation_preflight_responses() + [
            completed(
                stdout="POSTGRES_PASSWORD=compose-output-secret",
                stderr="JWT_SECRET=compose-error-secret",
                returncode=1,
            ),
            completed(),
            running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
        ]
        core, runner, _, health_checker = self.new_activation_core(responses)
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual(
            activation_preflight_calls(30)
            + [
                (COMPOSE_COMMAND, 60),
                (COMPOSE_COMMAND, 120),
                (["docker", "inspect", "sub2api"], 30),
            ],
            runner.calls,
        )
        self.assertEqual(1, len(health_checker.calls))
        self.assertEqual("rolled_back", state.state)
        self.assertEqual(CURRENT_IMAGE, state.current_image)
        self.assertEqual(TARGET_IMAGE, state.target_image)
        self.assertEqual(CURRENT_IMAGE, state.previous_image)
        self.assertEqual(
            "activation failed; previous image restored",
            state.message,
        )
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_activation_rollback_preserves_external_environment_change(self):
        external_environment = (
            b"POSTGRES_PASSWORD=do-not-leak\n"
            b"JWT_SECRET=externally-rotated\n"
            b"OPERATOR_NOTE=preserve-this\n"
            b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150\n"
        )
        fake_runner = FakeRunner(
            successful_activation_preflight_responses()
            + [
                completed(returncode=1),
                completed(returncode=1),
            ]
        )
        target_compose_seen = False

        def runner(args, timeout):
            nonlocal target_compose_seen
            result = fake_runner(args, timeout)
            if list(args) == COMPOSE_COMMAND and not target_compose_seen:
                target_compose_seen = True
                self.environment_file.write_bytes(external_environment)
            return result

        clock = FakeClock()
        health_checker = RecordingHealthChecker([])
        core = UpdaterCore(
            self.config,
            runner=runner,
            monotonic=clock.monotonic,
            sleep=clock.sleep,
            health_checker=health_checker,
            environment_path=self.environment_file,
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual("rollback_failed", state.state)
        self.assertEqual("", state.current_image)
        self.assertEqual(
            (
                "activation failed; deployment environment changed; "
                "manual recovery required"
            ),
            state.message,
        )
        self.assertEqual(
            [
                (["docker", "image", "inspect", TARGET_IMAGE], 30),
                (["docker", "inspect", "sub2api"], 30),
                (["docker", "image", "inspect", CURRENT_IMAGE], 30),
                (COMPOSE_COMMAND, 60),
            ],
            fake_runner.calls,
        )
        self.assertEqual([], health_checker.calls)
        self.assertEqual(
            external_environment,
            self.environment_file.read_bytes(),
        )
        state_text = Path(self.config.state_file).read_text(encoding="utf-8")
        for secret in [
            "JWT_SECRET",
            "externally-rotated",
            "OPERATOR_NOTE",
            "preserve-this",
        ]:
            self.assertNotIn(secret, state.message)
            self.assertNotIn(secret, state_text)

    def test_activation_and_rollback_failure_marks_rollback_failed(self):
        responses = successful_activation_preflight_responses() + [
            completed(returncode=1),
            completed(returncode=1),
        ]
        core, runner, _, health_checker = self.new_activation_core(
            responses,
            health_responses=(),
        )
        self.save_prepared_state(core)

        state = core.activate()

        self.assertEqual(
            activation_preflight_calls(30)
            + [
                (COMPOSE_COMMAND, 60),
                (COMPOSE_COMMAND, 120),
            ],
            runner.calls,
        )
        self.assertEqual([], health_checker.calls)
        self.assertEqual("rollback_failed", state.state)
        self.assertEqual("", state.current_image)
        self.assertEqual(TARGET_IMAGE, state.target_image)
        self.assertEqual(CURRENT_IMAGE, state.previous_image)
        self.assertEqual(
            "activation failed and automatic rollback failed",
            state.message,
        )
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_activation_rejects_without_prepared_target(self):
        core, runner = self.new_core([])

        with self.assertRaises(UpdaterError) as raised:
            core.begin_activation()

        self.assertEqual("NO_PREPARED_UPDATE", raised.exception.code)
        core.save_state(
            UpdateState(
                state="activating",
                current_image=CURRENT_IMAGE,
                target_image=TARGET_IMAGE,
                previous_image=CURRENT_IMAGE,
            )
        )

        with self.assertRaises(UpdaterError) as raised:
            core.begin_activation()

        self.assertEqual("ACTIVATION_IN_PROGRESS", raised.exception.code)
        self.assertEqual([], runner.calls)

    def test_stale_activating_state_reconciles_to_healthy(self):
        previous_environment = (
            b"POSTGRES_PASSWORD=do-not-leak\n"
            b"JWT_SECRET=also-do-not-leak\n"
            b"X_SUB2API_IMAGE=leave-this-alone\n"
            b"SUB2API_IMAGE=xqian/sub2api:equivalent-cache-20260709-083433\n"
            b"UNRELATED_BYTES=\xff\xfe\n"
        )
        self.environment_file.write_bytes(previous_environment)
        responses = [
            running_container_inspect(TARGET_IMAGE_ID, "healthy"),
        ]
        core, runner, _, health_checker = self.new_activation_core(responses)
        self.save_activating_state(core)

        state = core.reconcile_state()

        self.assertEqual(
            [(["docker", "inspect", "sub2api"], 30)],
            runner.calls,
        )
        self.assertEqual(1, len(health_checker.calls))
        self.assertEqual("healthy", state.state)
        self.assertEqual(TARGET_IMAGE, state.current_image)
        self.assertEqual("target image activated and healthy", state.message)
        self.assertEqual(
            (
                b"POSTGRES_PASSWORD=do-not-leak\n"
                b"JWT_SECRET=also-do-not-leak\n"
                b"X_SUB2API_IMAGE=leave-this-alone\n"
                b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150\n"
                b"UNRELATED_BYTES=\xff\xfe\n"
            ),
            self.environment_file.read_bytes(),
        )

    def test_stale_reconciliation_uses_persisted_id_after_target_retag(self):
        retagged_target_id = "sha256:retagged-target"
        state_path = Path(self.config.state_file)
        state_path.write_text(
            json.dumps(
                {
                    "state": "activating",
                    "current_image": CURRENT_IMAGE,
                    "target_image": TARGET_IMAGE,
                    "previous_image": CURRENT_IMAGE,
                    "message": "activation scheduled",
                    "updated_at": "2026-07-10T00:00:00Z",
                    "target_image_id": TARGET_IMAGE_ID,
                    "previous_image_id": PREVIOUS_IMAGE_ID,
                }
            ),
            encoding="utf-8",
        )
        core, runner, _, health_checker = self.new_activation_core(
            [
                running_container_inspect(TARGET_IMAGE_ID, "healthy"),
                image_id_inspect(retagged_target_id),
            ]
        )

        try:
            state = core.reconcile_state()
        except UpdaterError as error:
            self.fail(f"persisted image IDs were rejected: {error.code}")

        self.assertEqual("healthy", state.state)
        self.assertEqual(TARGET_IMAGE, state.current_image)
        self.assertEqual(
            [(["docker", "inspect", "sub2api"], 30)],
            runner.calls,
        )
        self.assertEqual(1, len(health_checker.calls))

    def test_old_stale_activating_state_without_ids_fails_closed(self):
        core, runner, _, health_checker = self.new_activation_core(
            [
                running_container_inspect(TARGET_IMAGE_ID, "healthy"),
                image_id_inspect(TARGET_IMAGE_ID),
            ]
        )
        core.save_state(
            UpdateState(
                state="activating",
                current_image=CURRENT_IMAGE,
                target_image=TARGET_IMAGE,
                previous_image=CURRENT_IMAGE,
                message="activation scheduled",
                updated_at="2026-07-10T00:00:00Z",
            )
        )

        state = core.reconcile_state()

        self.assertEqual("failed", state.state)
        self.assertEqual("", state.current_image)
        self.assertEqual(
            "unable to reconcile interrupted activation",
            state.message,
        )
        self.assertEqual([], runner.calls)
        self.assertEqual([], health_checker.calls)
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_stale_activating_state_reconciles_to_rolled_back(self):
        target_environment = (
            b"POSTGRES_PASSWORD=do-not-leak\n"
            b"JWT_SECRET=also-do-not-leak\n"
            b"X_SUB2API_IMAGE=leave-this-alone\n"
            b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150\n"
            b"UNRELATED_BYTES=\xff\xfe\n"
        )
        self.environment_file.write_bytes(target_environment)
        responses = [
            running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
        ]
        core, runner, _, health_checker = self.new_activation_core(responses)
        self.save_activating_state(core)

        state = core.reconcile_state()

        self.assertEqual(
            [(["docker", "inspect", "sub2api"], 30)],
            runner.calls,
        )
        self.assertEqual(1, len(health_checker.calls))
        self.assertEqual("rolled_back", state.state)
        self.assertEqual(CURRENT_IMAGE, state.current_image)
        self.assertEqual(
            "activation failed; previous image restored",
            state.message,
        )
        self.assertEqual(
            (
                b"POSTGRES_PASSWORD=do-not-leak\n"
                b"JWT_SECRET=also-do-not-leak\n"
                b"X_SUB2API_IMAGE=leave-this-alone\n"
                b"SUB2API_IMAGE=xqian/sub2api:equivalent-cache-20260709-083433\n"
                b"UNRELATED_BYTES=\xff\xfe\n"
            ),
            self.environment_file.read_bytes(),
        )

    def test_stale_reconciliation_retries_transient_target_health_failures(self):
        previous_environment = (
            b"POSTGRES_PASSWORD=do-not-leak\n"
            b"JWT_SECRET=also-do-not-leak\n"
            b"UNRELATED=leave-this-alone\n"
            b"SUB2API_IMAGE=xqian/sub2api:equivalent-cache-20260709-083433\n"
        )
        self.environment_file.write_bytes(previous_environment)
        responses = [
            completed(returncode=1),
            running_container_inspect(TARGET_IMAGE_ID, "healthy"),
            running_container_inspect(TARGET_IMAGE_ID, "healthy"),
        ]
        core, runner, clock, health_checker = self.new_activation_core(
            responses,
            health_responses=(False, True),
            activation_timeout_seconds=6,
            poll_interval_seconds=2,
        )
        self.save_activating_state(core)

        state = core.reconcile_state()

        self.assertEqual("healthy", state.state)
        self.assertEqual(TARGET_IMAGE, state.current_image)
        self.assertEqual(
            [
                (["docker", "inspect", "sub2api"], 6),
                (["docker", "inspect", "sub2api"], 4),
                (["docker", "inspect", "sub2api"], 2),
            ],
            runner.calls,
        )
        self.assertEqual([2, 2], clock.sleeps)
        self.assertEqual(2, len(health_checker.calls))
        self.assertGreater(health_checker.calls[0][1], 0)
        self.assertLessEqual(health_checker.calls[0][1], 4)
        self.assertGreater(health_checker.calls[1][1], 0)
        self.assertLessEqual(health_checker.calls[1][1], 2)
        self.assertEqual(
            (
                b"POSTGRES_PASSWORD=do-not-leak\n"
                b"JWT_SECRET=also-do-not-leak\n"
                b"UNRELATED=leave-this-alone\n"
                b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150\n"
            ),
            self.environment_file.read_bytes(),
        )

    def test_stale_reconciliation_retries_transient_previous_health_failures(
        self,
    ):
        target_environment = (
            b"POSTGRES_PASSWORD=do-not-leak\n"
            b"JWT_SECRET=also-do-not-leak\n"
            b"UNRELATED=leave-this-alone\n"
            b"SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150\n"
        )
        self.environment_file.write_bytes(target_environment)
        responses = [
            completed(returncode=1),
            running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
            running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
        ]
        core, runner, clock, health_checker = self.new_activation_core(
            responses,
            health_responses=(
                http.client.BadStatusLine("transient health response"),
                True,
            ),
            activation_timeout_seconds=6,
            poll_interval_seconds=2,
        )
        self.save_activating_state(core)

        state = core.reconcile_state()

        self.assertEqual("rolled_back", state.state)
        self.assertEqual(CURRENT_IMAGE, state.current_image)
        self.assertEqual(
            [
                (["docker", "inspect", "sub2api"], 6),
                (["docker", "inspect", "sub2api"], 4),
                (["docker", "inspect", "sub2api"], 2),
            ],
            runner.calls,
        )
        self.assertEqual([2, 2], clock.sleeps)
        self.assertEqual(2, len(health_checker.calls))
        self.assertEqual(
            (
                b"POSTGRES_PASSWORD=do-not-leak\n"
                b"JWT_SECRET=also-do-not-leak\n"
                b"UNRELATED=leave-this-alone\n"
                b"SUB2API_IMAGE=xqian/sub2api:equivalent-cache-20260709-083433\n"
            ),
            self.environment_file.read_bytes(),
        )

    def test_stale_reconciliation_retries_until_deadline_then_marks_failed(self):
        responses = [completed(returncode=1) for _ in range(2)]
        core, runner, clock, health_checker = self.new_activation_core(
            responses,
            health_responses=(),
            activation_timeout_seconds=4,
            poll_interval_seconds=2,
        )
        self.save_activating_state(core)

        state = core.reconcile_state()

        self.assertEqual("failed", state.state)
        self.assertEqual(
            "unable to reconcile interrupted activation",
            state.message,
        )
        self.assertEqual("", state.current_image)
        self.assertEqual(
            [
                (["docker", "inspect", "sub2api"], 4),
                (["docker", "inspect", "sub2api"], 2),
            ],
            runner.calls,
        )
        self.assertEqual([2, 2], clock.sleeps)
        self.assertEqual([], health_checker.calls)
        self.assertEqual(original_env, self.environment_file.read_bytes())

    def test_stale_reconciliation_duplicate_environment_key_marks_failed(self):
        duplicate_environment = (
            b"POSTGRES_PASSWORD=do-not-leak\n"
            b"SUB2API_IMAGE=first\n"
            b"JWT_SECRET=also-do-not-leak\n"
            b"SUB2API_IMAGE=second\n"
        )
        self.environment_file.write_bytes(duplicate_environment)
        responses = [
            running_container_inspect(TARGET_IMAGE_ID, "healthy"),
        ]
        core, runner, _, health_checker = self.new_activation_core(responses)
        self.save_activating_state(core)

        state = core.reconcile_state()

        self.assertEqual("failed", state.state)
        self.assertEqual(TARGET_IMAGE, state.current_image)
        self.assertEqual(
            "unable to reconcile deployment environment",
            state.message,
        )
        self.assertEqual(
            [(["docker", "inspect", "sub2api"], 30)],
            runner.calls,
        )
        self.assertEqual(1, len(health_checker.calls))
        self.assertEqual(
            duplicate_environment,
            self.environment_file.read_bytes(),
        )
        state_text = Path(self.config.state_file).read_text(encoding="utf-8")
        for secret in [
            "POSTGRES_PASSWORD",
            "JWT_SECRET",
            "do-not-leak",
            "also-do-not-leak",
            "first",
            "second",
        ]:
            self.assertNotIn(secret, state.message)
            self.assertNotIn(secret, state_text)

    def test_stale_reconciliation_environment_write_failure_marks_failed(self):
        previous_environment = (
            b"POSTGRES_PASSWORD=do-not-leak\n"
            b"JWT_SECRET=also-do-not-leak\n"
            b"SUB2API_IMAGE=xqian/sub2api:equivalent-cache-20260709-083433\n"
            b"UNRELATED=leave-this-alone\n"
        )
        self.environment_file.write_bytes(previous_environment)
        responses = [
            running_container_inspect(TARGET_IMAGE_ID, "healthy"),
        ]
        core, runner, _, health_checker = self.new_activation_core(responses)
        self.save_activating_state(core)
        real_replace = os.replace

        def fail_environment_replace(source, destination):
            if Path(destination) == self.environment_file:
                raise OSError(
                    f"{self.environment_file}: {previous_environment!r}"
                )
            return real_replace(source, destination)

        with mock.patch(
            "deploy.updater.updater_core.os.replace",
            side_effect=fail_environment_replace,
        ):
            state = core.reconcile_state()

        self.assertEqual("failed", state.state)
        self.assertEqual(TARGET_IMAGE, state.current_image)
        self.assertEqual(
            "unable to reconcile deployment environment",
            state.message,
        )
        self.assertEqual(
            [(["docker", "inspect", "sub2api"], 30)],
            runner.calls,
        )
        self.assertEqual(1, len(health_checker.calls))
        self.assertEqual(
            previous_environment,
            self.environment_file.read_bytes(),
        )
        state_text = Path(self.config.state_file).read_text(encoding="utf-8")
        for secret in [
            "POSTGRES_PASSWORD",
            "JWT_SECRET",
            "do-not-leak",
            "also-do-not-leak",
            str(self.environment_file),
        ]:
            self.assertNotIn(secret, state.message)
            self.assertNotIn(secret, state_text)

    def test_stale_reconciliation_timeout_preserves_observed_candidate(self):
        stale_recorded_image = "stale/recorded:image"
        cases = [
            ("target", TARGET_IMAGE_ID, TARGET_IMAGE),
            ("previous", PREVIOUS_IMAGE_ID, CURRENT_IMAGE),
        ]
        for index, (name, observed_image_id, expected_image) in enumerate(cases):
            with self.subTest(candidate=name):
                state_file = self.directory / f"observed-{index}-state.json"
                core, runner, clock, health_checker = self.new_activation_core(
                    [
                        running_container_inspect(
                            observed_image_id,
                            "starting",
                        ),
                        running_container_inspect(
                            observed_image_id,
                            "starting",
                        ),
                    ],
                    health_responses=(),
                    activation_timeout_seconds=4,
                    poll_interval_seconds=2,
                    state_file=str(state_file),
                )
                core.save_state(
                    UpdateState(
                        state="activating",
                        current_image=stale_recorded_image,
                        target_image=TARGET_IMAGE,
                        previous_image=CURRENT_IMAGE,
                        message="activation scheduled",
                        updated_at="2026-07-10T00:00:00Z",
                        target_image_id=TARGET_IMAGE_ID,
                        previous_image_id=PREVIOUS_IMAGE_ID,
                    )
                )

                state = core.reconcile_state()

                self.assertEqual("failed", state.state)
                self.assertEqual(expected_image, state.current_image)
                self.assertEqual(
                    [
                        (["docker", "inspect", "sub2api"], 4),
                        (["docker", "inspect", "sub2api"], 2),
                    ],
                    runner.calls,
                )
                self.assertEqual([2, 2], clock.sleeps)
                self.assertEqual([], health_checker.calls)

    def test_stale_reconciliation_matches_persisted_previous_id(self):
        responses = [
            running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
        ]
        core, runner, _, health_checker = self.new_activation_core(responses)
        self.save_activating_state(core)

        state = core.reconcile_state()

        self.assertEqual(
            [(["docker", "inspect", "sub2api"], 30)],
            runner.calls,
        )
        self.assertEqual(1, len(health_checker.calls))
        self.assertEqual("rolled_back", state.state)
        self.assertEqual(CURRENT_IMAGE, state.current_image)

    def test_reconciliation_state_write_failure_is_not_downgraded(self):
        responses = [
            running_container_inspect(TARGET_IMAGE_ID, "healthy"),
        ]
        core, _, _, _ = self.new_activation_core(responses)
        self.save_activating_state(core)
        state_write_error = UpdaterError(
            "STATE_WRITE_FAILED",
            "failed to persist update state",
        )

        with mock.patch.object(
            core,
            "save_state",
            side_effect=[state_write_error, None],
        ):
            with self.assertRaises(UpdaterError) as raised:
                core.reconcile_state()

        self.assertIs(state_write_error, raised.exception)

    def test_stale_preparing_state_reconciles_to_failed(self):
        core, runner = self.new_core([])
        core.save_state(
            UpdateState(
                state="preparing",
                current_image=CURRENT_IMAGE,
                target_image=TARGET_IMAGE,
                previous_image=CURRENT_IMAGE,
                message="preparing target image",
                updated_at="2026-07-10T00:00:00Z",
            )
        )

        state = core.reconcile_state()

        self.assertEqual("failed", state.state)
        self.assertEqual(
            "interrupted image preparation requires retry",
            state.message,
        )
        self.assertEqual([], runner.calls)

    def test_error_state_does_not_include_environment_secrets(self):
        core, runner, _, health_checker = self.new_activation_core(
            successful_activation_preflight_responses()
            + [
                completed(),
                running_container_inspect(PREVIOUS_IMAGE_ID, "healthy"),
            ]
        )
        self.save_prepared_state(core)
        core.begin_activation()
        real_replace = os.replace
        environment_replace_attempts = 0

        def fail_first_environment_replace(source, destination):
            nonlocal environment_replace_attempts
            if Path(destination) == self.environment_file:
                environment_replace_attempts += 1
                if environment_replace_attempts == 1:
                    raise OSError(
                        f"{self.environment_file}: {original_env!r}"
                    )
            return real_replace(source, destination)

        with mock.patch(
            "deploy.updater.updater_core.os.replace",
            side_effect=fail_first_environment_replace,
        ):
            state = core.run_activation()

        self.assertEqual("rolled_back", state.state)
        self.assertEqual(original_env, self.environment_file.read_bytes())
        self.assertEqual(
            activation_preflight_calls(30)
            + [
                (COMPOSE_COMMAND, 120),
                (["docker", "inspect", "sub2api"], 30),
            ],
            runner.calls,
        )
        self.assertEqual(1, len(health_checker.calls))
        state_text = Path(self.config.state_file).read_text(encoding="utf-8")
        for secret in [
            "POSTGRES_PASSWORD",
            "JWT_SECRET",
            "do-not-leak",
            "also-do-not-leak",
            str(self.environment_file),
        ]:
            self.assertNotIn(secret, state.message)
            self.assertNotIn(secret, state_text)

        with mock.patch(
            "deploy.updater.updater_core.os.replace",
            side_effect=OSError(
                f"{self.environment_file}: POSTGRES_PASSWORD=do-not-leak"
            ),
        ):
            with self.assertRaises(UpdaterError) as raised:
                replace_sub2api_image(self.environment_file, TARGET_IMAGE)

        self.assertEqual("ENVIRONMENT_WRITE_FAILED", raised.exception.code)
        self.assertEqual(
            "failed to update deployment environment",
            raised.exception.message,
        )
        self.assertNotIn("POSTGRES_PASSWORD", raised.exception.message)
        self.assertNotIn("do-not-leak", raised.exception.message)

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

    def test_activate_holds_operation_lock_across_begin_run_boundary(self):
        core, runner, _, health_checker = self.new_activation_core(
            successful_activation_preflight_responses()
            + [
                completed(),
                running_container_inspect(TARGET_IMAGE_ID, "healthy"),
            ]
        )
        self.save_prepared_state(core)
        boundary_reached = threading.Event()
        release_boundary = threading.Event()
        activation_result = {}

        if hasattr(core, "_begin_activation"):
            begin_method_name = "_begin_activation"
            real_begin = core._begin_activation
        else:
            begin_method_name = "begin_activation"
            real_begin = core.begin_activation

        def blocking_begin():
            state = real_begin()
            boundary_reached.set()
            if not release_boundary.wait(2):
                raise AssertionError("timed out at activation boundary")
            return state

        def run_activate():
            try:
                activation_result["state"] = core.activate()
            except BaseException as error:
                activation_result["error"] = error

        activation_thread = threading.Thread(target=run_activate)
        with mock.patch.object(
            core,
            begin_method_name,
            side_effect=blocking_begin,
        ):
            activation_thread.start()
            try:
                self.assertTrue(boundary_reached.wait(2))
                boundary_state = core.load_state()
                self.assertEqual("activating", boundary_state.state)

                with self.assertRaises(UpdaterError) as raised:
                    core.reconcile_state()

                self.assertEqual("AGENT_BUSY", raised.exception.code)
                self.assertEqual(boundary_state, core.load_state())
            finally:
                release_boundary.set()
                activation_thread.join(2)

        self.assertFalse(activation_thread.is_alive())
        self.assertNotIn("error", activation_result)
        self.assertEqual("healthy", activation_result["state"].state)
        self.assertEqual(
            activation_preflight_calls(30)
            + [
                (COMPOSE_COMMAND, 60),
                (["docker", "inspect", "sub2api"], 30),
            ],
            runner.calls,
        )
        self.assertEqual(1, len(health_checker.calls))

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

    def test_load_state_accepts_legacy_six_field_state(self):
        state_path = Path(self.config.state_file)
        state_path.write_text(
            json.dumps(
                {
                    "state": "prepared",
                    "current_image": CURRENT_IMAGE,
                    "target_image": TARGET_IMAGE,
                    "previous_image": CURRENT_IMAGE,
                    "message": "target image prepared",
                    "updated_at": "2026-07-10T00:00:00Z",
                }
            ),
            encoding="utf-8",
        )
        core, runner = self.new_core([])

        state = core.load_state()

        self.assertEqual("prepared", state.state)
        self.assertEqual("", getattr(state, "target_image_id", None))
        self.assertEqual("", getattr(state, "previous_image_id", None))
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
        core, runner = self.new_core([])

        invalid_states = [
            {
                "state": "prepared",
                "unexpected": "JWT_SECRET=do-not-leak",
            },
            {
                "state": "activating",
                "target_image_id": 42,
                "previous_image_id": PREVIOUS_IMAGE_ID,
            },
        ]
        for invalid_state in invalid_states:
            with self.subTest(invalid_state=invalid_state):
                state_path.write_text(
                    json.dumps(invalid_state),
                    encoding="utf-8",
                )
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
