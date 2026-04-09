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

ROOT_DIR="$(pwd)"
VERSION_FILE="$ROOT_DIR/.toolchain/go.version"
DEBIAN_FILE="$ROOT_DIR/.toolchain/debian.env"

if [[ ! -f "$VERSION_FILE" ]]; then
    echo "Go version file not found at $VERSION_FILE" >&2
    exit 1
fi

GO_VERSION="$(tr -d '[:space:]' < "$VERSION_FILE")"
if [[ -z "$GO_VERSION" ]]; then
    echo "Go version file is empty" >&2
    exit 1
fi

IFS='.' read -r GO_MAJOR GO_MINOR GO_PATCH <<< "$GO_VERSION"
if [[ -z "${GO_MAJOR:-}" || -z "${GO_MINOR:-}" ]]; then
    echo "Go version must include major and minor components (found '$GO_VERSION')" >&2
    exit 1
fi
GO_VERSION_MAJOR_MINOR="${GO_MAJOR}.${GO_MINOR}"

if [[ ! -f "$DEBIAN_FILE" ]]; then
    echo "Debian configuration not found at $DEBIAN_FILE" >&2
    exit 1
fi

# shellcheck disable=SC1090
source "$DEBIAN_FILE"

if [[ -z "${DEBIAN_CODENAME:-}" ]]; then
    echo "DEBIAN_CODENAME missing from $DEBIAN_FILE" >&2
    exit 1
fi
if [[ -z "${DISTROLESS_TAG:-}" ]]; then
    echo "DISTROLESS_TAG missing from $DEBIAN_FILE" >&2
    exit 1
fi

cat <<EOF_CHECK
Verifying toolchain configuration (canonical values)
  Go version          : $GO_VERSION
  Go major.minor      : $GO_VERSION_MAJOR_MINOR
  Debian codename     : $DEBIAN_CODENAME
  Distroless tag      : $DISTROLESS_TAG
EOF_CHECK

verify_go_mod() {
    local requested_path="$1"
    local path="$requested_path"

    if [[ ! -f "$path" ]]; then
        local template_path="${requested_path}.template"
        if [[ -f "$template_path" ]]; then
            path="$template_path"
        else
            echo "Skipping Go module verification because neither $requested_path nor $template_path exists."
            return
        fi
    fi

    local go_pattern="^go (${GO_VERSION_MAJOR_MINOR}|\\{\\{GO_VERSION_MAJOR_MINOR\\}\\})[[:space:]]*$"
    if ! grep -Eq "$go_pattern" "$path"; then
        echo "Mismatch: expected 'go ${GO_VERSION_MAJOR_MINOR}' in $path" >&2
        exit 1
    fi

    local toolchain_pattern="^toolchain (go${GO_VERSION}|\\{\\{GO_TOOLCHAIN\\}\\})[[:space:]]*$"
    if ! grep -Eq "$toolchain_pattern" "$path"; then
        echo "Mismatch: expected 'toolchain go${GO_VERSION}' in $path" >&2
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

while IFS= read -r go_mod; do
    verify_go_mod "$go_mod"
done < <(find "$ROOT_DIR/services" -name go.mod -print | sort)

while IFS= read -r dockerfile; do
    verify_dockerfile "$dockerfile"
done < <(find "$ROOT_DIR/services" -name Dockerfile -print | sort)

echo "Toolchain configuration checks passed."
