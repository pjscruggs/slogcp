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

usage() {
    cat <<'EOF'
Usage: run_e2e_cloud_build.sh [options]

Options override environment variables when both are provided.

Options:
  --source-mode MODE
  --project ID
  --region REGION
  --artifact-registry-repo REPO
  --gcs-bucket-name BUCKET
  --runtime-service-account EMAIL
  --caller-service-account EMAIL
  --github-token-secret-version VERSION_NAME
  --trace-pubsub-topic TOPIC
  --trace-pubsub-subscription SUBSCRIPTION
  --repo-full-name OWNER/REPO
  --sha SHA
  --pr-number NUMBER
  --check-run-id ID
  --lib-run-id ID
  --e2e-run-id ID
  --help

Env-backed defaults:
  E2E_SOURCE_MODE
  GCP_PROJECT_ID
  RUN_REGION
  ARTIFACT_REGISTRY_REPO
  GCS_BUCKET_NAME
  E2E_SERVICE_ACCOUNT
  E2E_CALLER_SERVICE_ACCOUNT
  GITHUB_TOKEN_SECRET_VERSION
  TRACE_PUBSUB_TOPIC
  TRACE_PUBSUB_SUBSCRIPTION
  LIB_REPO_FULL_NAME
  PR_SHA
  PR_NUMBER
  CHECK_RUN_ID
  LIB_RUN_ID
  E2E_RUN_ID
EOF
}

derive_repo_full_name() {
    local origin_url path
    origin_url="$(git remote get-url origin 2>/dev/null || true)"
    case "$origin_url" in
        https://github.com/*) path="${origin_url#https://github.com/}" ;;
        git@github.com:*) path="${origin_url#git@github.com:}" ;;
        ssh://git@github.com/*) path="${origin_url#ssh://git@github.com/}" ;;
        *) return 0 ;;
    esac
    path="${path%.git}"
    printf '%s\n' "$path"
}

derive_slogcp_ref() {
    local ref="$1"
    local commit_date commit_time short_commit

    if [[ "$ref" =~ ^v[0-9]+\.[0-9]+\.[0-9]+.*$ ]]; then
        printf '%s\n' "$ref"
        return 0
    fi

    commit_date="$(git show -s --format=%cI "$ref" 2>/dev/null || true)"
    if [[ -z "$commit_date" ]]; then
        return 1
    fi

    commit_time="$(date -u -d "$commit_date" +%Y%m%d%H%M%S 2>/dev/null || true)"
    if [[ -z "$commit_time" ]]; then
        return 1
    fi

    short_commit="${ref:0:12}"
    printf 'v0.0.0-%s-%s\n' "$commit_time" "$short_commit"
}

normalize_service_account_resource() {
    local project_id="$1"
    local service_account="$2"

    if [[ -z "$service_account" ]]; then
        return 0
    fi
    if [[ "$service_account" == projects/*/serviceAccounts/* ]]; then
        printf '%s\n' "$service_account"
        return 0
    fi

    printf 'projects/%s/serviceAccounts/%s\n' "$project_id" "$service_account"
}

stage_local_build_source() {
    local staging_root="$1"

    rm -rf "$staging_root"
    mkdir -p "$staging_root"

    cp -R ".e2e/." "$staging_root/"
    mkdir -p "$staging_root/lib-repo-checkout"

    find . -type f \( -name "*.go" -o -name "go.mod" -o -name "go.sum" \) \
        ! -path './.git/*' \
        -exec cp --parents {} "$staging_root/lib-repo-checkout" \;
}

E2E_SOURCE_MODE="${E2E_SOURCE_MODE:-}"
GCP_PROJECT_ID="${GCP_PROJECT_ID:-}"
RUN_REGION="${RUN_REGION:-}"
ARTIFACT_REGISTRY_REPO="${ARTIFACT_REGISTRY_REPO:-}"
GCS_BUCKET_NAME="${GCS_BUCKET_NAME:-}"
E2E_SERVICE_ACCOUNT="${E2E_SERVICE_ACCOUNT:-}"
E2E_CALLER_SERVICE_ACCOUNT="${E2E_CALLER_SERVICE_ACCOUNT:-}"
GITHUB_TOKEN_SECRET_VERSION="${GITHUB_TOKEN_SECRET_VERSION:-}"
TRACE_PUBSUB_TOPIC="${TRACE_PUBSUB_TOPIC:-}"
TRACE_PUBSUB_SUBSCRIPTION="${TRACE_PUBSUB_SUBSCRIPTION:-}"
LIB_REPO_FULL_NAME="${LIB_REPO_FULL_NAME:-}"
PR_SHA="${PR_SHA:-}"
PR_NUMBER="${PR_NUMBER:-}"
CHECK_RUN_ID="${CHECK_RUN_ID:-}"
LIB_RUN_ID="${LIB_RUN_ID:-}"
E2E_RUN_ID="${E2E_RUN_ID:-}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --source-mode)
            E2E_SOURCE_MODE="${2:?missing value for --source-mode}"
            shift 2
            ;;
        --source-mode=*)
            E2E_SOURCE_MODE="${1#*=}"
            shift
            ;;
        --project)
            GCP_PROJECT_ID="${2:?missing value for --project}"
            shift 2
            ;;
        --project=*)
            GCP_PROJECT_ID="${1#*=}"
            shift
            ;;
        --region)
            RUN_REGION="${2:?missing value for --region}"
            shift 2
            ;;
        --region=*)
            RUN_REGION="${1#*=}"
            shift
            ;;
        --artifact-registry-repo)
            ARTIFACT_REGISTRY_REPO="${2:?missing value for --artifact-registry-repo}"
            shift 2
            ;;
        --artifact-registry-repo=*)
            ARTIFACT_REGISTRY_REPO="${1#*=}"
            shift
            ;;
        --gcs-bucket-name)
            GCS_BUCKET_NAME="${2:?missing value for --gcs-bucket-name}"
            shift 2
            ;;
        --gcs-bucket-name=*)
            GCS_BUCKET_NAME="${1#*=}"
            shift
            ;;
        --runtime-service-account)
            E2E_SERVICE_ACCOUNT="${2:?missing value for --runtime-service-account}"
            shift 2
            ;;
        --runtime-service-account=*)
            E2E_SERVICE_ACCOUNT="${1#*=}"
            shift
            ;;
        --caller-service-account)
            E2E_CALLER_SERVICE_ACCOUNT="${2:?missing value for --caller-service-account}"
            shift 2
            ;;
        --caller-service-account=*)
            E2E_CALLER_SERVICE_ACCOUNT="${1#*=}"
            shift
            ;;
        --github-token-secret-version)
            GITHUB_TOKEN_SECRET_VERSION="${2:?missing value for --github-token-secret-version}"
            shift 2
            ;;
        --github-token-secret-version=*)
            GITHUB_TOKEN_SECRET_VERSION="${1#*=}"
            shift
            ;;
        --trace-pubsub-topic)
            TRACE_PUBSUB_TOPIC="${2:?missing value for --trace-pubsub-topic}"
            shift 2
            ;;
        --trace-pubsub-topic=*)
            TRACE_PUBSUB_TOPIC="${1#*=}"
            shift
            ;;
        --trace-pubsub-subscription)
            TRACE_PUBSUB_SUBSCRIPTION="${2:?missing value for --trace-pubsub-subscription}"
            shift 2
            ;;
        --trace-pubsub-subscription=*)
            TRACE_PUBSUB_SUBSCRIPTION="${1#*=}"
            shift
            ;;
        --repo-full-name)
            LIB_REPO_FULL_NAME="${2:?missing value for --repo-full-name}"
            shift 2
            ;;
        --repo-full-name=*)
            LIB_REPO_FULL_NAME="${1#*=}"
            shift
            ;;
        --sha)
            PR_SHA="${2:?missing value for --sha}"
            shift 2
            ;;
        --sha=*)
            PR_SHA="${1#*=}"
            shift
            ;;
        --pr-number)
            PR_NUMBER="${2:?missing value for --pr-number}"
            shift 2
            ;;
        --pr-number=*)
            PR_NUMBER="${1#*=}"
            shift
            ;;
        --check-run-id)
            CHECK_RUN_ID="${2:?missing value for --check-run-id}"
            shift 2
            ;;
        --check-run-id=*)
            CHECK_RUN_ID="${1#*=}"
            shift
            ;;
        --lib-run-id)
            LIB_RUN_ID="${2:?missing value for --lib-run-id}"
            shift 2
            ;;
        --lib-run-id=*)
            LIB_RUN_ID="${1#*=}"
            shift
            ;;
        --e2e-run-id)
            E2E_RUN_ID="${2:?missing value for --e2e-run-id}"
            shift 2
            ;;
        --e2e-run-id=*)
            E2E_RUN_ID="${1#*=}"
            shift
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            usage >&2
            exit 1
            ;;
    esac
done

if [[ -z "$E2E_SOURCE_MODE" ]]; then
    E2E_SOURCE_MODE="github"
fi
if [[ -z "$GCP_PROJECT_ID" ]]; then
    GCP_PROJECT_ID="slogcp"
fi
if [[ -z "$RUN_REGION" ]]; then
    RUN_REGION="us-central1"
fi
if [[ -z "$ARTIFACT_REGISTRY_REPO" ]]; then
    ARTIFACT_REGISTRY_REPO="${RUN_REGION}-docker.pkg.dev/${GCP_PROJECT_ID}/slogcp-images"
fi
if [[ -z "$GCS_BUCKET_NAME" ]]; then
    GCS_BUCKET_NAME="slogcp-e2e-artifacts"
fi
if [[ -z "$E2E_SERVICE_ACCOUNT" ]]; then
    E2E_SERVICE_ACCOUNT="core-log-app-runtime@${GCP_PROJECT_ID}.iam.gserviceaccount.com"
fi
if [[ -z "$E2E_CALLER_SERVICE_ACCOUNT" ]]; then
    E2E_CALLER_SERVICE_ACCOUNT="custom-cloudbuild-runner@${GCP_PROJECT_ID}.iam.gserviceaccount.com"
fi
if [[ -z "$TRACE_PUBSUB_TOPIC" ]]; then
    TRACE_PUBSUB_TOPIC="slogcp-trace-pubsub"
fi
if [[ -z "$TRACE_PUBSUB_SUBSCRIPTION" ]]; then
    TRACE_PUBSUB_SUBSCRIPTION="slogcp-trace-pubsub-sub"
fi
if [[ -z "$LIB_REPO_FULL_NAME" ]]; then
    LIB_REPO_FULL_NAME="$(derive_repo_full_name)"
fi
if [[ -z "$PR_SHA" ]]; then
    PR_SHA="$(git rev-parse HEAD 2>/dev/null || true)"
fi
if [[ -z "$CHECK_RUN_ID" ]]; then
    CHECK_RUN_ID="0"
fi
if [[ -z "$LIB_RUN_ID" ]]; then
    LIB_RUN_ID="manual"
fi

if [[ -z "$LIB_REPO_FULL_NAME" ]]; then
    echo "LIB_REPO_FULL_NAME is required" >&2
    exit 1
fi
if [[ -z "$PR_SHA" ]]; then
    echo "PR_SHA is required (set PR_SHA or pass --sha)" >&2
    exit 1
fi
if [[ -z "$E2E_RUN_ID" ]]; then
    E2E_RUN_ID="$(date -u +%Y%m%dT%H%M%S)-${RANDOM}"
fi
if [[ "$E2E_SOURCE_MODE" != "github" && "$E2E_SOURCE_MODE" != "local" ]]; then
    echo "E2E_SOURCE_MODE must be 'github' or 'local' (got '$E2E_SOURCE_MODE')" >&2
    exit 1
fi
if [[ "$E2E_SOURCE_MODE" == "github" && -z "$GITHUB_TOKEN_SECRET_VERSION" ]]; then
    echo "GITHUB_TOKEN_SECRET_VERSION is required (set it in env or pass --github-token-secret-version)" >&2
    exit 1
fi

SHORT_SHA="${PR_SHA:0:7}"
BUILD_TIME_ISO="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
SLOGCP_REF_OVERRIDE=""
SOURCE_PATH=".e2e"
CONFIG_PATH=".e2e/cloudbuild/cloudbuild.yaml"
TEMP_SOURCE_ROOT=""
BUILD_SUBMIT_SERVICE_ACCOUNT=""

if [[ "$E2E_SOURCE_MODE" == "local" ]]; then
    SLOGCP_REF_OVERRIDE="$(derive_slogcp_ref "$PR_SHA" || true)"
    if [[ -z "$SLOGCP_REF_OVERRIDE" ]]; then
        echo "Failed to derive a slogcp pseudo-version from local git metadata for $PR_SHA" >&2
        exit 1
    fi

    TEMP_SOURCE_ROOT="$(mktemp -d 2>/dev/null || mktemp -d -t slogcp-e2e-local)"
    trap 'if [[ -n "$TEMP_SOURCE_ROOT" && -d "$TEMP_SOURCE_ROOT" ]]; then rm -rf "$TEMP_SOURCE_ROOT"; fi' EXIT
    stage_local_build_source "$TEMP_SOURCE_ROOT"
    SOURCE_PATH="$TEMP_SOURCE_ROOT"
    CONFIG_PATH="$TEMP_SOURCE_ROOT/cloudbuild/cloudbuild.yaml"
fi

if [[ -n "$E2E_CALLER_SERVICE_ACCOUNT" ]]; then
    BUILD_SUBMIT_SERVICE_ACCOUNT="$(normalize_service_account_resource "$GCP_PROJECT_ID" "$E2E_CALLER_SERVICE_ACCOUNT")"
fi

STREAM_TAG_DEFAULTED="main"
if [[ -n "$PR_NUMBER" ]]; then
    STREAM_TAG_DEFAULTED="pr-${PR_NUMBER}"
fi

BUILD_STATUS="FAILURE"
BUILD_ID=""
BUILD_LOG_URL=""
ARTIFACTS_URL=""

emit_outputs() {
    local run_id_for_link
    run_id_for_link="$E2E_RUN_ID"
    if [[ -z "$run_id_for_link" ]]; then
        run_id_for_link="$BUILD_ID"
    fi

    ARTIFACTS_URL="https://console.cloud.google.com/storage/browser/${GCS_BUCKET_NAME}/${STREAM_TAG_DEFAULTED}/${PR_SHA}/${run_id_for_link}/"

    if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
        {
            printf 'build_status=%s\n' "$BUILD_STATUS"
            printf 'build_id=%s\n' "$BUILD_ID"
            printf 'build_log_url=%s\n' "$BUILD_LOG_URL"
            printf 'artifacts_url=%s\n' "$ARTIFACTS_URL"
            printf 'e2e_run_id=%s\n' "$E2E_RUN_ID"
            printf 'stream_tag_defaulted=%s\n' "$STREAM_TAG_DEFAULTED"
            printf 'short_sha=%s\n' "$SHORT_SHA"
        } >> "$GITHUB_OUTPUT"
    fi
}

SUBSTITUTIONS="_LIB_REPO_FULL_NAME=${LIB_REPO_FULL_NAME},_PR_SHA=${PR_SHA},_PR_NUMBER=${PR_NUMBER},_SHORT_SHA=${SHORT_SHA},_BUILD_TIME=${BUILD_TIME_ISO},_GCP_REGION=${RUN_REGION},_ARTIFACT_REGISTRY_REPO=${ARTIFACT_REGISTRY_REPO},_GCS_BUCKET_NAME=${GCS_BUCKET_NAME},_E2E_RUN_ID=${E2E_RUN_ID},_RUNTIME_SERVICE_ACCOUNT=${E2E_SERVICE_ACCOUNT},_CALLER_SERVICE_ACCOUNT=${E2E_CALLER_SERVICE_ACCOUNT},_GITHUB_TOKEN_SECRET_VERSION=${GITHUB_TOKEN_SECRET_VERSION},_TRACE_PUBSUB_TOPIC=${TRACE_PUBSUB_TOPIC},_TRACE_PUBSUB_SUBSCRIPTION=${TRACE_PUBSUB_SUBSCRIPTION},_E2E_SOURCE_MODE=${E2E_SOURCE_MODE},_SLOGCP_REF_OVERRIDE=${SLOGCP_REF_OVERRIDE}"

echo "Submitting e2e Cloud Build for ${LIB_REPO_FULL_NAME}@${PR_SHA}"
echo "Run ID: ${E2E_RUN_ID}"
echo "Source mode: ${E2E_SOURCE_MODE}"

submit_args=(
    "$SOURCE_PATH"
    --config="$CONFIG_PATH"
    --project="$GCP_PROJECT_ID"
    --region="$RUN_REGION"
    --async
    --substitutions="$SUBSTITUTIONS"
    "--format=value(metadata.build.id)"
)
if [[ -n "$E2E_CALLER_SERVICE_ACCOUNT" ]]; then
    submit_args+=(--service-account="$BUILD_SUBMIT_SERVICE_ACCOUNT")
fi

submit_output=""
if ! submit_output="$(gcloud builds submit "${submit_args[@]}" 2>&1)"; then
    printf '%s\n' "$submit_output" >&2
    echo "Failed to start Cloud Build." >&2
    emit_outputs
    exit 1
fi

BUILD_ID="$(printf '%s\n' "$submit_output" | tr -d '\r' | tail -n 1)"

if [[ -z "$BUILD_ID" ]]; then
    printf '%s\n' "$submit_output" >&2
    echo "Failed to start Cloud Build and resolve build ID." >&2
    emit_outputs
    exit 1
fi

BUILD_LOG_URL="https://console.cloud.google.com/cloud-build/builds/${BUILD_ID}?project=${GCP_PROJECT_ID}"
echo "Cloud Build started: ${BUILD_ID}"
echo "Logs: ${BUILD_LOG_URL}"

MAX_WAIT_SECONDS=7800
POLL_INTERVAL=30
elapsed=0

while true; do
    status="$(gcloud builds describe "$BUILD_ID" --project="$GCP_PROJECT_ID" --region="$RUN_REGION" --format='value(status)' 2>/dev/null || true)"
    if [[ -z "$status" ]]; then
        status="NOT_FOUND"
    fi

    echo "Build status at $(date +%T): ${status}"

    case "$status" in
        SUCCESS|FAILURE|TIMEOUT|CANCELLED|INTERNAL_ERROR)
            BUILD_STATUS="$status"
            break
            ;;
    esac

    if [[ $elapsed -ge $MAX_WAIT_SECONDS ]]; then
        echo "Build did not complete within ${MAX_WAIT_SECONDS} seconds."
        BUILD_STATUS="TIMEOUT"
        break
    fi

    sleep $POLL_INTERVAL
    elapsed=$((elapsed + POLL_INTERVAL))
done

emit_outputs

if [[ "$BUILD_STATUS" == "SUCCESS" ]]; then
    echo "E2E Cloud Build finished successfully."
else
    echo "E2E Cloud Build finished with status: ${BUILD_STATUS}"
fi

exit 0
