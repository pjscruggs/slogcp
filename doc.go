// Copyright 2025 Patrick J. Scruggs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package slogcp provides a structured logging solution for Go applications
// that need to integrate cleanly with Google Cloud Logging. It builds on the
// standard library's [log/slog] package and emits JSON payloads that follow
// Cloud Logging conventions while writing to any [io.Writer]. The default
// destination is stdout, keeping the library container- and developer-friendly.
//
// The primary entry point is [NewHandler], which returns an [slog.Handler]
// configured with sensible defaults:
//   - Structured JSON output that emits Cloud Logging fields such as
//     `severity`, `logging.googleapis.com/trace`, `httpRequest`, and runtime
//     metadata discovered from Cloud Run, Cloud Functions, Cloud Run Jobs,
//     App Engine, GKE, and Compute Engine environments.
//   - Optional source locations, explicit RFC3339 timestamps, and automatic
//     stack traces triggered at or above a configurable level.
//   - Seamless trace correlation helpers that leverage the extended
//     [Level] definitions (`DEBUG` through `EMERGENCY`) plus the ability to
//     emit single-letter severity aliases when desired, defaulting to full names
//     outside Cloud Run, Cloud Run Jobs, Cloud Functions, and App Engine.
//   - Timestamp emission that mirrors Cloud Logging expectations: omit the field
//     on those managed runtimes and emit millisecond precision timestamps
//     elsewhere (configurable to RFC3339Nano).
//   - Middleware hooks that allow additional [slog.Handler] layers to enrich or
//     filter records before they are encoded.
//
// Handlers can be redirected to stderr, a file managed by slogcp, or a custom
// writer. When slogcp opens the file it also provides [Handler.ReopenLogFile]
// to cooperate with external rotation tools. The handler exposes [Handler.LevelVar]
// and [Handler.SetLevel] for dynamic severity adjustments and honours many
// environment variables (for example `SLOGCP_LEVEL`,
// `SLOGCP_STACK_TRACE_ENABLED`, or `SLOGCP_TARGET`) so the same binary can
// run locally and in
// production without code changes. [ContextWithLogger] and [Logger] store and
// retrieve request-scoped loggers so integrations can pass loggers through
// call stacks.
//
// # Subpackages
//
//   - [github.com/pjscruggs/slogcp/http] offers net/http middleware and client
//     transports that derive request-scoped loggers, propagate trace context,
//     record latency/size metadata, and expose Cloud Logging friendly helpers
//     such as [slogcp/http.HTTPRequestAttr] and [slogcp/http.ScopeFromContext].
//     Legacy `X-Cloud-Trace-Context` handling is available when required.
//   - [github.com/pjscruggs/slogcp/grpc] provides client and server
//     interceptors that capture RPC metadata, surface errors with stack traces,
//     and propagate trace context. Helper functions such as
//     [slogcp/grpc.ServerOptions], [slogcp/grpc.DialOptions], and
//     [slogcp/grpc.InfoFromContext] simplify wiring in both directions.
//
// # Quick Start
//
// A basic logger only needs a handler and slog:
//
//	handler, err := slogcp.NewHandler(os.Stdout)
//	if err != nil {
//	    log.Fatalf("create slogcp handler: %v", err)
//	}
//	defer handler.Close() // flushes buffered logs and owned writers
//
//	logger := slog.New(handler)
//	logger.Info("application started")
//
// # Configuration
//
// Use functional options such as [WithLevel], [WithSourceLocationEnabled],
// [WithStackTraceEnabled], [WithRedirectToFile], [WithRedirectWriter],
// [WithSeverityAliases], and [WithTraceProjectID] to adjust behaviour
// programmatically. Refer to the package documentation and configuration guide
// in docs/CONFIGURATION.md for the complete list of options, environment
// variables, and integration helpers. Importing slogcp automatically installs
// a composite OpenTelemetry propagator; call [EnsurePropagation] explicitly if
// you disable the automatic behaviour via the `SLOGCP_DISABLE_PROPAGATOR_AUTOSET`
// environment variable.
package slogcp
