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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/testing/protocmp"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// customValuer is a helper type implementing slog.LogValuer for testing.
type customValuer struct {
	Name string
	ID   int
}

func (cv customValuer) LogValue() slog.Value {
	return slog.GroupValue(slog.String("name", cv.Name), slog.Int("id", cv.ID))
}

// recursiveValuer is a helper type for testing recursive LogValue calls.
type recursiveValuer struct {
	Name string
	Next *recursiveValuer
}

func (rv *recursiveValuer) LogValue() slog.Value {
	if rv == nil {
		return slog.GroupValue()
	}
	attrs := []slog.Attr{slog.String("name", rv.Name)}
	if rv.Next != nil {
		attrs = append(attrs, slog.Any("next", rv.Next))
	}
	return slog.GroupValue(attrs...)
}

// TestResolveSlogValue verifies conversion of slog.Value to JSON-marshalable Go types.
func TestResolveSlogValue(t *testing.T) {
	now := time.Now().UTC()
	testErr := errors.New("test error")
	// Correctly initialize logging.HTTPRequest by embedding an *http.Request.
	testHTTPRawRequestForLog, _ := http.NewRequest("GET", "/for-log", nil)
	testHTTPRequest := &logging.HTTPRequest{Request: testHTTPRawRequestForLog, Status: 200}
	testHTTPRawRequest, _ := http.NewRequest("GET", "/", nil) // Raw request should be filtered

	rec3 := &recursiveValuer{Name: "level3"}
	rec2 := &recursiveValuer{Name: "level2", Next: rec3}
	rec1 := &recursiveValuer{Name: "level1", Next: rec2}

	testCases := []struct {
		name  string
		value slog.Value
		want  any // Expected resolved Go value
	}{
		{"String", slog.StringValue("hello"), "hello"},
		{"Int64", slog.Int64Value(123), int64(123)},
		{"Uint64", slog.Uint64Value(456), uint64(456)},
		{"Float64", slog.Float64Value(3.14), float64(3.14)},
		{"BoolTrue", slog.BoolValue(true), true},
		{"BoolFalse", slog.BoolValue(false), false},
		{"Duration", slog.DurationValue(time.Second * 5), (time.Second * 5).String()},
		{"Time", slog.TimeValue(now), now.Format(time.RFC3339Nano)},
		{"Nil", slog.AnyValue(nil), nil},
		{"Error", slog.AnyValue(testErr), testErr},
		{"HTTPRequest", slog.AnyValue(testHTTPRequest), testHTTPRequest},
		{"RawHTTPRequest", slog.AnyValue(testHTTPRawRequest), nil}, // Filtered out
		{"SimpleGroup", slog.GroupValue(slog.String("a", "b"), slog.Int("c", 1)), map[string]any{"a": "b", "c": int64(1)}},
		{"EmptyGroup", slog.GroupValue(), nil},
		{"GroupWithEmptyKey", slog.GroupValue(slog.String("", "ignore"), slog.Int("c", 1)), map[string]any{"c": int64(1)}},
		{"GroupWithNilResolvedValue", slog.GroupValue(slog.Any("a", nil), slog.Int("b", 2)), map[string]any{"b": int64(2)}},
		{"GroupResolvingToEmpty", slog.GroupValue(slog.Group("empty_sub")), nil},
		{"NestedGroup", slog.GroupValue(slog.Group("sub", slog.Bool("ok", true))), map[string]any{"sub": map[string]any{"ok": true}}},
		{"CustomLogValuer", slog.AnyValue(customValuer{Name: "tester", ID: 99}), map[string]any{"name": "tester", "id": int64(99)}},
		{"RecursiveLogValuer_DepthCheck", slog.AnyValue(rec1), map[string]any{
			"name": "level1",
			"next": map[string]any{
				"name": "level2",
				"next": map[string]any{
					"name": "level3",
				},
			},
		}},
		{"ProtoDuration", slog.AnyValue(durationpb.New(time.Minute)), durationpb.New(time.Minute)},
		{"ProtoTimestamp", slog.AnyValue(timestamppb.New(now)), timestamppb.New(now)},
		{"ProtoStruct", slog.AnyValue(&structpb.Struct{Fields: map[string]*structpb.Value{"foo": structpb.NewStringValue("bar")}}), &structpb.Struct{Fields: map[string]*structpb.Value{"foo": structpb.NewStringValue("bar")}}},
		{"ByteSlice", slog.AnyValue([]byte("abc")), []byte("abc")},
	}

	// cmp options for comparing test results.
	// Use protocmp.Transform() for correct comparison of protobuf messages.
	protoOpts := cmp.Options{protocmp.Transform()}

	// Custom comparer for the recursive test case to only check the first few levels.
	recursiveComparer := cmp.Comparer(func(x, y map[string]any) bool {
		if x["name"] != y["name"] {
			return false
		}
		xNext, xOk := x["next"].(map[string]any)
		yNext, yOk := y["next"].(map[string]any)
		if xOk != yOk {
			return false
		}
		if !xOk {
			return true
		} // Both nil 'next' is okay

		if xNext["name"] != yNext["name"] {
			return false
		}
		xNext2, xOk2 := xNext["next"].(map[string]any)
		yNext2, yOk2 := yNext["next"].(map[string]any)
		if xOk2 != yOk2 {
			return false
		}
		if !xOk2 {
			return true
		} // Both nil 'next' at level 2 is okay

		if xNext2["name"] != yNext2["name"] {
			return false
		}
		// Don't check further down, assume slog handles deeper levels/cycles.
		return true
	})

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSlogValue(tc.value)

			// Use switch for special type comparisons (error, HTTPRequest), cmp.Diff for others.
			switch want := tc.want.(type) {
			case error:
				gotErr, ok := got.(error)
				// Use errors.Is for robust error comparison.
				if !ok || !errors.Is(gotErr, want) {
					t.Errorf("resolveSlogValue error mismatch: got %v (%T), want %v (%T)", got, got, want, want)
				}
			case *logging.HTTPRequest:
				gotReq, ok := got.(*logging.HTTPRequest)
				// Compare pointers for HTTPRequest struct.
				if !ok || gotReq != want {
					t.Errorf("resolveSlogValue HTTPRequest pointer mismatch: got %p, want %p", got, want)
				}
			case map[string]any:
				comparer := protoOpts // Start with proto options
				if tc.name == "RecursiveLogValuer_DepthCheck" {
					comparer = append(comparer, recursiveComparer)
				}
				// Use cmp.Diff for map comparison.
				if diff := cmp.Diff(want, got, comparer); diff != "" {
					t.Errorf("resolveSlogValue map mismatch (-want +got):\n%s", diff)
				}
			case []byte:
				// Use cmp.Diff for byte slice comparison.
				if diff := cmp.Diff(want, got); diff != "" {
					t.Errorf("resolveSlogValue byte slice mismatch (-want +got):\n%s", diff)
				}
			default:
				// Use cmp.Diff for other types (primitives, strings, protos).
				if diff := cmp.Diff(want, got, protoOpts); diff != "" {
					t.Errorf("resolveSlogValue mismatch (-want +got):\n%s", diff)
				}
			}
		})
	}
}

// TestFormatErrorForReporting verifies error formatting and origin stack trace extraction.
// Note: Fallback stack generation is now tested via TestGcpHandler_Handle.
func TestFormatErrorForReporting(t *testing.T) {
	basicErr := errors.New("a basic error")
	// Use the updated newStackError which captures PCs for the interface
	stackErr := newStackError("error with stack", "myFunc()\n\t/path/to/file.go:10\nmain.main()\n\t/path/to/main.go:20")
	wrappedStackErr := fmt.Errorf("wrapped: %w", stackErr)
	wrappedBasicErr := fmt.Errorf("wrapping basic: %w", basicErr)

	testCases := []struct {
		name              string
		err               error
		wantFormattedErr  formattedError
		wantStackNonEmpty bool   // Checks if origin stack was found
		wantStackContains string // Substring to check in origin stack (if non-empty)
	}{
		{
			name:              "Nil error",
			err:               nil,
			wantFormattedErr:  formattedError{Message: "<nil error>", Type: ""},
			wantStackNonEmpty: false,
		},
		{
			name:              "Basic error (no origin stack)",
			err:               basicErr,
			wantFormattedErr:  formattedError{Message: "a basic error", Type: "*errors.errorString"},
			wantStackNonEmpty: false, // No stackTracer interface
		},
		{
			name:              "Stack error (interface provides stack)",
			err:               stackErr,
			wantFormattedErr:  formattedError{Message: "error with stack", Type: "*gcp.stackError"},
			wantStackNonEmpty: true,
			// Expect stack from the error itself, starting with the helper function
			wantStackContains: "newStackError",
		},
		{
			name:              "Wrapped stack error (interface provides stack)",
			err:               wrappedStackErr,
			wantFormattedErr:  formattedError{Message: "wrapped: error with stack", Type: "*fmt.wrapError"}, // Type is wrapper
			wantStackNonEmpty: true,
			// Expect stack from underlying error via interface
			wantStackContains: "newStackError",
		},
		{
			name:              "Wrapped basic error (no origin stack)",
			err:               wrappedBasicErr,
			wantFormattedErr:  formattedError{Message: "wrapping basic: a basic error", Type: "*fmt.wrapError"}, // Type is wrapper
			wantStackNonEmpty: false,                                                                            // Basic error doesn't provide stack via interface
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Call the simplified formatErrorForReporting
			gotFormattedErr, gotOriginStackTrace := formatErrorForReporting(tc.err)

			// Compare formattedError struct using cmp.Diff.
			if diff := cmp.Diff(tc.wantFormattedErr, gotFormattedErr); diff != "" {
				t.Errorf("formattedError mismatch (-want +got):\n%s", diff)
			}

			// Verify origin stack trace presence/absence.
			gotStackNonEmpty := gotOriginStackTrace != ""
			if gotStackNonEmpty != tc.wantStackNonEmpty {
				t.Errorf("Origin stack trace presence mismatch: got non-empty=%v, want non-empty=%v.\nStack:\n%s",
					gotStackNonEmpty, tc.wantStackNonEmpty, gotOriginStackTrace)
			}

			// If origin stack trace expected, perform basic content check.
			if tc.wantStackNonEmpty && tc.wantStackContains != "" {
				if !strings.Contains(gotOriginStackTrace, tc.wantStackContains) {
					t.Errorf("Origin stack trace mismatch: expected to contain %q, but got:\n%s",
						tc.wantStackContains, gotOriginStackTrace)
				}
			}
		})
	}
}
