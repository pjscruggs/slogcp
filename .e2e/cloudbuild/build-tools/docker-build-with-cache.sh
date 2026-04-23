#!/bin/bash
# Copyright 2025-2026 Patrick J. Scruggs
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

set -euo pipefail

# Build a Docker image if required, leveraging Docker cache when available.
# Usage: docker-build-with-cache.sh <service-id>

SERVICE_ID="${1:?service id required}"
STATE_FILE="/workspace/cache/${SERVICE_ID}.env"

if [[ ! -f "${STATE_FILE}" ]]; then
  echo "Cache state file missing for ${SERVICE_ID}" >&2
  exit 1
fi

# shellcheck disable=SC1090
source "${STATE_FILE}"

RETAGGED="${RETAGGED:-false}"
NEEDS_RETAG="${NEEDS_RETAG:-false}"
RETAG_SOURCE_IMAGE="${RETAG_SOURCE_IMAGE:-}"

if [[ "${SHOULD_BUILD}" == false ]]; then
  if [[ "${NEEDS_RETAG}" == true ]]; then
    if [[ -z "${RETAG_SOURCE_IMAGE}" ]]; then
      echo "[${SERVICE_ID}] Hash cache requested retag but no source image was recorded; aborting." >&2
      exit 1
    fi
    echo "[${SERVICE_ID}] Retagging cached image ${RETAG_SOURCE_IMAGE}"
    docker pull "${RETAG_SOURCE_IMAGE}" >/dev/null 2>&1
    docker tag "${RETAG_SOURCE_IMAGE}" "${VERSIONED_TAG}"
    docker tag "${RETAG_SOURCE_IMAGE}" "${STREAM_TAG_TAG}"
    {
      echo "NEEDS_RETAG=false"
      echo "RETAGGED=true"
    } >> "${STATE_FILE}"
  else
    echo "[${SERVICE_ID}] Cache hit; skipping docker build."
  fi
  exit 0
fi

echo "[${SERVICE_ID}] Building Docker image ${VERSIONED_TAG}"
if [[ -n "${CACHE_FROM_TAG}" ]]; then
  echo "[${SERVICE_ID}] Using cache-from ${CACHE_FROM_TAG}"
fi

GO_VERSION="$(cat /workspace/go_version.txt)"
DEBIAN_CODENAME="$(cat /workspace/debian_codename.txt)"
DISTROLESS_TAG="$(cat /workspace/distroless_tag.txt)"
APP_VERSION="$(cat /workspace/app_version.txt)"
DEPENDENCY_MODE="$(cat /workspace/dependency_mode.txt 2>/dev/null || echo "${DEPENDENCY_MODE:-floor}")"
TOOLCHAIN_MODE="$(cat /workspace/toolchain_mode.txt 2>/dev/null || echo "${TOOLCHAIN_MODE:-repo}")"
if [[ -n "${_BUILD_TIME:-}" ]]; then
  BUILD_TIME="${_BUILD_TIME}"
elif [[ -n "${BUILD_TIME:-}" ]]; then
  BUILD_TIME="${BUILD_TIME}"
elif [[ -f /workspace/build_time.txt ]]; then
  BUILD_TIME="$(tr -d '[:space:]' < /workspace/build_time.txt)"
else
  BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
fi
if [[ -z "${BUILD_TIME}" ]]; then
  BUILD_TIME="1970-01-01T00:00:00Z"
fi

BUILD_ARGS=(docker build
  --tag "${VERSIONED_TAG}"
  --tag "${STREAM_TAG_TAG}"
  --label "slogcp.build.hash=${HASH}"
  --label "slogcp.e2e.dependency-mode=${DEPENDENCY_MODE}"
  --label "slogcp.e2e.toolchain-mode=${TOOLCHAIN_MODE}"
  --build-arg "GO_VERSION=${GO_VERSION}"
  --build-arg "DEBIAN_CODENAME=${DEBIAN_CODENAME}"
  --build-arg "DISTROLESS_TAG=${DISTROLESS_TAG}"
  --build-arg "APP_VERSION=${APP_VERSION}"
  --build-arg "BUILD_TIME=${BUILD_TIME}"
  --file "${DOCKERFILE_PATH}")

if [[ -n "${CACHE_FROM_TAG}" ]]; then
  BUILD_ARGS+=(--cache-from "${CACHE_FROM_TAG}")
fi

BUILD_ARGS+=(--build-arg "BUILDKIT_INLINE_CACHE=1" "${BUILD_CONTEXT}")

export DOCKER_BUILDKIT=1
"${BUILD_ARGS[@]}"

{
  echo "CACHE_FROM_TAG=${VERSIONED_TAG}"
  echo "BUILT=true"
} >> "${STATE_FILE}"
