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
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// TestSplitMethodName verifies parsing of full gRPC method strings.
func TestSplitMethodName(t *testing.T) {
	testCases := []struct {
		fullMethod string
		wantSvc    string
		wantMethod string
	}{
		{"/package.Service/Method", "package.Service", "Method"},
		{"/Service/Method", "Service", "Method"}, // No package
		{"/Method", "unknown", "Method"},         // Root level method
		{"/", "unknown", "."},                    // Slash only
		{"", "unknown", "."},                     // Empty string
		{"/pkg.svc.version1.Service/CreateUser", "pkg.svc.version1.Service", "CreateUser"},
		{"MethodWithoutSlash", "unknown", "MethodWithoutSlash"}, // No leading slash
	}

	for _, tc := range testCases {
		t.Run(tc.fullMethod, func(t *testing.T) {
			gotSvc, gotMethod := splitMethodName(tc.fullMethod)
			if gotSvc != tc.wantSvc || gotMethod != tc.wantMethod {
				t.Errorf("splitMethodName(%q) = (%q, %q), want (%q, %q)",
					tc.fullMethod, gotSvc, gotMethod, tc.wantSvc, tc.wantMethod)
			}
		})
	}
}

// TestFilterMetadata verifies default and custom metadata filtering logic.
func TestFilterMetadata(t *testing.T) {
	// Input MD with mixed casing and headers to be filtered/kept
	inputMD := metadata.MD{
		"Authorization":  {"Bearer secret"},    // Filtered (default)
		"content-type":   {"application/grpc"}, // Kept
		"User-Agent":     {"my-client"},        // Kept (original case)
		"cookie":         {"session=abc"},      // Filtered (default)
		"X-Custom-ID":    {"123", "456"},       // Kept (original case)
		"grpc-trace-bin": {"binarydata"},       // Filtered (default)
		"set-cookie":     {"pref=a"},           // Filtered (default)
		"x-csrf-token":   {"token"},            // Filtered (default)
	}
	inputMDCopy := inputMD.Copy() // Create a copy for modification test

	t.Run("DefaultFilter", func(t *testing.T) {
		// Expected output map preserves original casing for kept keys.
		want := metadata.MD{
			"content-type": {"application/grpc"},
			"User-Agent":   {"my-client"},
			"X-Custom-ID":  {"123", "456"},
		}
		got := filterMetadata(inputMDCopy, nil) // nil uses default filter

		// Use cmp.Diff for robust map comparison.
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("filterMetadata with default filter mismatch (-want +got):\n%s", diff)
		}

		// Verify input slice modification doesn't affect output (copy check).
		// Modify the *copy* of the input MD.
		inputMDCopy["X-Custom-ID"][0] = "999"
		// Check the *result* map using the correct casing.
		if got["X-Custom-ID"][0] == "999" {
			t.Errorf("filterMetadata output slice was not copied, modification affected result (got[X-Custom-ID][0] == %q)", got["X-Custom-ID"][0])
		}
	})

	t.Run("CustomFilter", func(t *testing.T) {
		// Custom filter allows only user-agent, case-insensitive
		customFilter := func(key string) bool {
			return strings.ToLower(key) == "user-agent"
		}
		// Expect the original casing of the kept key in the output.
		want := metadata.MD{
			"User-Agent": {"my-client"},
		}
		// Use original inputMD again, passing a copy to avoid interference
		got := filterMetadata(inputMD.Copy(), customFilter)
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("filterMetadata with custom filter mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("FilterAll", func(t *testing.T) {
		filterAll := func(key string) bool { return false }
		got := filterMetadata(inputMD.Copy(), filterAll)
		if got != nil { // Expect nil for empty result
			t.Errorf("filterMetadata with filterAll expected nil, got %v (len %d)", got, len(got))
		}
	})

	t.Run("NilInput", func(t *testing.T) {
		got := filterMetadata(nil, nil)
		if got != nil {
			t.Errorf("filterMetadata with nil input expected nil, got %v", got)
		}
	})

	t.Run("EmptyInput", func(t *testing.T) {
		got := filterMetadata(metadata.MD{}, nil)
		if got != nil { // Expect nil for empty result
			t.Errorf("filterMetadata with empty input expected nil, got %v", got)
		}
	})
}

// TestAssembleFinishAttrs verifies the construction of common log attributes for finished RPC calls.
func TestAssembleFinishAttrs(t *testing.T) {
	testDuration := 55 * time.Millisecond
	testPeer := "1.2.3.4:5678"
	testStatusErr := status.Error(codes.NotFound, "item not found")
	testPlainErr := errors.New("a plain error")
	testNilErr := error(nil) // Explicitly nil error

	testCases := []struct {
		name     string
		duration time.Duration
		err      error
		peerAddr string
		want     []slog.Attr
	}{
		{
			name:     "Success with peer",
			duration: testDuration,
			err:      testNilErr,
			peerAddr: testPeer,
			want: []slog.Attr{
				slog.Duration(grpcDurationKey, testDuration),
				slog.String(grpcCodeKey, codes.OK.String()),
				slog.String(peerAddressKey, testPeer),
			},
		},
		{
			name:     "Success without peer (client)",
			duration: testDuration,
			err:      testNilErr,
			peerAddr: "",
			want: []slog.Attr{
				slog.Duration(grpcDurationKey, testDuration),
				slog.String(grpcCodeKey, codes.OK.String()),
			},
		},
		{
			name:     "Status Error with peer",
			duration: testDuration,
			err:      testStatusErr,
			peerAddr: testPeer,
			want: []slog.Attr{
				slog.Duration(grpcDurationKey, testDuration),
				slog.String(grpcCodeKey, codes.NotFound.String()),
				slog.String(peerAddressKey, testPeer),
				slog.Any("error", testStatusErr), // Key should be "error"
			},
		},
		{
			name:     "Plain Error without peer (client)",
			duration: testDuration,
			err:      testPlainErr,
			peerAddr: "",
			want: []slog.Attr{
				slog.Duration(grpcDurationKey, testDuration),
				slog.String(grpcCodeKey, codes.Unknown.String()), // Defaults to Unknown for non-status errors
				slog.Any("error", testPlainErr),
			},
		},
	}

	// Use cmp options to handle slog.Attr comparison, ignoring unexported fields
	// and using errors.Is for robust error comparison.
	opts := []cmp.Option{
		cmpopts.IgnoreUnexported(slog.Value{}),
		cmp.Comparer(func(x, y error) bool {
			if x == nil && y == nil {
				return true
			}
			if x == nil || y == nil {
				return false
			}
			// Use errors.Is for potential wrapping, also check message for basic cases.
			return errors.Is(x, y) || errors.Is(y, x) || x.Error() == y.Error()
		}),
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := assembleFinishAttrs(tc.duration, tc.err, tc.peerAddr)
			// Use cmp.Diff with options for comparing slog.Attr slices. Order matters.
			if diff := cmp.Diff(tc.want, got, opts...); diff != "" {
				t.Errorf("assembleFinishAttrs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// mockPanicLogger is a mock slog.Handler to capture logs for TestHandlePanic.
type mockPanicLogger struct {
	mu        sync.Mutex
	lastLevel slog.Level
	lastMsg   string
	lastAttrs []slog.Attr
	logged    bool
}

func (m *mockPanicLogger) Enabled(context.Context, slog.Level) bool { return true }
func (m *mockPanicLogger) Handle(_ context.Context, r slog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastLevel = r.Level
	m.lastMsg = r.Message
	attrs := []slog.Attr{}
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})
	m.lastAttrs = attrs
	m.logged = true
	return nil
}
func (m *mockPanicLogger) WithAttrs(attrs []slog.Attr) slog.Handler { return m } // Simplistic mock
func (m *mockPanicLogger) WithGroup(name string) slog.Handler       { return m } // Simplistic mock
func (m *mockPanicLogger) GetLog() (slog.Level, string, []slog.Attr, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastLevel, m.lastMsg, m.lastAttrs, m.logged
}
func (m *mockPanicLogger) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logged = false
	m.lastAttrs = nil
	m.lastMsg = ""
	m.lastLevel = 0
}

// findAttr is a helper to find an attribute in a slice for verification.
func findAttr(attrs []slog.Attr, key string) (slog.Value, bool) {
	for _, a := range attrs {
		if a.Key == key {
			return a.Value, true
		}
	}
	return slog.Value{}, false
}

// TestHandlePanic verifies panic recovery, logging, and error wrapping.
func TestHandlePanic(t *testing.T) {
	mockHandler := &mockPanicLogger{}
	testLogger := slog.New(mockHandler) // Use slog.New with mock handler
	ctx := context.Background()

	t.Run("NoPanic", func(t *testing.T) {
		mockHandler.Reset()
		var recoveredValue any // Simulate recover() returning nil
		isPanic, err := handlePanic(ctx, testLogger, recoveredValue)

		if isPanic {
			t.Error("handlePanic reported panic when none occurred")
		}
		if err != nil {
			t.Errorf("handlePanic returned error %v when no panic occurred", err)
		}
		_, _, _, logged := mockHandler.GetLog()
		if logged {
			t.Error("handlePanic logged when no panic occurred")
		}
	})

	t.Run("WithPanic", func(t *testing.T) {
		mockHandler.Reset()
		panicValue := "oh dear"
		var isPanic bool
		var err error

		// Use defer/recover/panic structure to simulate real panic handling.
		func() {
			defer func() {
				if r := recover(); r != nil {
					isPanic, err = handlePanic(ctx, testLogger, r)
				}
			}()
			panic(panicValue) // Induce panic
		}()

		// Verify isPanic flag and returned error status/code.
		if !isPanic {
			t.Error("handlePanic did not report panic when one occurred")
		}
		if err == nil {
			t.Fatal("handlePanic did not return an error after panic")
		}
		st, ok := status.FromError(err)
		if !ok {
			t.Fatalf("handlePanic returned non-status error: %v", err)
		}
		if st.Code() != codes.Internal {
			t.Errorf("handlePanic returned error with code %v, want %v", st.Code(), codes.Internal)
		}
		if !strings.Contains(st.Message(), "panic") {
			t.Errorf("handlePanic error message %q does not contain 'panic'", st.Message())
		}

		// Verify the log entry details (level, message, attributes).
		level, msg, attrs, logged := mockHandler.GetLog()
		if !logged {
			t.Fatal("handlePanic did not log after panic occurred")
		}
		if level != internalLevelCritical {
			t.Errorf("handlePanic logged at level %v, want %v", level, internalLevelCritical)
		}
		if !strings.Contains(msg, "Recovered panic") {
			t.Errorf("handlePanic log message %q does not contain 'Recovered panic'", msg)
		}

		// Verify specific panic-related attributes using helper.
		panicValAttr, foundPanicValue := findAttr(attrs, panicValueKey)
		stackAttr, foundStack := findAttr(attrs, panicStackKey)

		if !foundPanicValue {
			t.Error("handlePanic log attributes missing panic.value")
		} else if panicValAttr.Any() != panicValue { // Compare recovered value
			t.Errorf("handlePanic logged panic.value = %v, want %v", panicValAttr.Any(), panicValue)
		}

		if !foundStack {
			t.Error("handlePanic log attributes missing panic.stack")
		} else {
			loggedStack := stackAttr.String()
			if loggedStack == "" {
				t.Error("handlePanic logged empty panic.stack")
			}
			// Check stack trace contains expected function names for basic validation.
			// Note: We check for functions *in* the stack, not runtime.Stack itself.
			if !strings.Contains(loggedStack, "handlePanic") {
				t.Errorf("Stack trace missing handlePanic: %s", loggedStack)
			}
			if !strings.Contains(loggedStack, "TestHandlePanic") {
				t.Errorf("Stack trace missing TestHandlePanic: %s", loggedStack)
			}
			// Check that it does NOT contain runtime.Stack, as that's the generator, not the call site.
			if strings.Contains(loggedStack, "runtime.Stack") {
				t.Errorf("Stack trace unexpectedly contains runtime.Stack: %s", loggedStack)
			}
		}
	})
}
