#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
validator="$script_dir/validate-release-version.sh"

failures=0

expect_accept() {
  local name="$1"
  local release_tag="$2"
  local image_tag="$3"

  if ! "$validator" "$release_tag" "$image_tag"; then
    printf 'FAIL: expected acceptance: %s\n' "$name" >&2
    failures=$((failures + 1))
  fi
}

expect_reject() {
  local name="$1"
  local release_tag="$2"
  local image_tag="$3"

  if "$validator" "$release_tag" "$image_tag"; then
    printf 'FAIL: expected rejection: %s\n' "$name" >&2
    failures=$((failures + 1))
  fi
}

expect_accept "official release" "v0.1.169" "0.1.169"
expect_accept "multi-digit components" "v12.34.56" "12.34.56"

expect_reject "image v prefix" "v0.1.169" "v0.1.169"
expect_reject "latest alias" "v0.1.169" "latest"
expect_reject "bootstrap suffix" "v0.1.169" "bootstrap-0.1.169-update-agent"
expect_reject "architecture suffix" "v0.1.169" "0.1.169-amd64"
expect_reject "four-part version" "v0.1.169.1" "0.1.169.1"
expect_reject "release image mismatch" "v0.1.169" "0.1.170"

if ((failures > 0)); then
  printf '%d validation case(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'All release version validation cases passed\n'
