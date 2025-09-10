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
// tailored for Google Cloud Platform (GCP). It integrates seamlessly with
// standard Go `log/slog` and offers enhanced features for cloud environments,
// including automatic trace correlation, full GCP severity level support, and
// robust error reporting.
//
// ⚠️ This module is untested, and not recommended for any production use. ⚠️
//
// The core of the package is the [Logger] type, which can be configured to
// send structured JSON logs directly to the Cloud Logging API or to local
// destinations like stdout, stderr, or a file. It intelligently detects
// the environment, automatically falling back to local logging (e.g., stdout)
// when GCP credentials or project information are unavailable, simplifying
// local development.
//
// # Key Features
//
//   - Structured JSON Logging: Optimized for Cloud Logging, enabling powerful
//     filtering and analysis.
//   - GCP Integration: Direct API integration for reliable log delivery,
//     automatic resource detection, and correlation with Cloud Trace and
//     Cloud Error Reporting.
//   - Full Severity Spectrum: Supports all GCP logging severity levels, from
//     DEBUG to EMERGENCY, extending standard slog levels via the [Level] type.
//   - Trace Correlation: Automatically extracts and injects trace context
//     (Trace ID, Span ID) for seamless log correlation in Cloud Trace.
//   - Dynamic Log Levels: Adjust logging verbosity at runtime without
//     restarting the application using [Logger.SetLevel].
//   - Enhanced Error Reporting: Includes options for automatic stack trace
//     capture with errors.
//   - Graceful Shutdown: Ensures buffered logs are flushed before application exit
//     via [Logger.Close].
//
// # Subpackages
//
//   - [github.com/pjscruggs/slogcp/http]: Provides net/http middleware for
//     logging HTTP server requests and responses, including trace context injection,
//     and an opt-in outbound RoundTripper (TracePropagationTransport) that injects
//     both W3C `traceparent` and `X-Cloud-Trace-Context` headers on outgoing requests
//     for cross-service log/trace correlation.
//   - [github.com/pjscruggs/slogcp/grpc]: Offers gRPC client and server
//     interceptors for comprehensive logging of RPC calls, with similar
//     features for trace handling and payload/metadata logging options.
//
// # Quick Start
//
// To get started, create a new logger:
//
//	// Create a logger with default settings.
//	// Assumes GOOGLE_CLOUD_PROJECT is set or running on GCP.
//	logger, err := slogcp.New()
//	if err != nil {
//	    log.Fatalf("Failed to create logger: %v", err)
//	}
//	// Important: Always call Close to flush buffered logs, especially in GCP mode.
//	defer logger.Close()
//
//	// Log a simple message.
//	logger.Info("Application started successfully")
//
// # Configuration
//
// The [New] function accepts various [Option] functions to customize behavior,
// such as setting the log level ([WithLevel]), enabling source location
// ([WithSourceLocationEnabled]), or configuring GCP-specific parameters
// (e.g., [WithGCPCommonLabel], [WithGCPEntryCountThreshold]).
// Many settings can also be controlled via environment variables.
// See the [Logger] type and specific option functions for more details.
package slogcp
