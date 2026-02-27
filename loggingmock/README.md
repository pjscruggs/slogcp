# loggingmock

`loggingmock` is the ingestion-shape fixture used by `slogcp`'s tests.

## Parsing Model

The central helper, `TransformLogEntryJSON`, models the parsing step that reads
a JSON log entry and decides which fields are elevated from `jsonPayload` to
top-level `LogEntry` fields.

The transformation rules are based on the Cloud Logging Fluent plugin source
([`out_google_cloud.rb`](https://github.com/GoogleCloudPlatform/fluent-plugin-google-cloud/blob/master/lib/fluent/plugin/out_google_cloud.rb)), and are refined
using direct observations of backend field-handling behavior. It is a focused best-effort model of this parsing/elevation stage, not a full
reproduction of every step of the GCP backend's `stdout`-to-`LogEntry` flow.

The fixture focuses on promotion behavior such as:

- `time`/`timestamp` normalization
- severity normalization and promotion
- `httpRequest` parsing, promotion, pruning, and residual-field handling
- elevation of trace/span/operation/source-location/labels keys

## Test Usage

`slogcp`'s tests use it as a deterministic reference:

1. Emit JSON with `slogcp`.
2. Wrap that output as `{"jsonPayload": ...}`.
3. Run `TransformLogEntryJSON`.
4. Assert promoted top-level fields and remaining `jsonPayload` shape.
