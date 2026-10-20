#!/usr/bin/env python3
"""三段基线与四段修复共用的正式构建、归档和发布入口。"""

import argparse
from datetime import datetime, timezone
import hashlib
import io
import os
from pathlib import Path
import re
import stat
import subprocess
import sys
import tarfile
import zipfile

TARGETS = (("linux", "amd64"), ("linux", "arm64"),
           ("darwin", "amd64"), ("darwin", "arm64"), ("windows", "amd64"))


def run(root, args, *, env=None, timeout=300):
    return subprocess.run(args, cwd=root, env=env, check=True, text=True,
                          encoding="utf-8", stdout=subprocess.PIPE, timeout=timeout).stdout.strip()


def source_identity(root, tag):
    version = (root / "backend/cmd/server/VERSION").read_text(encoding="utf-8").strip()
    run(root, ["bash", ".github/scripts/validate-release-version.sh", tag, version])
    commit = run(root, ["git", "rev-parse", "HEAD"])
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        raise ValueError("构建来源必须为完整提交 SHA")
    if run(root, ["git", "rev-parse", f"refs/tags/{tag}^{{commit}}"] ) != commit:
        raise ValueError("精确标签未指向当前提交")
    if run(root, ["git", "status", "--porcelain"]):
        raise ValueError("工作树不清洁，停止发布")
    if run(root, ["git", "show", "HEAD:backend/cmd/server/VERSION"]) != version:
        raise ValueError("版本值未提交")
    return version, commit


def bundled_files(root):
    # 沿用 LICENSE*、README*、deploy/* 随包范围，只接受已提交的普通文件。
    paths = run(root, ["git", "ls-files", "--stage", "-z", "--", "LICENSE*", "README*", "deploy/*"])
    files = {}
    modes = {}
    for record in paths.rstrip("\0").split("\0"):
        header, name = record.split("\t", 1)
        mode, _, stage = header.split()
        path = root / name
        if mode not in ("100644", "100755") or stage != "0" or not name or path.is_symlink() or not path.is_file():
            raise ValueError(f"随包成员不是普通文件：{name}")
        files[name] = path.read_bytes()
        modes[name] = int(mode, 8) & 0o777
    if not any(n.startswith("LICENSE") for n in files) or not any(n.startswith("README") for n in files) or not any(n.startswith("deploy/") for n in files):
        raise ValueError("随包文件缺少许可证、说明或部署文件")
    return files, modes


def verify_archive(path, expected, binary_name, modes=None):
    modes = modes or {name: 0o755 if name == binary_name else 0o644 for name in expected}
    if path.suffix == ".zip":
        with zipfile.ZipFile(path) as archive:
            names = archive.namelist()
            if len(names) != len(set(names)) or set(names) != set(expected):
                raise ValueError("ZIP 成员与构建来源不符")
            if not all(stat.S_ISREG(info.external_attr >> 16) for info in archive.infolist()):
                raise ValueError("ZIP 成员必须为普通文件")
            if any((archive.getinfo(name).external_attr >> 16) & 0o777 != mode for name, mode in modes.items()):
                raise ValueError("ZIP 文件权限与来源不符")
            actual = {name: archive.read(name) for name in names}
    else:
        with tarfile.open(path, "r:gz") as archive:
            members = archive.getmembers()
            names = [m.name for m in members]
            if len(names) != len(set(names)) or set(names) != set(expected) or not all(m.isfile() for m in members):
                raise ValueError("tar 成员与构建来源不符")
            if archive.getmember(binary_name).mode != 0o755:
                raise ValueError("Unix 二进制权限必须为 0755")
            if any(archive.getmember(name).mode != mode for name, mode in modes.items()):
                raise ValueError("tar 文件权限与来源不符")
            actual = {m.name: archive.extractfile(m).read() for m in members}
    for name, data in expected.items():
        if hashlib.sha256(actual[name]).digest() != hashlib.sha256(data).digest():
            raise ValueError(f"归档字节与构建来源不符：{name}")


def pack(path, files, binary_name, modes=None):
    # 显式写普通文件及 Unix 权限，不继承 Windows 宿主机缺少执行位的 stat。
    modes = modes or {name: 0o755 if name == binary_name else 0o644 for name in files}
    if path.suffix == ".zip":
        with zipfile.ZipFile(path, "x", compression=zipfile.ZIP_DEFLATED) as archive:
            for name, data in sorted(files.items()):
                info = zipfile.ZipInfo(name)
                info.create_system = 3
                info.external_attr = (stat.S_IFREG | modes[name]) << 16
                info.compress_type = zipfile.ZIP_DEFLATED
                archive.writestr(info, data)
    else:
        with tarfile.open(path, "x:gz") as archive:
            for name, data in sorted(files.items()):
                info = tarfile.TarInfo(name)
                info.size = len(data)
                info.mode = modes[name]
                archive.addfile(info, io.BytesIO(data))
    verify_archive(path, files, binary_name, modes)


def build(root, tag, output):
    version, commit = source_identity(root, tag)
    # 已有目录视为已有验收产物，绝不清理或覆盖。
    output.mkdir(parents=True, exist_ok=False)
    run(root, ["go", "-C", "backend", "mod", "verify"])
    if not (root / "backend/internal/web/dist/index.html").is_file():
        raise ValueError("缺少真实前端构建结果，停止嵌入发布")
    files, modes = bundled_files(root)
    date = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    verified = []
    for target_os, arch in TARGETS:
        binary_name = "sub2api.exe" if target_os == "windows" else "sub2api"
        binary = output / f"{target_os}_{arch}" / binary_name
        binary.parent.mkdir()
        env = os.environ.copy()
        env.update(CGO_ENABLED="0", GOOS=target_os, GOARCH=arch)
        run(root, ["go", "-C", "backend", "build", "-tags=embed", "-mod=readonly",
                   f"-ldflags=-s -w -X main.Commit={commit} -X main.Date={date} -X main.BuildType=release",
                   "-o", str(binary), "./cmd/server"], env=env, timeout=1800)
        run(root, ["bash", ".github/scripts/verify-release-binary.sh", str(binary), commit, target_os, arch])
        verified.append((target_os, arch, binary_name, binary))
    # 五个平台全部通过来源门禁后才开始归档，避免半套产物被误作可发布资产。
    assets = []
    for target_os, arch, binary_name, binary in verified:
        suffix = "zip" if target_os == "windows" else "tar.gz"
        archive = output / f"sub2api_{version}_{target_os}_{arch}.{suffix}"
        pack(archive, {**files, binary_name: binary.read_bytes()}, binary_name, {**modes, binary_name: 0o755})
        assets.append(archive)
    if source_identity(root, tag) != (version, commit):
        raise ValueError("构建期间来源发生变化")
    checksums = output / "checksums.txt"
    content = "".join(f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n" for p in assets)
    with checksums.open("x", encoding="utf-8", newline="\n") as handle:
        handle.write(content)
    if checksums.read_text(encoding="utf-8") != content:
        raise ValueError("校验和文件读回不一致")
    return version, commit, assets + [checksums]


def remote_preflight(root, tag, version, commit, repository, *, check_image):
    """所有远端写入前共用只读核验；仅明确不存在才允许继续。"""
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
        raise ValueError("发布仓库必须为 owner/name")
    if source_identity(root, tag) != (version, commit):
        raise ValueError("发布前来源发生变化")
    # --verify-tag 禁止自动建标签；另核对远端 peeled commit，拒绝同名不同来源。
    refs = run(root, ["git", "ls-remote", "--tags", f"https://github.com/{repository}.git",
                      f"refs/tags/{tag}", f"refs/tags/{tag}^{{}}"], timeout=60)
    refs = dict(line.split()[::-1] for line in refs.splitlines())
    if refs.get(f"refs/tags/{tag}^{{}}", refs.get(f"refs/tags/{tag}")) != commit:
        raise ValueError("远端精确标签与已验收提交不符")
    existing = subprocess.run(["gh", "api", f"repos/{repository}/releases/tags/{tag}"],
                              cwd=root, text=True, encoding="utf-8", capture_output=True, timeout=60)
    if existing.returncode == 0:
        raise ValueError("该精确版本已有 Release，不覆盖或追加资产")
    if "HTTP 404" not in existing.stderr:
        raise RuntimeError("Release 预检结果未知，停止写入")
    if check_image:
        image = f"ghcr.io/{repository.split('/')[0].lower()}/sub2api:{version}"
        result = subprocess.run(["docker", "manifest", "inspect", image], cwd=root,
                                text=True, encoding="utf-8", capture_output=True, timeout=60)
        if result.returncode == 0:
            raise ValueError("该精确版本已有 GHCR 镜像，不覆盖")
        if result.stderr.strip() not in (f"no such manifest: {image}", "manifest unknown", "manifest unknown: manifest unknown"):
            raise RuntimeError("GHCR 精确镜像预检结果未知，停止写入")


def publish(root, tag, version, commit, assets, repository):
    # GHCR 写入前已由工作流调用相同预检；这里再次核验 Release，不覆盖已有资产。
    remote_preflight(root, tag, version, commit, repository, check_image=False)
    message = os.environ.get("TAG_MESSAGE")
    if message is None:
        message = run(root, ["git", "tag", "-l", "--format=%(contents:body)", tag])
    owner = repository.split("/")[0].lower()
    notes = (f"> AI API 网关平台\n\n{message}\n\n---\n\n## Docker\n\n```bash\n"
             f"docker pull ghcr.io/{owner}/sub2api:{version}\n```\n\n## 文档\n\n"
             f"- [GitHub 仓库](https://github.com/{repository})\n"
             f"- [安装指南](https://github.com/{repository}/blob/main/deploy/README.md)\n")
    notes_file = assets[-1].parent / "release-notes.md"
    with notes_file.open("x", encoding="utf-8", newline="\n") as handle:
        handle.write(notes)
    try:
        run(root, ["gh", "release", "create", tag, *map(str, assets), "--repo", repository,
                   "--verify-tag", "--latest", "--title", f"Sub2API {version}", "--notes-file", str(notes_file)])
    except (subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        # 接受状态不明时仅读回一次，不重发 create 或覆盖资产。
        try:
            subprocess.run(["gh", "release", "view", tag, "--repo", repository,
                            "--json", "tagName,url,assets"], cwd=root, check=False, timeout=60)
        except subprocess.TimeoutExpired:
            print("Release 读回也超时，保留未知接受状态，未重试写入", file=sys.stderr)
        raise RuntimeError("Release 创建失败或接受状态未知，已停止；请核对读回结果") from error


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--output", default="dist")
    parser.add_argument("--repository", default=os.environ.get("GITHUB_REPOSITORY", ""))
    stage = parser.add_mutually_exclusive_group()
    stage.add_argument("--publish", action="store_true", help="验收五包后显式创建正式 Release")
    stage.add_argument("--preflight-only", action="store_true", help="写入 GHCR 前只读核验精确标签、Release 和镜像")
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[2]
    output = Path(args.output).resolve()
    try:
        if args.preflight_only:
            version, commit = source_identity(root, args.tag)
            remote_preflight(root, args.tag, version, commit, args.repository, check_image=True)
            print(f"远端写前预检通过：{args.tag} {commit}")
            return 0
        version, commit, assets = build(root, args.tag, output)
        if args.publish:
            publish(root, args.tag, version, commit, assets, args.repository)
        print(f"构建和归档验收通过：{version} {commit}，共 {len(assets)} 项资产；发布={args.publish}")
    except (OSError, ValueError, RuntimeError, subprocess.CalledProcessError, subprocess.TimeoutExpired) as error:
        print(f"发布执行停止：{error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
