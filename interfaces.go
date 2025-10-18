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

package slogcp

import (
	"log/slog"

	"cloud.google.com/go/logging"

	"github.com/pjscruggs/slogcp/internal/gcp"
)

// ClientManagerInterface manages the Google Cloud Logging client's lifecycle.
// It encapsulates the initialization, teardown, and access to the underlying
// Cloud Logging resources when operating in GCP API mode. This represents
// the boundary between the public API and the internal implementation.
//
// The Handler returned by slogcp holds a reference to an implementation of this interface
// and delegates client operations to it. By defining this interface, the
// slogcp package can mock Cloud Logging for testing while providing a
// consistent API to application code.
//
// Applications should not implement this interface directly. It is exported
// primarily for testing and advanced extension scenarios.
type ClientManagerInterface interface {
	// Initialize creates and configures the Cloud Logging client connection.
	// It prepares the client with the parent resource, authentication, and
	// buffering settings from the configuration. This should be called only once,
	// typically during logger creation. All other methods expect initialization
	// to be complete.
	//
	// Returns nil on success or an error describing why initialization failed.
	// Common error cases include missing project ID or authentication issues.
	Initialize() error

	// Close releases all resources held by the client manager, including
	// the Cloud Logging client connection. It automatically flushes any
	// pending log entries before shutdown.
	//
	// This should be called during application shutdown. After Close, no other
	// methods should be called on the manager. Multiple calls to Close are safe
	// and will return the error from the first call.
	//
	// Returns the first significant error encountered during flush or close.
	Close() error

	// GetLogger returns the concrete logger instance used to send log entries
	// to Cloud Logging. This provides access to the underlying logging.Logger
	// wrapped with the interface required by the handler.
	//
	// Returns nil and an error if the client manager has not been successfully
	// initialized or has been closed.
	GetLogger() (*gcp.RealGcpLogger, error)

	// GetLeveler returns the slog.Leveler controlling the dynamic logging level.
	// This allows external code to check the current minimum logging level.
	//
	// Returns nil if the client manager has not been successfully initialized
	// or has been closed.
	GetLeveler() slog.Leveler

	// Flush forces immediate transmission of all buffered log entries.
	// This is useful to ensure logs are sent before an application pauses
	// or when immediate visibility is required.
	//
	// Returns an error if the client manager is not initialized, has been closed,
	// or if the underlying Cloud Logging client encounters an error during flush.
	Flush() error
}

// GcpLoggerAPI defines the operations for sending log entries to Cloud Logging.
// It abstracts the concrete *logging.Logger implementation, allowing the
// rest of the code to work with a simplified interface focused on the
// operations needed by slogcp.
//
// This interface enables testing and creating alternative implementations
// for special logging scenarios. The primary implementation is internal to
// the package and wraps the standard Cloud Logging client.
//
// Operations through this interface are typically asynchronous - entries are
// buffered and sent in batches according to the configured buffering parameters.
type GcpLoggerAPI interface {
	// Log submits a log entry to be sent to Cloud Logging.
	// The entry is buffered according to Cloud Logging client configuration
	// and sent asynchronously. If an error occurs during the background send,
	// it is reported through the OnError callback configured with the client.
	//
	// This method does not return an error as failures are handled asynchronously.
	Log(logging.Entry)

	// Flush sends all buffered log entries immediately.
	// This bypasses the normal buffering timers and thresholds, ensuring
	// all entries buffered so far are transmitted to Cloud Logging.
	//
	// Returns an error if the flush operation fails, typically due to
	// network issues, context cancellation, or invalid entries.
	Flush() error
}
