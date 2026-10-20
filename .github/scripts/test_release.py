#!/usr/bin/env python3
"""不调用真实构建或发布服务的执行器回归，CI 与本地使用同一入口。"""

import importlib.util
import hashlib
import os
from pathlib import Path
import subprocess
import stat
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch
import zipfile

spec = importlib.util.spec_from_file_location("release", Path(__file__).with_name("release.py"))
release = importlib.util.module_from_spec(spec)
spec.loader.exec_module(release)
SHA = "1" * 40


class ReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        for name, data in {
            "backend/cmd/server/VERSION": b"0.2.17.1\n",
            "backend/internal/web/dist/index.html": b"<html>release</html>",
            "LICENSE": b"license", "README.md": b"readme", "deploy/install.sh": b"installer",
        }.items():
            path = self.root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(data)
        self.calls = []

    def fake_run(self, root, args, *, env=None, timeout=300):
        self.calls.append((args, env))
        if args == ["git", "rev-parse", "HEAD"] or args[:2] == ["git", "rev-parse"]:
            return SHA
        if args[:2] == ["git", "show"]:
            return "0.2.17.1"
        if args[:2] == ["git", "ls-files"]:
            return f"100644 {SHA} 0\tLICENSE\0" + f"100644 {SHA} 0\tREADME.md\0" + f"100755 {SHA} 0\tdeploy/install.sh\0"
        if args[:3] == ["git", "ls-remote", "--tags"]:
            return f"{'2' * 40}\trefs/tags/v0.2.17.1\n{SHA}\trefs/tags/v0.2.17.1^{{}}"
        if args[:4] == ["go", "-C", "backend", "build"]:
            Path(args[args.index("-o") + 1]).write_bytes(f"binary:{env['GOOS']}/{env['GOARCH']}".encode())
        return ""

    def test_source_identity_rejects_dirty_wrong_tag_and_uncommitted_version(self):
        for args_to_break, wrong in [
            (["git", "status", "--porcelain"], " M backend/go.sum"),
            (["git", "rev-parse", "refs/tags/v0.2.17.1^{commit}"], "2" * 40),
            (["git", "show", "HEAD:backend/cmd/server/VERSION"], "0.2.17"),
            (["git", "rev-parse", "HEAD"], "1234567"),
        ]:
            with self.subTest(command=args_to_break):
                def fake(root, args, **kwargs):
                    return wrong if args == args_to_break else self.fake_run(root, args, **kwargs)
                with patch.object(release, "run", side_effect=fake), self.assertRaises(ValueError):
                    release.source_identity(self.root, "v0.2.17.1")

    def test_five_archives_full_commit_build_gates_and_checksums(self):
        with patch.object(release, "run", side_effect=self.fake_run):
            version, commit, assets = release.build(self.root, "v0.2.17.1", self.root / "out")
        self.assertEqual((version, commit), ("0.2.17.1", SHA))
        expected_targets = {("linux", "amd64"), ("linux", "arm64"), ("darwin", "amd64"), ("darwin", "arm64"), ("windows", "amd64")}
        self.assertEqual(set(release.TARGETS), expected_targets)
        self.assertEqual(len(assets), 6)
        self.assertEqual(len(list((self.root / "out").glob("*.tar.gz"))), 4)
        self.assertEqual(len(list((self.root / "out").glob("*.zip"))), 1)
        builds = [(args, env) for args, env in self.calls if args[:4] == ["go", "-C", "backend", "build"]]
        gates = [args for args, _ in self.calls if args[:2] == ["bash", ".github/scripts/verify-release-binary.sh"]]
        self.assertEqual(len(builds), 5)
        self.assertEqual(len(gates), 5)
        self.assertIn((["go", "-C", "backend", "mod", "verify"], None), self.calls)
        for (args, env), gate in zip(builds, gates):
            self.assertIn("-mod=readonly", args)
            self.assertIn("-tags=embed", args)
            self.assertIn(f"main.Commit={SHA}", " ".join(args))
            self.assertNotIn("-buildvcs=false", " ".join(args))
            self.assertEqual(env["CGO_ENABLED"], "0")
            self.assertEqual(gate[2:], [args[args.index("-o") + 1], SHA, env["GOOS"], env["GOARCH"]])
            self.assertLess(self.calls.index((args, env)), self.calls.index((gate, None)))
        for archive in assets[:-1]:
            binary_name = "sub2api.exe" if archive.suffix == ".zip" else "sub2api"
            if archive.suffix == ".zip":
                with zipfile.ZipFile(archive) as opened:
                    self.assertEqual(set(opened.namelist()), {binary_name, "LICENSE", "README.md", "deploy/install.sh"})
                    self.assertTrue(opened.read(binary_name).startswith(b"binary:"))
                    self.assertEqual((opened.getinfo("deploy/install.sh").external_attr >> 16) & 0o777, 0o755)
            else:
                with tarfile.open(archive) as opened:
                    self.assertEqual(opened.getmember(binary_name).mode, 0o755)
                    self.assertTrue(opened.getmember(binary_name).isfile())
                    self.assertEqual(opened.getmember("deploy/install.sh").mode, 0o755)
                    self.assertEqual(opened.extractfile("deploy/install.sh").read(), b"installer")
        expected = "".join(f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n" for p in assets[:-1])
        self.assertEqual(assets[-1].read_text(), expected)

    def test_existing_output_is_preserved(self):
        out = self.root / "out"
        out.mkdir()
        sentinel = out / "old.tar.gz"
        sentinel.write_bytes(b"keep")
        with patch.object(release, "run", side_effect=self.fake_run), self.assertRaises(FileExistsError):
            release.build(self.root, "v0.2.17.1", out)
        self.assertEqual(sentinel.read_bytes(), b"keep")
        self.assertFalse(any(args[0] == "go" for args, _ in self.calls))

    def test_dependency_and_binary_gate_failures_stop_before_archives(self):
        for failure in (["go", "-C", "backend", "mod", "verify"], ["bash", ".github/scripts/verify-release-binary.sh"]):
            with self.subTest(command=failure):
                def fake(root, args, **kwargs):
                    if args[:len(failure)] == failure:
                        raise subprocess.CalledProcessError(1, args)
                    return self.fake_run(root, args, **kwargs)
                output = self.root / ("mod-fail" if failure[0] == "go" else "gate-fail")
                with patch.object(release, "run", side_effect=fake), self.assertRaises(subprocess.CalledProcessError):
                    release.build(self.root, "v0.2.17.1", output)
                self.assertFalse(list(output.glob("*.tar.gz")))
                self.assertFalse((output / "checksums.txt").exists())

    def test_missing_frontend_stops(self):
        (self.root / "backend/internal/web/dist/index.html").unlink()
        with patch.object(release, "run", side_effect=self.fake_run), self.assertRaises(ValueError):
            release.build(self.root, "v0.2.17.1", self.root / "no-ui")

    def test_last_target_failure_still_produces_no_archive(self):
        def fake(root, args, **kwargs):
            if args[:2] == ["bash", ".github/scripts/verify-release-binary.sh"] and args[-2:] == ["windows", "amd64"]:
                raise subprocess.CalledProcessError(1, args)
            return self.fake_run(root, args, **kwargs)
        output = self.root / "last-fail"
        with patch.object(release, "run", side_effect=fake), self.assertRaises(subprocess.CalledProcessError):
            release.build(self.root, "v0.2.17.1", output)
        self.assertEqual(len([args for args, _ in self.calls if args[:4] == ["go", "-C", "backend", "build"]]), 5)
        self.assertFalse(list(output.glob("*.tar.gz")))
        self.assertFalse(list(output.glob("*.zip")))

    def test_zip_symlink_and_wrong_tar_permissions_are_rejected(self):
        expected = {"sub2api": b"binary"}
        archive = self.root / "symlink.zip"
        with zipfile.ZipFile(archive, "x") as opened:
            member = zipfile.ZipInfo("sub2api")
            member.create_system = 3
            member.external_attr = (stat.S_IFLNK | 0o755) << 16
            opened.writestr(member, b"binary")
        with self.assertRaisesRegex(ValueError, "普通文件"):
            release.verify_archive(archive, expected, "sub2api")
        archive = self.root / "mode.tar.gz"
        with self.assertRaisesRegex(ValueError, "0755"):
            release.pack(archive, expected, "sub2api", {"sub2api": 0o644})

    def test_bundled_git_symlink_mode_is_rejected(self):
        with patch.object(release, "run", return_value=f"120000 {SHA} 0\tLICENSE\0"), self.assertRaises(ValueError):
            release.bundled_files(self.root)

    def test_archive_byte_and_member_tampering_is_rejected(self):
        expected = {"sub2api": b"binary", "LICENSE": b"license"}
        for suffix in ("tar.gz", "zip"):
            for mode in ("bytes", "member"):
                with self.subTest(suffix=suffix, mode=mode):
                    archive = self.root / f"{mode}.{suffix}"
                    actual = dict(expected)
                    actual["sub2api" if mode == "bytes" else "extra"] = b"changed"
                    release.pack(archive, actual, "sub2api")
                    with self.assertRaises(ValueError):
                        release.verify_archive(archive, expected, "sub2api")

    def test_publish_only_verified_tag_no_overwrite_and_original_notes(self):
        assets = [self.root / "checksums.txt"]
        with patch.object(release, "run", side_effect=self.fake_run), patch.object(release.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "HTTP 404")), patch.dict(os.environ, {"TAG_MESSAGE": "本轮修复"}):
            release.publish(self.root, "v0.2.17.1", "0.2.17.1", SHA, assets, "Owner/sub2api")
        create = [args for args, _ in self.calls if args[:3] == ["gh", "release", "create"]]
        self.assertEqual(len(create), 1)
        self.assertIn("--verify-tag", create[0])
        self.assertIn("--latest", create[0])
        self.assertNotIn("--clobber", create[0])
        notes = (self.root / "release-notes.md").read_text(encoding="utf-8")
        for value in ("本轮修复", "> AI API 网关平台", "## 文档", "ghcr.io/owner/sub2api:0.2.17.1", "https://github.com/Owner/sub2api/blob/main/deploy/README.md"):
            self.assertIn(value, notes)

    def test_existing_or_unknown_release_and_wrong_remote_tag_stop(self):
        for code, stderr in ((0, ""), (1, "HTTP 403"), (1, "network unknown")):
            with self.subTest(status=stderr):
                self.calls.clear()
                with patch.object(release, "run", side_effect=self.fake_run), patch.object(release.subprocess, "run", return_value=subprocess.CompletedProcess([], code, "", stderr)), self.assertRaises((ValueError, RuntimeError)):
                    release.publish(self.root, "v0.2.17.1", "0.2.17.1", SHA, [], "owner/sub2api")
                self.assertFalse(any(args[:3] == ["gh", "release", "create"] for args, _ in self.calls))
        with patch.object(release, "source_identity", return_value=("0.2.17.1", SHA)), patch.object(release, "run", return_value=""), self.assertRaises(ValueError):
            release.publish(self.root, "v0.2.17.1", "0.2.17.1", SHA, [], "owner/sub2api")

    def test_image_preflight_requires_explicit_absence(self):
        image = "ghcr.io/owner/sub2api:0.2.17.1"
        for code, stderr, accepts in ((1, f"no such manifest: {image}", True),
                                       (1, "manifest unknown: manifest unknown", True),
                                       (0, "", False), (1, "unauthorized", False),
                                       (1, "network timeout", False), (1, "HTTP 500", False),
                                       (1, "no such manifest: other/image:tag", False)):
            with self.subTest(stderr=stderr):
                responses = [subprocess.CompletedProcess([], 1, "", "HTTP 404"), subprocess.CompletedProcess([], code, "", stderr)]
                with patch.object(release, "run", side_effect=self.fake_run), patch.object(release.subprocess, "run", side_effect=responses) as raw:
                    if accepts:
                        release.remote_preflight(self.root, "v0.2.17.1", "0.2.17.1", SHA, "owner/sub2api", check_image=True)
                    else:
                        with self.assertRaises((ValueError, RuntimeError)):
                            release.remote_preflight(self.root, "v0.2.17.1", "0.2.17.1", SHA, "owner/sub2api", check_image=True)
                    self.assertEqual(raw.call_count, 2)
                    self.assertEqual(raw.call_args.args[0], ["docker", "manifest", "inspect", image])

    def test_preflight_cli_has_no_build_or_release_write(self):
        argv = ["release.py", "--tag", "v0.2.17.1", "--repository", "owner/sub2api", "--preflight-only"]
        with patch.object(sys, "argv", argv), patch.object(release, "source_identity", return_value=("0.2.17.1", SHA)), patch.object(release, "remote_preflight") as preflight, patch.object(release, "build") as build, patch.object(release, "publish") as writer:
            self.assertEqual(release.main(), 0)
            self.assertTrue(preflight.call_args.kwargs["check_image"])
            build.assert_not_called()
            writer.assert_not_called()

    def test_unknown_create_acceptance_is_read_once_without_retry(self):
        def fake(root, args, **kwargs):
            if args[:3] == ["gh", "release", "create"]:
                raise subprocess.CalledProcessError(1, args)
            return self.fake_run(root, args, **kwargs)
        with patch.object(release, "run", side_effect=fake), patch.object(release.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "", "HTTP 404")) as raw, self.assertRaises(RuntimeError):
            release.publish(self.root, "v0.2.17.1", "0.2.17.1", SHA, [self.root / "checksums.txt"], "owner/sub2api")
        self.assertEqual(raw.call_count, 2)
        self.assertEqual(raw.call_args_list[-1].args[0][:3], ["gh", "release", "view"])

    def test_create_timeout_and_readback_timeout_do_not_retry(self):
        def fake(root, args, **kwargs):
            if args[:3] == ["gh", "release", "create"]:
                raise subprocess.TimeoutExpired(args, 300)
            return self.fake_run(root, args, **kwargs)
        with patch.object(release, "run", side_effect=fake), patch.object(release.subprocess, "run", side_effect=[subprocess.CompletedProcess([], 1, "", "HTTP 404"), subprocess.TimeoutExpired("readback", 60)]) as raw, self.assertRaises(RuntimeError):
            release.publish(self.root, "v0.2.17.1", "0.2.17.1", SHA, [self.root / "checksums.txt"], "owner/sub2api")
        self.assertEqual(raw.call_count, 2)
        self.assertEqual(raw.call_args_list[-1].kwargs["timeout"], 60)

    def test_command_deadline_and_explicit_publish_switch(self):
        with patch.object(release.subprocess, "run", return_value=subprocess.CompletedProcess([], 0, "ok")) as command:
            self.assertEqual(release.run(self.root, ["git", "status"], timeout=17), "ok")
            self.assertEqual(command.call_args.kwargs["timeout"], 17)
        for publish in (False, True):
            argv = ["release.py", "--tag", "v0.2.17.1", "--output", str(self.root / "out")]
            if publish:
                argv.append("--publish")
            with self.subTest(publish=publish), patch.object(sys, "argv", argv), patch.object(release, "build", return_value=("0.2.17.1", SHA, [])) as build, patch.object(release, "publish") as writer:
                self.assertEqual(release.main(), 0)
                self.assertEqual(build.call_count, 1)
                self.assertEqual(writer.call_count, int(publish))


if __name__ == "__main__":
    unittest.main(verbosity=2)
