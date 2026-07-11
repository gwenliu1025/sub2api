#!/usr/bin/env bash
# 本地构建镜像的快速脚本，避免在命令行反复输入构建参数。

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
VERSION_FILE="${REPO_ROOT}/backend/cmd/server/VERSION"
VERSION="$(tr -d '\r\n' < "${VERSION_FILE}")"
IMAGE_REF="${SUB2API_IMAGE:-ghcr.io/gwenliu1025/sub2api:${VERSION}}"

docker build -t "${IMAGE_REF}" \
    --build-arg VERSION="${VERSION}" \
    --build-arg GOPROXY=https://goproxy.cn,direct \
    --build-arg GOSUMDB=sum.golang.google.cn \
    -f "${REPO_ROOT}/Dockerfile" \
    "${REPO_ROOT}"
