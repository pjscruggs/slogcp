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
	"net/http"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/testing/protocmp"
)

// mockEntryLogger captures logging.Entry structs for handler tests
// without sending them to the actual Cloud Logging service.
// It implements the entryLogger interface required by gcpHandler.
type mockEntryLogger struct {
	mu      sync.Mutex
	entries []logging.Entry // Store all entries logged during a test run
}

// Log captures the entry passed to it.
func (m *mockEntryLogger) Log(e logging.Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Make a copy of the payload map to avoid race conditions if the caller reuses it.
	// Note: This is a shallow copy. Deep copy might be needed if payload values are modified later.
	if payloadMap, ok := e.Payload.(map[string]any); ok {
		copiedPayload := make(map[string]any, len(payloadMap))
		for k, v := range payloadMap {
			copiedPayload[k] = v
		}
		e.Payload = copiedPayload
	}
	m.entries = append(m.entries, e)
}

// GetEntries returns a copy of all entries logged.
func (m *mockEntryLogger) GetEntries() []logging.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Return a copy to prevent external modification.
	entriesCopy := make([]logging.Entry, len(m.entries))
	copy(entriesCopy, m.entries)
	return entriesCopy
}

// GetLastEntry returns the last entry logged and a boolean indicating if any entry was logged.
func (m *mockEntryLogger) GetLastEntry() (logging.Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.entries) == 0 {
		return logging.Entry{}, false
	}
	// Return a copy of the last entry.
	lastEntry := m.entries[len(m.entries)-1]
	return lastEntry, true
}

// Reset clears the captured log state.
func (m *mockEntryLogger) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = nil
}

// Compile-time check that mockEntryLogger implements entryLogger.
var _ entryLogger = (*mockEntryLogger)(nil)

// getHandlerTestPC returns the program counter for source-location tests.
func getHandlerTestPC(t *testing.T) uintptr {
	t.Helper()
	pcs := make([]uintptr, 10)
	// Get all frames by using skip=0
	n := runtime.Callers(0, pcs)
	if n == 0 {
		t.Fatal("getHandlerTestPC: runtime.Callers returned no PCs")
	}

	frames := runtime.CallersFrames(pcs[:n])

	// Skip past runtime.Callers and this helper function to find the test function
	var foundHelper bool
	var testFramePC uintptr

	for {
		frame, more := frames.Next()

		// Skip runtime frames
		if strings.HasPrefix(frame.Function, "runtime.") {
			if !more {
				break
			}
			continue
		}

		// First identify this helper function
		if !foundHelper && strings.HasSuffix(frame.Function, "getHandlerTestPC") {
			foundHelper = true
			if !more {
				break
			}
			continue
		}

		// After finding helper, the next non-testing frame is the caller we want
		if foundHelper && !strings.HasPrefix(frame.Function, "testing.") {
			testFramePC = frame.PC
			break
		}

		if !more {
			break
		}
	}

	if testFramePC == 0 {
		t.Fatal("getHandlerTestPC: Could not find caller frame PC")
	}

	return testFramePC
}

// contextWithTrace creates a context containing trace information for testing.
func contextWithTrace(projectID string) (context.Context, string, string, bool) {
	traceIDHex := "11111111111111112222222222222222"
	spanIDHex := "3333333333333333"
	traceID, _ := trace.TraceIDFromHex(traceIDHex)
	spanID, _ := trace.SpanIDFromHex(spanIDHex)
	sampled := true
	flags := trace.FlagsSampled.WithSampled(sampled)
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID, TraceFlags: flags, Remote: true})
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), sc)
	formattedTraceID := ""
	// Only format if projectID is provided, mimicking ExtractTraceSpan logic.
	if projectID != "" {
		formattedTraceID = "projects/" + projectID + "/traces/" + traceIDHex
	}
	return ctx, formattedTraceID, spanIDHex, sampled
}

// TestGcpHandler_Enabled verifies the handler's Enabled method respects the configured level.
func TestGcpHandler_Enabled(t *testing.T) {
	levelVar := new(slog.LevelVar)
	mockLogger := &mockEntryLogger{}
	cfg := Config{} // Config doesn't affect Enabled directly
	handler := NewGcpHandler(cfg, mockLogger, levelVar)

	testCases := []struct {
		handlerLevel slog.Level
		recordLevel  slog.Level
		wantEnabled  bool
	}{
		{slog.LevelInfo, slog.LevelDebug, false},
		{slog.LevelInfo, slog.LevelInfo, true},
		{slog.LevelInfo, slog.LevelWarn, true},
		{slog.LevelDebug, slog.LevelDebug, true},
		{slog.LevelDebug, internalLevelDefault, false}, // Check custom level
		{slog.LevelWarn, slog.LevelInfo, false},
		{slog.LevelWarn, slog.LevelWarn, true},
		{slog.LevelWarn, slog.LevelError, true},
		{slog.LevelError, internalLevelCritical, true}, // Check custom level
	}

	for _, tc := range testCases {
		t.Run(fmt.Sprintf("Handler%v/Record%v", tc.handlerLevel, tc.recordLevel), func(t *testing.T) {
			levelVar.Set(tc.handlerLevel) // Set the dynamic level
			gotEnabled := handler.Enabled(context.Background(), tc.recordLevel)
			if gotEnabled != tc.wantEnabled {
				t.Errorf("Enabled(ctx, %v) with handler level %v = %v, want %v",
					tc.recordLevel, tc.handlerLevel, gotEnabled, tc.wantEnabled)
			}
		})
	}

	// Test nil leveler defaults to Info
	handlerNilLeveler := NewGcpHandler(cfg, mockLogger, nil)
	if !handlerNilLeveler.Enabled(context.Background(), slog.LevelInfo) {
		t.Error("Handler with nil leveler should enable Info level")
	}
	if handlerNilLeveler.Enabled(context.Background(), slog.LevelDebug) {
		t.Error("Handler with nil leveler should disable Debug level")
	}
}

// TestGcpHandler_Handle verifies the core transformation logic of the handler.
func TestGcpHandler_Handle(t *testing.T) {
	projectID := "handle-test-proj"
	baseTime := time.Now()
	testBasicErr := errors.New("handler test error")
	// Use the updated newStackError which captures PCs for the interface
	testStackErr := newStackError("stack error msg", "") // Stack string arg is optional
	// Correctly initialize logging.HTTPRequest by embedding an *http.Request.
	testHTTPRawRequestForLog, err := http.NewRequest("POST", "/test?q=1", nil)
	if err != nil {
		t.Fatalf("Setup error: failed to create test HTTP request: %v", err)
	}
	testHTTPRawRequestForLog.RemoteAddr = "1.2.3.4:5678"
	testHTTPRawRequestForLog.Header.Set("User-Agent", "test-agent")
	testHTTPRawRequestForLog.Header.Set("Referer", "http://example.com")
	testHTTPRawRequestForLog.ContentLength = 15
	testHTTPEntry := &logging.HTTPRequest{Request: testHTTPRawRequestForLog, Status: 400, ResponseSize: 50}

	mockLogger := &mockEntryLogger{}
	// Base config used unless overridden in a test case.
	baseCfg := Config{ProjectID: projectID, AddSource: false, StackTraceEnabled: false, StackTraceLevel: slog.LevelError}

	// testCase defines the structure for table-driven tests of the Handle method.
	type testCase struct {
		name             string
		ctx              context.Context
		cfgOverride      *Config     // Optional config overrides for this case
		handlerGroups    []string    // Groups applied via WithGroup before Handle
		handlerAttrs     []slog.Attr // Attrs applied via WithAttrs before Handle
		record           slog.Record // The input log record
		wantSeverity     logging.Severity
		wantTraceID      string
		wantSpanID       string
		wantSampled      bool
		wantSourceCheck  func(t *testing.T, sl *loggingpb.LogEntrySourceLocation) // Custom check for source location
		wantHTTPRequest  *logging.HTTPRequest                                     // Expected HTTPRequest on the entry
		wantPayload      map[string]any                                           // Expected base payload structure
		wantPayloadCheck func(t *testing.T, payload map[string]any)               // Custom checks for payload details (errors, stacks)
	}

	// Define test cases covering various scenarios.
	testCases := []testCase{
		{
			name:         "Basic Info Log",
			ctx:          context.Background(),
			record:       slog.NewRecord(baseTime, slog.LevelInfo, "Info message", 0),
			wantSeverity: logging.Info,
			wantPayload: map[string]any{
				messageKey: "Info message",
			},
		},
		{
			name: "Debug Log with Attrs",
			ctx:  context.Background(),
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelDebug, "Debug data", 0)
				r.AddAttrs(slog.String("user", "test"), slog.Int("count", 5))
				return r
			}(),
			wantSeverity: logging.Debug,
			wantPayload: map[string]any{
				messageKey: "Debug data",
				"user":     "test",
				"count":    int64(5),
			},
		},
		{
			name: "Notice Log (Custom Level)",
			ctx:  context.Background(),
			record: func() slog.Record {
				// Use the internal level constant for Notice
				r := slog.NewRecord(baseTime, internalLevelNotice, "Just a notice", 0)
				r.AddAttrs(slog.Bool("processed", true))
				return r
			}(),
			wantSeverity: logging.Notice,
			wantPayload: map[string]any{
				messageKey:  "Just a notice",
				"processed": true,
			},
		},
		{
			name: "Critical Log (Custom Level)",
			ctx:  context.Background(),
			record: func() slog.Record {
				// Use the internal level constant for Critical
				r := slog.NewRecord(baseTime, internalLevelCritical, "System critical", 0)
				r.AddAttrs(slog.String("component", "db"))
				return r
			}(),
			wantSeverity: logging.Critical,
			wantPayload: map[string]any{
				messageKey:  "System critical",
				"component": "db",
			},
		},
		{
			name: "Warning Log with Time and Duration",
			ctx:  context.Background(),
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelWarn, "Timing out", 0)
				r.AddAttrs(slog.Time("event_time", baseTime), slog.Duration("timeout", time.Second*2))
				return r
			}(),
			wantSeverity: logging.Warning,
			wantPayload: map[string]any{
				messageKey:   "Timing out",
				"event_time": baseTime.UTC().Format(time.RFC3339Nano),
				"timeout":    (time.Second * 2).String(),
			},
		},
		{
			name: "Error Log with Basic Error (Stack Disabled)",
			ctx:  context.Background(),
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelError, "Op failed", 0)
				r.AddAttrs(slog.Any("err", testBasicErr)) // Use non-standard key
				return r
			}(),
			wantSeverity: logging.Error,
			wantPayload: map[string]any{
				messageKey:   "Op failed",
				"err":        testBasicErr, // Original attribute remains
				errorTypeKey: "*errors.errorString",
			},
			wantPayloadCheck: func(t *testing.T, payload map[string]any) {
				if _, ok := payload[stackTraceKey]; ok {
					t.Error("Stack trace found but should be disabled")
				}
			},
		},
		{
			name:        "Error Log with Basic Error (Stack Enabled, Fallback)",
			ctx:         context.Background(),
			cfgOverride: &Config{StackTraceEnabled: true, StackTraceLevel: slog.LevelError},
			record: func() slog.Record {
				// Provide PC for source location context, though fallback stack uses runtime.Callers
				r := slog.NewRecord(baseTime, slog.LevelError, "Failure", getHandlerTestPC(t))
				r.AddAttrs(slog.Any("error", testBasicErr)) // Use standard key
				return r
			}(),
			wantSeverity: logging.Error,
			// Base payload check ignores error/stack keys
			wantPayload: map[string]any{
				messageKey:   "Failure",
				"error":      testBasicErr, // Original attribute remains
				errorTypeKey: "*errors.errorString",
			},
			wantPayloadCheck: func(t *testing.T, payload map[string]any) {
				stackVal, ok := payload[stackTraceKey]
				if !ok {
					t.Fatal("Missing stackTrace key")
				}
				stackStr, ok := stackVal.(string)
				if !ok {
					t.Fatalf("stackTrace value is not a string: %T", stackVal)
				}
				// Check that the fallback stack contains the test function name.
				if !strings.Contains(stackStr, "TestGcpHandler_Handle") {
					t.Errorf("Expected fallback stack trace containing 'TestGcpHandler_Handle', got:\n%s", stackStr)
				}
				// Check that it does NOT contain internal frames we tried to skip.
				if strings.Contains(stackStr, "runtime.Callers") {
					t.Errorf("Fallback stack trace unexpectedly contains 'runtime.Callers':\n%s", stackStr)
				}
				if strings.Contains(stackStr, "gcpHandler.Handle") {
					t.Errorf("Fallback stack trace unexpectedly contains 'gcpHandler.Handle':\n%s", stackStr)
				}
			},
		},
		{
			name:        "Error Log with Stack Error (Stack Enabled, Interface)",
			ctx:         context.Background(),
			cfgOverride: &Config{StackTraceEnabled: true, StackTraceLevel: slog.LevelError},
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelError, "Stack failure", 0)
				r.AddAttrs(slog.Any("detail", testStackErr)) // Use non-standard key
				return r
			}(),
			wantSeverity: logging.Error,
			wantPayload: map[string]any{
				messageKey:   "Stack failure",
				"detail":     testStackErr, // Original attribute remains
				errorTypeKey: "*gcp.stackError",
			},
			wantPayloadCheck: func(t *testing.T, payload map[string]any) {
				stackVal, ok := payload[stackTraceKey]
				if !ok {
					t.Fatal("Missing stackTrace key")
				}
				stackStr, ok := stackVal.(string)
				if !ok {
					t.Fatalf("stackTrace value is not a string: %T", stackVal)
				}
				// Check for content from the stack captured by newStackError
				if !strings.Contains(stackStr, "newStackError") {
					t.Errorf("Expected stack trace from error containing 'newStackError', got:\n%s", stackStr)
				}
			},
		},
		{
			name:        "Warn Log with Error (Stack Enabled, Below Threshold)",
			ctx:         context.Background(),
			cfgOverride: &Config{StackTraceEnabled: true, StackTraceLevel: slog.LevelError}, // Threshold Error
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelWarn, "Warn with error", 0) // Log level Warn
				r.AddAttrs(slog.Any("error", testBasicErr))
				return r
			}(),
			wantSeverity: logging.Warning,
			wantPayload: map[string]any{
				messageKey:   "Warn with error",
				"error":      testBasicErr,
				errorTypeKey: "*errors.errorString",
			},
			wantPayloadCheck: func(t *testing.T, payload map[string]any) {
				if _, ok := payload[stackTraceKey]; ok {
					t.Error("Stack trace found but level was below threshold")
				}
			},
		},
		{
			name:         "Log with Trace Context",
			ctx:          func() context.Context { ctx, _, _, _ := contextWithTrace(projectID); return ctx }(),
			record:       slog.NewRecord(baseTime, slog.LevelInfo, "Tracing this", 0),
			wantSeverity: logging.Info,
			wantTraceID:  func() string { _, tid, _, _ := contextWithTrace(projectID); return tid }(),
			wantSpanID:   func() string { _, _, sid, _ := contextWithTrace(projectID); return sid }(),
			wantSampled:  func() bool { _, _, _, s := contextWithTrace(projectID); return s }(),
			wantPayload: map[string]any{
				messageKey: "Tracing this",
			},
		},
		{
			name:         "Log with Trace Context (No Project ID)",
			ctx:          func() context.Context { ctx, _, _, _ := contextWithTrace(""); return ctx }(), // Empty project ID
			cfgOverride:  &Config{ProjectID: ""},                                                        // Explicitly override project ID to empty string
			record:       slog.NewRecord(baseTime, slog.LevelInfo, "No project trace", 0),
			wantSeverity: logging.Info,
			wantTraceID:  "",    // Expect empty because project ID was missing
			wantSpanID:   "",    // Expect empty because trace ID is empty
			wantSampled:  false, // Expect false because trace ID is empty
			wantPayload: map[string]any{
				messageKey: "No project trace",
			},
		},
		{
			name:         "Log with Source Location",
			ctx:          context.Background(),
			cfgOverride:  &Config{AddSource: true}, // Enable source
			record:       slog.NewRecord(baseTime, slog.LevelInfo, "Source test", getHandlerTestPC(t)),
			wantSeverity: logging.Info,
			wantSourceCheck: func(t *testing.T, sl *loggingpb.LogEntrySourceLocation) {
				if sl == nil {
					t.Fatal("Expected SourceLocation, got nil")
				}
				wantFileSuffix := "internal/gcp/handler_test.go"
				if !strings.HasSuffix(sl.File, wantFileSuffix) {
					t.Errorf("SourceLocation.File = %q, want suffix %q", sl.File, wantFileSuffix)
				}
				wantFuncSubstring := "TestGcpHandler_Handle"
				if !strings.Contains(sl.Function, wantFuncSubstring) {
					t.Errorf("SourceLocation.Function = %q, want substring %q", sl.Function, wantFuncSubstring)
				}
				if sl.Line <= 0 {
					t.Errorf("SourceLocation.Line = %d, want positive", sl.Line)
				}
			},
			wantPayload: map[string]any{
				messageKey: "Source test",
			},
		},
		{
			name:         "Log with Source Disabled (PC present)",
			ctx:          context.Background(),
			cfgOverride:  &Config{AddSource: false}, // Explicitly disable
			record:       slog.NewRecord(baseTime, slog.LevelInfo, "No source", getHandlerTestPC(t)),
			wantSeverity: logging.Info,
			wantSourceCheck: func(t *testing.T, sl *loggingpb.LogEntrySourceLocation) {
				if sl != nil {
					t.Errorf("Expected nil SourceLocation when disabled, got %v", sl)
				}
			},
			wantPayload: map[string]any{
				messageKey: "No source",
			},
		},
		{
			name:          "Log with Groups (Handler)",
			ctx:           context.Background(),
			handlerGroups: []string{"g1", "g2"},
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelInfo, "Nested msg", 0)
				r.AddAttrs(slog.String("a", "b"))
				return r
			}(),
			wantSeverity: logging.Info,
			wantPayload: map[string]any{
				messageKey: "Nested msg",
				"g1": map[string]any{
					"g2": map[string]any{
						"a": "b",
					},
				},
			},
		},
		{
			name: "Log with Groups (Record)",
			ctx:  context.Background(),
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelInfo, "Grouped attrs", 0)
				r.AddAttrs(slog.Group("req", slog.String("id", "123"), slog.Int("size", 100)))
				return r
			}(),
			wantSeverity: logging.Info,
			wantPayload: map[string]any{
				messageKey: "Grouped attrs",
				"req": map[string]any{
					"id":   "123",
					"size": int64(100),
				},
			},
		},
		{
			name:          "Log with Groups (Handler and Record)",
			ctx:           context.Background(),
			handlerGroups: []string{"handler_group"},
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelInfo, "Mixed groups", 0)
				r.AddAttrs(slog.Group("record_group", slog.Bool("ok", true)))
				return r
			}(),
			wantSeverity: logging.Info,
			wantPayload: map[string]any{
				messageKey: "Mixed groups",
				"handler_group": map[string]any{
					"record_group": map[string]any{
						"ok": true,
					},
				},
			},
		},
		{
			name:         "Log with Handler Attrs",
			ctx:          context.Background(),
			handlerAttrs: []slog.Attr{slog.String("common", "val1"), slog.Int("id", 1)},
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelInfo, "With common", 0)
				r.AddAttrs(slog.String("local", "val2"))
				return r
			}(),
			wantSeverity: logging.Info,
			wantPayload: map[string]any{
				messageKey: "With common",
				"common":   "val1",
				"id":       int64(1),
				"local":    "val2",
			},
		},
		{
			name: "Log with HTTP Request Attr",
			ctx:  context.Background(),
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelWarn, "Client Error", 0)
				r.AddAttrs(slog.Any(httpRequestKey, testHTTPEntry))
				return r
			}(),
			wantSeverity:    logging.Warning,
			wantHTTPRequest: testHTTPEntry, // Expect it on the Entry struct
			wantPayload: map[string]any{ // Expect it *removed* from payload
				messageKey: "Client Error",
			},
		},
		{
			name:        "Log with multiple errors (first wins for formatting)",
			ctx:         context.Background(),
			cfgOverride: &Config{StackTraceEnabled: true, StackTraceLevel: slog.LevelError},
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelError, "Multi fail", 0)
				r.AddAttrs(slog.Any("error1", testBasicErr), slog.Any("error2", errors.New("second error")))
				return r
			}(),
			wantSeverity: logging.Error,
			// Base payload check ignores error/stack keys
			wantPayload: map[string]any{
				messageKey: "Multi fail",
				"error1":   testBasicErr,
				"error2":   errors.New("second error"), // Original attributes remain
			},
			wantPayloadCheck: func(t *testing.T, payload map[string]any) {
				// Check that formatting used the *first* error encountered ("error1")
				if typeVal, ok := payload[errorTypeKey]; !ok || typeVal != "*errors.errorString" {
					t.Errorf("Expected errorType from first error (*errors.errorString), got %v (found: %v)", typeVal, ok)
				}
				// Check stack trace exists (from first error's fallback)
				stackVal, ok := payload[stackTraceKey]
				if !ok {
					t.Error("Missing stackTrace key")
				} else if _, ok := stackVal.(string); !ok {
					t.Errorf("stackTrace value is not a string: %T", stackVal)
				}
			},
		},
		{
			name: "Empty Message",
			ctx:  context.Background(),
			record: func() slog.Record {
				r := slog.NewRecord(baseTime, slog.LevelInfo, "", 0) // Empty message
				r.AddAttrs(slog.String("key", "value"))
				return r
			}(),
			wantSeverity: logging.Info,
			wantPayload: map[string]any{ // messageKey should be absent
				"key": "value",
			},
		},
		{
			name: "Zero Time",
			ctx:  context.Background(),
			record: func() slog.Record {
				r := slog.NewRecord(time.Time{}, slog.LevelInfo, "Zero time test", 0) // Zero time
				return r
			}(),
			wantSeverity: logging.Info,
			wantPayload: map[string]any{
				messageKey: "Zero time test",
			},
			// Note: We don't check the exact timestamp here, just that it runs.
			// The handler defaults to time.Now() if the record time is zero.
		},
	}

	// Options for comparing logging.HTTPRequest, ignoring unexported fields in http.Request.
	httpOpts := []cmp.Option{
		cmpopts.IgnoreUnexported(http.Request{}),
		protocmp.Transform(), // Needed if HTTPRequest contains protos
	}
	// Options for comparing payload maps.
	payloadOpts := []cmp.Option{
		protocmp.Transform(),
		cmpopts.EquateEmpty(), // Treat nil and empty maps/slices as equal
		// Ignore error attributes and special formatting keys during general comparison.
		cmpopts.IgnoreMapEntries(func(k string, v any) bool {
			if k == errorTypeKey || k == stackTraceKey {
				return true
			}
			_, isErr := v.(error)
			return isErr
		}),
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			mockLogger.Reset()
			// Apply config overrides for this test case.
			currentCfg := baseCfg
			if tc.cfgOverride != nil {
				// Create a copy to avoid modifying baseCfg
				cfgCopy := currentCfg
				// Handle ProjectID override explicitly
				if tc.cfgOverride.ProjectID != "" || tc.name == "Log with Trace Context (No Project ID)" {
					cfgCopy.ProjectID = tc.cfgOverride.ProjectID
				}
				if tc.cfgOverride.AddSource { // Check if AddSource is explicitly set true
					cfgCopy.AddSource = true
				} else {
					// If not explicitly true in override, use baseCfg's value (which might be false)
					// This handles the case where override only sets stack trace config.
					cfgCopy.AddSource = baseCfg.AddSource
				}
				cfgCopy.StackTraceEnabled = tc.cfgOverride.StackTraceEnabled
				// Only override level if explicitly set in override struct OR if stack trace is enabled
				if tc.cfgOverride.StackTraceLevel != 0 || tc.cfgOverride.StackTraceEnabled {
					cfgCopy.StackTraceLevel = tc.cfgOverride.StackTraceLevel
				} else {
					cfgCopy.StackTraceLevel = baseCfg.StackTraceLevel // Reset to base if not relevant
				}
				currentCfg = cfgCopy // Use the modified copy
			}

			// Ensure handler uses a level that allows all test records through for verification.
			var finalHandler slog.Handler = NewGcpHandler(currentCfg, mockLogger, slog.LevelDebug)

			// Apply handler groups and attrs state for the test case.
			if len(tc.handlerGroups) > 0 {
				for _, group := range tc.handlerGroups {
					finalHandler = finalHandler.WithGroup(group)
				}
			}
			if len(tc.handlerAttrs) > 0 {
				finalHandler = finalHandler.WithAttrs(tc.handlerAttrs)
			}

			// Execute the Handle method.
			err := finalHandler.Handle(tc.ctx, tc.record)
			if err != nil {
				t.Fatalf("handler.Handle failed: %v", err)
			}

			entry, logged := mockLogger.GetLastEntry()
			if !logged {
				// If the record level was below the handler's effective minimum,
				// it's expected that nothing is logged. Check this condition.
				minLevel := slog.LevelInfo // Default if leveler is nil
				if h, ok := finalHandler.(*gcpHandler); ok && h.leveler != nil {
					minLevel = h.leveler.Level()
				}
				if tc.record.Level >= minLevel {
					t.Fatal("No log entry was recorded by the mock logger, but level was enabled")
				} else {
					// Log was correctly skipped due to level, test passes for Handle execution.
					return
				}
			}

			if entry.Severity != tc.wantSeverity {
				t.Errorf("Severity mismatch: got %v, want %v", entry.Severity, tc.wantSeverity)
			}
			if entry.Trace != tc.wantTraceID {
				t.Errorf("TraceID mismatch: got %q, want %q", entry.Trace, tc.wantTraceID)
			}
			if entry.SpanID != tc.wantSpanID {
				t.Errorf("SpanID mismatch: got %q, want %q", entry.SpanID, tc.wantSpanID)
			}
			if entry.TraceSampled != tc.wantSampled {
				t.Errorf("Sampled mismatch: got %v, want %v", entry.TraceSampled, tc.wantSampled)
			}

			// Verify source location using the custom check function if provided.
			if tc.wantSourceCheck != nil {
				tc.wantSourceCheck(t, entry.SourceLocation)
			} else if entry.SourceLocation != nil {
				// Fail if source location exists but wasn't expected.
				t.Errorf("Got SourceLocation %v, want nil", entry.SourceLocation)
			}

			// Verify HTTP Request using cmp.Diff for better reporting.
			if diff := cmp.Diff(tc.wantHTTPRequest, entry.HTTPRequest, httpOpts...); diff != "" {
				t.Errorf("HTTPRequest mismatch (-want +got):\n%s", diff)
			}

			// Verify payload structure and content.
			payloadMap, ok := entry.Payload.(map[string]any)
			if !ok {
				// Handle cases where payload might be nil or not a map (shouldn't happen with current logic)
				if entry.Payload == nil && tc.wantPayload == nil && tc.wantPayloadCheck == nil {
					// This is okay if nothing was expected
				} else {
					t.Fatalf("Entry.Payload is not map[string]any, got %T: %v", entry.Payload, entry.Payload)
				}
			}

			// Execute custom payload checks if provided (for stack traces, error types, etc.).
			if tc.wantPayloadCheck != nil {
				tc.wantPayloadCheck(t, payloadMap)
			}

			// Perform base payload comparison using cmp.Diff if wantPayload is set.
			if tc.wantPayload != nil {
				if diff := cmp.Diff(tc.wantPayload, payloadMap, payloadOpts...); diff != "" {
					t.Errorf("Payload base mismatch (-want +got):\n%s", diff)
				}
			} else if tc.wantPayloadCheck == nil && len(payloadMap) > 0 {
				// If no specific checks defined and no base payload expected, it should be empty,
				// unless it only contains error formatting keys.
				hasOnlyErrorKeys := true
				for k := range payloadMap {
					if k != errorTypeKey && k != stackTraceKey {
						hasOnlyErrorKeys = false
						break
					}
				}
				if !hasOnlyErrorKeys {
					t.Errorf("Expected empty payload or only error keys, got %v", payloadMap)
				}
			}
		})
	}
}

// TestGcpHandler_WithGroup verifies group nesting and handler immutability.
func TestGcpHandler_WithGroup(t *testing.T) {
	mockLogger := &mockEntryLogger{}
	baseCfg := Config{ProjectID: "proj"}
	baseHandler := NewGcpHandler(baseCfg, mockLogger, slog.LevelInfo)

	// Add group "g1"
	h1 := baseHandler.WithGroup("g1")
	if h1 == baseHandler { // Check if a new instance was returned
		t.Fatal("WithGroup did not return a new handler instance")
	}
	// Check internal state (defensive copy)
	if &baseHandler.groups == &h1.(*gcpHandler).groups {
		t.Fatal("WithGroup did not copy the groups slice")
	}

	// Add group "g2" to the new handler
	h2 := h1.WithGroup("g2")
	if h2 == h1 { // Check if a new instance was returned
		t.Fatal("Second WithGroup did not return a new handler instance")
	}
	if &h1.(*gcpHandler).groups == &h2.(*gcpHandler).groups {
		t.Fatal("Second WithGroup did not copy the groups slice")
	}

	// Log with the final handler (h2)
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	record.AddAttrs(slog.String("a", "b"))
	err := h2.Handle(context.Background(), record)
	if err != nil {
		t.Fatalf("h2.Handle failed: %v", err)
	}

	// Check the logged entry's payload structure
	entry, logged := mockLogger.GetLastEntry()
	if !logged {
		t.Fatal("No log entry recorded")
	}
	payload, ok := entry.Payload.(map[string]any)
	if !ok {
		t.Fatalf("Payload is not map[string]any")
	}

	// Expected payload structure with nested groups.
	wantPayload := map[string]any{
		messageKey: "msg",
		"g1": map[string]any{
			"g2": map[string]any{
				"a": "b",
			},
		},
	}
	if diff := cmp.Diff(wantPayload, payload); diff != "" {
		t.Errorf("Payload mismatch for nested groups (-want +got):\n%s", diff)
	}

	// Verify original handler (baseHandler) is unchanged by logging with it.
	mockLogger.Reset()
	record2 := slog.NewRecord(time.Now(), slog.LevelInfo, "base msg", 0)
	record2.AddAttrs(slog.String("x", "y"))
	err = baseHandler.Handle(context.Background(), record2)
	if err != nil {
		t.Fatalf("baseHandler.Handle failed: %v", err)
	}
	entryBase, loggedBase := mockLogger.GetLastEntry()
	if !loggedBase {
		t.Fatal("No log entry for base handler")
	}
	payloadBase, _ := entryBase.Payload.(map[string]any)
	wantPayloadBase := map[string]any{messageKey: "base msg", "x": "y"}
	if diff := cmp.Diff(wantPayloadBase, payloadBase); diff != "" {
		t.Errorf("Base handler payload mismatch (-want +got):\n%s", diff)
	}
}

// TestGcpHandler_WithAttrs verifies attribute addition, overrides, and handler immutability.
func TestGcpHandler_WithAttrs(t *testing.T) {
	mockLogger := &mockEntryLogger{}
	baseCfg := Config{ProjectID: "proj"}
	baseHandler := NewGcpHandler(baseCfg, mockLogger, slog.LevelInfo)

	// Add attrs
	attrs1 := []slog.Attr{slog.String("a", "1"), slog.Int("b", 2)}
	h1 := baseHandler.WithAttrs(attrs1)
	if h1 == baseHandler { // Check if a new instance was returned
		t.Fatal("WithAttrs did not return a new handler instance")
	}
	// Check internal state (defensive copy)
	if &baseHandler.groupedAttrs == &h1.(*gcpHandler).groupedAttrs {
		t.Fatal("WithAttrs did not copy the groupedAttrs slice")
	}

	// Add more attrs, including an override for "a"
	attrs2 := []slog.Attr{slog.String("c", "3"), slog.String("a", "override")}
	h2 := h1.WithAttrs(attrs2)
	if h2 == h1 { // Check if a new instance was returned
		t.Fatal("Second WithAttrs did not return a new handler instance")
	}
	if &h1.(*gcpHandler).groupedAttrs == &h2.(*gcpHandler).groupedAttrs {
		t.Fatal("Second WithAttrs did not copy the groupedAttrs slice")
	}

	// Log with the final handler (h2)
	record := slog.NewRecord(time.Now(), slog.LevelInfo, "msg", 0)
	record.AddAttrs(slog.String("d", "4"))
	err := h2.Handle(context.Background(), record)
	if err != nil {
		t.Fatalf("h2.Handle failed: %v", err)
	}

	// Check the logged entry's payload structure
	entry, logged := mockLogger.GetLastEntry()
	if !logged {
		t.Fatal("No log entry recorded")
	}
	payload, ok := entry.Payload.(map[string]any)
	if !ok {
		t.Fatalf("Payload is not map[string]any")
	}

	// Expected payload includes merged attributes, with "a" overridden ("last wins").
	wantPayload := map[string]any{
		messageKey: "msg",
		"a":        "override",
		"b":        int64(2),
		"c":        "3",
		"d":        "4",
	}
	if diff := cmp.Diff(wantPayload, payload); diff != "" {
		t.Errorf("Payload mismatch for WithAttrs (-want +got):\n%s", diff)
	}

	// Verify original handler (baseHandler) is unchanged.
	mockLogger.Reset()
	recordBase := slog.NewRecord(time.Now(), slog.LevelInfo, "base msg", 0)
	recordBase.AddAttrs(slog.String("x", "y"))
	err = baseHandler.Handle(context.Background(), recordBase)
	if err != nil {
		t.Fatalf("baseHandler.Handle failed: %v", err)
	}
	entryBase, loggedBase := mockLogger.GetLastEntry()
	if !loggedBase {
		t.Fatal("No log entry for base handler")
	}
	payloadBase, _ := entryBase.Payload.(map[string]any)
	wantPayloadBase := map[string]any{messageKey: "base msg", "x": "y"}
	if diff := cmp.Diff(wantPayloadBase, payloadBase); diff != "" {
		t.Errorf("Base handler payload mismatch (-want +got):\n%s", diff)
	}
}

// TestGcpHandler_WithGroupAndAttrsInteraction verifies correct nesting based on call order.
func TestGcpHandler_WithGroupAndAttrsInteraction(t *testing.T) {
	mockLogger := &mockEntryLogger{}
	baseCfg := Config{ProjectID: "proj"}
	var baseHandler slog.Handler = NewGcpHandler(baseCfg, mockLogger, slog.LevelInfo)

	// Scenario 1: Attrs -> Group -> Attrs
	t.Run("Attrs_Group_Attrs", func(t *testing.T) {
		mockLogger.Reset()
		h := baseHandler.WithAttrs([]slog.Attr{slog.String("a", "1")}) // Attr 'a' added at top level
		h = h.WithGroup("g1")                                          // Group 'g1' started
		h = h.WithAttrs([]slog.Attr{slog.String("b", "2")})            // Attr 'b' added within 'g1'

		record_ag := slog.NewRecord(time.Now(), slog.LevelInfo, "AG", 0)
		record_ag.AddAttrs(slog.String("c", "3")) // Record attr 'c' added within 'g1'
		err := h.Handle(context.Background(), record_ag)
		if err != nil {
			t.Fatalf("Handle failed: %v", err)
		}

		entry_ag, logged := mockLogger.GetLastEntry()
		if !logged {
			t.Fatal("No log entry")
		}
		payload_ag, ok := entry_ag.Payload.(map[string]any)
		if !ok {
			t.Fatalf("Payload is not map[string]any")
		}

		// Expect 'a' at top level, 'b' and 'c' under 'g1'.
		want_ag := map[string]any{
			messageKey: "AG",
			"a":        "1",
			"g1": map[string]any{
				"b": "2",
				"c": "3",
			},
		}
		if diff := cmp.Diff(want_ag, payload_ag); diff != "" {
			t.Errorf("Payload mismatch (-want +got):\n%s", diff)
		}
	})

	// Scenario 2: Group -> Attrs -> Group -> Attrs
	t.Run("Group_Attrs_Group_Attrs", func(t *testing.T) {
		mockLogger.Reset()
		h := baseHandler.WithGroup("g1")
		h = h.WithAttrs([]slog.Attr{slog.String("a", "1")}) // Attr 'a' added within 'g1'
		h = h.WithGroup("g2")
		h = h.WithAttrs([]slog.Attr{slog.String("b", "2")}) // Attr 'b' added within 'g1/g2'

		record_ga := slog.NewRecord(time.Now(), slog.LevelInfo, "GA", 0)
		record_ga.AddAttrs(slog.String("c", "3")) // Record attr 'c' added within 'g1/g2'
		err := h.Handle(context.Background(), record_ga)
		if err != nil {
			t.Fatalf("Handle failed: %v", err)
		}

		entry_ga, logged := mockLogger.GetLastEntry()
		if !logged {
			t.Fatal("No log entry")
		}
		payload_ga, ok := entry_ga.Payload.(map[string]any)
		if !ok {
			t.Fatalf("Payload is not map[string]any")
		}

		// Expect 'a' under 'g1', 'b' and 'c' under 'g1/g2'.
		want_ga := map[string]any{
			messageKey: "GA",
			"g1": map[string]any{
				"a": "1",
				"g2": map[string]any{
					"b": "2",
					"c": "3",
				},
			},
		}
		if diff := cmp.Diff(want_ga, payload_ga); diff != "" {
			t.Errorf("Payload mismatch (-want +got):\n%s", diff)
		}
	})
}
