package gcp

import (
	"context"
	"log/slog"
	"runtime"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
)

// entryLogger defines the minimal interface needed by gcpHandler to send log entries.
// This typically wraps *logging.Logger but allows for testing.
type entryLogger interface {
	Log(e logging.Entry)
}

// groupedAttr holds a slog.Attr along with the group namespace active when
// the attribute was added via logger.With(...).
type groupedAttr struct {
	groups []string
	attr   slog.Attr
}

// gcpHandler implements the slog.Handler interface. It formats slog.Record
// objects into logging.Entry structs suitable for the Google Cloud Logging API
// and sends them via an entryLogger.
//
// It handles GCP-specific field mapping (severity, trace context, source location),
// structured payload creation, error formatting (including optional stack traces),
// and management of attributes added via WithAttrs/WithGroup.
type gcpHandler struct {
	// mu protects access to handler-level state (groupedAttrs, groups)
	// which can be modified by WithAttrs/WithGroup concurrently.
	mu sync.Mutex

	// cfg holds the configuration settings (ProjectID, AddSource, StackTrace config).
	cfg Config

	// entryLog is the interface used to send the final logging.Entry.
	entryLog entryLogger

	// leveler determines the minimum log level this handler will process.
	leveler slog.Leveler

	// groupedAttrs stores attributes added via WithAttrs, along with their group context.
	// These attributes are prepended to the attributes from the slog.Record.
	groupedAttrs []groupedAttr

	// groups holds the current group stack for this specific handler instance,
	// built up by calls to WithGroup. Record attributes are nested under these groups.
	groups []string
}

// NewGcpHandler creates a GCP-specific slog handler.
// It requires the application configuration, an initialized entryLogger (typically
// from the ClientManager), and a slog.Leveler to control the minimum log level.
// If leveler is nil, it defaults to slog.LevelInfo.
func NewGcpHandler(cfg Config, logger entryLogger, leveler slog.Leveler) *gcpHandler {
	if leveler == nil {
		leveler = slog.LevelInfo // Default level if none provided.
	}
	return &gcpHandler{
		cfg:          cfg,
		entryLog:     logger,
		leveler:      leveler,
		groupedAttrs: make([]groupedAttr, 0), // Initialize slices.
		groups:       make([]string, 0),
	}
}

// Enabled implements slog.Handler. It checks if the given log level is
// greater than or equal to the minimum level configured for this handler.
func (h *gcpHandler) Enabled(_ context.Context, level slog.Level) bool {
	min := slog.LevelInfo // Default minimum level.
	if h.leveler != nil {
		min = h.leveler.Level() // Get the dynamic level if available.
	}
	return level >= min // Check if the record's level meets the threshold.
}

// Handle implements slog.Handler. It transforms the slog.Record into a
// logging.Entry and sends it via the configured entryLogger.
// This method orchestrates severity mapping, trace extraction, source location
// resolution, attribute processing, error formatting, and payload construction.
func (h *gcpHandler) Handle(ctx context.Context, r slog.Record) error {
	// Discard the record immediately if the level is not enabled.
	if !h.Enabled(ctx, r.Level) {
		return nil
	}

	// Map the slog level to the corresponding GCP severity constant.
	severity := mapSlogLevelToGcpSeverity(r.Level)

	// Extract trace information (Trace ID, Span ID, Sampled flag) from the context.
	traceID, spanID, sampled := ExtractTraceSpan(ctx, h.cfg.ProjectID)

	// Resolve source code location if configured and program counter is available.
	var sourceLoc *loggingpb.LogEntrySourceLocation
	if h.cfg.AddSource && r.PC != 0 {
		sourceLoc = resolveSourceLocation(r.PC)
	}

	var httpReq *logging.HTTPRequest
	var firstErr error
	payload := make(map[string]any) // Map to hold the final JSON payload.

	// Helper to get or create nested maps for grouped attributes.
	getNestedMap := func(groups []string) map[string]any {
		curr := payload
		for _, g := range groups {
			if g == "" {
				continue
			}
			sub, ok := curr[g]
			var subMap map[string]any
			if ok {
				subMap, ok = sub.(map[string]any)
			}
			if !ok {
				subMap = make(map[string]any)
				curr[g] = subMap
			}
			curr = subMap
		}
		return curr
	}

	// Helper to process a single attribute, resolve its value, place it in the
	// correct nested map, and extract special types (error, httpRequest).
	// Implements "last wins" for duplicate keys within the same group level.
	processAttr := func(ga groupedAttr) {
		if ga.attr.Key == "" {
			return // Ignore attributes with empty keys.
		}

		// Handle the special httpRequestKey used by the http middleware.
		if ga.attr.Key == httpRequestKey {
			if maybeReq, ok := ga.attr.Value.Any().(*logging.HTTPRequest); ok {
				if httpReq == nil { // Capture only the first one found.
					httpReq = maybeReq
				}
			}
			// Do not add this special attribute to the JSON payload itself.
			return
		}

		// Resolve the slog.Value to a standard Go type.
		val := resolveSlogValue(ga.attr.Value)
		if val == nil {
			return // Skip nil resolved values (e.g., empty groups).
		}

		// Capture the first error encountered for potential stack trace logging.
		if errVal, ok := val.(error); ok && firstErr == nil {
			firstErr = errVal
		}

		// Add the resolved value to the payload map, nested according to groups.
		dstMap := getNestedMap(ga.groups)
		dstMap[ga.attr.Key] = val
	}

	// Snapshot handler state (attributes added via With, current group stack) safely.
	h.mu.Lock()
	handlerAttrs := append([]groupedAttr(nil), h.groupedAttrs...)
	currentGroups := append([]string(nil), h.groups...)
	h.mu.Unlock()

	// Process handler-level attributes first.
	for _, ha := range handlerAttrs {
		processAttr(ha)
	}

	// Process record-level attributes, applying the handler's current group stack.
	r.Attrs(func(a slog.Attr) bool {
		processAttr(groupedAttr{groups: currentGroups, attr: a})
		return true // Continue processing attributes.
	})

	// Add the main log message if present.
	if r.Message != "" {
		payload[messageKey] = r.Message
	}

	// Format error details and potentially capture/format stack trace.
	var finalStackTrace string
	if firstErr != nil {
		// Get basic error info and attempt to get origin stack trace.
		// Call the updated formatErrorForReporting with only the error.
		formattedErr, originStackTrace := formatErrorForReporting(firstErr)
		payload[errorTypeKey] = formattedErr.Type // Add error type.
		finalStackTrace = originStackTrace        // Use origin stack if available.

		// Check if fallback stack capture is needed.
		needsFallbackStack := finalStackTrace == "" && h.cfg.StackTraceEnabled && r.Level >= h.cfg.StackTraceLevel
		if needsFallbackStack {
			// Capture stack trace at the logging site.
			pcs := make([]uintptr, maxStackFrames)
			// Skip runtime.Callers, this Handle method, and the slog.Logger method that called Handle.
			// Adjust skip count if Handle's internal structure changes significantly.
			num := runtime.Callers(2, pcs)
			if num > 0 {
				// Format the captured PCs (no prefix skipping needed here).
				finalStackTrace = formatPCsToStackString(pcs[:num])
			}
		}
	}

	// Add the final stack trace (origin or fallback) to the payload if available.
	if finalStackTrace != "" {
		payload[stackTraceKey] = finalStackTrace
	}

	// Determine the final timestamp, defaulting to Now() if zero.
	tstamp := r.Time
	if tstamp.IsZero() {
		tstamp = time.Now()
	}

	entry := logging.Entry{
		Timestamp:      tstamp,
		Severity:       severity,
		Payload:        payload, // The structured payload map.
		Trace:          traceID,
		SpanID:         spanID,
		TraceSampled:   sampled,
		SourceLocation: sourceLoc, // Populated if enabled and available.
		HTTPRequest:    httpReq,   // Populated if provided via special attribute.
		// Resource and Labels are typically set via CommonResource/CommonLabels
		// options on the underlying logging.Logger instance.
	}

	// Send the entry asynchronously. Errors are handled by the client's OnError.
	h.entryLog.Log(entry)

	// Handle method completed its task of processing and queueing the record.
	return nil
}

// WithAttrs implements slog.Handler. It returns a new handler instance
// that includes the provided attributes, ensuring they are logged with
// every subsequent record processed by the new handler. Attributes are
// associated with the group context active when WithAttrs is called.
func (h *gcpHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h // Optimization: return self if no attributes are added.
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	// Create a clone to ensure the original handler remains unmodified.
	h2 := h.clone() // clone() safely copies internal slices.

	// Append new attributes, capturing the current group stack for each.
	for _, a := range attrs {
		// Copy the group stack from the *new* handler instance (h2).
		stackCopy := make([]string, len(h2.groups))
		copy(stackCopy, h2.groups)
		h2.groupedAttrs = append(h2.groupedAttrs, groupedAttr{
			groups: stackCopy,
			attr:   a,
		})
	}
	return h2 // Return the new handler with the added attributes.
}

// WithGroup implements slog.Handler. It returns a new handler instance
// with the specified group name added to its group stack. Subsequent logs
// and attributes added via the returned handler will be nested under this group.
func (h *gcpHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h // Optimization: return self if the group name is empty.
	}
	h.mu.Lock()
	defer h.mu.Unlock()

	// Create a clone to ensure the original handler remains unmodified.
	h2 := h.clone() // clone() safely copies internal slices.

	// Append the new group name to the cloned handler's group stack.
	h2.groups = append(h2.groups, name)
	return h2 // Return the new handler with the added group context.
}

// clone creates a shallow copy of the handler. It specifically duplicates
// the groupedAttrs and groups slices to ensure that modifications to the
// new handler's state do not affect the original handler.
// Assumes the caller holds the mutex (h.mu).
func (h *gcpHandler) clone() *gcpHandler {
	h2 := &gcpHandler{
		// Copy configuration and interface references.
		cfg:      h.cfg,
		entryLog: h.entryLog,
		leveler:  h.leveler,
		// Create new slices with the same capacity as the original.
		groupedAttrs: make([]groupedAttr, len(h.groupedAttrs), cap(h.groupedAttrs)),
		groups:       make([]string, len(h.groups), cap(h.groups)),
	}
	// Copy the contents of the slices into the newly created slices.
	copy(h2.groupedAttrs, h.groupedAttrs)
	copy(h2.groups, h.groups)
	return h2
}

// Compile-time check that gcpHandler implements slog.Handler.
var _ slog.Handler = (*gcpHandler)(nil)
