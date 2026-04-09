# Local `.e2e`

This folder is the local entrypoint for running `slogcp/.e2e` against a Google Cloud project from a developer machine.

It is intentionally thin. The goal is to reuse the same Cloud Build and Cloud Run path that GitHub CI uses, while letting a local developer supply project- specific configuration and build from the current local checkout.

## Start Here

If you need to bootstrap a project or understand the required IAM/resources, read [GOOGLE_CLOUD_PROJECT_SETUP.md](docs/GOOGLE_CLOUD_PROJECT_SETUP.md).

If your project already exists:

```bash
cp .e2e/local/.env.template .e2e/local/.env
.e2e/local/scripts/sync-env-from-gcloud.sh --project your-project-id
.e2e/local/scripts/submit-cloud-build.sh
```

CLI flags override `.env` values on each run.

## What's Here

- [`.env.template`](.env.template): starting point for local config
- [`.env`](.env): ignored local settings for your project
- [`scripts/sync-env-from-gcloud.sh`](scripts/sync-env-from-gcloud.sh): populate/update `.env` from a GCP project
- [`scripts/submit-cloud-build.sh`](scripts/submit-cloud-build.sh): submit the `.e2e` Cloud Build using local config
- [`docs/`](docs): deeper setup and project-shape documentation

## Notes

- Local runs reuse the shared `.e2e` Cloud Build and Cloud Run path.
- Local runs stage the current checkout into Cloud Build.
- The local `.env` file is ignored by git; keep secrets and project-specific values there.
- If you need a one-off change for a run, prefer passing flags to the scripts instead of editing checked-in files.
