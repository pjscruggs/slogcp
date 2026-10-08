# Dependency vulnerability evidence

slogcp maintains minimum working library requirements and raises them when a
security repair or correctness fix requires it. Examples and development tools
have independent module graphs. Their requirements do not become requirements
of applications importing the library.

## Release decisions

The release gate inventories every selected dependency in the root, examples,
benchmarks, development tools, and generated E2E consumers. It queries exact
versions against OSV and retains the complete inventory and raw advisory records.
It does not consider a checksum entry alone a selected dependency.

A module advisory remains visible even when its affected code is absent. The
gate can classify that finding as `not_affected`, with the VEX justification
`vulnerable_code_not_present`, only when:

- The retained advisory's identity, modification timestamp, content hash, and
  affected-package facts verify. Non-Go advisory aliases require a verified,
  reciprocal Go advisory that is also a finding for the exact selected version.
- Go advisory metadata identifies every affected package in the selected module.
  Missing metadata or unsupported package patterns cannot authorize an exemption.
- Complete package inventories, including tests and declared tool entry points,
  exclude every affected package across every build profile of the finding's
  module scope. Other scopes can select different versions of the same dependency;
  each scope's findings receive their own applicability decision.
- The evidence is bound to the source, manifests, selected inventory, compiler
  versions, build configuration, and profile hashes. Publication refreshes
  advisory evidence; evidence more than one hour old cannot authorize release.

The audited package profiles currently cover Linux amd64 with and without cgo,
and Linux arm64 without cgo, using default build tags at the module's declared Go
floor and current compiler. Additional latest-stable compiler coverage is
collected when different. A declared tool-only scope must identify its executable
roots and is audited with the current and latest-stable compilers.

An affected package appearing in any profile blocks this automatic exemption.
A clean function-level `govulncheck` result does not override that decision.
Unknown applicability also blocks publication. Findings are not deleted or
hidden through severity thresholds, blanket ignores, or artificial imports added
to retain otherwise unused version overrides.

The deprecated `golang.org/x/crypto/openpgp` advisory additionally retains its
reviewed record identity and package list. Updating the containing crypto module
does not fix an advisory with no fixed version; package absence must still be
proven. Importing the deprecated packages invalidates that exemption.

## Evidence for adopters

The automatic release workflow retains `release-selected-graph.json`, identifying
the candidate commit, base, tracked and generated dependency inventories, raw
advisories, and applicability decisions. Decisions include affected imports,
status, justification, and references to the verified package-profile hashes.
This JSON uses the VEX status and justification vocabulary; it is supporting
audit evidence, not a standalone standards-conformant VEX document or SBOM.
Workflow artifacts have limited retention, so organizations needing longer
retention should archive the report with the release source and signed tag.

A package-absence claim applies to the recorded builds of slogcp and its tools.
The published Go module is source code, not an application binary: its dependency
archives may contain other packages that slogcp does not import. Adding imports,
custom build tags, platform targets, or unrelated application dependencies can
change applicability. Adopters should run `govulncheck` for their own application
and deployment configuration, including tests where appropriate. This release
evidence cannot certify an arbitrary consuming application's complete graph.

## Basis for the policy

[Go vulnerability management](https://go.dev/doc/security/vuln/) distinguishes
module, package, and function-level findings.
[govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) documents static
analysis limitations, including reflection and unsafe code.
[CISA's VEX minimum requirements](https://www.cisa.gov/sites/default/files/2023-04/minimum-requirements-for-vex-508c.pdf)
distinguish vulnerable code absence from code that is present but cannot execute.
The automatic exemption uses package absence rather than an inference about
attacker control or unreachable functions.
