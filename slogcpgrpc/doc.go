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

// Package slogcpgrpc provides gRPC interceptors that integrate slogcp with Google
// Cloud Logging and OpenTelemetry.
//
// The package exposes unary and stream interceptors for both clients and
// servers. Each interceptor derives a request-scoped slog.Logger, attaches it
// to the RPC context, and records a [RequestInfo] with service/method metadata,
// latency, status codes, peer information, and (optionally) payload sizes.
// When [WithOTel] is enabled (the default) otelgrpc StatsHandlers are installed
// automatically so spans and metrics are emitted without extra wiring. Legacy
// X-Cloud-Trace-Context propagation can be synthesized on outgoing RPCs when
// [WithLegacyXCloudInjection] is selected.
//
// Convenience helpers are available:
//
//   - [UnaryServerInterceptor] and [StreamServerInterceptor]
//   - [UnaryClientInterceptor] and [StreamClientInterceptor]
//   - [ServerOptions] and [DialOptions], which bundle otelgrpc handlers together
//     with slogcp interceptors for easy registration.
//   - [InfoFromContext], which retrieves the RequestInfo captured by the
//     interceptors so handlers can inspect RPC metadata.
//
// Typical usage:
//
//	server := grpc.NewServer(
//	    slogcpgrpc.ServerOptions(
//	        slogcpgrpc.WithLogger(appLogger),
//	    )...,
//	)
//
//	conn, err := grpc.NewClient(
//	    target,
//	    append(
//	        []grpc.DialOption{grpc.WithTransportCredentials(creds)},
//	        slogcpgrpc.DialOptions()...,
//	    )...,
//	)
//	if err != nil {
//	    // handle error
//	}
//	conn.Connect() // optional: start dialing immediately, otherwise RPCs will trigger it
package slogcpgrpc
