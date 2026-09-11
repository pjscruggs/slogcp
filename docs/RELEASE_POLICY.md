# slogcp release policy

[slogcp](../README.md) provides a Go `log/slog` handler and HTTP, gRPC, and Pub/Sub
integrations for Google Cloud. The library keeps an intentional Go compatibility
floor and dependency minimums. Security repairs can advance those minimums
through automated patch releases. Examples and development tools update
independently.

A release identifies an exact source commit, passes local validation, and
requires matching cloud test evidence when the release affects library or E2E
inputs. The release check covers changes since the previous verified published
release, including changes merged before the version bump.

## Go compatibility and dependency requirements

The library's compatibility floor is Go 1.26, declared as `go 1.26.0` in
[`go.mod`](../go.mod). Renovate does not update that directive. Raising it requires
a deliberate compatibility decision.

[The `toolchain` directive](https://go.dev/doc/toolchain) selects the preferred
compiler for work on slogcp itself. It advances independently of the
compatibility floor. CI tests the library with a patched compiler on the Go 1.26
line and with the preferred compiler. Those tests set `GOTOOLCHAIN=local` and
verify the running compiler, so automatic toolchain switching cannot conceal a
floor-compatibility failure.

Dependency requirements are lower bounds, not a lockfile for consuming
applications. An application's other dependencies may select newer versions
under [Go's minimal version
selection](https://go.dev/ref/mod#minimal-version-selection). Routine upstream
releases do not automatically increase slogcp's library requirements. A security
repair or a library correctness fix can justify an increase; using a newer
compiler or example dependency does not.

The optional
[`slogcp-grpc-adapter`](https://github.com/pjscruggs/slogcp-grpc-adapter) is
released separately. Keeping that integration in its own module leaves
go-grpc-middleware out of the core slogcp dependency graph.

## Automated maintenance and release intent

[`renovate.json`](../renovate.json) separates library dependencies, the preferred
Go toolchain, examples, CI tools, GitHub Actions, and E2E Docker dependencies.
Eligible updates merge through Renovate after validation, without a routine
PR-approval step. Renovate rebases branches that fall behind `main` and uses
squash merging.

For reported library vulnerabilities, Renovate is configured to select the
lowest fixed version, update direct and indirect requirements, and use native Go
tooling to tidy the result. The repair may require additional dependency changes
to satisfy the module graph. It must still pass the library's compatibility
tests.

A library security PR also increments the patch version in
[`version.go`](../version.go). That version change is part of the candidate being
validated. The release publisher does not make a second version commit or open a
separate release PR. Security repairs to examples or CI tools do not receive a
library version bump.

| Change | Maintenance policy | Library release intent |
| --- | --- | --- |
| Library security dependency repair | Lowest fixed candidate, native Go dependency resolution, required validation, and automerge | Patch increment in the repair PR |
| Other library fix or API change | Maintainers select the change and its validation | Explicit version increment appropriate to the complete release |
| Preferred Go toolchain | Update and test the compiler; retain the library's `go` directive | None |
| Example Go versions and dependencies | Track the latest stable Go release and compatible dependencies; validate the example modules | None |
| CI tools and GitHub Actions | Update separately from the library graph; execute the changed tools or actions | None |
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
stable `v0.x.y` and `v1.x.y` versions for the unsuffixed module path; prerelease
and build-metadata versions are outside this automated publication path.

## Validation before merge

The [`Validation Pipeline`](../.github/workflows/validation_pipeline.yml) checks
out immutable commit SHAs. Its normal local validation includes the
compatibility-floor and preferred-compiler tests, race detection, module
tidiness, formatting, modernization, linting, license headers, and
`govulncheck`. It also tests the release and CI helpers, generated E2E harness,
and checked-in example modules.

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
require `Local Validation Policy` and `E2E Tests (GCP)`. They use non-strict
status checks. The workflow's final freshness check is not an atomic merge
guarantee: `main` can advance between that check and the merge. The release
workflow separately validates the resulting release commit.

### Cloud E2E coverage

Cloud E2E tests deployed logging and tracing behavior on Google Cloud. It is
used for library and integration changes that those tests exercise. Recognized
example-dependency, preferred-toolchain-only, and CI-tool updates use local
validation without launching cloud E2E.

The PR workflow classifies changes by paths and update scope. Its CI-only
exemption also covers `.github/` changes without module metadata. That exemption
is not evidence that a changed cloud submission or authentication path executed.
Changes to those mechanisms need validation of the affected path; the test
record should identify that execution.

Eligible same-repository Renovate PRs use the automatic E2E route. Other PRs
that need cloud validation use the authorized [manual E2E
workflow](../.github/workflows/manual-e2e-trigger.yml). This authorization controls
access to the cloud runner, rather than adding routine release approval to
automated dependency updates.

The required E2E check can report success because cloud tests completed or
because the PR's scope does not require them. Only an actual qualifying
execution receipt can satisfy the release-time cloud evidence requirement.

## Validation of the complete release

[`release_policy.py`](../.github/scripts/release_policy.py) resolves the previous
lower-version, published stable release on the candidate's first-parent history.
It verifies that release's annotated tag and signature and uses the tag's commit
as the comparison boundary. A moving `main` reference, a draft release, or the
release PR's immediate base is not that boundary.

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

[`Auto Release`](../.github/workflows/auto-release.yml) runs when a push to `main`
changes `version.go`. A comment-only edit or an unchanged version does not
publish a release. Manual dispatch is restricted to a mainline
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

The update rules are in [`renovate.json`](../renovate.json), with candidate scope
and version checks in
[`validate_renovate_pr.py`](../.github/scripts/validate_renovate_pr.py). The
[validation workflow](../.github/workflows/validation_pipeline.yml), [release
policy code](../.github/scripts/release_policy.py), and [release
workflow](../.github/workflows/auto-release.yml) define the checks described above.
Changes to those rules should update this document in the same PR.
