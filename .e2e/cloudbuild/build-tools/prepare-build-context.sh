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

# Prepare a Docker build context using generated module manifests.
# Usage:
#   prepare-build-context.sh <service-source-dir> <output-dir> <slogcp-graph-dir> [local-slogcp-dir] [extra-source-dir1,extra-source-dir2,...]

SERVICE_SOURCE_DIR="${1:-}"
OUTPUT_DIR="${2:-}"
SLOGCP_GRAPH_DIR="${3:-}"
LOCAL_SLOGCP_DIR="${4:-}"
EXTRA_SOURCE_DIRS="${5:-}"

preview_build_context_files() {
    local dir="$1"
    if ! { find -- "$dir" -type f -print | head -n 20; }; then
        local statuses=("${PIPESTATUS[@]}")
        if [[ ${#statuses[@]} -ge 2 && ${statuses[1]} -eq 0 && ${statuses[0]} -eq 141 ]]; then
            return 0
        fi
        return "${statuses[0]:-1}"
    fi
}

copy_service_sources() {
    local src_dir="$1"
    local dest_dir="$2"

    pushd "$src_dir" > /dev/null
    find . -type f \
        ! -name "go.mod" \
        ! -name "go.sum" \
        ! -name "go.mod.template" \
        -exec cp --parents {} "$dest_dir" \;
    popd > /dev/null
}

copy_slogcp_workspace() {
    local src_dir="$1"
    local dest_dir="$2"

    mkdir -p "$dest_dir/slogcp"
    pushd "$src_dir" > /dev/null
    find . -type f \( -name "*.go" -o -name "go.mod" -o -name "go.sum" \) \
        -exec cp --parents {} "$dest_dir/slogcp" \;
    popd > /dev/null
}

copy_extra_source_dir() {
    local extra_dir="$1"
    local dest_dir="$2"
    local base_name

    base_name="$(basename "$extra_dir")"
    mkdir -p "$dest_dir/$base_name"
    cp -R "$extra_dir"/. "$dest_dir/$base_name"/
}

if [[ -z "$SERVICE_SOURCE_DIR" ]] || [[ -z "$OUTPUT_DIR" ]] || [[ -z "$SLOGCP_GRAPH_DIR" ]]; then
    echo "Error: Usage: prepare-build-context.sh <service-source-dir> <output-dir> <slogcp-graph-dir> [local-slogcp-dir] [extra-source-dir1,extra-source-dir2,...]" >&2
    exit 1
fi

if [[ ! -d "$SERVICE_SOURCE_DIR" ]]; then
    echo "Error: service source directory not found: $SERVICE_SOURCE_DIR" >&2
    exit 1
fi
if [[ ! -d "$SLOGCP_GRAPH_DIR" ]]; then
    echo "Error: slogcp graph directory not found: $SLOGCP_GRAPH_DIR" >&2
    exit 1
fi
if [[ ! -f "$SLOGCP_GRAPH_DIR/go.mod" ]]; then
    echo "Error: slogcp graph directory does not contain go.mod: $SLOGCP_GRAPH_DIR" >&2
    exit 1
fi

BUILD_TYPE="$(cat /workspace/build_type.txt 2>/dev/null || echo "main")"

echo "=== Preparing Docker build context ==="
echo "  Service source: $SERVICE_SOURCE_DIR"
echo "  Output dir: $OUTPUT_DIR"
echo "  Build type: $BUILD_TYPE"
echo "  Slogcp graph source: $SLOGCP_GRAPH_DIR"
if [[ -n "$LOCAL_SLOGCP_DIR" ]]; then
    echo "  Local slogcp source: $LOCAL_SLOGCP_DIR"
fi
if [[ -n "$EXTRA_SOURCE_DIRS" ]]; then
    echo "  Extra sources: $EXTRA_SOURCE_DIRS"
fi

rm -rf "$OUTPUT_DIR"
mkdir -p "$OUTPUT_DIR"

copy_service_sources "$SERVICE_SOURCE_DIR" "$OUTPUT_DIR"

if [[ -f "$SERVICE_SOURCE_DIR/Dockerfile" ]]; then
    cp "$SERVICE_SOURCE_DIR/Dockerfile" "$OUTPUT_DIR/Dockerfile"
fi
if [[ -f "$SERVICE_SOURCE_DIR/.dockerignore" ]]; then
    cp "$SERVICE_SOURCE_DIR/.dockerignore" "$OUTPUT_DIR/.dockerignore"
fi

if [[ -n "$LOCAL_SLOGCP_DIR" ]]; then
    if [[ ! -d "$LOCAL_SLOGCP_DIR" ]]; then
        echo "Error: local slogcp directory not found: $LOCAL_SLOGCP_DIR" >&2
        exit 1
    fi
    copy_slogcp_workspace "$LOCAL_SLOGCP_DIR" "$OUTPUT_DIR"
fi

if [[ -n "$EXTRA_SOURCE_DIRS" ]]; then
    IFS=',' read -r -a extra_dirs <<< "$EXTRA_SOURCE_DIRS"
    for extra_dir in "${extra_dirs[@]}"; do
        trimmed_dir="$(echo "$extra_dir" | xargs)"
        if [[ -z "$trimmed_dir" ]]; then
            continue
        fi
        if [[ ! -d "$trimmed_dir" ]]; then
            echo "Error: extra source directory not found: $trimmed_dir" >&2
            exit 1
        fi
        echo "Copying extra source directory $trimmed_dir"
        copy_extra_source_dir "$trimmed_dir" "$OUTPUT_DIR"
    done
fi

cat > "$OUTPUT_DIR/.build-manifest" <<EOF
Build Context Manifest
=====================
Build Type: $BUILD_TYPE
Go Version: $(cat /workspace/go_version.txt)
Slogcp Reference: $(cat /workspace/slogcp_reference.txt)
EOF

echo ""
echo "=== Build context structure ==="
preview_build_context_files "$OUTPUT_DIR"
TOTAL_FILES=$(find -- "$OUTPUT_DIR" -type f | wc -l)
echo "... (total $TOTAL_FILES files)"

echo ""
echo "=== Build context preparation complete ==="
