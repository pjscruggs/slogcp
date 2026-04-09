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
Usage: sync-env-from-gcloud.sh [options]

Writes `.e2e/local/.env` based on the active or specified GCP project.
Options override values loaded from the existing env file.

Options:
  --env-file PATH
  --project ID
  --region REGION
  --artifact-registry-repo REPO
  --gcs-bucket-name BUCKET
  --runtime-service-account EMAIL
  --caller-service-account EMAIL
  --trace-pubsub-topic TOPIC
  --trace-pubsub-subscription SUBSCRIPTION
  --repo-full-name OWNER/REPO
  --sha SHA
  --lib-run-id ID
  --e2e-run-id ID
  --pr-number NUMBER
  --check-run-id ID
  --help
EOF
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/common.sh"

require_command gcloud

ENV_FILE="$(resolve_env_file_from_args "$@")"
load_env_file "$ENV_FILE"

GCP_PROJECT_ID="${GCP_PROJECT_ID:-}"
RUN_REGION="${RUN_REGION:-}"
ARTIFACT_REGISTRY_REPO="${ARTIFACT_REGISTRY_REPO:-}"
GCS_BUCKET_NAME="${GCS_BUCKET_NAME:-}"
E2E_SERVICE_ACCOUNT="${E2E_SERVICE_ACCOUNT:-}"
E2E_CALLER_SERVICE_ACCOUNT="${E2E_CALLER_SERVICE_ACCOUNT:-}"
TRACE_PUBSUB_TOPIC="${TRACE_PUBSUB_TOPIC:-}"
TRACE_PUBSUB_SUBSCRIPTION="${TRACE_PUBSUB_SUBSCRIPTION:-}"
LIB_REPO_FULL_NAME="${LIB_REPO_FULL_NAME:-}"
PR_SHA="${PR_SHA:-}"
LIB_RUN_ID="${LIB_RUN_ID:-}"
E2E_RUN_ID="${E2E_RUN_ID:-}"
PR_NUMBER="${PR_NUMBER:-}"
CHECK_RUN_ID="${CHECK_RUN_ID:-}"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --env-file)
            shift 2
            ;;
        --env-file=*)
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
        --help|-h)
            usage
            exit 0
            ;;
        *)
            die "unknown option: $1"
            ;;
    esac
done

if [[ -z "$GCP_PROJECT_ID" ]]; then
    GCP_PROJECT_ID="$(current_gcloud_project)"
fi
if [[ -z "$GCP_PROJECT_ID" ]]; then
    die "GCP project is required; set it in gcloud, the env file, or pass --project"
fi
if [[ -z "$RUN_REGION" ]]; then
    RUN_REGION="us-central1"
fi
if [[ -z "$ARTIFACT_REGISTRY_REPO" ]]; then
    ARTIFACT_REGISTRY_REPO="${RUN_REGION}-docker.pkg.dev/${GCP_PROJECT_ID}/slogcp-images"
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
    LIB_REPO_FULL_NAME="$(detect_repo_full_name)"
fi
if [[ -z "$LIB_RUN_ID" ]]; then
    LIB_RUN_ID="local"
fi
if [[ -z "$CHECK_RUN_ID" ]]; then
    CHECK_RUN_ID="0"
fi

PROJECT_NUMBER="$(gcloud projects describe "$GCP_PROJECT_ID" --format='value(projectNumber)' 2>/dev/null || true)"

if [[ -z "$GCS_BUCKET_NAME" ]]; then
    for candidate in "${GCP_PROJECT_ID}-slogcp-e2e-artifacts" "slogcp-e2e-artifacts"; do
        if gcloud storage buckets describe "gs://${candidate}" --project "$GCP_PROJECT_ID" >/dev/null 2>&1; then
            GCS_BUCKET_NAME="$candidate"
            break
        fi
    done
fi
if [[ -z "$GCS_BUCKET_NAME" ]]; then
    GCS_BUCKET_NAME="${GCP_PROJECT_ID}-slogcp-e2e-artifacts"
fi

cat > "$ENV_FILE" <<EOF
# Generated by .e2e/local/scripts/sync-env-from-gcloud.sh
# Project number: ${PROJECT_NUMBER}

GCP_PROJECT_ID=${GCP_PROJECT_ID}
RUN_REGION=${RUN_REGION}

ARTIFACT_REGISTRY_REPO=${ARTIFACT_REGISTRY_REPO}
GCS_BUCKET_NAME=${GCS_BUCKET_NAME}

E2E_SERVICE_ACCOUNT=${E2E_SERVICE_ACCOUNT}
E2E_CALLER_SERVICE_ACCOUNT=${E2E_CALLER_SERVICE_ACCOUNT}

LIB_REPO_FULL_NAME=${LIB_REPO_FULL_NAME}
LIB_RUN_ID=${LIB_RUN_ID}
PR_SHA=${PR_SHA}
E2E_RUN_ID=${E2E_RUN_ID}
PR_NUMBER=${PR_NUMBER}
CHECK_RUN_ID=${CHECK_RUN_ID}

TRACE_PUBSUB_TOPIC=${TRACE_PUBSUB_TOPIC}
TRACE_PUBSUB_SUBSCRIPTION=${TRACE_PUBSUB_SUBSCRIPTION}
EOF

note "Wrote ${ENV_FILE}"
