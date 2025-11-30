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

// Package slogcpasync adds an opt-in async wrapper around slogcp's synchronous
// handler. The wrapper queues records on a bounded channel and drains them with
// worker goroutines, leaving slogcp's default behaviour unchanged for callers
// that never import this package.
//
// Basic usage:
//
//	handler, _ := slogcp.NewHandler(os.Stdout)
//	async := slogcpasync.Wrap(handler,
//		slogcpasync.WithQueueSize(4096),
//		slogcpasync.WithDropMode(slogcpasync.DropModeDropNewest),
//	)
//	logger := slog.New(async)
//
// Environment-driven opt-in:
//
//	handler, _ := slogcp.NewHandler(os.Stdout,
//		slogcp.WithMiddleware(
//			slogcpasync.Middleware(
//				slogcpasync.WithEnabled(false), // stay synchronous by default
//				slogcpasync.WithEnv(),          // enable/size via SLOGCP_ASYNC_* vars
//			),
//		),
//	)
//
// The following environment variables are recognized when [WithEnv] is
// supplied:
//   - SLOGCP_ASYNC_ENABLED: true/false to toggle the wrapper
//   - SLOGCP_ASYNC_QUEUE_SIZE: channel capacity (0 makes the queue unbuffered)
//   - SLOGCP_ASYNC_DROP_MODE: block | drop_newest | drop_oldest
//   - SLOGCP_ASYNC_WORKERS: number of worker goroutines
//   - SLOGCP_ASYNC_FLUSH_TIMEOUT: duration string used by Close
package slogcpasync
