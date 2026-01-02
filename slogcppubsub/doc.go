// Copyright 2025-2026 Patrick J. Scruggs
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

// Package slogcppubsub provides Pub/Sub helpers for slogcp.
//
// Pub/Sub is an event boundary, so trace context does not automatically flow
// the way it does for HTTP or gRPC. slogcppubsub helps by:
//   - Injecting OpenTelemetry trace context into Pub/Sub message attributes
//     before publishing (typically W3C `traceparent`/`tracestate`).
//   - Extracting trace context from message attributes on the subscriber side
//     so application logs and spans can correlate correctly. WrapReceiveHandler
//     treats message attributes as the source of truth, even when the callback
//     context already contains a span context from other instrumentation.
//   - Optionally starting an application-level consumer span around message
//     processing (without duplicating the Pub/Sub client library's internal
//     tracing).
//   - Deriving a message-scoped *slog.Logger, storing it on the context via
//     slogcp.ContextWithLogger, and exposing a MessageInfo snapshot via
//     InfoFromContext.
//
// The official Go Pub/Sub client can inject/extract trace context using
// `googclient_`-prefixed message attribute keys (for example
// `googclient_traceparent`). Enable WithGoogClientCompat when you need to
// interoperate with that behavior.
package slogcppubsub
