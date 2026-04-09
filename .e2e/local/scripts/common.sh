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

LOCAL_SCRIPTS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
LOCAL_DIR="$(cd "${LOCAL_SCRIPTS_DIR}/.." && pwd -P)"
REPO_ROOT="$(cd "${LOCAL_DIR}/../.." && pwd -P)"
DEFAULT_ENV_FILE="${LOCAL_DIR}/.env"

note() {
    printf '[local-e2e] %s\n' "$*" >&2
}

warn() {
    printf '[local-e2e] warning: %s\n' "$*" >&2
}

die() {
    printf '[local-e2e] error: %s\n' "$*" >&2
    exit 1
}

require_command() {
    command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

resolve_env_file_from_args() {
    local env_file="$DEFAULT_ENV_FILE"
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env-file)
                [[ $# -ge 2 ]] || die "missing value for --env-file"
                env_file="$2"
                shift 2
                ;;
            --env-file=*)
                env_file="${1#*=}"
                shift
                ;;
            *)
                shift
                ;;
        esac
    done
    printf '%s\n' "$env_file"
}

load_env_file() {
    local env_file="$1"
    if [[ ! -f "$env_file" ]]; then
        return 0
    fi

    set -a
    # shellcheck disable=SC1090
    source "$env_file"
    set +a
}

strip_env_file_args() {
    local forwarded=()
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env-file)
                [[ $# -ge 2 ]] || die "missing value for --env-file"
                shift 2
                ;;
            --env-file=*)
                shift
                ;;
            *)
                forwarded+=("$1")
                shift
                ;;
        esac
    done
    if [[ ${#forwarded[@]} -gt 0 ]]; then
        printf '%s\0' "${forwarded[@]}"
    fi
}

current_gcloud_project() {
    gcloud config get-value project 2>/dev/null | tr -d '\r'
}

detect_repo_full_name() {
    local origin_url path
    origin_url="$(git -C "$REPO_ROOT" remote get-url origin 2>/dev/null || true)"
    case "$origin_url" in
        https://github.com/*) path="${origin_url#https://github.com/}" ;;
        git@github.com:*) path="${origin_url#git@github.com:}" ;;
        ssh://git@github.com/*) path="${origin_url#ssh://git@github.com/}" ;;
        *) return 0 ;;
    esac
    path="${path%.git}"
    printf '%s\n' "$path"
}
