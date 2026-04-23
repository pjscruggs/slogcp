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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd -P)"
PYTHON_BIN="$(command -v python3 || command -v python || true)"
RESOLVER_SCRIPT="${ROOT_DIR}/scripts/resolve_latest_go_version.py"

if [[ -z "$PYTHON_BIN" ]]; then
    echo "Error: python3 or python is required to resolve the latest stable Go version" >&2
    exit 1
fi
if [[ ! -f "$RESOLVER_SCRIPT" ]]; then
    echo "Error: Go resolver not found at $RESOLVER_SCRIPT" >&2
    exit 1
fi

GO_VERSION="$("$PYTHON_BIN" "$RESOLVER_SCRIPT")"

echo ".e2e no longer syncs checked-in go.mod/go.sum files."
echo "Generated .e2e modules will currently use the latest stable Go release: $GO_VERSION"
