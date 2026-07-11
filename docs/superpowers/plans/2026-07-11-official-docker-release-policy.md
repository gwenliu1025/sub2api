# Official Docker Release Policy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the temporary bootstrap-tag release path with one official
GitHub Release and one matching GHCR image tag per version.

**Architecture:** Keep GoReleaser responsible for GitHub Release assets. Move
GHCR image publication to Docker Buildx in the normal tag-triggered release
workflow, which emits one multi-platform OCI index tagged only with the
application version. The updater remains unchanged: it derives the image tag
from the selected release version.

**Tech Stack:** Bash, GitHub Actions, Docker Buildx, GoReleaser, GHCR

---

### Task 1: Define the Version Validator

**Files:**

- Create: `.github/scripts/validate-release-version.sh`
- Create: `.github/scripts/test-validate-release-version.sh`

- [ ] **Step 1: Write the failing validator test**

Test these accepted inputs:

```bash
expect_accept "official release" "v0.1.150" "0.1.150"
expect_accept "multi-digit components" "v12.34.56" "12.34.56"
```

Test these rejected inputs:

```bash
expect_reject "image v prefix" "v0.1.150" "v0.1.150"
expect_reject "latest alias" "v0.1.150" "latest"
expect_reject "bootstrap suffix" "v0.1.150" "bootstrap-0.1.150-update-agent"
expect_reject "architecture suffix" "v0.1.150" "0.1.150-amd64"
expect_reject "four-part version" "v0.1.150.1" "0.1.150.1"
expect_reject "release image mismatch" "v0.1.150" "0.1.151"
```

- [ ] **Step 2: Run the test and confirm it fails because the validator is absent**

Run:

```bash
bash .github/scripts/test-validate-release-version.sh
```

Expected: nonzero exit because
`.github/scripts/validate-release-version.sh` does not exist.

- [ ] **Step 3: Add the minimal validator**

The validator accepts two arguments and enforces:

```bash
[[ "$release_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ "$image_tag" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ "${release_tag#v}" == "$image_tag" ]]
```

- [ ] **Step 4: Run the validator test**

Run:

```bash
bash .github/scripts/test-validate-release-version.sh
```

Expected: `All release version validation cases passed`.

### Task 2: Publish Only the Official GHCR Tag

**Files:**

- Modify: `.github/workflows/release.yml`
- Modify: `.goreleaser.yaml`
- Modify: `.goreleaser.simple.yaml`
- Delete: `.github/workflows/bootstrap-image.yml`
- Delete: `.github/scripts/validate-bootstrap-image-inputs.sh`
- Delete: `.github/scripts/test-validate-bootstrap-image-inputs.sh`

- [ ] **Step 1: Add workflow assertions before publishing**

The workflow derives:

```bash
RELEASE_TAG="${{ github.event.inputs.tag || github.ref_name }}"
IMAGE_TAG="${RELEASE_TAG#v}"
.github/scripts/validate-release-version.sh "$RELEASE_TAG" "$IMAGE_TAG"
```

The workflow exports `RELEASE_TAG` and `IMAGE_TAG` through `GITHUB_ENV`.

- [ ] **Step 2: Replace GoReleaser Docker publication**

Remove all `dockers` and `docker_manifests` configuration from both
GoReleaser files. Add a Docker Buildx workflow step with:

```yaml
platforms: linux/amd64,linux/arm64
tags: ghcr.io/${{ steps.lowercase.outputs.owner }}/sub2api:${{ env.IMAGE_TAG }}
build-args: |
  VERSION=${{ env.IMAGE_TAG }}
  COMMIT=${{ github.sha }}
  OCI_SOURCE=https://github.com/${{ github.repository }}
```

No other `tags:` value is allowed for the GHCR publish step.

- [ ] **Step 3: Remove temporary image behavior**

Delete the bootstrap workflow and validator scripts. Remove the
`FORCE_UNHEALTHY_HEALTHCHECK` build argument and Compose condition because no
release image may intentionally fail health checks.

- [ ] **Step 4: Verify static policy**

Run:

```bash
bash .github/scripts/test-validate-release-version.sh
rg -n "bootstrap-|force_unhealthy|FORCE_UNHEALTHY|:latest|-[aA][mM][dD]64|-[aA][rR][mM]64" \
  .github .goreleaser.yaml .goreleaser.simple.yaml Dockerfile deploy/Dockerfile \
  deploy/docker-compose.yml
```

Expected: the search has no matches in the official GHCR release paths.

### Task 3: Record the Durable Operator Contract

**Files:**

- Create: `AGENTS.md`
- Modify: `backend/cmd/server/VERSION`
- Modify: `deploy/.env.example`
- Modify: `docs/superpowers/specs/2026-07-10-docker-update-agent-design.md`
- Modify: `docs/superpowers/plans/2026-07-10-docker-update-agent.md`

- [ ] **Step 1: Add project rules**

Record the exact release tag, application version, GHCR image tag, and
two-artifact eligibility contract in `AGENTS.md`.

- [ ] **Step 2: Bump the pending official application version**

Set:

```text
0.1.150
```

in `backend/cmd/server/VERSION`.

- [ ] **Step 3: Update example and historical update-agent documents**

Use:

```dotenv
SUB2API_IMAGE=ghcr.io/gwenliu1025/sub2api:0.1.150
```

Document that the first migration directly recreates only `sub2api` on the
official image, and all subsequent management-panel updates use prepare then
restart.

### Task 4: Verify, Commit, and Publish

**Files:**

- Modify only files required to correct a verification failure.

- [ ] **Step 1: Run local verification**

Run:

```bash
bash .github/scripts/test-validate-release-version.sh
python -m unittest discover -s deploy/updater/tests -v
go test -tags=unit ./... -C backend
pnpm --dir frontend run test:run
pnpm --dir frontend run build
git diff --check
```

- [ ] **Step 2: Commit and push**

Commit the policy correction with:

```bash
git commit -m "fix: publish only official docker release tags"
git push origin gwen-main-v0.1.149-custom
```

- [ ] **Step 3: Publish the official release**

Create and push annotated tag `v0.1.150`, wait for the normal Release
workflow, then inspect `ghcr.io/gwenliu1025/sub2api:0.1.150` before any later
host deployment.
