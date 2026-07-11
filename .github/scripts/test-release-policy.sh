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

require_absent_file() {
  local path="$1"
  [[ ! -e "$path" ]] || fail "unexpected file: $path"
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
ci_workflow=".github/workflows/backend-ci.yml"
goreleaser=".goreleaser.yaml"
version_file="backend/cmd/server/VERSION"
env_example="deploy/.env.example"
image_repository="ghcr.io/gwenliu1025/sub2api"

require_file "$version_file"
release_version="$(tr -d '\r\n' < "$version_file")"
if [[ ! "$release_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  fail "backend/cmd/server/VERSION must contain X.Y.Z"
fi

require_file ".github/scripts/validate-release-version.sh"
require_file "$workflow"
require_file "$ci_workflow"
require_file "$goreleaser"
require_file "$env_example"

require_contains "$workflow" "platforms: linux/amd64,linux/arm64"
require_contains "$workflow" 'tags: ghcr.io/${{ steps.lowercase.outputs.owner }}/sub2api:${{ needs.validate_release_version.outputs.image_tag }}'
require_contains "$workflow" '.github/scripts/validate-release-version.sh'
require_contains "$workflow" "id: revision"
require_contains "$workflow" "git rev-parse HEAD"
require_contains "$workflow" 'COMMIT=${{ steps.revision.outputs.sha }}'
require_absent "$workflow" "simple_release"
require_absent "$workflow" "SIMPLE_RELEASE"
require_absent "$workflow" "DockerHub"
require_absent "$workflow" "sync-version-file"

require_contains "$ci_workflow" ".github/scripts/test-validate-release-version.sh"
require_contains "$ci_workflow" ".github/scripts/test-release-policy.sh"

require_absent "$goreleaser" "dockers:"
require_absent "$goreleaser" "docker_manifests:"
require_absent "$goreleaser" "Docker Hub"
require_absent "$goreleaser" ":latest"
require_absent "$goreleaser" ":{{ .Version }}-amd64"
require_absent "$goreleaser" ":{{ .Version }}-arm64"

require_absent_file ".goreleaser.simple.yaml"
require_absent_file ".github/workflows/bootstrap-image.yml"
require_absent_file ".github/scripts/validate-bootstrap-image-inputs.sh"
require_absent_file ".github/scripts/test-validate-bootstrap-image-inputs.sh"
require_absent_file "Dockerfile.goreleaser"

for path in Dockerfile deploy/Dockerfile deploy/docker-compose.yml; do
  require_absent "$path" "FORCE_UNHEALTHY"
done

require_exact_line "$env_example" "SUB2API_IMAGE=${image_repository}:${release_version}"
require_exact_line "$env_example" "UPDATE_REPO=gwenliu1025/sub2api"
require_exact_line "$env_example" "UPDATE_MODE=docker_agent"
require_exact_line "$env_example" "UPDATE_IMAGE_REPOSITORY=${image_repository}"

for path in \
  deploy/docker-compose.yml \
  deploy/docker-compose.local.yml \
  deploy/docker-compose.standalone.yml; do
  require_contains "$path" 'image: ${SUB2API_IMAGE}'
  require_contains "$path" '/run/sub2api-updater:/run/sub2api-updater:ro'
  require_contains "$path" 'UPDATE_REPO=${UPDATE_REPO:-gwenliu1025/sub2api}'
  require_contains "$path" 'UPDATE_MODE=${UPDATE_MODE:-docker_agent}'
  require_contains "$path" 'UPDATE_AGENT_SOCKET=${UPDATE_AGENT_SOCKET:-/run/sub2api-updater/updater.sock}'
  require_contains "$path" 'UPDATE_AGENT_TIMEOUT_SECONDS=${UPDATE_AGENT_TIMEOUT_SECONDS:-600}'
  require_contains "$path" 'UPDATE_IMAGE_REPOSITORY=${UPDATE_IMAGE_REPOSITORY:-ghcr.io/gwenliu1025/sub2api}'
  require_absent "$path" "weishaw/sub2api"
  require_absent "$path" "sub2api:latest"
done

require_contains "deploy/docker-deploy.sh" "raw.githubusercontent.com/gwenliu1025/sub2api/main/deploy"
require_absent "deploy/docker-deploy.sh" "Wei-Shaw/sub2api"
require_contains "deploy/install.sh" 'GITHUB_REPO="gwenliu1025/sub2api"'
require_contains "deploy/build_image.sh" 'VERSION_FILE="${REPO_ROOT}/backend/cmd/server/VERSION"'
require_absent "deploy/build_image.sh" "sub2api:latest"
require_absent "deploy/DOCKER.md" "weishaw/sub2api"
require_absent "deploy/DOCKER.md" "sub2api:latest"
require_absent "deploy/README.md" "Wei-Shaw/sub2api"

require_exact_line "frontend/src/views/HomeView.vue" \
  "const githubUrl = 'https://github.com/gwenliu1025/sub2api'"
require_exact_line "frontend/src/views/KeyUsageView.vue" \
  "const githubUrl = 'https://github.com/gwenliu1025/sub2api'"
require_contains "frontend/src/components/layout/AppHeader.vue" \
  'href="https://github.com/gwenliu1025/sub2api"'

if ((failures > 0)); then
  printf '%d release policy check(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'All release policy checks passed\n'
