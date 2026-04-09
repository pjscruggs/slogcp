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

# Push Docker image tags when a build occurred.
# Usage: docker-push-if-needed.sh <service-id>

SERVICE_ID="${1:?service id required}"
STATE_FILE="/workspace/cache/${SERVICE_ID}.env"

if [[ ! -f "${STATE_FILE}" ]]; then
  echo "Cache state file missing for ${SERVICE_ID}" >&2
  exit 1
fi

# shellcheck disable=SC1090
source "${STATE_FILE}"

RETAGGED="${RETAGGED:-false}"

if [[ "${SHOULD_BUILD}" == false ]]; then
  if [[ "${RETAGGED}" == true ]]; then
    echo "[${SERVICE_ID}] Pushing retagged image ${VERSIONED_TAG}"
    docker push "${VERSIONED_TAG}"
    docker push "${STREAM_TAG_TAG}"
    echo "PUSHED=true" >> "${STATE_FILE}"
  else
    echo "[${SERVICE_ID}] Reusing existing image ${VERSIONED_TAG}; skipping push."
  fi
  exit 0
fi

echo "[${SERVICE_ID}] Pushing Docker image ${VERSIONED_TAG}"
docker push "${VERSIONED_TAG}"
docker push "${STREAM_TAG_TAG}"

echo "PUSHED=true" >> "${STATE_FILE}"
