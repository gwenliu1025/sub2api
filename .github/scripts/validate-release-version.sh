#!/usr/bin/env bash
set -euo pipefail

release_tag="${1:-}"
image_tag="${2:-}"

[[ "$release_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "release_tag must match vX.Y.Z with numeric components" >&2
  exit 1
}

[[ "$image_tag" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || {
  echo "image_tag must match X.Y.Z with numeric components" >&2
  exit 1
}

[[ "${release_tag#v}" == "$image_tag" ]] || {
  echo "image_tag must equal release_tag without the v prefix" >&2
  exit 1
}
