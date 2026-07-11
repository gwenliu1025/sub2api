#!/usr/bin/env bash
set -euo pipefail

reported_version="${1:-}"
image_tag="${2:-}"
force_unhealthy="${3:-}"

[[ "$reported_version" =~ ^[0-9]+(\.[0-9]+)+$ ]] || {
  echo "reported_version must contain numeric dot-separated components" >&2
  exit 1
}

[[ "$image_tag" =~ ^[A-Za-z0-9_.-]+$ ]] || {
  echo "image_tag contains unsupported characters" >&2
  exit 1
}

case "$force_unhealthy" in
  false)
    [[ "$image_tag" == bootstrap-* ]] || {
      echo "image_tag must start with bootstrap- unless force_unhealthy is true" >&2
      exit 1
    }
    ;;
  true)
    [[ "$reported_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.999$ ]] || {
      echo "forced unhealthy reported_version must be four numeric parts ending in .999" >&2
      exit 1
    }
    [[ "$image_tag" == "$reported_version" ]] || {
      echo "forced unhealthy image_tag must equal reported_version" >&2
      exit 1
    }
    ;;
  *)
    echo "force_unhealthy must be true or false" >&2
    exit 1
    ;;
esac
