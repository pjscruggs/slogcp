# Understanding slogcp benchmarks

The [README](../README.md#performance) reports the cost of handling common log
records. Use these measurements to understand how structured attributes, trace
correlation, and stack capture affect logging work in your application.

## Reading the results

Each row shows the median of ten one-second Go benchmark runs.

| Metric | Meaning |
| --- | --- |
| `ns/op` | Nanoseconds spent handling one record |
| `B/op` | Bytes allocated while handling one record |
| `allocs/op` | Heap allocations made while handling one record |
| Time range | The smallest and largest per-operation times across the ten runs |

The time range shows variation between runs. It is not a confidence interval or
a p95/p99 request latency. The report identifies the source commit, Go version,
runner image, CPU, and command used to collect the results. Check that context
when comparing reports because hosted runner hardware and load can change.

## What each case measures

| Case | Record and configuration |
| --- | --- |
| `Typical` | Four ordinary structured attributes |
| `NestedMixed` | Nested groups, a timestamp, and a duration |
| `TraceAbsent` | A small record without an active trace |
| `TracePresent` | The same record with an active OpenTelemetry span context |
| `ErrorStackDisabled` | An error without automatic stack capture |
| `ErrorStackEnabled` | The same error with automatic stack capture |

The trace pair shows the work added by correlation. The error pair shows the
work added by capturing a stack. Use the [configuration
reference](CONFIGURATION.md) to choose the behavior your service needs.

These cases call the JSON handler with prebuilt records and an `io.Discard`
writer. Timing covers attribute processing and JSON encoding. It excludes
handler setup, record construction, output I/O, and Cloud Logging ingestion.
Timestamp emission and service context are enabled for every case.

A full logging call also constructs the record and writes to its destination.
File output, buffering, and stdout collection add work that this table does not
measure.

## Comparing logging destinations

The [Cloud Run comparison suite](../.benchmarks/README.md) runs a batch
application with several logging configurations.

| Configuration | Destination |
| --- | --- |
| slogcp | Structured JSON to stdout |
| Google client with `RedirectAsJSON` | Structured JSON to stdout |
| Google client with buffered API logging | Cloud Logging API |
| No logging | Application work without log output |

The two stdout configurations share Cloud Run's collection path. Buffered API
logging can return before a record has been sent, so its producer timing and
final flush time describe different parts of the work. Use completed throughput
when comparing a full batch, and check delivery verification alongside the
performance results.

These are batch application measurements. They do not include an HTTP listener,
Cloud Run ingress, autoscaling, or a network load generator. Process CPU
excludes the platform's log collector. Compare matching payloads and concurrency
levels when using them to evaluate a logging destination.

## Measuring your application

Use representative records and the output destination your application will use.
Include any redaction, source locations, trace fields, and error stacks that it
needs. The [examples](../.examples) provide runnable starting points, and the
[usage guide](USAGE.md) covers application setup and shutdown.

When comparing versions, run both on the same machine with the same toolchain
and configuration. Alternate runs between the two versions to reduce the effect
of changing load. [`benchstat`][benchstat] can summarize repeated Go benchmark
runs and compare the results.

For buffered logging, measure both producer time and the time needed to flush
pending records. Verify how many records reach the destination. An application
latency or cost estimate needs those delivery measurements as well as the
handler timings.

[benchstat]:
  https://pkg.go.dev/golang.org/x/perf/cmd/benchstat
