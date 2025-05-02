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

package slogcp

import (
	"log/slog"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
	"google.golang.org/protobuf/testing/protocmp"
)

// applyOptions applies a series of Option functions to a new options struct
// and returns the resulting struct.
func applyOptions(opts ...Option) *options {
	o := &options{} // Start with a zero-value options struct
	for _, opt := range opts {
		if opt != nil { // Guard against nil options
			opt(o)
		}
	}
	return o
}

// TestOptionsApplication verifies that each With... Option function correctly
// modifies the internal options struct used during logger initialization.
func TestOptionsApplication(t *testing.T) {
	t.Parallel() // Allow subtests for different options to run in parallel.

	// Test WithLogTarget
	t.Run("WithLogTarget", func(t *testing.T) {
		t.Parallel()
		testCases := []struct {
			name string
			in   LogTarget
			want LogTarget
		}{
			{"GCP", LogTargetGCP, LogTargetGCP},
			{"Stdout", LogTargetStdout, LogTargetStdout},
			{"Stderr", LogTargetStderr, LogTargetStderr},
		}
		for _, tc := range testCases {
			tc := tc // Capture range variable.
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				opts := applyOptions(WithLogTarget(tc.in))
				if opts.logTarget == nil {
					t.Fatalf("WithLogTarget(%v): opts.logTarget is nil, want pointer to %v", tc.in, tc.want)
				}
				if *opts.logTarget != tc.want {
					t.Errorf("WithLogTarget(%v): got *opts.logTarget = %v, want %v", tc.in, *opts.logTarget, tc.want)
				}
			})
		}
	})

	// Test WithProjectID
	t.Run("WithProjectID", func(t *testing.T) {
		t.Parallel()
		wantID := "my-test-project-id"
		opts := applyOptions(WithProjectID(wantID))
		if opts.projectID == nil {
			t.Fatalf("WithProjectID(%q): opts.projectID is nil, want pointer to %q", wantID, wantID)
		}
		if *opts.projectID != wantID {
			t.Errorf("WithProjectID(%q): got *opts.projectID = %q, want %q", wantID, *opts.projectID, wantID)
		}
	})

	// Test WithMonitoredResource
	t.Run("WithMonitoredResource", func(t *testing.T) {
		t.Parallel()
		wantRes := &mrpb.MonitoredResource{Type: "gce_instance", Labels: map[string]string{"instance_id": "123"}}
		opts := applyOptions(WithMonitoredResource(wantRes))
		if opts.monitoredResource == nil {
			t.Fatal("WithMonitoredResource(): opts.monitoredResource is nil, want non-nil")
		}
		// Use cmp.Diff with protocmp for comparing proto messages.
		if diff := cmp.Diff(wantRes, opts.monitoredResource, protocmp.Transform()); diff != "" {
			t.Errorf("WithMonitoredResource() mismatch (-want +got):\n%s", diff)
		}
		// Check pointer equality to ensure the same instance was stored.
		if opts.monitoredResource != wantRes {
			t.Error("WithMonitoredResource() pointer mismatch, expected same instance")
		}
	})

	// Test WithLevel
	t.Run("WithLevel", func(t *testing.T) {
		t.Parallel()
		testCases := []struct {
			name string
			in   slog.Level
			want slog.Level
		}{
			{"Debug", slog.LevelDebug, slog.LevelDebug},
			{"Info", slog.LevelInfo, slog.LevelInfo},
			{"Warn", slog.LevelWarn, slog.LevelWarn},
			{"Error", slog.LevelError, slog.LevelError},
			{"Notice", LevelNotice.Level(), LevelNotice.Level()},
			{"Critical", LevelCritical.Level(), LevelCritical.Level()},
		}
		for _, tc := range testCases {
			tc := tc // Capture range variable.
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				opts := applyOptions(WithLevel(tc.in))
				if opts.level == nil {
					t.Fatalf("WithLevel(%v): opts.level is nil, want pointer to %v", tc.in, tc.want)
				}
				if *opts.level != tc.want {
					t.Errorf("WithLevel(%v): got *opts.level = %v, want %v", tc.in, *opts.level, tc.want)
				}
			})
		}
	})

	// Test WithSourceLocationEnabled
	t.Run("WithSourceLocationEnabled", func(t *testing.T) {
		t.Parallel()
		// Test true
		optsTrue := applyOptions(WithSourceLocationEnabled(true))
		if optsTrue.addSource == nil {
			t.Fatal("WithSourceLocationEnabled(true): opts.addSource is nil, want pointer to true")
		}
		if !*optsTrue.addSource {
			t.Error("WithSourceLocationEnabled(true): got *opts.addSource = false, want true")
		}
		// Test false
		optsFalse := applyOptions(WithSourceLocationEnabled(false))
		if optsFalse.addSource == nil {
			t.Fatal("WithSourceLocationEnabled(false): opts.addSource is nil, want pointer to false")
		}
		if *optsFalse.addSource {
			t.Error("WithSourceLocationEnabled(false): got *opts.addSource = true, want false")
		}
	})

	// Test WithStackTraceEnabled
	t.Run("WithStackTraceEnabled", func(t *testing.T) {
		t.Parallel()
		// Test true
		optsTrue := applyOptions(WithStackTraceEnabled(true))
		if optsTrue.stackTraceEnabled == nil {
			t.Fatal("WithStackTraceEnabled(true): opts.stackTraceEnabled is nil, want pointer to true")
		}
		if !*optsTrue.stackTraceEnabled {
			t.Error("WithStackTraceEnabled(true): got *opts.stackTraceEnabled = false, want true")
		}
		// Test false
		optsFalse := applyOptions(WithStackTraceEnabled(false))
		if optsFalse.stackTraceEnabled == nil {
			t.Fatal("WithStackTraceEnabled(false): opts.stackTraceEnabled is nil, want pointer to false")
		}
		if *optsFalse.stackTraceEnabled {
			t.Error("WithStackTraceEnabled(false): got *opts.stackTraceEnabled = true, want false")
		}
	})

	// Test WithStackTraceLevel
	t.Run("WithStackTraceLevel", func(t *testing.T) {
		t.Parallel()
		wantLevel := slog.LevelWarn
		opts := applyOptions(WithStackTraceLevel(wantLevel))
		if opts.stackTraceLevel == nil {
			t.Fatalf("WithStackTraceLevel(%v): opts.stackTraceLevel is nil, want pointer to %v", wantLevel, wantLevel)
		}
		if *opts.stackTraceLevel != wantLevel {
			t.Errorf("WithStackTraceLevel(%v): got *opts.stackTraceLevel = %v, want %v", wantLevel, *opts.stackTraceLevel, wantLevel)
		}
	})

	// Test WithAttrs
	t.Run("WithAttrs", func(t *testing.T) {
		t.Parallel()
		attr1 := slog.String("key1", "val1")
		attr2 := slog.Int("key2", 123)

		// Test single call
		t.Run("SingleCall", func(t *testing.T) {
			t.Parallel()
			opts1 := applyOptions(WithAttrs([]slog.Attr{attr1, attr2}))
			want1 := []slog.Attr{attr1, attr2}
			if diff := cmp.Diff(want1, opts1.initialAttrs, cmpopts.IgnoreUnexported(slog.Value{})); diff != "" {
				t.Errorf("WithAttrs() mismatch (-want +got):\n%s", diff)
			}
		})

		// Test multiple calls (cumulative)
		t.Run("MultipleCalls", func(t *testing.T) {
			t.Parallel()
			opts2 := applyOptions(WithAttrs([]slog.Attr{attr1}), WithAttrs([]slog.Attr{attr2}))
			want2 := []slog.Attr{attr1, attr2}
			if diff := cmp.Diff(want2, opts2.initialAttrs, cmpopts.IgnoreUnexported(slog.Value{})); diff != "" {
				t.Errorf("WithAttrs() multiple calls mismatch (-want +got):\n%s", diff)
			}
		})

		// Test empty slice
		t.Run("EmptySlice", func(t *testing.T) {
			t.Parallel()
			optsEmpty := applyOptions(WithAttrs([]slog.Attr{}))
			if len(optsEmpty.initialAttrs) != 0 {
				t.Errorf("WithAttrs([]slog.Attr{}): got length %d, want 0", len(optsEmpty.initialAttrs))
			}
		})

		// Test nil slice
		t.Run("NilSlice", func(t *testing.T) {
			t.Parallel()
			optsNil := applyOptions(WithAttrs(nil))
			if len(optsNil.initialAttrs) != 0 {
				t.Errorf("WithAttrs(nil): got length %d, want 0", len(optsNil.initialAttrs))
			}
		})

		// Test immutability (slice copying)
		t.Run("Immutability", func(t *testing.T) {
			t.Parallel()
			originalAttrs := []slog.Attr{attr1}
			optsCopy := applyOptions(WithAttrs(originalAttrs))
			// Modify the original slice *after* applying the option
			originalAttrs[0] = attr2
			// Check if the stored slice in options was affected
			if len(optsCopy.initialAttrs) > 0 && optsCopy.initialAttrs[0].Key == attr2.Key {
				t.Error("WithAttrs() did not copy the input slice; modification affected stored attributes")
			} else if len(optsCopy.initialAttrs) == 0 {
				t.Error("WithAttrs() stored slice is unexpectedly empty after original modification")
			} else if optsCopy.initialAttrs[0].Key != attr1.Key {
				t.Errorf("WithAttrs() stored attribute key = %q, want %q (original value)", optsCopy.initialAttrs[0].Key, attr1.Key)
			}
		})
	})

	// Test WithGroup
	t.Run("WithGroup", func(t *testing.T) {
		t.Parallel()
		// Test single call
		t.Run("SingleCall", func(t *testing.T) {
			t.Parallel()
			opts1 := applyOptions(WithGroup("group1"))
			if opts1.initialGroup != "group1" {
				t.Errorf("WithGroup(%q): got %q, want %q", "group1", opts1.initialGroup, "group1")
			}
		})

		// Test multiple calls (last wins)
		t.Run("MultipleCalls", func(t *testing.T) {
			t.Parallel()
			opts2 := applyOptions(WithGroup("group1"), WithGroup("group2"))
			if opts2.initialGroup != "group2" {
				t.Errorf("WithGroup() multiple calls: got %q, want %q (last one)", opts2.initialGroup, "group2")
			}
		})

		// Test empty group name
		t.Run("EmptyName", func(t *testing.T) {
			t.Parallel()
			optsEmpty := applyOptions(WithGroup(""))
			if optsEmpty.initialGroup != "" {
				t.Errorf("WithGroup(%q): got %q, want empty string", "", optsEmpty.initialGroup)
			}
		})
	})

	// Test WithEntryCountThreshold
	t.Run("WithEntryCountThreshold", func(t *testing.T) {
		t.Parallel()
		wantCount := 500
		opts := applyOptions(WithEntryCountThreshold(wantCount))
		if opts.entryCountThreshold == nil {
			t.Fatalf("WithEntryCountThreshold(%d): opts.entryCountThreshold is nil, want pointer to %d", wantCount, wantCount)
		}
		if *opts.entryCountThreshold != wantCount {
			t.Errorf("WithEntryCountThreshold(%d): got *opts.entryCountThreshold = %d, want %d", wantCount, *opts.entryCountThreshold, wantCount)
		}
	})

	// Test WithDelayThreshold
	t.Run("WithDelayThreshold", func(t *testing.T) {
		t.Parallel()
		wantDelay := 10 * time.Second
		opts := applyOptions(WithDelayThreshold(wantDelay))
		if opts.delayThreshold == nil {
			t.Fatalf("WithDelayThreshold(%v): opts.delayThreshold is nil, want pointer to %v", wantDelay, wantDelay)
		}
		if *opts.delayThreshold != wantDelay {
			t.Errorf("WithDelayThreshold(%v): got *opts.delayThreshold = %v, want %v", wantDelay, *opts.delayThreshold, wantDelay)
		}
	})

	// Test WithCloudRunPayloadAttributes
	t.Run("WithCloudRunPayloadAttributes", func(t *testing.T) {
		t.Parallel()
		// Test true
		optsTrue := applyOptions(WithCloudRunPayloadAttributes(true))
		if optsTrue.addCloudRunPayloadAttributes == nil {
			t.Fatal("WithCloudRunPayloadAttributes(true): opts.addCloudRunPayloadAttributes is nil, want pointer to true")
		}
		if !*optsTrue.addCloudRunPayloadAttributes {
			t.Error("WithCloudRunPayloadAttributes(true): got *opts.addCloudRunPayloadAttributes = false, want true")
		}
		// Test false
		optsFalse := applyOptions(WithCloudRunPayloadAttributes(false))
		if optsFalse.addCloudRunPayloadAttributes == nil {
			t.Fatal("WithCloudRunPayloadAttributes(false): opts.addCloudRunPayloadAttributes is nil, want pointer to false")
		}
		if *optsFalse.addCloudRunPayloadAttributes {
			t.Error("WithCloudRunPayloadAttributes(false): got *opts.addCloudRunPayloadAttributes = true, want false")
		}
	})
}
