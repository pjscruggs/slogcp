# slogcp release policy

[slogcp](../README.md) provides a Go `log/slog` handler and HTTP and gRPC
integrations for Google Cloud. Optional modules provide Pub/Sub helpers,
Cloud Logging API delivery, and gRPC middleware event logging. The library keeps
an intentional Go compatibility floor and dependency minimums. Security repairs
can advance those minimums through automated patch releases. Examples and
development tools update independently.

A release identifies an exact source commit, passes local validation, and
requires matching cloud test evidence when the release affects library or E2E
inputs. The release check covers changes since the previous verified published
release, including changes merged before the version bump.

## Go compatibility and dependency requirements

The library's compatibility floor is the `go` directive in
[`go.mod`](../go.mod). Renovate does not update that directive. Raising it
requires a deliberate compatibility decision.

[The `toolchain` directive](https://go.dev/doc/toolchain) selects the preferred
compiler for work on slogcp itself. It advances independently of the
compatibility floor. CI tests the library with a patched compiler on the
declared compatibility line and with the preferred compiler. Those tests set
`GOTOOLCHAIN=local` and verify the running compiler, so automatic toolchain
switching cannot conceal a floor compatibility failure.

Dependency requirements set lower bounds for consuming applications.
An application's other dependencies may select newer versions
under [Go's minimal version
selection](https://go.dev/ref/mod#minimal-version-selection). Routine upstream
releases do not automatically increase slogcp's library requirements. A security
repair or a library correctness fix can justify an increase. Using a newer
compiler or example dependency does not.

The optional modules are released separately and keep their integration
dependencies outside the core slogcp module graph.

| Module | Integration |
| --- | --- |
| [`slogcp-pubsub`](https://github.com/pjscruggs/slogcp-pubsub) | Pub/Sub trace propagation and message loggers |
| [`slogcp-grpc`](https://github.com/pjscruggs/slogcp-grpc) | Cloud Logging API delivery through the official client |
| [`slogcp-grpc-adapter`](https://github.com/pjscruggs/slogcp-grpc-adapter) | gRPC middleware event logging |

Each repository has its own dependency maintenance, validation, cloud E2E
evidence, and signed release workflow. Application examples import these
modules through ordinary Go requirements.

## Automated maintenance and release intent

[`renovate.json`](../renovate.json) separates library dependencies, the
preferred Go toolchain, examples, the independent benchmark module and runtime,
CI tools, GitHub Actions, and E2E Docker dependencies. Eligible updates merge
through Renovate after validation, without a routine PR-approval step. Renovate
rebases branches that fall behind `main` and uses squash merging.

Routine major example migrations require review. The narrower transition of the
two optional modules from a v0 pseudo-version to the same-path v1 release is
treated as first-stable adoption: Renovate runs native Go tidying for the changed
modules but does not rewrite source imports. A later `/v2` migration remains a
reviewed coordinated source change.

For reported library vulnerabilities, Renovate is configured to select the
lowest fixed version, update direct and indirect requirements, and use native Go
tooling to tidy the result. The repair may require additional dependency changes
to satisfy the module graph. It must still pass the library's compatibility
tests.

A library security PR also increments the patch version in
[`version.go`](../version.go). That version change is part of the candidate
being validated. The release publisher does not make a second version commit or
open a separate release PR. Security repairs to examples or CI tools do not
receive a library version bump.

| Change | Maintenance policy | Library release intent |
| --- | --- | --- |
| Library security dependency repair | Lowest fixed candidate, native Go dependency resolution, required validation, and automerge | Patch increment in the repair PR |
| Other library fix or API change | Maintainers select the change and its validation | Explicit version increment appropriate to the complete release |
| Preferred Go toolchain | Update and test the compiler; retain the library's `go` directive | None |
| Example Go versions and dependencies | Track the latest stable Go release and compatible dependencies; validate the example modules | None |
| Benchmark Go dependencies | Maintain the independent benchmark graph; validate its selected compiler, tidy result, tests, race detector, and vulnerability scan | None |
| Benchmark Python runtime | Build the actual Dockerfile and run the Python suite plus a no-cloud runner smoke in that image | None |
| CI tools and GitHub Actions | Update separately from the library graph; execute the changed tools or actions | None |
| README benchmark results | Documentation refresh | None (`version.go` stays unchanged) |
| E2E Docker dependencies | Validate the changed build and integration path before automerge | None |
| Library Go compatibility floor | Explicit compatibility decision, outside routine automated updates | Explicit release decision |

Non-releasing changes may appear in the source snapshot and notes of a later
library release. They do not independently cause one. Example modules use local
slogcp source during validation; their newer requirements do not redefine the
published library's minimums.

The publisher recognizes an increase in `Version` relative to the preceding
mainline commit. A label, PR title, or Conventional Commit prefix is not release
intent. For non-security library changes, maintainers select the version after
considering the complete unreleased change. The publisher accepts canonical
stable semantic versions across major releases. Module paths follow Go's
major version suffix rules. Prerelease and build metadata versions are outside
this automated publication path.

## Validation before merge

The [`Validation Pipeline`](../.github/workflows/validation_pipeline.yml) checks
out immutable commit SHAs. Its normal local validation includes the
compatibility-floor and preferred-compiler tests, race detection, module
tidiness, formatting, modernization, linting, license headers, and
`govulncheck`. It also tests the release and CI helpers, generated E2E harness,
checked-in example modules, and the independent benchmark module when its real
inputs are affected.

Benchmark validation selects the compiler declared by `.benchmarks/go.mod`,
tidies and tests that graph independently, runs its Python tests, builds the
actual `.benchmarks/Dockerfile`, and repeats the Python tests and runner-loading
smoke inside that image. It does not execute Cloud Run benchmark jobs, collect
load data, or use cloud credentials. The locally replaced slogcp requirement is
excluded from Renovate proposals; other benchmark dependencies remain visible.

CI tool versions come from [the separate tools module](../.github/tools/go.mod).
Validation invokes those tools against the candidate source. Formatters and
fixers must leave the checked-in source unchanged, and the subsequent
verification commands must pass. GitHub Action updates also have
[candidate-action smoke tests](../.github/workflows/ci-action-smoke.yml). A
successful download or version printout is not the tool's substantive
validation.

`Local Validation Policy` requires successful results from the applicable jobs.
Failure, cancellation, or an unexpected skip does not satisfy it. Latest-Go
canaries provide additional compatibility information; they do not replace the
normal required workflow.

### Current-base checks

PR validation requires the candidate to contain the observed current base
commit. Before accepting success, it rechecks the PR head and repository, the
base branch and commit, and the workflow run and attempt. A changed base,
changed head, or superseded run requires updated validation.

The [main branch rules](https://github.com/pjscruggs/slogcp/rules/15539731)
require `Local Validation Policy` and `E2E Tests (GCP)` with strict current-base
status checks. The workflow also revalidates the exact head and base before it
accepts a result. That application-level check is defense in depth, not an
atomic merge guarantee; the release workflow separately validates the resulting
release commit.

### Cloud E2E coverage

Cloud E2E tests deployed logging and tracing behavior on Google Cloud. It is
used for library and integration changes that those tests exercise. Recognized
example-dependency, preferred-toolchain-only, CI-tool, and recognized
benchmark-maintenance updates use scoped local validation without launching
root cloud E2E. README-only changes, including [benchmark results](BENCHMARKS.md),
also follow this path. They still require local validation and checks of the
current PR head and base.

The benchmark exemption is path- and shape-based. It accepts only same-repository
Renovate updates to benchmark module metadata or a single modified benchmark
Dockerfile after the required benchmark job succeeds. Benchmark source changes,
renames, mixed root and benchmark changes, unknown benchmark files, and cloud
runner/authentication changes fail closed into their stronger validation path.
Root API and module changes used through the benchmark's local replacement run
both root and benchmark validation.

The PR workflow classifies changes by paths and update scope. Its CI-only
exemption also covers `.github/` changes without module metadata. That exemption
is not evidence that a changed cloud submission or authentication path executed.
Changes to those mechanisms need validation of the affected path; the test
record should identify that execution.

Eligible same-repository Renovate PRs use the automatic E2E route. Other PRs
that need cloud validation use the authorized [manual E2E
workflow](../.github/workflows/manual-e2e-trigger.yml). This authorization
controls access to the cloud runner, rather than adding routine release approval
to automated dependency updates.

Maintainers dispatch that workflow from `main`. To validate a runner repair,
`reviewed_infrastructure_sha` may select the exact reviewed PR head. The PR must
belong to this repository and pass local validation against the current base
before runner credentials are acquired. The execution receipt records the
selected infrastructure revision. Leaving the input empty uses the workflow's
own revision on `main`.

The required E2E check can report success because cloud tests completed or
because the PR's scope does not require them. A benchmark no-cloud result proves
maintenance validation only; it is neither a cloud benchmark execution nor a
root-parity E2E receipt. Only an actual qualifying execution receipt can satisfy
the release-time cloud evidence requirement.

## Validation of the complete release

[`release_policy.py`](../.github/scripts/release_policy.py) resolves the
previous lower-version, published stable release whose commit or identical
complete Git tree appears on the candidate's first-parent history. Exact tree
equality permits a published release to serve as the baseline after its source
was squash merged. It verifies the release's annotated tag and signature and
uses the original tag commit as the comparison boundary, recording the matching
mainline commit separately. A moving `main` reference, a draft release, or the
release PR's immediate base is not that boundary. New release tags must still
target the exact release commit.

The resulting delta covers every changed path between the published release and
the candidate. Earlier unreleased library changes remain in scope even when the
last PR changes only `version.go`. Local validation runs again on the exact
release commit before signing.

The release classifier requires cloud evidence for library module metadata,
non-test library Go source, and `.e2e/` changes. It is path-based, rather than a
package-by-package analysis of which dependencies a test exercised. `version.go`
is included as library source, so a version-only release also requires
qualifying cloud evidence under this classifier.

### Reusing a PR's cloud result after merge

A squash merge changes the commit SHA without necessarily changing its files.
The publisher can reuse the merged release PR's cloud result when its head has
exactly the same Git tree as the release commit. The specified cloud build,
generator, runner, and workflow inputs must also be unchanged from the recorded
infrastructure revision.

The evidence check requires the expected E2E GitHub App, the newest matching
check, and a successful completed run of an allowed workflow with the recorded
attempt. The receipt must identify the tested root SHA, `root-parity` profile,
dependency mode `floor`, toolchain mode `repo`, and a successful result. A newer
failed or incomplete check cannot be replaced by an older success. A
latest-dependency run or a combined root-and-adapter run is not a substitute for
this profile.

This permits reuse across an identical-source squash merge and a publication
retry. It does not permit reuse across arbitrary source edits or different test
profiles. Reusing cloud evidence does not remove release-time local validation.

## Signed tags and publication

[`Auto Release`](../.github/workflows/auto-release.yml) runs when a push to
`main` changes `version.go`. A comment-only edit or an unchanged version does
not publish a release. Manual dispatch is restricted to a mainline
version-transition commit, and an explicitly requested version must match that
commit's `Version`.

After local validation and the complete-release evidence check, the publisher
creates an annotated SSH-signed tag at the exact workflow commit. It verifies
the tag locally before pushing. Before publishing the GitHub release, it also
requires GitHub to report a valid signature and checks that the tag points to
the expected commit. A conflicting tag stops publication; the workflow does not
move it.

Signing is performed in the `release-tags` environment. Publication uses a
repository-scoped GitHub App token with Contents write permission. The workflow
supports a dedicated release App or the configured E2E App; the signing identity
and key are configured separately. Cloud execution credentials and E2E check
permissions belong to their own workflow boundary.

GitHub releases are published with generated release notes and use [release
immutability](https://docs.github.com/en/code-security/concepts/supply-chain-security/immutable-releases).
The associated tag and attached assets are locked; release titles and notes can
still be corrected. Publication precedes the best-effort Go proxy indexing
request. An indexing delay does not invalidate a completed tag and release.

## Recovery

Release runs are serialized, and a later run does not cancel a running release.
The publisher retries transient API and tag-push failures. When a matching
signed tag already exists, it verifies that tag and resumes publication. When
the matching published release exists, publication is a no-op.

After a failed run, rerun the original release workflow for its
version-transition commit. Dispatching against a later `main` commit without
that transition is rejected. A retry re-evaluates validation; it does not retag
a different commit, write another version, or automatically rerun cloud E2E when
qualifying evidence is available. There is no scheduled release-recovery
reconciler in this workflow.

## Implementation and release records

The update rules are in [`renovate.json`](../renovate.json), with candidate
scope and version checks in
[`validate_renovate_pr.py`](../.github/scripts/validate_renovate_pr.py). The
[validation workflow](../.github/workflows/validation_pipeline.yml), [release
policy code](../.github/scripts/release_policy.py), and [release
workflow](../.github/workflows/auto-release.yml) define the checks described
above. Changes to those rules should update this document in the same PR.
