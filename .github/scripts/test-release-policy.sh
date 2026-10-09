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
require_contains ".github/workflows/backend-ci.yml" '.github/scripts/test-validate-release-version.sh'
require_contains ".github/workflows/backend-ci.yml" '.github/scripts/test-release-policy.sh'

require_exact_line "$version_file" "0.2.15"
require_exact_line "deploy/.env.example" "SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.2.15"
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
require_contains "$workflow" 'Verify checked-out release source version'
require_contains "$workflow" 'SOURCE_VERSION="$(tr -d '\''\r\n'\'' < backend/cmd/server/VERSION)"'
require_contains "$workflow" 'test "$SOURCE_VERSION" = "$IMAGE_TAG"'
require_contains "$workflow" 'GITHUB_REPO_NAME: ${{ github.event.repository.name }}'
require_contains "$workflow" 'image="ghcr.io/${REPOSITORY,,}:$IMAGE_TAG"'
require_contains "$workflow" 'Sub2API %s 已发布'
require_absent "$workflow" 'update_version:'
require_absent "$workflow" 'name: version-file'
require_absent "$workflow" 'git commit'
require_absent "$workflow" 'git push'
require_absent "$workflow" 'sync-version-file'
require_absent "$workflow" 'DOCKERHUB'
require_absent "$workflow" 'simple_release'
require_absent "$workflow" 'SIMPLE_RELEASE'

require_exact_line "frontend/src/components/common/VersionBadge.vue" "const GITHUB_REPO = 'gwenliu1025/sub2api'"
require_exact_line "frontend/src/components/common/VersionBadge.vue" "const DOCKER_IMAGE = 'ghcr.io/gwenliu1025/sub2api'"
require_absent "frontend/src/components/common/VersionBadge.vue" 'Wei-Shaw/sub2api'
require_absent "frontend/src/components/common/VersionBadge.vue" 'weishaw/sub2api'
require_exact_line "deploy/install.sh" 'GITHUB_REPO="gwenliu1025/sub2api"'
require_absent "deploy/install.sh" 'Wei-Shaw/sub2api'
require_contains "deploy/docker-deploy.sh" 'https://raw.githubusercontent.com/gwenliu1025/sub2api/v0.2.15/deploy'
require_absent "deploy/docker-deploy.sh" 'Wei-Shaw/sub2api'
require_exact_line "deploy/.env.example" 'APPLE_CONTAINER_SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.2.15'
require_contains "deploy/apple-container.sh" 'ghcr.io/gwenliu1025/sub2api:0.2.15'
require_absent "deploy/apple-container.sh" 'weishaw/sub2api:latest'

require_exact_line "deploy/docker-deploy.sh" 'GITHUB_RAW_URL="https://raw.githubusercontent.com/gwenliu1025/sub2api/v0.2.15/deploy"'
for release_doc in deploy/README.md deploy/DOCKER.md deploy/APPLE_CONTAINER.md; do
  require_absent "$release_doc" 'Wei-Shaw/sub2api'
  require_absent "$release_doc" 'weishaw/sub2api'
  require_absent "$release_doc" 'sub2api:latest'
  require_contains "$release_doc" 'ghcr.io/gwenliu1025/sub2api:0.2.15'
done

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
require_contains "$goreleaser" 'formats: [tar.gz]'
require_contains "$goreleaser" 'formats: [zip]'
require_contains "$goreleaser" 'name_template: checksums.txt'
require_contains "$goreleaser" '> AI API 网关平台'
require_contains "$goreleaser" '## 文档'

# 来源验收属于发布前的构建门，既不能关闭，也不能只检查启动时的 Git 快照。
gate=".github/scripts/verify-release-binary.sh"
require_file "$gate"
require_contains "$goreleaser" '    - go -C backend mod verify'
require_absent "$goreleaser" 'go mod tidy'
require_contains "$goreleaser" '      - -mod=readonly'
require_contains "$goreleaser" "        - cmd: bash $gate '{{ .Path }}' '{{ .FullCommit }}' '{{ .Os }}' '{{ .Arch }}'"
require_contains "$workflow" '          args: release --clean'
require_absent "$workflow" '--skip=validate'
require_absent "$workflow" '--skip=hooks'
require_absent "$goreleaser" '-buildvcs=false'

# 隔离工具输出验证正反例，不改真实源码、依赖或 Git 状态。
fixture_dir="$(mktemp -d)"
trap 'rm -f -- "$fixture_dir/go" "$fixture_dir/git"; rmdir -- "$fixture_dir"' EXIT
cat > "$fixture_dir/go" <<'EOF'
#!/usr/bin/env bash
[[ "${FIXTURE_GO_EXIT:-0}" == 0 ]] || exit "$FIXTURE_GO_EXIT"
printf '%s\n' "$FIXTURE_INFO"
EOF
cat > "$fixture_dir/git" <<'EOF'
#!/usr/bin/env bash
case "$*" in
  'rev-parse --show-toplevel') printf '%s\n' "$FIXTURE_ROOT" ;;
  'rev-parse HEAD') printf '%s\n' "$FIXTURE_HEAD" ;;
  'status --porcelain') printf '%s' "${FIXTURE_STATUS:-}" ;;
  *) exit 2 ;;
esac
EOF
chmod +x "$fixture_dir/go" "$fixture_dir/git"
fixture_commit=1111111111111111111111111111111111111111
other_commit=2222222222222222222222222222222222222222
fixture_go="$(awk '$1 == "go" { print $2; exit }' backend/go.mod)"
fixture_grpc="$(awk '$1 == "google.golang.org/grpc" { print $2; exit }' backend/go.mod)"
fixture_info="$(printf 'sub2api:\tgo%s\n\tdep\tgoogle.golang.org/grpc\t%s\th1:fixture\n\tbuild\t-ldflags="-X main.Commit=%s -X main.BuildType=release"\n\tbuild\t-tags=embed\n\tbuild\tCGO_ENABLED=0\n\tbuild\tGOOS=linux\n\tbuild\tGOARCH=amd64\n\tbuild\tvcs.revision=%s\n\tbuild\tvcs.modified=false\n' "$fixture_go" "$fixture_grpc" "$fixture_commit" "$fixture_commit")"

check_gate_case() {
  local name="$1" expected="$2" info="$3" status="${4:-}" head="${5:-$fixture_commit}" go_exit="${6:-0}" code=0 output
  output="$(PATH="$fixture_dir:$PATH" FIXTURE_ROOT="$PWD" FIXTURE_INFO="$info" FIXTURE_HEAD="$head" FIXTURE_STATUS="$status" FIXTURE_GO_EXIT="$go_exit" \
    bash "$gate" fixture.bin "$fixture_commit" linux amd64 2>&1)" || code=$?
  if [[ "$expected" == pass && "$code" != 0 ]] || [[ "$expected" == fail && "$code" == 0 ]]; then
    fail "release binary gate: $name (exit=$code): $output"
  fi
}

check_gate_case clean pass "$fixture_info"
check_gate_case dirty_worktree fail "$fixture_info" ' M backend/go.sum'
check_gate_case wrong_head fail "$fixture_info" '' "$other_commit"
check_gate_case wrong_revision fail "${fixture_info/vcs.revision=$fixture_commit/vcs.revision=$other_commit}"
check_gate_case dirty_binary fail "${fixture_info/vcs.modified=false/vcs.modified=true}"
check_gate_case missing_dirty_flag fail "${fixture_info/$'\tbuild\tvcs.modified=false'/}"
check_gate_case wrong_app_commit fail "${fixture_info/main.Commit=$fixture_commit/main.Commit=$other_commit}"
check_gate_case wrong_platform fail "${fixture_info/GOOS=linux/GOOS=windows}"
check_gate_case wrong_go fail "${fixture_info/go$fixture_go/go0.0.0}"
check_gate_case wrong_grpc fail "${fixture_info/$fixture_grpc/v0.0.0}"
check_gate_case missing_embed fail "${fixture_info/-tags=embed/-tags=unit}"
check_gate_case buildinfo_failure fail "$fixture_info" '' "$fixture_commit" 7

for compose in $compose_files; do
  require_contains "$compose" 'image: ${SUB2API_IMAGE:-ghcr.io/gwenliu1025/sub2api:0.2.15}'
  require_absent "$compose" 'weishaw/sub2api:latest'
done

if ((failures > 0)); then
  printf '%d release policy check(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'All release policy checks passed\n'
