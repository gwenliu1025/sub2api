#!/bin/sh
set -eu

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repo_root"

fail() {
  printf 'docker runtime resources test failed: %s\n' "$1" >&2
  exit 1
}

assert_line() {
  file=$1
  line=$2
  grep -Fqx "$line" "$file" || fail "$file is missing: $line"
}

assert_count() {
  file=$1
  line=$2
  expected=$3
  actual=$(grep -Fxc "$line" "$file" || true)
  [ "$actual" -eq "$expected" ] || fail "$file has $actual occurrences of '$line', expected $expected"
}

assert_absent() {
  file=$1
  text=$2
  if grep -Fq "$text" "$file"; then
    fail "$file contains obsolete content: $text"
  fi
}

test -s backend/resources/model-pricing/model_prices_and_context_window.json || \
  fail 'fallback pricing data is missing or empty'

assert_line Dockerfile 'COPY --from=backend-builder --chown=sub2api:sub2api /app/backend/resources /app/resources'
assert_line deploy/Dockerfile 'COPY --from=backend-builder --chown=sub2api:sub2api /app/backend/resources /app/resources'
test ! -e .goreleaser.yaml || fail 'obsolete .goreleaser.yaml must be removed'
# 发布执行器只读检查镜像；拒绝实际 Python argv 形状的镜像写命令。
assert_absent .github/scripts/release.py '["docker", "build"'
assert_absent .github/scripts/release.py '["docker", "buildx", "build"'
assert_absent .github/scripts/release.py '["docker", "push"'
assert_absent .github/scripts/release.py '["docker", "image", "build"'
assert_absent .github/scripts/release.py '["docker", "image", "push"'
assert_absent .github/scripts/release.py '["docker", "manifest", "create"'
assert_absent .github/scripts/release.py '["docker", "manifest", "push"'
assert_absent .github/scripts/release.py '["docker", "buildx", "imagetools", "create"'
test ! -e Dockerfile.goreleaser || fail 'obsolete Dockerfile.goreleaser must be removed'
test ! -e .goreleaser.simple.yaml || fail 'obsolete .goreleaser.simple.yaml must be removed'

printf 'docker runtime resources test passed\n'
