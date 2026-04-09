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

# Extract metadata from slogcp repository for template substitution
# Usage: extract-slogcp-metadata.sh <slogcp-dir> <pr-sha> <pr-number> [source-mode] [slogcp-ref-override]

SLOGCP_DIR="${1:-}"
PR_SHA="${2:-}"
PR_NUMBER="${3:-}"
SOURCE_MODE="${4:-github}"
SLOGCP_REF_OVERRIDE="${5:-}"
SLOGCP_COMMIT=""

# Enable debug mode if requested
[[ "${DEBUG:-}" == "true" ]] && set -x

if [[ -z "$SLOGCP_DIR" ]]; then
    echo "Error: slogcp directory path required as first argument" >&2
    exit 1
fi

echo "=== Extracting metadata from slogcp repository ==="

# Resolve Go version from workspace bootstrap (preferred) or canonical file
if [[ -f /workspace/go_version.txt ]]; then
    GO_VERSION=$(tr -d '[:space:]' < /workspace/go_version.txt)
    SOURCE_DESC="/workspace/go_version.txt"
else
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    GO_VERSION_FILE="${GO_VERSION_FILE:-$SCRIPT_DIR/.toolchain/go.version}"
    if [[ ! -f "$GO_VERSION_FILE" ]]; then
        echo "Error: Go version file not found at $GO_VERSION_FILE" >&2
        exit 1
    fi
    GO_VERSION=$(tr -d '[:space:]' < "$GO_VERSION_FILE")
    SOURCE_DESC="$GO_VERSION_FILE"
    echo "$GO_VERSION" > /workspace/go_version.txt
fi

if [[ -z "$GO_VERSION" ]]; then
    echo "Error: Go version value is empty" >&2
    exit 1
fi
echo "Using Go version ($SOURCE_DESC): $GO_VERSION"

if [[ ! -f "$SLOGCP_DIR/go.mod" ]]; then
    echo "Error: go.mod not found in $SLOGCP_DIR" >&2
    exit 1
fi

# Determine slogcp reference ({{SLOGCP_GIT_REF}})
if [[ -n "$SLOGCP_REF_OVERRIDE" ]]; then
    SLOGCP_REF="$SLOGCP_REF_OVERRIDE"
    echo "Using provided slogcp reference override: $SLOGCP_REF"
elif [[ -n "$PR_SHA" ]]; then
    # For PR builds, use the specific commit
    SLOGCP_COMMIT="$PR_SHA"
    echo "Using PR SHA for slogcp: $SLOGCP_COMMIT"
else
    # For main builds, get the current commit from the checked out repo
    pushd "$SLOGCP_DIR" > /dev/null
    SLOGCP_COMMIT=$(git rev-parse HEAD)
    popd > /dev/null
    echo "Using main branch commit for slogcp: $SLOGCP_COMMIT"
fi

if [[ -z "$SLOGCP_REF_OVERRIDE" ]]; then
    # If the reference already looks like a semantic version, use it directly.
    if [[ "$SLOGCP_COMMIT" =~ ^v[0-9]+\.[0-9]+\.[0-9]+.*$ ]]; then
        SLOGCP_REF="$SLOGCP_COMMIT"
    else
        # Convert commit hash into a pseudo-version so go modules accept it
        pushd "$SLOGCP_DIR" > /dev/null
        COMMIT_DATE=$(git show -s --format=%cI "$SLOGCP_COMMIT")
        popd > /dev/null
        if [[ -z "$COMMIT_DATE" ]]; then
            echo "Error: could not determine commit date for $SLOGCP_COMMIT" >&2
            exit 1
        fi
        COMMIT_TIME=$(date -u -d "$COMMIT_DATE" +%Y%m%d%H%M%S)
        SHORT_COMMIT=${SLOGCP_COMMIT:0:12}
        SLOGCP_REF="v0.0.0-${COMMIT_TIME}-${SHORT_COMMIT}"
    fi
fi

echo "$SLOGCP_REF" > /workspace/slogcp_reference.txt

# Store build type for later use
if [[ "$SOURCE_MODE" == "local" ]]; then
    echo "local" > /workspace/build_type.txt
elif [[ -n "$PR_SHA" ]]; then
    echo "pr" > /workspace/build_type.txt
else
    echo "main" > /workspace/build_type.txt
fi

echo "=== Metadata extraction complete ==="
echo "  Go version: $GO_VERSION"
echo "  Slogcp reference: $SLOGCP_REF"
echo "  Build type: $(cat /workspace/build_type.txt)"
