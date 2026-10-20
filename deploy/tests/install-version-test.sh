#!/usr/bin/env bash
set -euo pipefail

root="$(cd -- "$(dirname -- "$0")/../.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf -- "$fixture"' EXIT
INSTALL_DIR="$fixture/installed"
mkdir -p "$INSTALL_DIR" "$fixture/payload"
SERVICE_USER=fixture
GITHUB_REPO=gwenliu1025/sub2api
OS=linux
ARCH=amd64
if ! command -v sha256sum >/dev/null 2>&1; then
  # macOS CI 使用系统 shasum 模拟安装器调用的同一 SHA-256 输出契约。
  sha256sum() { shasum -a 256 "$@"; }
fi

# 只加载正式函数，避免安装器 main、权限检查或真实服务动作。
load_function() {
  eval "$(awk -v name="$1" '$0 == name "() {" { active=1 } active { print } active && /^}/ { exit }' "$root/deploy/install.sh")"
}
for name in get_current_version upgrade install_version download_and_extract; do
  load_function "$name"
done
msg() { printf '%s' "$1"; }
print_info() { :; }
print_success() { :; }
print_warning() { :; }
print_error() { printf '%s\n' "$*" >&2; }
systemctl() { [[ "$1" != is-active ]]; }
chown() { :; }
validate_version() { printf '%s\n' "$1"; }
get_latest_version() { LATEST_VERSION=v0.2.17.2; }

write_binary() {
  printf '#!/usr/bin/env bash\nprintf "Sub2API %s (commit: fixture, built: fixture)\\n"\n' "$1" > "$INSTALL_DIR/sub2api"
  chmod +x "$INSTALL_DIR/sub2api"
}

# 执行实际读取函数和两个升级函数；下载暂以记录版本的桩隔离。
download_and_extract() { printf '%s' "$LATEST_VERSION" > "$fixture/requested-version"; }
for version in 0.2.17 0.2.17.1 0.2.17.12; do
  write_binary "$version"
  [[ "$(get_current_version)" == "$version" ]]
  install_version v0.2.17.2 >/dev/null
  test -f "$INSTALL_DIR/sub2api.backup.$version"
  cmp "$INSTALL_DIR/sub2api" "$INSTALL_DIR/sub2api.backup.$version"
  [[ "$(cat "$fixture/requested-version")" == v0.2.17.2 ]]
  upgrade >/dev/null
  [[ "$CURRENT_VERSION" == "$version" ]]
done

# 真正运行下载/解包函数，网络桩只回传本地归档和校验和并核对完整资产名。
load_function download_and_extract
write_binary 0.2.17.2
cp "$INSTALL_DIR/sub2api" "$fixture/payload/sub2api"
asset=sub2api_0.2.17.2_linux_amd64.tar.gz
tar -czf "$fixture/$asset" -C "$fixture/payload" sub2api
sha256sum "$fixture/$asset" | awk -v asset="$asset" '{print $1 "  " asset}' > "$fixture/checksums.txt"
curl() {
  local url="$2" output="$4"
  case "$url" in
    "https://github.com/$GITHUB_REPO/releases/download/v0.2.17.2/$asset") cp "$fixture/$asset" "$output" ;;
    "https://github.com/$GITHUB_REPO/releases/download/v0.2.17.2/checksums.txt") cp "$fixture/checksums.txt" "$output" ;;
    *) printf '资产 URL 未保留完整版本：%s\n' "$url" >&2; return 1 ;;
  esac
}
LATEST_VERSION=v0.2.17.2
# 原函数有自己的临时目录 trap，放入子进程保留测试清理 owner。
( download_and_extract )
[[ "$(get_current_version)" == 0.2.17.2 ]]
printf '安装器三段/四段读取、升级备份及资产名回归通过\n'
