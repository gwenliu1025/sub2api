#!/usr/bin/env bash
set -euo pipefail

failures=0

fail() {
  printf 'FAIL: %s\n' "$1" >&2
  failures=$((failures + 1))
}

require_file() {
  local path="$1"
  [[ -f "$path" ]] || fail "expected file: $path"
}

require_exact_line() {
  local path="$1"
  local text="$2"
  grep -Fxq -- "$text" "$path" || fail "expected exact line '$text' in $path"
}

require_contains() {
  local path="$1"
  local text="$2"
  grep -Fq -- "$text" "$path" || fail "expected '$text' in $path"
}

require_absent() {
  local path="$1"
  local text="$2"
  if grep -Fq -- "$text" "$path"; then
    fail "unexpected '$text' in $path"
  fi
}

workflow=".github/workflows/release.yml"
goreleaser=".goreleaser.yaml"
version_file="backend/cmd/server/VERSION"
compose_files="deploy/docker-compose.yml deploy/docker-compose.local.yml deploy/docker-compose.standalone.yml"

require_file "$workflow"
require_file "$goreleaser"
require_file "$version_file"
require_file ".github/scripts/validate-release-version.sh"

require_exact_line "$version_file" "0.1.169"
require_exact_line "deploy/.env.example" "SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.169"
require_contains "$workflow" 'tags:'
require_contains "$workflow" '"v*"'
require_contains "$workflow" 'workflow_dispatch:'
require_contains "$workflow" 'RELEASE_TAG: ${{ github.event_name == '\''workflow_dispatch'\'' && inputs.tag || github.ref_name }}'
require_contains "$workflow" 'IMAGE_TAG="${RELEASE_TAG#v}"'
require_contains "$workflow" '.github/scripts/validate-release-version.sh "$RELEASE_TAG" "$IMAGE_TAG"'
require_contains "$workflow" 'context: .'
require_contains "$workflow" 'platforms: linux/amd64,linux/arm64'
require_contains "$workflow" 'tags: ghcr.io/${{ steps.lowercase.outputs.owner }}/sub2api:${{ needs.validate_release_version.outputs.image_tag }}'
require_contains "$workflow" 'VERSION=${{ needs.validate_release_version.outputs.image_tag }}'
require_contains "$workflow" 'COMMIT=${{ steps.revision.outputs.sha }}'
require_contains "$workflow" 'OCI_SOURCE=https://github.com/${{ github.repository }}'
require_contains "$workflow" 'GITHUB_REPO_NAME: ${{ github.event.repository.name }}'
require_contains "$workflow" 'image="ghcr.io/${REPOSITORY,,}:$IMAGE_TAG"'
require_contains "$workflow" 'update_version:'
require_contains "$workflow" 'actions/upload-artifact@v7'
require_absent "$workflow" 'git commit'
require_absent "$workflow" 'git push'
require_absent "$workflow" 'sync-version-file'
require_absent "$workflow" 'DOCKERHUB'
require_absent "$workflow" 'simple_release'
require_absent "$workflow" 'SIMPLE_RELEASE'

require_absent "$goreleaser" 'dockers:'
require_absent "$goreleaser" 'docker_manifests:'
require_absent "$goreleaser" ':latest'
require_absent "$goreleaser" ':{{ .Major }}'
require_absent "$goreleaser" ':{{ .Major }}.{{ .Minor }}'
require_absent "$goreleaser" '-amd64'
require_absent "$goreleaser" '-arm64'
require_contains "$goreleaser" '      - linux'
require_contains "$goreleaser" '      - windows'
require_contains "$goreleaser" '      - darwin'
require_contains "$goreleaser" '      - amd64'
require_contains "$goreleaser" '      - arm64'
require_contains "$goreleaser" 'goos: windows'
require_contains "$goreleaser" 'format: zip'
require_contains "$goreleaser" 'name_template: checksums.txt'

for compose in $compose_files; do
  require_contains "$compose" 'image: ${SUB2API_IMAGE:-ghcr.io/gwenliu1025/sub2api:0.1.169}'
  require_absent "$compose" 'weishaw/sub2api:latest'
done

if ((failures > 0)); then
  printf '%d release policy check(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'All release policy checks passed\n'
