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

package gcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync"

	"cloud.google.com/go/logging"
	"google.golang.org/api/option"
)

// gcpClientAPI defines the internal interface for interacting with the underlying
// Cloud Logging client (*logging.Client).
type gcpClientAPI interface {
	Logger(logID string, opts ...logging.LoggerOption) *logging.Logger
	Close() error
}

// realGcpClientWrapper adapts a concrete *logging.Client to the internal gcpClientAPI interface.
type realGcpClientWrapper struct {
	realClient *logging.Client
}

func (w *realGcpClientWrapper) Logger(logID string, opts ...logging.LoggerOption) *logging.Logger {
	return w.realClient.Logger(logID, opts...)
}
func (w *realGcpClientWrapper) Close() error { return w.realClient.Close() }

var _ gcpClientAPI = (*realGcpClientWrapper)(nil)

// RealGcpLogger adapts a concrete *logging.Logger.
// Its methods must structurally match the public slogcp.GcpLoggerAPI.
type RealGcpLogger struct {
	*logging.Logger
}

func (r *RealGcpLogger) Log(e logging.Entry) { r.Logger.Log(e) }
func (r *RealGcpLogger) Flush() error        { return r.Logger.Flush() }

// ClientManager manages the lifecycle and interactions with the Cloud Logging client.
// It structurally implements the public slogcp.ClientManagerInterface.
type ClientManager struct {
	cfg            Config
	userAgent      string
	newClientFn    newClientFuncType // Uses internal gcpClientAPI
	internalLogger *slog.Logger

	client    gcpClientAPI   // Holds internal interface
	logger    *RealGcpLogger // Holds the concrete logger type
	levelVar  *slog.LevelVar
	initOnce  sync.Once
	initErr   error
	closeOnce sync.Once
}

// newClientFnType defines the signature for the internal factory function.
type newClientFuncType func(ctx context.Context, parent string, onError func(error), opts ...option.ClientOption) (gcpClientAPI, error)

// NewClientManager creates a new ClientManager. This is the internal constructor.
// It requires the resolved Config, user agent string, slog.LevelVar, and an optional
// internal diagnostics logger used for reporting lifecycle events.
func NewClientManager(cfg Config, userAgent string, levelVar *slog.LevelVar, internalLogger *slog.Logger) *ClientManager {
	cm := &ClientManager{
		cfg:            cfg,
		userAgent:      userAgent,
		levelVar:       levelVar,
		internalLogger: internalLogger,
	}
	// Assign the production factory function for creating the real client.
	cm.newClientFn = func(ctx context.Context, parent string, onError func(error), clientAPIOpts ...option.ClientOption) (gcpClientAPI, error) {
		realClient, err := logging.NewClient(ctx, parent, clientAPIOpts...)
		if err != nil {
			return nil, err
		}
		// Set OnError immediately after creation using the provided function.
		// If onError is nil, use a default that logs through the diagnostics logger.
		if onError != nil {
			realClient.OnError = onError
		} else {
			realClient.OnError = func(err error) {
				logDiagnostic(cm.internalLogger, slog.LevelError, "Cloud Logging background error", slog.Any("error", err))
			}
		}
		return &realGcpClientWrapper{realClient: realClient}, nil
	}
	return cm
}

// Initialize creates and configures the underlying Cloud Logging client and logger
// instance using the settings from the resolved Config.
//
// It applies a timeout to client creation to prevent indefinite blocking,
// configures the logger with all specified options, and ensures proper cleanup
// on initialization failure.
//
// This method is idempotent - subsequent calls after the first have no effect.
func (cm *ClientManager) Initialize() error {
	cm.initOnce.Do(func() {
		// Both Parent and ProjectID are expected to be resolved by LoadConfig for GCP target.
		if cm.cfg.Parent == "" {
			cm.initErr = fmt.Errorf("GCP parent is required for client initialization: %w", ErrProjectIDMissing)
			return
		}
		if cm.cfg.ProjectID == "" {
			cm.initErr = fmt.Errorf("GCP project ID is required for client initialization: %w", ErrProjectIDMissing)
			return
		}

		// The Logging client expects the plain project ID.
		projectIDArg := cm.cfg.ProjectID

		clientAPIOpts := []option.ClientOption{option.WithUserAgent(cm.userAgent)}
		if cm.cfg.ClientScopes != nil {
			clientAPIOpts = append(clientAPIOpts, option.WithScopes(cm.cfg.ClientScopes...))
		}

		// Create a timeout context for client initialization
		timeout := defaultClientInitTimeout
		if cm.cfg.GCPDefaultContextTimeout > 0 {
			timeout = cm.cfg.GCPDefaultContextTimeout
		}
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()

		// Create client with timeout context (using project ID, not "projects/<id>")
		clientWrapper, err := cm.newClientFn(ctx, projectIDArg, cm.cfg.ClientOnErrorFunc, clientAPIOpts...)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				cm.initErr = fmt.Errorf("GCP client creation timed out after %v: %w",
					timeout, ErrClientInitializationFailed)
			} else {
				cm.initErr = fmt.Errorf("GCP client creation failed: %w", ErrClientInitializationFailed)
			}
			return
		}
		cm.client = clientWrapper

		// Assemble logger options from configuration
		var loggerOpts []logging.LoggerOption
		if cm.cfg.GCPCommonLabels != nil {
			loggerOpts = append(loggerOpts, logging.CommonLabels(cm.cfg.GCPCommonLabels))
		}
		if cm.cfg.GCPMonitoredResource != nil {
			loggerOpts = append(loggerOpts, logging.CommonResource(cm.cfg.GCPMonitoredResource))
		}
		if cm.cfg.GCPConcurrentWriteLimit != nil {
			loggerOpts = append(loggerOpts, logging.ConcurrentWriteLimit(*cm.cfg.GCPConcurrentWriteLimit))
		}
		if cm.cfg.GCPDelayThreshold != nil {
			loggerOpts = append(loggerOpts, logging.DelayThreshold(*cm.cfg.GCPDelayThreshold))
		}
		if cm.cfg.GCPEntryCountThreshold != nil {
			loggerOpts = append(loggerOpts, logging.EntryCountThreshold(*cm.cfg.GCPEntryCountThreshold))
		}
		if cm.cfg.GCPEntryByteThreshold != nil {
			loggerOpts = append(loggerOpts, logging.EntryByteThreshold(*cm.cfg.GCPEntryByteThreshold))
		}
		if cm.cfg.GCPEntryByteLimit != nil {
			loggerOpts = append(loggerOpts, logging.EntryByteLimit(*cm.cfg.GCPEntryByteLimit))
		}
		if cm.cfg.GCPBufferedByteLimit != nil {
			loggerOpts = append(loggerOpts, logging.BufferedByteLimit(*cm.cfg.GCPBufferedByteLimit))
		}
		if cm.cfg.GCPContextFunc != nil {
			loggerOpts = append(loggerOpts, logging.ContextFunc(cm.cfg.GCPContextFunc))
		}
		if cm.cfg.GCPPartialSuccess != nil && *cm.cfg.GCPPartialSuccess {
			loggerOpts = append(loggerOpts, logging.PartialSuccess())
		}

		// Create the logger with assembled options
		escapedLogID := url.PathEscape(cm.cfg.GCPLogID)
		concreteLogger := cm.client.Logger(escapedLogID, loggerOpts...)
		if concreteLogger == nil {
			cm.initErr = fmt.Errorf("client.Logger(%q) returned nil: %w", escapedLogID, ErrClientInitializationFailed)
			if cm.client != nil {
				_ = cm.client.Close() // Attempt cleanup
			}
			cm.client = nil
			return
		}

		// Store the concrete logger
		cm.logger = &RealGcpLogger{Logger: concreteLogger}
	})
	return cm.initErr
}

// GetLogger returns the internal logger instance (*RealGcpLogger).
// This method's return type *structurally matches* the public slogcp.GcpLoggerAPI interface.
// The public slogcp.ClientManagerInterface requires a return type of slogcp.GcpLoggerAPI.
// Go allows returning the concrete *RealGcpLogger here, and the assignment
// to the interface variable in the calling package (slogcp) will work due to structural typing.
func (cm *ClientManager) GetLogger() (*RealGcpLogger, error) { // Return concrete type
	if cm.initErr != nil {
		return nil, cm.initErr
	}
	if cm.logger == nil {
		return nil, ErrClientNotInitialized
	}
	return cm.logger, nil
}

// Close gracefully shuts down the Cloud Logging client, flushing buffers first.
// It is idempotent. Returns the first significant error encountered.
func (cm *ClientManager) Close() error {
	var closeErr error
	cm.closeOnce.Do(func() {
		if cm.initErr != nil {
			logDiagnostic(cm.internalLogger, slog.LevelInfo, "Close called after initialization failure",
				slog.Any("error", cm.initErr),
			)
			closeErr = cm.initErr
			return
		}
		if cm.client == nil {
			logDiagnostic(cm.internalLogger, slog.LevelInfo, "Close called before client initialization")
			closeErr = ErrClientNotInitialized
			return
		}

		logDiagnostic(cm.internalLogger, slog.LevelInfo, "Closing Cloud Logging client")
		if cm.logger != nil {
			logDiagnostic(cm.internalLogger, slog.LevelInfo, "Flushing Cloud Logging client")
			// Call concrete type's method
			if flushErr := cm.logger.Flush(); flushErr != nil {
				if closeErr == nil {
					closeErr = flushErr
				}
				logDiagnostic(cm.internalLogger, slog.LevelWarn, "Error flushing logs during close",
					slog.Any("error", flushErr),
				)
			} else {
				logDiagnostic(cm.internalLogger, slog.LevelInfo, "Log flush completed")
			}
		} else {
			logDiagnostic(cm.internalLogger, slog.LevelWarn, "Client exists but logger is nil during close; skipping flush")
			if closeErr == nil {
				closeErr = errors.New("internal inconsistency: client exists but logger is nil during close")
			}
		}
		// Close the underlying client connection via the internal interface
		if clientCloseErr := cm.client.Close(); clientCloseErr != nil {
			logDiagnostic(cm.internalLogger, slog.LevelError, "Error closing Cloud Logging client",
				slog.Any("error", clientCloseErr),
			)
			// Prioritize client close error
			closeErr = clientCloseErr
		} else {
			logDiagnostic(cm.internalLogger, slog.LevelInfo, "Cloud Logging client closed")
		}
	})
	return closeErr
}

// Flush forces buffered log entries to be sent immediately.
// Returns an error if the client is not initialized or flushing fails.
func (cm *ClientManager) Flush() error {
	if cm.initErr != nil {
		return cm.initErr
	}
	if cm.logger == nil {
		return ErrClientNotInitialized
	}
	// Call concrete type's method
	err := cm.logger.Flush()
	if err != nil {
		logDiagnostic(cm.internalLogger, slog.LevelError, "Failed to flush Cloud Logging client",
			slog.Any("error", err),
		)
		return err
	}
	return nil
}

// GetLeveler returns the slog.Leveler associated with this manager.
// Returns nil if the manager has not been successfully initialized.
func (cm *ClientManager) GetLeveler() slog.Leveler {
	// Ensure initialization completed successfully before returning leveler
	if cm.initErr != nil || cm.logger == nil || cm.levelVar == nil {
		return nil
	}
	return cm.levelVar
}
