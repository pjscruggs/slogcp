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

# Evaluate whether a service image must be rebuilt or can be reused.
# Usage: evaluate-image-cache.sh <service-id> <hash-source-path> <build-context> <dockerfile> <image-name>

SERVICE_ID="${1:?service id required}"
HASH_SOURCE_PATH="${2:?hash source path required}"
BUILD_CONTEXT="${3:?build context path required}"
DOCKERFILE_PATH="${4:?dockerfile path required}"
IMAGE_NAME="${5:?image name required}"

STREAM_TAG="$(cat /workspace/stream_tag_defaulted.txt)"
VERSION_TAG="$(cat /workspace/app_version.txt)"
ARTIFACT_REPO="${_ARTIFACT_REGISTRY_REPO:-${ARTIFACT_REGISTRY_REPO:-}}"
MANIFEST_BUCKET="${_GCS_BUCKET_NAME:-${GCS_BUCKET_NAME:-}}"

if [[ -z "${ARTIFACT_REPO}" ]]; then
  echo "Artifact registry repo missing (expected _ARTIFACT_REGISTRY_REPO)" >&2
  exit 1
fi

if [[ -z "${MANIFEST_BUCKET}" ]]; then
  echo "Manifest bucket missing (expected _GCS_BUCKET_NAME)" >&2
  exit 1
fi

VERSIONED_TAG="${ARTIFACT_REPO}/${IMAGE_NAME}:${VERSION_TAG}"
STREAM_TAG_TAG="${ARTIFACT_REPO}/${IMAGE_NAME}:${STREAM_TAG}"
MANIFEST_PATH="gs://${MANIFEST_BUCKET}/metadata/${SERVICE_ID}/${STREAM_TAG}/${VERSION_TAG}.json"
STATE_DIR="/workspace/cache"
STATE_FILE="${STATE_DIR}/${SERVICE_ID}.env"
MANIFEST_FILE="${STATE_DIR}/${SERVICE_ID}.manifest.json"

mkdir -p "${STATE_DIR}"

if [[ ! -d "${HASH_SOURCE_PATH}" ]]; then
  echo "Hash source path not found: ${HASH_SOURCE_PATH}" >&2
  exit 1
fi

if [[ ! -d "${BUILD_CONTEXT}" ]]; then
  echo "Build context path not found: ${BUILD_CONTEXT}" >&2
  exit 1
fi

GO_VERSION="$(cat /workspace/go_version.txt)"
DEBIAN_CODENAME="$(cat /workspace/debian_codename.txt)"
DISTROLESS_TAG="$(cat /workspace/distroless_tag.txt)"
DEPENDENCY_MODE="$(cat /workspace/dependency_mode.txt 2>/dev/null || echo floor)"
TOOLCHAIN_MODE="$(cat /workspace/toolchain_mode.txt 2>/dev/null || echo repo)"
PYTHON_BIN="$(command -v python3 || command -v python || true)"

# Compute a deterministic hash of the build inputs and toolchain metadata
HASH="$(
  {
    printf 'GO_VERSION=%s\n' "${GO_VERSION}"
    printf 'DEBIAN_CODENAME=%s\n' "${DEBIAN_CODENAME}"
    printf 'DISTROLESS_TAG=%s\n' "${DISTROLESS_TAG}"
    printf 'DEPENDENCY_MODE=%s\n' "${DEPENDENCY_MODE}"
    printf 'TOOLCHAIN_MODE=%s\n' "${TOOLCHAIN_MODE}"
    tar --sort=name --mtime='@0' --owner=0 --group=0 --numeric-owner -cf - -C "${HASH_SOURCE_PATH}" . 2>/dev/null || true
  } | sha256sum | cut -d' ' -f1
)"

if [[ -z "${HASH}" ]]; then
  echo "Failed to compute hash for ${SERVICE_ID}" >&2
  exit 1
fi

HASH_MANIFEST_PATH="gs://${MANIFEST_BUCKET}/metadata/${SERVICE_ID}/${STREAM_TAG}/${HASH}.json"
HASH_MANIFEST_FILE="${STATE_DIR}/${SERVICE_ID}.${HASH}.manifest.json"
NEEDS_RETAG=false
RETAG_SOURCE_VERSION_TAG=""
RETAG_SOURCE_IMAGE=""

SHOULD_BUILD=true
CACHE_FROM_TAG=""

echo "[${SERVICE_ID}] Input hash: ${HASH}"

if gsutil -q stat "${MANIFEST_PATH}"; then
  gsutil cp "${MANIFEST_PATH}" "${MANIFEST_FILE}" >/dev/null
  if [[ -n "${PYTHON_BIN}" ]]; then
    STORED_HASH="$("${PYTHON_BIN}" -c 'import json,sys; print(json.load(open(sys.argv[1])).get("hash",""))' "${MANIFEST_FILE}" 2>/dev/null || true)"
  else
    STORED_HASH=""
  fi
  if [[ "${STORED_HASH}" == "${HASH}" ]]; then
    if gcloud artifacts docker images describe "${VERSIONED_TAG}" >/dev/null 2>&1; then
      echo "[${SERVICE_ID}] Cache manifest hit for ${VERSIONED_TAG}"
      SHOULD_BUILD=false
      CACHE_FROM_TAG="${VERSIONED_TAG}"
    else
      echo "Manifest exists for ${SERVICE_ID} but ${VERSIONED_TAG} is missing; scheduling rebuild."
    fi
  else
    echo "[${SERVICE_ID}] Manifest hash mismatch (stored=${STORED_HASH}); will rebuild."
    CACHE_FROM_TAG="${VERSIONED_TAG}"
  fi
fi

if [[ "${SHOULD_BUILD}" == true ]]; then
  if gsutil -q stat "${HASH_MANIFEST_PATH}"; then
    gsutil cp "${HASH_MANIFEST_PATH}" "${HASH_MANIFEST_FILE}" >/dev/null
    if [[ -n "${PYTHON_BIN}" ]]; then
      STORED_VERSION_TAG="$(${PYTHON_BIN} -c 'import json,sys; print(json.load(open(sys.argv[1])).get("versionTag",""))' "${HASH_MANIFEST_FILE}" 2>/dev/null || true)"
    else
      STORED_VERSION_TAG=""
    fi
    if [[ -n "${STORED_VERSION_TAG}" ]]; then
      SOURCE_IMAGE="${ARTIFACT_REPO}/${IMAGE_NAME}:${STORED_VERSION_TAG}"
      if gcloud artifacts docker images describe "${SOURCE_IMAGE}" >/dev/null 2>&1; then
        echo "[${SERVICE_ID}] Cache hit via hash manifest ${HASH}; will retag from ${SOURCE_IMAGE}"
        SHOULD_BUILD=false
        CACHE_FROM_TAG="${SOURCE_IMAGE}"
        NEEDS_RETAG=true
        RETAG_SOURCE_VERSION_TAG="${STORED_VERSION_TAG}"
        RETAG_SOURCE_IMAGE="${SOURCE_IMAGE}"
      else
        echo "[${SERVICE_ID}] Hash manifest references missing image ${SOURCE_IMAGE}; will rebuild."
      fi
    else
      echo "[${SERVICE_ID}] Hash manifest ${HASH_MANIFEST_PATH} missing versionTag; ignoring."
    fi
  fi
fi

if [[ "${SHOULD_BUILD}" == true && -z "${CACHE_FROM_TAG}" ]]; then
  if gcloud artifacts docker images describe "${STREAM_TAG_TAG}" >/dev/null 2>&1; then
    echo "[${SERVICE_ID}] Will seed cache-from ${STREAM_TAG_TAG}"
    CACHE_FROM_TAG="${STREAM_TAG_TAG}"
  fi
fi

# Warm the Docker cache if we found an image to reuse
if [[ -n "${CACHE_FROM_TAG}" ]]; then
  docker pull "${CACHE_FROM_TAG}" >/dev/null 2>&1 || true
fi

cat > "${STATE_FILE}" <<EOF
SERVICE_ID=${SERVICE_ID}
HASH_SOURCE_PATH=${HASH_SOURCE_PATH}
BUILD_CONTEXT=${BUILD_CONTEXT}
DOCKERFILE_PATH=${DOCKERFILE_PATH}
IMAGE_NAME=${IMAGE_NAME}
STREAM_TAG=${STREAM_TAG}
VERSION_TAG=${VERSION_TAG}
VERSIONED_TAG=${VERSIONED_TAG}
STREAM_TAG_TAG=${STREAM_TAG_TAG}
MANIFEST_PATH=${MANIFEST_PATH}
HASH_MANIFEST_PATH=${HASH_MANIFEST_PATH}
HASH=${HASH}
SHOULD_BUILD=${SHOULD_BUILD}
CACHE_FROM_TAG=${CACHE_FROM_TAG}
NEEDS_RETAG=${NEEDS_RETAG}
RETAG_SOURCE_VERSION_TAG=${RETAG_SOURCE_VERSION_TAG}
RETAG_SOURCE_IMAGE=${RETAG_SOURCE_IMAGE}
DEPENDENCY_MODE=${DEPENDENCY_MODE}
TOOLCHAIN_MODE=${TOOLCHAIN_MODE}
RETAGGED=false
BUILT=false
PUSHED=false
EOF
