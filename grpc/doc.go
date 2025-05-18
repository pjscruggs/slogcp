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

// Package grpc provides gRPC client and server interceptors that integrate
// with the slogcp logger. These interceptors automatically log details about
// RPC calls in a structured format suitable for Google Cloud Logging, Cloud
// Trace, and Cloud Error Reporting.
//
// # Server Interceptors
//
// The server interceptors ([UnaryServerInterceptor], [StreamServerInterceptor])
// capture the following information for each incoming RPC:
//   - gRPC service and method names
//   - Duration of the call
//   - Final gRPC status code
//   - Peer address (client address)
//   - Any error returned by the handler (formatted for Cloud Error Reporting)
//   - Panic recovery details (if a handler panics)
//
// Trace context (trace ID, span ID) is extracted from the incoming
// context.Context and included in log entries by the slogcp handler,
// correlating logs in Cloud Trace.
//
// # Client Interceptors
//
// The client interceptors ([NewUnaryClientInterceptor], [NewStreamClientInterceptor])
// capture similar information for outgoing RPCs:
//   - gRPC service and method names
//   - Duration of the call
//   - Final gRPC status code
//   - Any error returned by the call
//
// Trace context is propagated in outgoing metadata and included in log entries.
//
// # Basic Usage (Server)
//
// First, obtain a logger instance from the main slogcp package:
//
//	// Assumes GOOGLE_CLOUD_PROJECT is set or running on GCP.
//	slogcpLogger, err := slogcp.New()
//	if err != nil {
//	    log.Fatalf("Failed to create slogcp logger: %v", err)
//	}
//	defer slogcpLogger.Close()
//
// Then, create your gRPC server, applying the interceptors:
//
//	// Import the interceptor package
//	import slogcpgrpc "github.com/pjscruggs/slogcp/grpc"
//
//	server := grpc.NewServer(
//	    grpc.ChainUnaryInterceptor(
//	        slogcpgrpc.UnaryServerInterceptor(slogcpLogger),
//	        // Add other unary interceptors here...
//	    ),
//	    grpc.ChainStreamInterceptor(
//	        slogcpgrpc.StreamServerInterceptor(slogcpLogger),
//	        // Add other stream interceptors here...
//	    ),
//	    // ... other server options (credentials, etc.)
//	)
//
//	// Register your service implementation...
//	// pb.RegisterYourServiceServer(server, &yourService{})
//
//	// Start the server...
//
// # Configuration
//
// Both client and server interceptors can be customized using functional options
// passed to their respective constructor functions:
//   - [WithLevels]: Customize how gRPC status codes map to slog.Level.
//   - [WithShouldLog]: Filter which RPC calls should be logged (e.g., skip health checks).
//   - [WithPayloadLogging]: Enable logging of request/response message payloads
//     (default: false). Use with caution due to potential log volume and data sensitivity.
//   - [WithMaxPayloadSize]: Limit the size of logged payloads when payload logging is enabled.
//   - [WithMetadataLogging]: Enable logging of request/response metadata (headers/trailers).
//   - [WithMetadataFilter]: Filter which metadata keys are logged (a default filter applies).
//   - [WithSkipPaths]: Exclude specific gRPC methods from logging based on path matching.
//   - [WithSamplingRate]: Log only a fraction of requests (0.0 to 1.0).
//   - [WithLogCategory]: Add a custom category attribute to logs (defaults to "grpc_request").
//
// By default, all calls are logged, payloads and metadata are not logged, and a
// standard mapping from gRPC codes to log levels is used (e.g., OK -> Info,
// InvalidArgument -> Warn, Internal -> Error).
//
// # Composability
//
// These interceptors are designed to be composed with other interceptors using
// chaining functions like [grpc.ChainUnaryInterceptor] and [grpc.ChainStreamInterceptor].
// Ensure the slogcp interceptor runs after interceptors that populate context
// (like tracing or auth) if its logging depends on that context information.
// For complex scenarios involving multiple cross-cutting concerns, consider
// writing a custom interceptor that calls the `slogcp.Logger` directly.
package grpc
