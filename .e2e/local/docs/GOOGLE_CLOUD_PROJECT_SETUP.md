# Google Cloud Setup For Local `.e2e`

This guide explains how to set up your own Google Cloud project so you can run `slogcp/.e2e` from a developer machine using the same Cloud Build + Cloud Run based e2e path that GitHub CI uses.

## What Your Project Needs

Your project needs these building blocks:

- enabled Google Cloud APIs for Cloud Build, Cloud Run, Artifact Registry, Storage, IAM, Pub/Sub, Logging, and Trace
- one runtime service account for the deployed Cloud Run services
- one caller/build service account for Cloud Build and the e2e harness job
- one Artifact Registry Docker repository for test images
- one GCS bucket for test artifacts
- Pub/Sub permissions for per-run trace test resources

## Suggested Variables

Set variables like these before running the setup commands:

```bash
PROJECT_ID="your-project-id"
REGION="us-central1"

ARTIFACT_REPO="slogcp-images"
ARTIFACT_REGISTRY_REPO="${REGION}-docker.pkg.dev/${PROJECT_ID}/${ARTIFACT_REPO}"
ARTIFACT_BUCKET="${PROJECT_ID}-slogcp-e2e-artifacts"

RUNTIME_SA="core-log-app-runtime@${PROJECT_ID}.iam.gserviceaccount.com"
CALLER_SA="custom-cloudbuild-runner@${PROJECT_ID}.iam.gserviceaccount.com"

TRACE_TOPIC="slogcp-trace-pubsub"
TRACE_SUBSCRIPTION="slogcp-trace-pubsub-sub"

YOUR_MEMBER="user:you@example.com"
```

`TRACE_TOPIC` and `TRACE_SUBSCRIPTION` are base names. The e2e flow creates run-scoped Pub/Sub resources from them and cleans those up at the end of the run.

## 1. Enable APIs

```bash
gcloud services enable \
  artifactregistry.googleapis.com \
  cloudbuild.googleapis.com \
  cloudtrace.googleapis.com \
  iam.googleapis.com \
  iamcredentials.googleapis.com \
  logging.googleapis.com \
  pubsub.googleapis.com \
  run.googleapis.com \
  storage.googleapis.com \
  --project "${PROJECT_ID}"
```

## 2. Create Service Accounts

```bash
gcloud iam service-accounts create core-log-app-runtime \
  --display-name "Core Logging Target App Runtime SA" \
  --project "${PROJECT_ID}"

gcloud iam service-accounts create custom-cloudbuild-runner \
  --display-name "Custom Cloud Build Runner SA" \
  --project "${PROJECT_ID}"
```

## 3. Grant Project Roles

Grant roles to the runtime service account:

```bash
for role in \
  roles/cloudtrace.agent \
  roles/logging.logWriter \
  roles/pubsub.publisher \
  roles/pubsub.subscriber; do
  gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member "serviceAccount:${RUNTIME_SA}" \
    --role "${role}"
done
```

Grant roles to the caller/build service account:

```bash
for role in \
  roles/artifactregistry.writer \
  roles/cloudtrace.viewer \
  roles/logging.logWriter \
  roles/logging.viewer \
  roles/pubsub.editor \
  roles/run.admin; do
  gcloud projects add-iam-policy-binding "${PROJECT_ID}" \
    --member "serviceAccount:${CALLER_SA}" \
    --role "${role}"
done
```

For Storage, the simplest path is to grant object admin at the bucket level after the bucket is created.

## 4. Allow The Caller Identity To Use Service Accounts

Allow the caller/build identity to deploy services that run as the runtime service account:

```bash
gcloud iam service-accounts add-iam-policy-binding "${RUNTIME_SA}" \
  --member "serviceAccount:${CALLER_SA}" \
  --role "roles/iam.serviceAccountUser" \
  --project "${PROJECT_ID}"
```

Allow the caller/build identity to run jobs as itself:

```bash
gcloud iam service-accounts add-iam-policy-binding "${CALLER_SA}" \
  --member "serviceAccount:${CALLER_SA}" \
  --role "roles/iam.serviceAccountUser" \
  --project "${PROJECT_ID}"
```

Allow your user account to submit Cloud Builds as the caller/build service account:

```bash
gcloud iam service-accounts add-iam-policy-binding "${CALLER_SA}" \
  --member "${YOUR_MEMBER}" \
  --role "roles/iam.serviceAccountUser" \
  --project "${PROJECT_ID}"
```

## 5. Create Artifact Registry And The Artifact Bucket

Create the Docker repository:

```bash
gcloud artifacts repositories create "${ARTIFACT_REPO}" \
  --repository-format docker \
  --location "${REGION}" \
  --description "Docker repository for slogcp e2e images" \
  --project "${PROJECT_ID}"
```

Grant repo-level writer access:

```bash
gcloud artifacts repositories add-iam-policy-binding "${ARTIFACT_REPO}" \
  --location "${REGION}" \
  --member "serviceAccount:${CALLER_SA}" \
  --role "roles/artifactregistry.writer" \
  --project "${PROJECT_ID}"
```

Create the artifacts bucket:

```bash
gcloud storage buckets create "gs://${ARTIFACT_BUCKET}" \
  --project "${PROJECT_ID}" \
  --location "${REGION}" \
  --uniform-bucket-level-access
```

Grant artifact write access on the bucket:

```bash
gcloud storage buckets add-iam-policy-binding "gs://${ARTIFACT_BUCKET}" \
  --member "serviceAccount:${CALLER_SA}" \
  --role "roles/storage.objectAdmin" \
  --project "${PROJECT_ID}"
```

Reasonable optional bucket settings:

- enable public access prevention
- enable Autoclass
- add a lifecycle rule for old artifacts

## Local Workflow

Start from the checked-in template:

```bash
cp .e2e/local/.env.template .e2e/local/.env
```

Populate `.env` from your GCP project:

```bash
.e2e/local/scripts/sync-env-from-gcloud.sh --project "${PROJECT_ID}"
```

Submit the Cloud Build using `.env` values:

```bash
.e2e/local/scripts/submit-cloud-build.sh
```

Local runs use the current checkout as the slogcp source.

Flags override `.env` values per run. Example:

```bash
.e2e/local/scripts/submit-cloud-build.sh \
  --project "${PROJECT_ID}" \
  --region "${REGION}" \
  --sha "$(git rev-parse HEAD)" \
  --e2e-run-id "manual-$(date -u +%Y%m%dT%H%M%S)"
```
