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

// Note: stackError type and newStackError function are defined in common_test.go
// and are accessible within this package.

// TestFormatErrorForReporting verifies error formatting and conditional stack trace generation.
func TestFormatErrorForReporting(t *testing.T) {
	basicErr := errors.New("a basic error")
	stackErr := newStackError("error with stack", "myFunc()\n\t/path/to/file.go:10\nmain.main()\n\t/path/to/main.go:20")
	wrappedStackErr := fmt.Errorf("wrapped: %w", stackErr)
	wrappedBasicErr := fmt.Errorf("wrapping basic: %w", basicErr)

	testCases := []struct {
		name              string
		err               error
		level             slog.Level // Level of the log record
		stackTraceEnabled bool
		stackTraceLevel   slog.Level // Threshold for generating stack
		wantFormattedErr  formattedError
		wantStackNonEmpty bool
		wantStackContains string // Substring to check in stack (if non-empty)
	}{
		{
			name: "Nil error",
			err:  nil, level: slog.LevelError, stackTraceEnabled: true, stackTraceLevel: slog.LevelError,
			wantFormattedErr:  formattedError{Message: "<nil error>", Type: ""},
			wantStackNonEmpty: false,
		},
		{
			name: "Basic error, stack disabled",
			err:  basicErr, level: slog.LevelError, stackTraceEnabled: false, stackTraceLevel: slog.LevelError,
			wantFormattedErr:  formattedError{Message: "a basic error", Type: "*errors.errorString"},
			wantStackNonEmpty: false,
		},
		{
			name: "Basic error, stack enabled, level below threshold",
			err:  basicErr, level: slog.LevelWarn, stackTraceEnabled: true, stackTraceLevel: slog.LevelError, // Level < Threshold
			wantFormattedErr:  formattedError{Message: "a basic error", Type: "*errors.errorString"},
			wantStackNonEmpty: false,
		},
		{
			name: "Basic error, stack enabled, level meets threshold (fallback stack)",
			err:  basicErr, level: slog.LevelError, stackTraceEnabled: true, stackTraceLevel: slog.LevelError, // Level == Threshold
			wantFormattedErr:  formattedError{Message: "a basic error", Type: "*errors.errorString"},
			wantStackNonEmpty: true,
			wantStackContains: "runtime.Callers", // Check for runtime call in fallback
		},
		{
			name: "Basic error, stack enabled, level above threshold (fallback stack)",
			err:  basicErr, level: internalLevelCritical, stackTraceEnabled: true, stackTraceLevel: slog.LevelError, // Level > Threshold
			wantFormattedErr:  formattedError{Message: "a basic error", Type: "*errors.errorString"},
			wantStackNonEmpty: true,
			wantStackContains: "runtime.Callers",
		},
		{
			name: "Stack error (%+v), stack enabled, level meets threshold",
			err:  stackErr, level: slog.LevelError, stackTraceEnabled: true, stackTraceLevel: slog.LevelError,
			wantFormattedErr:  formattedError{Message: "error with stack", Type: "*gcp.stackError"}, // Type comes from common_test.go now
			wantStackNonEmpty: true,
			wantStackContains: "myFunc()", // Expect stack from error itself via %+v
		},
		{
			name: "Wrapped stack error (%+v), stack enabled, level meets threshold",
			err:  wrappedStackErr, level: slog.LevelError, stackTraceEnabled: true, stackTraceLevel: slog.LevelError,
			wantFormattedErr:  formattedError{Message: "wrapped: error with stack", Type: "*fmt.wrapError"}, // Type is wrapper
			wantStackNonEmpty: true,
			wantStackContains: "myFunc()", // Expect stack from underlying error via %+v
		},
		{
			name: "Wrapped basic error, stack enabled (fallback stack)",
			err:  wrappedBasicErr, level: slog.LevelError, stackTraceEnabled: true, stackTraceLevel: slog.LevelError,
			wantFormattedErr:  formattedError{Message: "wrapping basic: a basic error", Type: "*fmt.wrapError"}, // Type is wrapper
			wantStackNonEmpty: true,
			wantStackContains: "runtime.Callers", // Basic error doesn't provide stack via %+v, use fallback
		},
		{
			name: "Stack error (%+v), stack disabled",
			err:  stackErr, level: slog.LevelError, stackTraceEnabled: false, stackTraceLevel: slog.LevelError,
			wantFormattedErr:  formattedError{Message: "error with stack", Type: "*gcp.stackError"},
			wantStackNonEmpty: false, // Stack disabled
		},
		{
			name: "Stack error (%+v), stack enabled, level below threshold",
			err:  stackErr, level: slog.LevelWarn, stackTraceEnabled: true, stackTraceLevel: slog.LevelError, // Level < Threshold
			wantFormattedErr:  formattedError{Message: "error with stack", Type: "*gcp.stackError"},
			wantStackNonEmpty: false, // Stack not generated due to level
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gotFormattedErr, gotStackTrace := formatErrorForReporting(
				tc.err,
				tc.level,
				tc.stackTraceEnabled,
				tc.stackTraceLevel,
			)

			// Compare formattedError struct using cmp.Diff.
			if diff := cmp.Diff(tc.wantFormattedErr, gotFormattedErr); diff != "" {
				t.Errorf("formattedError mismatch (-want +got):\n%s", diff)
			}

			// Verify stack trace presence/absence.
			gotStackNonEmpty := gotStackTrace != ""
			if gotStackNonEmpty != tc.wantStackNonEmpty {
				t.Errorf("Stack trace presence mismatch: got non-empty=%v, want non-empty=%v.\nStack:\n%s",
					gotStackNonEmpty, tc.wantStackNonEmpty, gotStackTrace)
			}

			// If stack trace expected, perform basic content check using substring matching.
			// Exact stack trace content is fragile and depends on runtime/build details.
			if tc.wantStackNonEmpty && tc.wantStackContains != "" {
				if !strings.Contains(gotStackTrace, tc.wantStackContains) {
					t.Errorf("Stack trace mismatch: expected to contain %q, but got:\n%s",
						tc.wantStackContains, gotStackTrace)
				}
			}
		})
	}
}
