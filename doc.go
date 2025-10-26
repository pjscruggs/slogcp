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

// Package slogcp provides a "batteries included" structured logging solution
// for Go applications. It builds on the standard library's [log/slog] package
// and emits JSON payloads that follow Google Cloud Logging conventions while
// writing to any [io.Writer]. The default destination is stdout, which keeps
// the library friendly for container platforms and local development alike.
//
// ⚠️ This module is untested, and not recommended for any production use. ⚠️
//
// The primary entry point is [NewHandler], which returns an [slog.Handler]
// configured with sensible defaults:
//   - Structured JSON output that includes common Google Cloud keys such as
//     `severity`, `timestamp`, `logging.googleapis.com/trace`, and
//     `httpRequest`.
//   - Automatic runtime metadata detection for Cloud Run and Cloud Functions.
//   - Optional stack traces and source locations.
//   - Seamless trace correlation helpers that leverage the extended
//     [Level] definitions (`DEBUG` through `EMERGENCY`).
//
// Handlers can be redirected to stderr, a file managed by slogcp, or a custom
// writer. When slogcp opens the file it also provides [Handler.ReopenLogFile]
// to cooperate with external rotation tools. Many aspects of the handler can
// be controlled through environment variables (for example `LOG_LEVEL` or
// `SLOGCP_REDIRECT_AS_JSON_TARGET`) so the same binary can run locally and in
// production without code changes.
//
// # Subpackages
//
//   - [github.com/pjscruggs/slogcp/http] offers net/http middleware, optional
//     panic recovery, trace propagation utilities, and request/response
//     logging compatible with the handler's JSON format.
//   - [github.com/pjscruggs/slogcp/grpc] provides client and server
//     interceptors that capture RPC metadata, surface errors with stack traces,
//     and propagate trace context when desired.
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
// [WithStackTraceEnabled], [WithRedirectToFile], [WithRedirectWriter], and
// [WithTraceProjectID] to adjust behaviour programmatically. Refer to the
// package documentation and configuration guide in docs/CONFIGURATION.md for
// the complete list of options and environment variables.
package slogcp
