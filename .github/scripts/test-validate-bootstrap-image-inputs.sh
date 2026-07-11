#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
validator="$script_dir/validate-bootstrap-image-inputs.sh"

failures=0

expect_accept() {
  local name="$1"
  local reported_version="$2"
  local image_tag="$3"
  local force_unhealthy="$4"

  if ! "$validator" "$reported_version" "$image_tag" "$force_unhealthy"; then
    printf 'FAIL: expected acceptance: %s\n' "$name" >&2
    failures=$((failures + 1))
  fi
}

expect_reject() {
  local name="$1"
  local reported_version="$2"
  local image_tag="$3"
  local force_unhealthy="$4"

  if "$validator" "$reported_version" "$image_tag" "$force_unhealthy"; then
    printf 'FAIL: expected rejection: %s\n' "$name" >&2
    failures=$((failures + 1))
  fi
}

expect_accept "normal bootstrap image" \
  "0.1.149" "bootstrap-0.1.149-update-agent" "false"
expect_accept "controlled unhealthy test image" \
  "0.1.150.999" "0.1.150.999" "true"

expect_reject "plain three-part release version" \
  "0.1.150" "0.1.150" "false"
expect_reject "latest tag" \
  "0.1.150" "latest" "false"
expect_reject "release tag" \
  "0.1.150" "release-0.1.150" "false"
expect_reject "controlled tag without force flag" \
  "0.1.150.999" "0.1.150.999" "false"
expect_reject "unhealthy tag and version mismatch" \
  "0.1.150.999" "0.1.151.999" "true"
expect_reject "unhealthy three-part version" \
  "0.1.150" "0.1.150" "true"
expect_reject "unhealthy suffix other than 999" \
  "0.1.150.998" "0.1.150.998" "true"
expect_reject "unhealthy bootstrap tag" \
  "0.1.150.999" "bootstrap-0.1.150.999" "true"

if ((failures > 0)); then
  printf '%d validation case(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'All bootstrap image input validation cases passed\n'
