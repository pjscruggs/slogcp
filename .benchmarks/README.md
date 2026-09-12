# Logging comparison benchmarks on Cloud Run

This independent Go module runs the same application workload with `slogcp` and
`cloud.google.com/go/logging`. Each operation constructs request metadata,
performs the same deterministic hash work, and emits one structured record. The
runner measures application latency, completed throughput, process CPU,
allocations, memory, and logging drain time on Google Cloud Run Jobs.

These are batch application measurements. The latency values do not include an
HTTP listener, Cloud Run ingress, or a network load generator. The application
workload and dependency versions stay fixed when comparing slogcp
implementations.

## What is compared

| Mode | Destination | Interpretation |
| --- | --- | --- |
| `slogcp` | Structured stdout | Synchronous slogcp encoding and output |
| `google-stdout` | `logging.RedirectAsJSON` to stdout | Google client using the same Cloud Run collection path |
| `google-api` | Buffered Cloud Logging API | Google's default asynchronous gRPC transport, with final flush |
| `none` | No logging | Shared application work without logging |

`slogcp` and `google-stdout` also run with `sink=discard` as an
encoding/application CPU diagnostic. Discard results do not measure delivered
cloud logging.

The stdout pair is the primary library comparison. API logging has different
buffering, transport, and completion behavior. Report producer time and final
flush time separately; use the completed rate when accounting for buffered work.
Source location and stack collection are disabled for every client. See the
[Google client API](https://pkg.go.dev/cloud.google.com/go/logging) and [Cloud
Run logging guidance](https://cloud.google.com/run/docs/logging).

Each trial runs in its own child process, with setup, warmup, and a pre-trial GC
outside the measured interval. Final API flush work is included. The process's
peak RSS includes initialization and warmup; it is distinct from allocated bytes
measured during the interval. Platform collector CPU is outside the process CPU
measurements.

## Prerequisites and private configuration

Run commands from the `slogcp` checkout. Install Python 3.12 or newer, Go
1.27.1, Git, and an authenticated Google Cloud CLI. Prepare an Artifact Registry
Docker repository, a private Cloud Storage result bucket, and build/runtime
identities in the intended project. Every GCP command must name the project
explicitly.

The runtime identity needs `roles/logging.logWriter` for API-mode records and
`roles/storage.objectCreator` restricted to the result bucket's benchmark
prefix. The runner creates objects with a generation precondition and cannot
replace earlier results. The operator also needs permission to read logs/results
and to deploy/execute jobs using the runtime identity. The build identity needs
the existing project's permissions for Cloud Build, image publishing, and
staging.

Pass exact values through CLI arguments or private, ignored local configuration.
Keep project numbers, account emails, provider names, bucket names, generated
URLs, job/build descriptions, and raw evidence outside public repository files.
This workflow runs locally and does not require GitHub repository secrets. If it
is later automated, supply exact configuration through repository/environment
secrets.

The following Bash variables are placeholders; substitute private values
locally:

```bash
PROJECT=PROJECT_ID
REGION=REGION
BUCKET=RESULT_BUCKET
BUILD_SA=BUILD_SERVICE_ACCOUNT
RUNTIME_SA=RUNTIME_SERVICE_ACCOUNT
IMAGE_REPOSITORY=REGION-docker.pkg.dev/PROJECT_ID/REPOSITORY/logging-benchmark
ARCHIVE=../scratch/jsonv2-benchmarks
```

## Freeze the complete benchmark before migration

Finish the harness, run its checks, and commit the benchmark implementation
while slogcp still uses its original codec. Do not change the minimum Go version
or encoding path before collecting and preserving the baseline.

```bash
python -m unittest discover -s .benchmarks -p 'test_*.py'
go -C .benchmarks test ./...
python .benchmarks/manage.py freeze --name baseline --archive "$ARCHIVE/baseline"
```

`freeze` requires a clean checkout. It builds a Linux/amd64 executable with the
exact Go 1.27.1 toolchain and records its checksum, source archive, module
graph, source revision, build settings, and harness hashes. Use a fresh archive
directory for every freeze. Keep the baseline executable unchanged throughout
the experiment.

Build a container from that executable:

```bash
python .benchmarks/manage.py image --archive "$ARCHIVE/baseline-image" \
  --binary "baseline=$ARCHIVE/baseline/baseline" \
  --project "$PROJECT" --region "$REGION" --bucket "$BUCKET" \
  --build-sa "$BUILD_SA" --image "$IMAGE_REPOSITORY:baseline"
```

The submission is asynchronous; `build.json` contains its identifier. Wait for a
successful build and obtain `results.images[0].digest` with
`gcloud builds describe BUILD_ID --project "$PROJECT" --region "$REGION"`. Use
`"$IMAGE_REPOSITORY@sha256:IMAGE_DIGEST"` for `--image` below so the actual
executed image is immutable. Save the completed build description in the
archive.

## Pilot, then collect the baseline

First run a small pilot with a fresh run ID:

```bash
python .benchmarks/manage.py run --archive "$ARCHIVE/pilot" \
  --project "$PROJECT" --region "$REGION" --bucket "$BUCKET" \
  --runtime-sa "$RUNTIME_SA" --image "$IMAGE_REPOSITORY@sha256:IMAGE_DIGEST" \
  --run-id benchmark-pilot-unique --variant baseline \
  --repeats 1 --stdout-count 100 --discard-count 1000 --warmup 10
```

Run IDs must contain only lowercase letters, digits, and hyphens. Choose a new
ID for every execution; do not reuse result prefixes. The pilot should establish
that the app, uploads, real log delivery, and bounded resource settings work.
Use it to choose operation counts before collecting performance results. Freeze
those counts for all baseline and candidate comparisons.

For the baseline, use a fresh archive/run ID and the chosen counts. The defaults
are 10 repetitions, two payload shapes, concurrency 1 and 16, and 240 trials per
variant. With 2,500 measured stdout/API operations per trial, that produces
300,000 measured cloud records per variant, plus 24,000 warmup records at the
default warmup count of 200. A two-variant comparison doubles those totals. The
discard trials do not produce cloud records. These are configurable bounds, not
a target for maximum-volume load testing.

Each job has one task, parallelism one, two vCPU, 1 GiB memory, no retry, and a
30-minute timeout. Each application trial has a separate timeout. Run one job at
a time to avoid competing workloads and shared logging quota pressure. Jobs
retain their configuration after execution, but execution ends when the task
exits. Cloud Run uses variable CPU platforms; its [runtime
contract](https://cloud.google.com/run/docs/container-contract) describes the
resource and execution limits.

## Collect, verify, and archive before any codec change

Wait for the execution to succeed. Save its final description locally and copy
all result objects from the unique run prefix:

```bash
gcloud run jobs describe RUN_ID --project "$PROJECT" --region "$REGION" \
  --format=json > "$ARCHIVE/baseline-run/job-final.json"
gcloud storage cp --recursive "gs://$BUCKET/benchmarks/RUN_ID" \
  "$ARCHIVE/baseline-run/" --project "$PROJECT"
python .benchmarks/verify_delivery.py --project "$PROJECT" --run-id RUN_ID \
  --results "$ARCHIVE/baseline-run/RUN_ID/suite.json" \
  --output "$ARCHIVE/baseline-run/delivery"
```

The verifier accepts either `suite.json`, a directory of per-trial results, or a
manifest containing `trials` with `trial_id`, `count`, `mode`, and `sink`. It
queries both payload layouts, ignores known warmup/instrumentation entries, and
checks every expected sequence. Missing, duplicate, malformed, unexpected, or
out-of-range records fail verification. It saves compressed API evidence and
`delivery-summary.json` locally. Successful stdout writes alone do not prove
receipt by Cloud Logging.

The default verification deadline is ten minutes. The verifier uses pagination,
bounded eventual-consistency polling, and a minimum 1.4 seconds between API
calls. `--max-wait`, `--poll-seconds`, and `--page-size` adjust those bounds.
Other tools share the project's read quota, so avoid parallel polling. See
[Logging quotas](https://cloud.google.com/logging/quotas) and Google's
[efficient listing
guidance](https://cloud.google.com/logging/docs/reference/api-overview).

Queries are restricted to the suite or trial time interval with a five-minute
margin. A plain manifest without timestamps requires `--start-time` in RFC3339
format; `--end-time` can also be supplied.

Before proceeding, confirm all of the following:

- The suite is complete and every application trial has zero errors and a zero
  exit status.
- Delivery verification passed for the expected measured records.
- The frozen source/binary, completed build/execution descriptions, raw results,
  verifier evidence, and a checksum manifest are stored in private local
  scratch.
- The initial baseline summary records configuration and observed variability.

Keep this archive even after deleting temporary cloud resources. Raw logs and
resource descriptions contain private infrastructure details.

## Migrate, then compare on the same host

After the archive gate passes, update slogcp's Go minimum and JSON
implementation. Keep the benchmark workload, runner, dependencies, toolchain,
and sample counts unchanged. Run correctness tests, commit, and freeze
`candidate` in a different directory. Build a comparison image containing both
original and new executables:

```bash
python .benchmarks/manage.py freeze --name candidate --archive "$ARCHIVE/candidate"
python .benchmarks/manage.py image --archive "$ARCHIVE/comparison-image" \
  --binary "baseline=$ARCHIVE/baseline/baseline" \
  --binary "candidate=$ARCHIVE/candidate/candidate" \
  --project "$PROJECT" --region "$REGION" --bucket "$BUCKET" \
  --build-sa "$BUILD_SA" --image "$IMAGE_REPOSITORY:comparison"
```

Run the completed comparison image by digest with both `--variant baseline` and
`--variant candidate`, a fresh run ID, and the fixed baseline counts. Variants
run adjacently in a seeded randomized order on the same allocated job host. This
reduces the hardware/time confounding of comparing only an earlier baseline job
with a later candidate job. Collect and verify the comparison exactly as above.

Generate local summaries and paired bootstrap intervals with:

```bash
python .benchmarks/summarize.py "$ARCHIVE/baseline-run/RUN_ID/suite.json" \
  --paired "$ARCHIVE/comparison-run/COMPARISON_RUN_ID/suite.json" \
  --output-dir "$ARCHIVE/summary"
```

Omit `--paired` when generating the initial baseline summary. The comparison
supports multiple named candidates and preserves each result separately. Reports
include slogcp comparisons with both Google modes and separate producer
throughput from throughput that includes the final flush. The bounded API trials
do not measure steady-state API saturation.

Report library-versus-Google results separately from baseline-versus-jsonv2
results. Compare the same scenarios and include uncertainty, not only favorable
samples. Both binaries use Go 1.27.1, so a native codec improvement is not
confused with a toolchain upgrade. Use at least ten predetermined repetitions;
twenty can improve sensitivity if chosen before measurement. Do not rerun
selectively until a result becomes significant. See the [Go benchstat
guidance](https://pkg.go.dev/golang.org/x/perf/cmd/benchstat).

Application CPU and allocations can improve even when stdout collection
dominates elapsed time. Discard diagnostics help explain that distinction. A
separate authenticated service with a controlled same-region load generator is
needed to measure actual ingress latency or autoscaling; follow [Cloud Run
load-testing guidance](https://cloud.google.com/run/docs/about-load-testing) for
that experiment. These benchmark branches do not create pull requests, publish
results, merge changes, or release versions.
