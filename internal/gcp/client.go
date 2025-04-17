package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
)

// gcpClientAPI defines the interface for interacting with the underlying
// Cloud Logging client (*logging.Client). This abstraction facilitates testing.
type gcpClientAPI interface {
	Logger(logID string, opts ...logging.LoggerOption) *logging.Logger
	Close() error
}

// gcpLoggerAPI defines the interface for sending log entries and flushing buffers,
// abstracting the underlying *logging.Logger.
type gcpLoggerAPI interface {
	Log(logging.Entry)
	Flush() error
}

// newClientFuncType defines the signature for the factory function used to create
// the Cloud Logging client dependency.
type newClientFuncType func(ctx context.Context, projectID string, opts ...option.ClientOption) (gcpClientAPI, error)

// realGcpClientWrapper adapts a concrete *logging.Client to the gcpClientAPI interface.
type realGcpClientWrapper struct {
	realClient *logging.Client
}

// Logger calls the underlying *logging.Client's Logger method.
func (w *realGcpClientWrapper) Logger(logID string, opts ...logging.LoggerOption) *logging.Logger {
	return w.realClient.Logger(logID, opts...)
}

// Close calls the underlying *logging.Client's Close method.
func (w *realGcpClientWrapper) Close() error {
	return w.realClient.Close()
}

// Compile-time check that realGcpClientWrapper implements gcpClientAPI.
var _ gcpClientAPI = (*realGcpClientWrapper)(nil)

// realGcpLogger adapts a concrete *logging.Logger to the gcpLoggerAPI interface.
type realGcpLogger struct {
	*logging.Logger
}

// Log calls the embedded *logging.Logger's Log method. This operation is
// asynchronous; background errors are handled via the client's OnError callback.
func (r *realGcpLogger) Log(e logging.Entry) {
	r.Logger.Log(e)
}

// Flush calls the embedded *logging.Logger's Flush method, attempting to send
// all buffered log entries immediately.
func (r *realGcpLogger) Flush() error {
	return r.Logger.Flush()
}

// Compile-time check that realGcpLogger implements gcpLoggerAPI.
var _ gcpLoggerAPI = (*realGcpLogger)(nil)

// ClientOptions holds optional configuration settings that influence the
// buffering behavior of the underlying Cloud Logging client logger instance.
// These are derived from the public slogcp.Option values.
type ClientOptions struct {
	EntryCountThreshold *int
	DelayThreshold      *time.Duration
	MonitoredResource   *mrpb.MonitoredResource // Explicit resource setting.
}

// ClientManager manages the lifecycle and interactions with the Cloud Logging
// client and its associated logger instance. It ensures initialization happens
// only once and handles graceful shutdown.
type ClientManager struct {
	cfg         Config            // Loaded configuration (ProjectID, etc.).
	userAgent   string            // User agent string for API calls.
	clientOpts  ClientOptions     // Specific options for the logging.Logger.
	newClientFn newClientFuncType // Factory function for client creation (allows mocking).

	client    gcpClientAPI   // Interface to the Cloud Logging client.
	logger    gcpLoggerAPI   // Interface to the specific logger instance.
	levelVar  *slog.LevelVar // Controller for the dynamic logging level.
	initOnce  sync.Once      // Ensures initialization runs only once.
	initErr   error          // Stores initialization error.
	closeOnce sync.Once      // Ensures closing runs only once.
}

// NewClientManager creates a new ClientManager.
// It requires the loaded Config, user agent string, specific ClientOptions,
// and the slog.LevelVar for dynamic level control.
// It configures the manager to use the real `cloud.google.com/go/logging` client.
func NewClientManager(cfg Config, userAgent string, opts ClientOptions, levelVar *slog.LevelVar) *ClientManager {
	cm := &ClientManager{
		cfg:        cfg,
		userAgent:  userAgent,
		clientOpts: opts,
		levelVar:   levelVar,
	}
	// Assign the production factory function for creating the real client.
	cm.newClientFn = func(ctx context.Context, projectID string, clientAPIOpts ...option.ClientOption) (gcpClientAPI, error) {
		realClient, err := logging.NewClient(ctx, projectID, clientAPIOpts...)
		if err != nil {
			// Wrap the error for consistent upstream checking.
			return nil, fmt.Errorf("%w: %v", ErrClientInitializationFailed, err)
		}
		// Configure a handler for errors occurring in the background
		// operations of the underlying logging client.
		realClient.OnError = func(err error) {
			// Log background errors directly to stderr, as using the primary
			// logger could lead to infinite loops if logging itself fails.
			fmt.Fprintf(os.Stderr, "[slogcp client background] ERROR: %v\n", err)
		}
		return &realGcpClientWrapper{realClient: realClient}, nil
	}
	return cm
}

// Initialize creates and configures the underlying Cloud Logging client and logger
// instance using the settings provided during manager creation.
// This method is idempotent; it performs initialization only on the first call.
// It returns any error encountered during the initialization process.
func (cm *ClientManager) Initialize() error {
	cm.initOnce.Do(func() {
		// Prepare options for the underlying client library API call.
		clientAPIOpts := []option.ClientOption{option.WithUserAgent(cm.userAgent)}

		// Create the client using the configured factory function.
		client, err := cm.newClientFn(context.Background(), cm.cfg.ProjectID, clientAPIOpts...)
		if err != nil {
			cm.initErr = err // Store the initialization error.
			// Log fatal error to stderr as the primary logger isn't available.
			fmt.Fprintf(os.Stderr, "[slogcp client] FATAL: Initialization failed: %v\n", cm.initErr)
			return // Stop initialization on client creation failure.
		}
		cm.client = client // Store the initialized client interface.

		// Define the log ID used within Cloud Logging.
		const logID = "slogcp_application_logs" // Consider if this needs configuration.

		// Prepare options for the specific logger instance.
		loggerOpts := []logging.LoggerOption{
			// Do NOT provide logging.SourceLocationPopulation option here.
			// This ensures the client library's automatic population (which uses
			// runtime.Caller) is disabled. The slogcp handler will populate
			// the SourceLocation field based on slog.Record.PC if configured.
			// Set a buffer limit appropriate for Cloud Run environments.
			logging.BufferedByteLimit(100 * 1024 * 1024), // 100MiB
		}
		// Apply user-configured batching thresholds.
		if cm.clientOpts.EntryCountThreshold != nil {
			loggerOpts = append(loggerOpts, logging.EntryCountThreshold(*cm.clientOpts.EntryCountThreshold))
		}
		if cm.clientOpts.DelayThreshold != nil {
			loggerOpts = append(loggerOpts, logging.DelayThreshold(*cm.clientOpts.DelayThreshold))
		}
		// Apply explicitly provided monitored resource, overriding auto-detection.
		if cm.clientOpts.MonitoredResource != nil {
			loggerOpts = append(loggerOpts, logging.CommonResource(cm.clientOpts.MonitoredResource))
		}

		// Get the concrete *logging.Logger from the client.
		concreteLogger := cm.client.Logger(logID, loggerOpts...)
		if concreteLogger == nil {
			// This indicates an internal issue if client creation succeeded but logger creation failed.
			cm.initErr = fmt.Errorf("client.Logger(%q) returned nil: %w", logID, ErrClientInitializationFailed)
			fmt.Fprintf(os.Stderr, "[slogcp client] FATAL: %v\n", cm.initErr)
			// Attempt to clean up the partially initialized client.
			if cm.client != nil {
				_ = cm.client.Close() // Ignore close error during failed init cleanup.
			}
			cm.client = nil // Ensure client is nil if logger creation failed.
			return          // Stop initialization.
		}

		// Wrap the concrete logger in our adapter and store the interface.
		cm.logger = &realGcpLogger{Logger: concreteLogger}

		// Log successful initialization to stderr for operational visibility.
		fmt.Fprintf(os.Stderr, "[slogcp client] INFO: Cloud Logging client initialized for project %q, logID %q\n", cm.cfg.ProjectID, logID)
	})
	// Return the stored error (nil if initialization was successful).
	return cm.initErr
}

// GetLogger retrieves the initialized logger instance conforming to gcpLoggerAPI.
// It should only be called after a successful Initialize().
// Returns ErrClientNotInitialized if Initialize() has not been called successfully,
// or the original initialization error if initialization failed.
func (cm *ClientManager) GetLogger() (gcpLoggerAPI, error) {
	// Return initialization error first if it occurred.
	if cm.initErr != nil {
		return nil, cm.initErr
	}
	// Check if the logger instance was successfully created during Initialize.
	if cm.logger == nil {
		return nil, ErrClientNotInitialized
	}
	return cm.logger, nil
}

// Log sends a single log entry asynchronously via the managed logger instance.
// It retrieves the logger using GetLogger. If the logger is not ready (due to
// initialization failure or Initialize not being called), an error message is
// printed to stderr, but the function returns nil because the underlying Log
// operation is asynchronous and doesn't surface background errors directly here.
func (cm *ClientManager) Log(entry logging.Entry) error {
	loggerInstance, err := cm.GetLogger()
	if err != nil {
		// Log directly to stderr if the client isn't ready to queue the log entry.
		// Include entry details to aid debugging the lost log.
		fmt.Fprintf(os.Stderr, "[slogcp client] ERROR: Cannot log entry, client not ready: %v. Entry: %+v\n", err, entry)
		return nil // Return nil as the Log operation itself is async.
	}

	// Delegate to the logger interface's Log method.
	loggerInstance.Log(entry)
	return nil // Async log call doesn't return application errors here.
}

// Close gracefully shuts down the Cloud Logging client.
// It attempts to flush any buffered log entries and then closes the underlying
// client connection and associated resources.
// This method is idempotent; subsequent calls have no effect.
// It returns the first significant error encountered during the flush or close
// process, or the initialization error if initialization had previously failed.
func (cm *ClientManager) Close() error {
	var closeErr error
	cm.closeOnce.Do(func() {
		// If initialization failed previously, return that error immediately.
		if cm.initErr != nil {
			fmt.Fprintf(os.Stderr, "[slogcp client] INFO: Close called but initialization previously failed: %v\n", cm.initErr)
			closeErr = cm.initErr
			return
		}
		// If client wasn't successfully created during Initialize, nothing to close.
		if cm.client == nil {
			fmt.Fprintf(os.Stderr, "[slogcp client] INFO: Close called but client is nil (not initialized).\n")
			closeErr = ErrClientNotInitialized // Indicate that close was called prematurely.
			return
		}

		fmt.Fprintf(os.Stderr, "[slogcp client] INFO: Closing Cloud Logging client...\n")

		// Attempt to flush logs if the logger instance was successfully created.
		if cm.logger != nil {
			fmt.Fprintf(os.Stderr, "[slogcp client] INFO: Flushing logs...\n")
			flushErr := cm.logger.Flush() // Call Flush via the interface.
			if flushErr != nil {
				// Log flush errors but don't necessarily stop the close process.
				fmt.Fprintf(os.Stderr, "[slogcp client] WARNING: Error flushing logs during close: %v\n", flushErr)
				// Record the first error encountered during shutdown.
				if closeErr == nil {
					closeErr = flushErr
				}
			} else {
				fmt.Fprintf(os.Stderr, "[slogcp client] INFO: Log flush completed.\n")
			}
		} else {
			// This state (client exists but logger is nil) indicates an internal inconsistency
			// likely stemming from a failure during the Initialize phase after client creation
			// but before logger creation.
			fmt.Fprintf(os.Stderr, "[slogcp client] WARNING: Client exists but logger is nil during close, cannot flush.\n")
			if closeErr == nil {
				closeErr = fmt.Errorf("internal inconsistency: client exists but logger is nil during close")
			}
		}

		// Close the underlying client connection.
		clientCloseErr := cm.client.Close() // Call Close via the interface.
		if clientCloseErr != nil {
			fmt.Fprintf(os.Stderr, "[slogcp client] ERROR: Error closing cloud logging client: %v\n", clientCloseErr)
			// Prioritize returning the client close error as it might indicate
			// more fundamental issues than a flush error.
			closeErr = clientCloseErr
		} else {
			fmt.Fprintf(os.Stderr, "[slogcp client] INFO: Cloud Logging client closed.\n")
		}
	})
	// Return the first significant error encountered during the close sequence.
	return closeErr
}

// GetLeveler returns the slog.Leveler (specifically, the *slog.LevelVar)
// associated with this manager. This allows the handler or application code
// to dynamically check or set the logging level.
// Returns nil if the manager has not been successfully initialized.
func (cm *ClientManager) GetLeveler() slog.Leveler {
	// Check initialization status before returning the leveler.
	if cm.initErr != nil || cm.levelVar == nil {
		return nil
	}
	return cm.levelVar
}
