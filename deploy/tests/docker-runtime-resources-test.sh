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
assert_absent .goreleaser.yaml 'dockers:'
assert_absent .goreleaser.yaml 'docker_manifests:'
test ! -e Dockerfile.goreleaser || fail 'obsolete Dockerfile.goreleaser must be removed'
test ! -e .goreleaser.simple.yaml || fail 'obsolete .goreleaser.simple.yaml must be removed'

printf 'docker runtime resources test passed\n'
