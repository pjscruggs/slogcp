package grpc

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
)

// TestDefaultCodeToLevel verifies the standard mapping from gRPC codes to log levels.
func TestDefaultCodeToLevel(t *testing.T) {
	testCases := []struct {
		code codes.Code
		want slog.Level
	}{
		{codes.OK, slog.LevelInfo},
		{codes.Canceled, slog.LevelInfo},
		{codes.InvalidArgument, slog.LevelWarn},
		{codes.NotFound, slog.LevelWarn},
		{codes.AlreadyExists, slog.LevelWarn},
		{codes.Unauthenticated, slog.LevelWarn},
		{codes.PermissionDenied, slog.LevelWarn},
		{codes.DeadlineExceeded, slog.LevelWarn},
		{codes.ResourceExhausted, slog.LevelWarn},
		{codes.FailedPrecondition, slog.LevelWarn},
		{codes.Aborted, slog.LevelWarn},
		{codes.OutOfRange, slog.LevelWarn},
		{codes.Unavailable, slog.LevelWarn},
		{codes.Unknown, slog.LevelError},
		{codes.Unimplemented, slog.LevelError},
		{codes.Internal, slog.LevelError},
		{codes.DataLoss, slog.LevelError},
		{codes.Code(999), slog.LevelError}, // Unrecognized code defaults to Error
	}

	for _, tc := range testCases {
		t.Run(tc.code.String(), func(t *testing.T) {
			got := defaultCodeToLevel(tc.code)
			if got != tc.want {
				t.Errorf("defaultCodeToLevel(%v) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

// checkShouldLog is a helper function for comparing ShouldLogFunc results.
func checkShouldLog(t *testing.T, name string, fn ShouldLogFunc, ctx context.Context, method string, want bool) {
	t.Helper()
	got := fn(ctx, method)
	if got != want {
		t.Errorf("%s: shouldLogFunc(%q) = %v, want %v", name, method, got, want)
	}
}

// TestProcessOptions verifies option application and the composite shouldLogFunc logic.
// Note: Deterministic testing of sampling rates between 0.0 and 1.0 is not performed here
// due to the reliance on time.Now() for randomization. Only boundary conditions (0.0, 1.0)
// and skip path logic are reliably tested.
func TestProcessOptions(t *testing.T) {

	t.Run("Defaults", func(t *testing.T) {
		opts := processOptions() // No options provided

		// Check default level func behavior
		if opts.levelFunc(codes.OK) != slog.LevelInfo || opts.levelFunc(codes.Internal) != slog.LevelError {
			t.Error("Default levelFunc has unexpected behavior")
		}
		// Check default shouldLog func (always true)
		if !opts.shouldLogFunc(context.Background(), "/Any/Method") {
			t.Error("Default shouldLogFunc returned false")
		}
		// Check other defaults
		if opts.logPayloads {
			t.Error("Default logPayloads should be false")
		}
		if opts.maxPayloadLogSize != defaultMaxPayloadLogSize {
			t.Errorf("Default maxPayloadLogSize = %d, want %d", opts.maxPayloadLogSize, defaultMaxPayloadLogSize)
		}
		if opts.logMetadata {
			t.Error("Default logMetadata should be false")
		}
		// Check default metadata filter behavior
		if !opts.metadataFilterFunc("content-type") || opts.metadataFilterFunc("authorization") {
			t.Error("Default metadataFilterFunc has unexpected behavior")
		}
		if opts.skipPaths != nil {
			t.Errorf("Default skipPaths should be nil, got %v", opts.skipPaths)
		}
		if opts.samplingRate != 1.0 {
			t.Errorf("Default samplingRate = %f, want 1.0", opts.samplingRate)
		}
		if opts.logCategory != defaultLogCategory {
			t.Errorf("Default logCategory = %q, want %q", opts.logCategory, defaultLogCategory)
		}
	})

	t.Run("WithOptions", func(t *testing.T) {
		// Define custom options
		customLevelFunc := func(c codes.Code) slog.Level { return slog.LevelDebug }
		customShouldLog := func(ctx context.Context, m string) bool { return strings.HasPrefix(m, "/Keep") } // Keep methods starting with /Keep
		customMetaFilter := func(k string) bool { return k == "x-request-id" }
		skip := []string{"/Keep/Healthz", "/Internal"} // Skip specific paths
		rate := 0.5                                    // Sampling rate (not reliably tested for intermediate values)
		category := "my_grpc"

		opts := processOptions(
			WithLevels(customLevelFunc),
			WithShouldLog(customShouldLog), // User filter
			WithPayloadLogging(true),
			WithMaxPayloadSize(1024),
			WithMetadataLogging(true),
			WithMetadataFilter(customMetaFilter),
			WithSkipPaths(skip), // Skip filter
			WithSamplingRate(rate),
			WithLogCategory(category),
		)

		// Check custom options are stored/applied where possible
		if opts.levelFunc(codes.OK) != slog.LevelDebug {
			t.Error("Custom levelFunc not applied")
		}
		if !opts.logPayloads {
			t.Error("WithPayloadLogging(true) not applied")
		}
		if opts.maxPayloadLogSize != 1024 {
			t.Errorf("WithMaxPayloadSize(1024) not applied, got %d", opts.maxPayloadLogSize)
		}
		if !opts.logMetadata {
			t.Error("WithMetadataLogging(true) not applied")
		}
		if !opts.metadataFilterFunc("x-request-id") || opts.metadataFilterFunc("content-type") {
			t.Error("Custom metadataFilterFunc not applied correctly")
		}
		// Check skipPaths storage
		if diff := cmp.Diff(skip, opts.skipPaths); diff != "" {
			t.Errorf("WithSkipPaths mismatch (-want +got):\n%s", diff)
		}
		if opts.samplingRate != rate {
			t.Errorf("WithSamplingRate(%f) not applied, got %f", rate, opts.samplingRate)
		}
		if opts.logCategory != category {
			t.Errorf("WithLogCategory(%q) not applied, got %q", category, opts.logCategory)
		}

		// Test the composite shouldLogFunc focusing on user func + skip paths interaction.
		// Sampling rate 0.5 makes the outcome non-deterministic, so we only test
		// cases guaranteed to be false by the other filters.
		ctx := context.Background()
		// checkShouldLog(t, "Keep_NotSkipped", opts.shouldLogFunc, ctx, "/Keep/ThisMethod", true) // Cannot reliably test this due to sampling
		checkShouldLog(t, "Filtered_Custom", opts.shouldLogFunc, ctx, "/Other/Method", false)           // Filtered by user func
		checkShouldLog(t, "Filtered_SkipPath", opts.shouldLogFunc, ctx, "/Keep/Healthz", false)         // Kept by user func, but skipped
		checkShouldLog(t, "Filtered_CustomAndSkip", opts.shouldLogFunc, ctx, "/Internal/Method", false) // Filtered by user func (skip irrelevant)

		// Verify sampling boundaries (0.0 and 1.0).
		optsRateZero := processOptions(WithSamplingRate(0.0))
		checkShouldLog(t, "SampleRateZero", optsRateZero.shouldLogFunc, ctx, "/Any/Method", false) // Now correctly handled
		optsRateOne := processOptions(WithSamplingRate(1.0))
		checkShouldLog(t, "SampleRateOne", optsRateOne.shouldLogFunc, ctx, "/Any/Method", true)
	})

	t.Run("NilOptions", func(t *testing.T) {
		// Explicitly verify that passing nil functions resets options to their defaults.
		optsNil := processOptions(
			WithLevels(nil),
			WithShouldLog(nil),
			WithMetadataFilter(nil),
		)

		// Check they reverted to default behavior
		if optsNil.levelFunc(codes.OK) == slog.LevelDebug { // Check against a non-default value
			t.Error("Nil WithLevels did not reset to default")
		}
		if !optsNil.shouldLogFunc(context.Background(), "/Any/Method") {
			t.Error("Nil WithShouldLog did not reset to default")
		}
		if optsNil.metadataFilterFunc("authorization") { // Check against a filtered key
			t.Error("Nil WithMetadataFilter did not reset to default")
		}
	})

	t.Run("MaxPayloadSizeEdges", func(t *testing.T) {
		// Verify handling of 0 and negative values for max payload size.
		optsZero := processOptions(WithMaxPayloadSize(0))
		if optsZero.maxPayloadLogSize != 0 {
			t.Errorf("WithMaxPayloadSize(0) resulted in %d, want 0", optsZero.maxPayloadLogSize)
		}
		optsNegative := processOptions(WithMaxPayloadSize(-100))
		if optsNegative.maxPayloadLogSize != 0 {
			t.Errorf("WithMaxPayloadSize(-100) resulted in %d, want 0", optsNegative.maxPayloadLogSize)
		}
	})
}
