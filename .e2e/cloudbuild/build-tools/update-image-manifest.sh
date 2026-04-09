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

# Update the manifest stored in GCS for a given service image.
# Usage: update-image-manifest.sh <service-id>

SERVICE_ID="${1:?service id required}"
STATE_FILE="/workspace/cache/${SERVICE_ID}.env"

if [[ ! -f "${STATE_FILE}" ]]; then
  echo "Cache state file missing for ${SERVICE_ID}" >&2
  exit 1
fi

# shellcheck disable=SC1090
source "${STATE_FILE}"

HASH_MANIFEST_PATH="${HASH_MANIFEST_PATH:-}"
RETAGGED="${RETAGGED:-false}"

if [[ -z "${MANIFEST_PATH:-}" ]]; then
  echo "MANIFEST_PATH missing for ${SERVICE_ID}" >&2
  exit 1
fi

DIGEST="$(gcloud artifacts docker images describe "${VERSIONED_TAG}" --format='value(image_summary.digest)')"
if [[ -z "${DIGEST}" ]]; then
  echo "Unable to resolve digest for ${VERSIONED_TAG}" >&2
  exit 1
fi

echo "[${SERVICE_ID}] Recording manifest for ${VERSIONED_TAG} (${DIGEST})"

TIMESTAMP="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
TMP_MANIFEST="/workspace/cache/${SERVICE_ID}.manifest.out.json"

cat > "${TMP_MANIFEST}" <<EOF
{
  "service": "${SERVICE_ID}",
  "image": "${IMAGE_NAME}",
  "versionTag": "${VERSION_TAG}",
  "streamTag": "${STREAM_TAG}",
  "hash": "${HASH}",
  "digest": "${DIGEST}",
  "built": "${BUILT}",
  "pushed": "${PUSHED}",
  "retagged": "${RETAGGED}",
  "updated": "${TIMESTAMP}"
}
EOF

gsutil cp "${TMP_MANIFEST}" "${MANIFEST_PATH}" >/dev/null
if [[ -n "${HASH_MANIFEST_PATH}" ]]; then
  gsutil cp "${TMP_MANIFEST}" "${HASH_MANIFEST_PATH}" >/dev/null
fi
