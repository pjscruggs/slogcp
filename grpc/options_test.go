// Copyright 2025 Patrick J. Scruggs
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License.

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
// Deterministic testing of sampling rates between 0.0 and 1.0 is not performed here
// due to the reliance on time.Now() for randomization. Only boundary conditions (0.0, 1.0)
// and skip path logic are reliably tested.
func TestProcessOptions(t *testing.T) {
	t.Run("Defaults", func(t *testing.T) {
		opts := processOptions() // No options provided

		// Check interceptor defaults
		if !opts.panicRecovery {
			t.Error("Default panicRecovery should be true")
		}
		if opts.autoStackTrace {
			t.Error("Default autoStackTrace should be false")
		}

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

	t.Run("BooleanOptions", func(t *testing.T) {
		// Toggle panicRecovery
		opts1 := processOptions(WithPanicRecovery(false))
		if opts1.panicRecovery {
			t.Error("WithPanicRecovery(false) did not set panicRecovery to false")
		}
		// Toggle back
		opts2 := processOptions(WithPanicRecovery(false), WithPanicRecovery(true))
		if !opts2.panicRecovery {
			t.Error("WithPanicRecovery(true) did not set panicRecovery to true")
		}

		// Toggle autoStackTrace
		opts3 := processOptions(WithAutoStackTrace(true))
		if !opts3.autoStackTrace {
			t.Error("WithAutoStackTrace(true) did not set autoStackTrace to true")
		}
		// Toggle back
		opts4 := processOptions(WithAutoStackTrace(true), WithAutoStackTrace(false))
		if opts4.autoStackTrace {
			t.Error("WithAutoStackTrace(false) did not set autoStackTrace to false")
		}
	})

	t.Run("WithOptions", func(t *testing.T) {
		// Define custom options
		customLevelFunc := func(c codes.Code) slog.Level { return slog.LevelDebug }
		customShouldLog := func(ctx context.Context, m string) bool { return strings.HasPrefix(m, "/Keep") }
		customMetaFilter := func(k string) bool { return k == "x-request-id" }
		skip := []string{"/Keep/Healthz", "/Internal"}
		rate := 0.5
		category := "my_grpc"

		opts := processOptions(
			WithLevels(customLevelFunc),
			WithShouldLog(customShouldLog),
			WithPayloadLogging(true),
			WithMaxPayloadSize(1024),
			WithMetadataLogging(true),
			WithMetadataFilter(customMetaFilter),
			WithSkipPaths(skip),
			WithSamplingRate(rate),
			WithLogCategory(category),
		)

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
		if diff := cmp.Diff(skip, opts.skipPaths); diff != "" {
			t.Errorf("WithSkipPaths mismatch (-want +got):\n%s", diff)
		}
		if opts.samplingRate != rate {
			t.Errorf("WithSamplingRate(%f) not applied, got %f", rate, opts.samplingRate)
		}
		if opts.logCategory != category {
			t.Errorf("WithLogCategory(%q) not applied, got %q", category, opts.logCategory)
		}

		ctx := context.Background()
		checkShouldLog(t, "Filtered_Custom", opts.shouldLogFunc, ctx, "/Other/Method", false)
		checkShouldLog(t, "Filtered_SkipPath", opts.shouldLogFunc, ctx, "/Keep/Healthz", false)
		checkShouldLog(t, "Filtered_CustomAndSkip", opts.shouldLogFunc, ctx, "/Internal/Method", false)

		optsRateZero := processOptions(WithSamplingRate(0.0))
		checkShouldLog(t, "SampleRateZero", optsRateZero.shouldLogFunc, ctx, "/Any/Method", false)
		optsRateOne := processOptions(WithSamplingRate(1.0))
		checkShouldLog(t, "SampleRateOne", optsRateOne.shouldLogFunc, ctx, "/Any/Method", true)
	})

	t.Run("NilOptions", func(t *testing.T) {
		optsNil := processOptions(
			WithLevels(nil),
			WithShouldLog(nil),
			WithMetadataFilter(nil),
		)

		if optsNil.levelFunc(codes.OK) == slog.LevelDebug {
			t.Error("Nil WithLevels did not reset to default")
		}
		if !optsNil.shouldLogFunc(context.Background(), "/Any/Method") {
			t.Error("Nil WithShouldLog did not reset to default")
		}
		if optsNil.metadataFilterFunc("authorization") {
			t.Error("Nil WithMetadataFilter did not reset to default")
		}
	})

	t.Run("MaxPayloadSizeEdges", func(t *testing.T) {
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
