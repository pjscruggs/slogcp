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
VERSION_FILE="${ROOT_DIR}/.toolchain/go.version"

if [[ ! -f "$VERSION_FILE" ]]; then
    echo "Error: canonical Go version file not found at $VERSION_FILE" >&2
    exit 1
fi

GO_VERSION="$(tr -d '[:space:]' < "$VERSION_FILE")"
if [[ -z "$GO_VERSION" ]]; then
    echo "Error: $VERSION_FILE is empty" >&2
    exit 1
fi

IFS='.' read -r GO_MAJOR GO_MINOR GO_PATCH <<< "$GO_VERSION"
if [[ -z "${GO_MAJOR:-}" || -z "${GO_MINOR:-}" ]]; then
    echo "Error: Go version must include major and minor components (found '$GO_VERSION')" >&2
    exit 1
fi
GO_VERSION_MAJOR_MINOR="${GO_MAJOR}.${GO_MINOR}"
GO_TOOLCHAIN="toolchain go${GO_VERSION}"

echo "Syncing Go version to $GO_VERSION (go directive ${GO_VERSION_MAJOR_MINOR})"

update_go_mod() {
    local path="$1"
    local tmp
    tmp="$(mktemp)"
    awk -v gom="$GO_VERSION_MAJOR_MINOR" -v gotool="$GO_TOOLCHAIN" '
        BEGIN {
            go_updated = 0;
            toolchain_updated = 0;
        }
        /^go[[:space:]]+/ && go_updated == 0 {
            print "go " gom;
            go_updated = 1;
            next;
        }
        /^toolchain[[:space:]]+/ && toolchain_updated == 0 {
            print gotool;
            toolchain_updated = 1;
            next;
        }
        {
            print;
        }
        END {
            if (go_updated == 0) {
                print "go " gom;
            }
            if (toolchain_updated == 0) {
                print gotool;
            }
        }
    ' "$path" > "$tmp"
    mv "$tmp" "$path"
    local rel="${path#${ROOT_DIR}/}"
    echo "  updated ${rel}"
}

update_dockerfile() {
    local path="$1"
    local tmp
    tmp="$(mktemp)"
    awk '
        BEGIN {
            go_arg_updated = 0;
            debian_arg_updated = 0;
            distroless_arg_updated = 0;
        }
        /^ARG[[:space:]]+GO_VERSION([[:space:]]*=.*)?[[:space:]]*$/ && go_arg_updated == 0 {
            print "ARG GO_VERSION";
            go_arg_updated = 1;
            next;
        }
        /^ARG[[:space:]]+DEBIAN_CODENAME([[:space:]]*=.*)?[[:space:]]*$/ && debian_arg_updated == 0 {
            print "ARG DEBIAN_CODENAME";
            debian_arg_updated = 1;
            next;
        }
        /^ARG[[:space:]]+DISTROLESS_TAG([[:space:]]*=.*)?[[:space:]]*$/ && distroless_arg_updated == 0 {
            print "ARG DISTROLESS_TAG";
            distroless_arg_updated = 1;
            next;
        }
        { print }
        END {
            if (go_arg_updated == 0 || debian_arg_updated == 0 || distroless_arg_updated == 0) {
                exit 64;
            }
        }
    ' "$path" > "$tmp" || {
        rm -f "$tmp"
        echo "Error: could not locate canonical toolchain ARG lines in $path" >&2
        exit 1
    }
    mv "$tmp" "$path"
    local rel="${path#${ROOT_DIR}/}"
    echo "  updated ${rel}"
}

while IFS= read -r go_mod; do
    update_go_mod "$go_mod"
done < <(find "${ROOT_DIR}/services" -name go.mod -print | sort)

while IFS= read -r dockerfile; do
    update_dockerfile "$dockerfile"
done < <(find "${ROOT_DIR}/services" -name Dockerfile -print | sort)

echo "Go version sync complete."
