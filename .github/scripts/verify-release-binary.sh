#!/usr/bin/env bash
set -euo pipefail

binary="${1:?缺少构建产物路径}"
commit="${2:?缺少源码提交}"
target_os="${3:?缺少目标操作系统}"
target_arch="${4:?缺少目标架构}"

fail() {
  printf '发布二进制验收失败：%s\n' "$1" >&2
  exit 1
}

[[ "$commit" =~ ^[0-9a-f]{40}$ ]] || fail '提交必须是完整 SHA'
root="$(git rev-parse --show-toplevel)"
[[ "$(git rev-parse HEAD)" == "$commit" ]] || fail '当前源码提交不匹配'

# GoReleaser 的初始 Git 检查早于 before hook；此处必须检查构建后的真实状态。
status="$(git status --porcelain)"
[[ -z "$status" ]] || fail "构建改变了工作树：$status"
info="$(go version -m "$binary")"
go_version="$(awk '$1 == "go" { print $2; exit }' "$root/backend/go.mod")"
grpc_version="$(awk '$1 == "google.golang.org/grpc" { print $2; exit }' "$root/backend/go.mod")"
[[ -n "$go_version" && -n "$grpc_version" ]] || fail '源码缺少工具链或 gRPC 版本'
[[ "${info%%$'\n'*}" == *": go$go_version" ]] || fail 'Go 版本不匹配'
[[ "$(awk -F '\t' '$2 == "dep" && $3 == "google.golang.org/grpc" { print $4 }' <<< "$info")" == "$grpc_version" ]] || fail 'gRPC 版本不匹配'

require_build_setting() {
  grep -Fxq -- $'\tbuild\t'"$1=$2" <<< "$info" || fail "构建字段不匹配：$1"
}

require_build_setting vcs.revision "$commit"
require_build_setting vcs.modified false
require_build_setting GOOS "$target_os"
require_build_setting GOARCH "$target_arch"
require_build_setting CGO_ENABLED 0
require_build_setting -tags embed
grep -Eq "(^|[[:space:]])main.Commit=$commit([[:space:]]|\")" <<< "$info" || fail '应用提交不匹配'

# 每个目标都经过 post hook 后才进入归档和发布，任何失败都由 GoReleaser 中止。
printf '发布二进制验收通过：%s/%s %s\n' "$target_os" "$target_arch" "$commit"
