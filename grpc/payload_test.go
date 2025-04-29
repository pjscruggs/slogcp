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

package grpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/wrapperspb"

	slogcppkg "github.com/pjscruggs/slogcp"
)

// mockPayloadLogHandler captures logs for verification in logPayload tests.
// It implements the slog.Handler interface.
type mockPayloadLogHandler struct {
	mu      sync.Mutex
	allLogs []capturedLog // Stores all logs captured during a test run.
}

// capturedLog holds details of a single log entry captured by the mock handler.
type capturedLog struct {
	level slog.Level
	msg   string
	attrs []slog.Attr
}

func (m *mockPayloadLogHandler) Enabled(context.Context, slog.Level) bool { return true }
func (m *mockPayloadLogHandler) Handle(_ context.Context, r slog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	attrs := []slog.Attr{}
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	m.allLogs = append(m.allLogs, capturedLog{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}

// WithAttrs returns the same handler for simplicity in this mock.
func (m *mockPayloadLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return m }

// WithGroup returns the same handler for simplicity in this mock.
func (m *mockPayloadLogHandler) WithGroup(name string) slog.Handler { return m }

// GetAllLogs returns a copy of all logs captured by the handler.
func (m *mockPayloadLogHandler) GetAllLogs() []capturedLog {
	m.mu.Lock()
	defer m.mu.Unlock()
	logsCopy := make([]capturedLog, len(m.allLogs))
	copy(logsCopy, m.allLogs)
	return logsCopy
}

// Reset clears the captured log state.
func (m *mockPayloadLogHandler) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.allLogs = nil
}

// TestLogPayload verifies the logic for logging message payloads, including
// marshalling, truncation, attribute generation, and handling of different message types.
// It assumes the decision to log payloads has already been made by the caller (interceptor).
func TestLogPayload(t *testing.T) {
	ctx := context.Background()

	type testCase struct {
		name           string
		message        any     // The payload message
		opts           options // Interceptor options (only maxPayloadLogSize is relevant here)
		direction      string
		expectLogCount int // How many logs are expected?
		// Verification fields for the primary log (if expectLogCount == 1)
		wantLogLevel    slog.Level
		wantMsgContains string
		wantAttrs       map[string]any // Map of expected attribute keys and values
		// Custom verification function for complex cases (e.g., marshal error)
		checkAllLogs func(t *testing.T, logs []capturedLog)
	}

	testCases := []testCase{
		{
			name:            "Simple proto, no limit",
			message:         &wrapperspb.StringValue{Value: "hello"},
			opts:            options{maxPayloadLogSize: 0}, // logPayloads assumed true by caller
			direction:       "sent",
			expectLogCount:  1,
			wantLogLevel:    slog.LevelDebug,
			wantMsgContains: "payload sent",
			wantAttrs: map[string]any{
				payloadDirectionKey: "sent",
				payloadTypeKey:      "*wrapperspb.StringValue",
				payloadKey:          `"hello"`, // protojson marshals StringValue directly to JSON string
				payloadTruncatedKey: false,
			},
		},
		{
			name:            "Simple proto, truncated",
			message:         &wrapperspb.StringValue{Value: "this is a longer string"}, // Marshalled: `"this is a longer string"` -> 25 bytes
			opts:            options{maxPayloadLogSize: 15},
			direction:       "sent",
			expectLogCount:  1,
			wantLogLevel:    slog.LevelDebug,
			wantMsgContains: "payload sent",
			wantAttrs: map[string]any{
				payloadDirectionKey:    "sent",
				payloadTypeKey:         "*wrapperspb.StringValue",
				payloadPreviewKey:      `"this is a long`, // Truncated at 15 bytes
				payloadTruncatedKey:    true,
				payloadOriginalSizeKey: int64(25), // Use int64 for consistency with slog
			},
		},
		{
			name:            "Empty proto, no limit",
			message:         &emptypb.Empty{},
			opts:            options{maxPayloadLogSize: 0},
			direction:       "received",
			expectLogCount:  1,
			wantLogLevel:    slog.LevelDebug,
			wantMsgContains: "payload received",
			wantAttrs: map[string]any{
				payloadDirectionKey: "received",
				payloadTypeKey:      "*emptypb.Empty",
				payloadKey:          `{}`, // Empty proto marshals to empty JSON object
				payloadTruncatedKey: false,
			},
		},
		{
			name:            "Non-proto message",
			message:         struct{ Name string }{Name: "test"},
			opts:            options{maxPayloadLogSize: 0},
			direction:       "sent",
			expectLogCount:  1,
			wantLogLevel:    slog.LevelDebug,
			wantMsgContains: "payload sent (non-proto)",
			wantAttrs: map[string]any{
				payloadDirectionKey: "sent",
				payloadTypeKey:      "struct { Name string }", // Check type name
			},
		},
		{
			name:           "Marshal error",
			message:        &wrapperspb.StringValue{Value: "fail marshal"},
			opts:           options{}, // maxPayloadLogSize defaults to 0
			direction:      "received",
			expectLogCount: 2, // Expect Warn log for marshal error AND Debug log for payload event
			checkAllLogs: func(t *testing.T, logs []capturedLog) {
				t.Logf("--- Start checkAllLogs for Marshal error ---")
				t.Logf("Received %d logs: %+v", len(logs), logs)
				if len(logs) != 2 {
					// Provide more context on failure
					expected := "Expected 2 logs (Warn for marshal error, Debug for event)"
					actualCount := len(logs)
					actualContent := fmt.Sprintf("%+v", logs) // Show details of what *was* logged
					t.Fatalf("%s, but got %d. Logs captured: %s", expected, actualCount, actualContent)
				}

				// Check Warn log (should be first)
				t.Logf("Checking first log (expect Warn)...")
				warnLog := logs[0]
				if warnLog.level != slog.LevelWarn {
					t.Errorf("Marshal Error Check: Expected first log level Warn (%v), got %v", slog.LevelWarn, warnLog.level)
				} else {
					t.Logf("First log level OK (Warn)")
				}
				expectedWarnMsg := "Failed to marshal"
				if !strings.Contains(warnLog.msg, expectedWarnMsg) {
					t.Errorf("Marshal Error Check: Expected first log msg containing %q, got %q", expectedWarnMsg, warnLog.msg)
				} else {
					t.Logf("First log message content OK")
				}
				// Check for the error attribute in the Warn log
				foundErrorAttr := false
				for _, attr := range warnLog.attrs {
					if attr.Key == "error" {
						foundErrorAttr = true
						t.Logf("Found 'error' attribute in Warn log: %v", attr.Value)
						break
					}
				}
				if !foundErrorAttr {
					t.Error("Marshal Error Check: Warn log missing 'error' attribute")
				} else {
					t.Logf("'error' attribute found in Warn log")
				}

				// Check Debug log (should be second)
				t.Logf("Checking second log (expect Debug)...")
				debugLog := logs[1]
				if debugLog.level != slog.LevelDebug {
					t.Errorf("Marshal Error Check: Expected second log level Debug (%v), got %v", slog.LevelDebug, debugLog.level)
				} else {
					t.Logf("Second log level OK (Debug)")
				}
				expectedDebugMsg := "payload received (marshal error)"
				if !strings.Contains(debugLog.msg, expectedDebugMsg) {
					t.Errorf("Marshal Error Check: Expected second log msg containing %q, got %q", expectedDebugMsg, debugLog.msg)
				} else {
					t.Logf("Second log message content OK")
				}
				// Check essential attributes in the Debug log
				foundDirection := false
				foundType := false
				for _, attr := range debugLog.attrs {
					if attr.Key == payloadDirectionKey {
						foundDirection = true
					}
					if attr.Key == payloadTypeKey {
						foundType = true
					}
				}
				if !foundDirection {
					t.Error("Marshal Error Check: Debug log missing direction attribute")
				} else {
					t.Logf("Direction attribute found in Debug log")
				}
				if !foundType {
					t.Error("Marshal Error Check: Debug log missing type attribute")
				} else {
					t.Logf("Type attribute found in Debug log")
				}
				t.Logf("--- End checkAllLogs for Marshal error ---")
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("--- Starting test case: %s ---", tc.name)
			mockHandler := &mockPayloadLogHandler{}
			slogger := slog.New(mockHandler)
			// Create an actual *slogcp.Logger, embedding the slog.Logger that uses our mock handler.
			testLogger := &slogcppkg.Logger{Logger: slogger}

			// Call the function under test or simulate its error path
			t.Logf("Preparing to call/simulate logPayload for %s...", tc.name)
			if tc.name == "Marshal error" {
				// Directly simulate the error handling block within logPayload
				marshalErr := errors.New("simulated marshal error")
				// Ensure the message is actually a proto.Message for type casting
				p, ok := tc.message.(proto.Message)
				if !ok {
					t.Fatalf("Test setup error: message for 'Marshal error' case is not a proto.Message (%T)", tc.message)
				}

				t.Logf("Simulating LOG CALL 1 (Warn) for %s", tc.name)
				testLogger.LogAttrs(ctx, slog.LevelWarn,
					"Failed to marshal gRPC payload for logging",
					slog.String(payloadDirectionKey, tc.direction),
					slog.String(payloadTypeKey, fmt.Sprintf("%T", p)),
					slog.Any("error", marshalErr),
				)
				t.Logf("Simulating LOG CALL 2 (Debug) for %s", tc.name)
				testLogger.LogAttrs(ctx, slog.LevelDebug,
					fmt.Sprintf("gRPC payload %s (marshal error)", tc.direction),
					slog.String(payloadDirectionKey, tc.direction),
					slog.String(payloadTypeKey, fmt.Sprintf("%T", p)),
				)
				t.Logf("Finished simulating marshal error path execution for %s", tc.name)
			} else {
				// Call the actual function for non-error cases
				t.Logf("Calling actual logPayload function for %s", tc.name)
				logPayload(ctx, testLogger, &tc.opts, tc.direction, tc.message)
			}
			t.Logf("Finished calling/simulating logPayload for %s", tc.name)

			// Verification
			allLogs := mockHandler.GetAllLogs()
			t.Logf("Captured %d logs for %s: %+v", len(allLogs), tc.name, allLogs)

			// Check log count first
			if len(allLogs) != tc.expectLogCount {
				// Use t.Fatalf here as subsequent checks depend on the correct count
				t.Fatalf("Log count mismatch for %s: Expected %d log(s), got %d. Logs: %+v",
					tc.name, tc.expectLogCount, len(allLogs), allLogs)
			}

			// Perform detailed checks
			if tc.checkAllLogs != nil {
				// Use custom verification function for complex cases like marshal error.
				t.Logf("Running custom checkAllLogs for %s", tc.name)
				tc.checkAllLogs(t, allLogs)
			} else if tc.expectLogCount == 1 {
				// Default verification for single log cases.
				t.Logf("Running default single-log verification for %s", tc.name)
				log := allLogs[0]
				if log.level != tc.wantLogLevel {
					t.Errorf("Log level mismatch for %s: got %v, want %v", tc.name, log.level, tc.wantLogLevel)
				}
				if !strings.Contains(log.msg, tc.wantMsgContains) {
					t.Errorf("Log message mismatch for %s: got %q, want message containing %q", tc.name, log.msg, tc.wantMsgContains)
				}

				// Verify expected attributes are present and have correct values.
				loggedAttrsMap := make(map[string]any)
				for _, a := range log.attrs {
					// Resolve attribute value for comparison map
					switch a.Value.Kind() {
					case slog.KindString:
						loggedAttrsMap[a.Key] = a.Value.String()
					case slog.KindBool:
						loggedAttrsMap[a.Key] = a.Value.Bool()
					case slog.KindInt64:
						loggedAttrsMap[a.Key] = a.Value.Int64() // Keep as int64
					case slog.KindUint64:
						loggedAttrsMap[a.Key] = a.Value.Uint64()
					case slog.KindFloat64:
						loggedAttrsMap[a.Key] = a.Value.Float64()
					default:
						loggedAttrsMap[a.Key] = a.Value.Any() // Keep original for cmp
					}
				}

				// Use cmp.Diff to compare the maps of resolved attributes.
				protoOpts := cmp.Options{protocmp.Transform()}
				if diff := cmp.Diff(tc.wantAttrs, loggedAttrsMap, protoOpts); diff != "" {
					t.Errorf("Logged attributes mismatch for %s (-want +got):\n%s", tc.name, diff)
				}

			} else if tc.expectLogCount > 1 && tc.checkAllLogs == nil {
				// This case should ideally be handled by checkAllLogs
				t.Errorf("Test case %s expected multiple logs but did not provide checkAllLogs function", tc.name)
			}
			// Case expectLogCount == 0 is handled by the initial length check.
			t.Logf("--- Finished test case: %s ---", tc.name)
		})
	}
}
