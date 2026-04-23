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

ROOT_DIR="${ROOT_DIR_OVERRIDE:-$(pwd)}"
WORKSPACE_DIR="${WORKSPACE_DIR:-/workspace}"
GO_VERSION_FILE="${GO_VERSION_FILE_OVERRIDE:-$WORKSPACE_DIR/go_version.txt}"
DEBIAN_CODENAME_FILE="${DEBIAN_CODENAME_FILE_OVERRIDE:-$WORKSPACE_DIR/debian_codename.txt}"
DISTROLESS_TAG_FILE="${DISTROLESS_TAG_FILE_OVERRIDE:-$WORKSPACE_DIR/distroless_tag.txt}"

if [[ ! -f "$GO_VERSION_FILE" ]]; then
    echo "Go version file not found at $GO_VERSION_FILE" >&2
    exit 1
fi

GO_VERSION="$(tr -d '[:space:]' < "$GO_VERSION_FILE")"
if [[ -z "$GO_VERSION" ]]; then
    echo "Go version file is empty: $GO_VERSION_FILE" >&2
    exit 1
fi

IFS='.' read -r GO_MAJOR GO_MINOR GO_PATCH <<< "$GO_VERSION"
if [[ -z "${GO_MAJOR:-}" || -z "${GO_MINOR:-}" ]]; then
    echo "Go version must include major and minor components (found '$GO_VERSION')" >&2
    exit 1
fi
GO_VERSION_MAJOR_MINOR="${GO_MAJOR}.${GO_MINOR}"

if [[ ! -f "$DEBIAN_CODENAME_FILE" ]]; then
    echo "Debian codename file not found at $DEBIAN_CODENAME_FILE" >&2
    exit 1
fi
if [[ ! -f "$DISTROLESS_TAG_FILE" ]]; then
    echo "Distroless tag file not found at $DISTROLESS_TAG_FILE" >&2
    exit 1
fi

DEBIAN_CODENAME="$(tr -d '[:space:]' < "$DEBIAN_CODENAME_FILE")"
DISTROLESS_TAG="$(tr -d '[:space:]' < "$DISTROLESS_TAG_FILE")"

if [[ -z "$DEBIAN_CODENAME" ]]; then
    echo "Debian codename file is empty: $DEBIAN_CODENAME_FILE" >&2
    exit 1
fi
if [[ -z "$DISTROLESS_TAG" ]]; then
    echo "Distroless tag file is empty: $DISTROLESS_TAG_FILE" >&2
    exit 1
fi

cat <<EOF_CHECK
Verifying toolchain configuration (canonical values)
  Go version          : $GO_VERSION
  Go major.minor      : $GO_VERSION_MAJOR_MINOR
  Debian codename     : $DEBIAN_CODENAME
  Distroless tag      : $DISTROLESS_TAG
EOF_CHECK

verify_metadata() {
    local path="$1"
    if ! grep -Eq '"module_path"[[:space:]]*:[[:space:]]*"[^"]+"' "$path"; then
        echo "missing module_path in $path" >&2
        exit 1
    fi
}

verify_dockerfile() {
    local path="$1"
    if ! grep -q "^ARG[[:space:]]\+GO_VERSION[[:space:]]*$" "$path"; then
        echo "Mismatch: expected 'ARG GO_VERSION' (without default) in $path" >&2
        exit 1
    fi
    if ! grep -q "^ARG[[:space:]]\+DEBIAN_CODENAME[[:space:]]*$" "$path"; then
        echo "Mismatch: expected 'ARG DEBIAN_CODENAME' in $path" >&2
        exit 1
    fi
    if ! grep -q "^ARG[[:space:]]\+DISTROLESS_TAG[[:space:]]*$" "$path"; then
        echo "Mismatch: expected 'ARG DISTROLESS_TAG' in $path" >&2
        exit 1
    fi
    if ! grep -F "FROM golang:\${GO_VERSION}-\${DEBIAN_CODENAME}" "$path"; then
        echo "Mismatch: expected builder stage to reference golang:\${GO_VERSION}-\${DEBIAN_CODENAME} in $path" >&2
        exit 1
    fi
    if ! grep -F "\${DISTROLESS_TAG}" "$path"; then
        echo "Mismatch: expected distroless stage to reference \${DISTROLESS_TAG} in $path" >&2
        exit 1
    fi
}

if find "$ROOT_DIR/services" \( -name go.mod -o -name go.sum \) -print -quit | grep -q .; then
    echo "Checked-in go.mod/go.sum files are not allowed under $ROOT_DIR/services" >&2
    exit 1
fi

if [[ ! -f "$ROOT_DIR/scripts/generate_go_module.py" ]]; then
    echo "Module generator not found at $ROOT_DIR/scripts/generate_go_module.py" >&2
    exit 1
fi

while IFS= read -r metadata_file; do
    verify_metadata "$metadata_file"
done < <(find "$ROOT_DIR/services" -name go.module.json -print | sort)

while IFS= read -r dockerfile; do
    verify_dockerfile "$dockerfile"
done < <(find "$ROOT_DIR/services" -name Dockerfile -print | sort)

echo "Dynamic toolchain and module metadata checks passed."
