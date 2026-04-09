#!/usr/bin/env bash
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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/common.sh"

require_command gcloud

ENV_FILE="$(resolve_env_file_from_args "$@")"
load_env_file "$ENV_FILE"

mapfile -d '' -t FORWARDED_ARGS < <(strip_env_file_args "$@")

cd "$REPO_ROOT"
exec "${REPO_ROOT}/.github/scripts/run_e2e_cloud_build.sh" "${FORWARDED_ARGS[@]}" --source-mode local
