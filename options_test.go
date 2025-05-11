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
	"reflect"
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
		if opt != nil {
			opt(o)
		}
	}
	return o
}

// TestOptionsApplication verifies that each With... Option function correctly
// modifies the internal options struct used during logger initialization.
func TestOptionsApplication(t *testing.T) {
	t.Parallel()

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
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				opts := applyOptions(WithLogTarget(tc.in))
				if opts.logTarget == nil {
					t.Fatalf("WithLogTarget(%v): opts.logTarget is nil, want %v", tc.in, tc.want)
				}
				if *opts.logTarget != tc.want {
					t.Errorf("WithLogTarget(%v): got %v, want %v", tc.in, *opts.logTarget, tc.want)
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
			t.Fatalf("WithProjectID(%q): opts.projectID is nil, want %q", wantID, wantID)
		}
		if *opts.projectID != wantID {
			t.Errorf("WithProjectID(%q): got %q, want %q", wantID, *opts.projectID, wantID)
		}
	})

	// Test WithGCPMonitoredResource
	t.Run("WithGCPMonitoredResource", func(t *testing.T) {
		t.Parallel()
		wantRes := &mrpb.MonitoredResource{Type: "gce_instance", Labels: map[string]string{"instance_id": "123"}}
		opts := applyOptions(WithGCPMonitoredResource(wantRes))
		if opts.gcpMonitoredResource == nil {
			t.Fatal("WithGCPMonitoredResource: gcpMonitoredResource is nil, want non-nil")
		}
		if diff := cmp.Diff(wantRes, opts.gcpMonitoredResource, protocmp.Transform()); diff != "" {
			t.Errorf("WithGCPMonitoredResource() mismatch (-want +got):\n%s", diff)
		}
		if opts.gcpMonitoredResource != wantRes {
			t.Error("WithGCPMonitoredResource: stored instance differs from input instance")
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
			tc := tc
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				opts := applyOptions(WithLevel(tc.in))
				if opts.level == nil {
					t.Fatalf("WithLevel(%v): opts.level is nil, want %v", tc.in, tc.want)
				}
				if *opts.level != tc.want {
					t.Errorf("WithLevel(%v): got %v, want %v", tc.in, *opts.level, tc.want)
				}
			})
		}
	})

	// Test WithSourceLocationEnabled
	t.Run("WithSourceLocationEnabled", func(t *testing.T) {
		t.Parallel()
		optsTrue := applyOptions(WithSourceLocationEnabled(true))
		if optsTrue.addSource == nil {
			t.Fatal("WithSourceLocationEnabled(true): addSource is nil, want pointer to true")
		}
		if !*optsTrue.addSource {
			t.Error("WithSourceLocationEnabled(true): got false, want true")
		}

		optsFalse := applyOptions(WithSourceLocationEnabled(false))
		if optsFalse.addSource == nil {
			t.Fatal("WithSourceLocationEnabled(false): addSource is nil, want pointer to false")
		}
		if *optsFalse.addSource {
			t.Error("WithSourceLocationEnabled(false): got true, want false")
		}
	})

	// Test WithStackTraceEnabled
	t.Run("WithStackTraceEnabled", func(t *testing.T) {
		t.Parallel()
		optsTrue := applyOptions(WithStackTraceEnabled(true))
		if optsTrue.stackTraceEnabled == nil {
			t.Fatal("WithStackTraceEnabled(true): stackTraceEnabled is nil, want pointer to true")
		}
		if !*optsTrue.stackTraceEnabled {
			t.Error("WithStackTraceEnabled(true): got false, want true")
		}

		optsFalse := applyOptions(WithStackTraceEnabled(false))
		if optsFalse.stackTraceEnabled == nil {
			t.Fatal("WithStackTraceEnabled(false): stackTraceEnabled is nil, want pointer to false")
		}
		if *optsFalse.stackTraceEnabled {
			t.Error("WithStackTraceEnabled(false): got true, want false")
		}
	})

	// Test WithStackTraceLevel
	t.Run("WithStackTraceLevel", func(t *testing.T) {
		t.Parallel()
		wantLevel := slog.LevelWarn
		opts := applyOptions(WithStackTraceLevel(wantLevel))
		if opts.stackTraceLevel == nil {
			t.Fatalf("WithStackTraceLevel(%v): stackTraceLevel is nil, want pointer to %v", wantLevel, wantLevel)
		}
		if *opts.stackTraceLevel != wantLevel {
			t.Errorf("WithStackTraceLevel(%v): got %v, want %v", wantLevel, *opts.stackTraceLevel, wantLevel)
		}
	})

	// Test WithAttrs
	t.Run("WithAttrs", func(t *testing.T) {
		t.Parallel()
		attr1 := slog.String("key1", "val1")
		attr2 := slog.Int("key2", 123)

		t.Run("SingleCall", func(t *testing.T) {
			t.Parallel()
			opts1 := applyOptions(WithAttrs([]slog.Attr{attr1, attr2}))
			want1 := []slog.Attr{attr1, attr2}
			if diff := cmp.Diff(want1, opts1.programmaticAttrs, cmpopts.IgnoreUnexported(slog.Value{})); diff != "" {
				t.Errorf("WithAttrs single call mismatch (-want +got):\n%s", diff)
			}
		})

		t.Run("MultipleCalls", func(t *testing.T) {
			t.Parallel()
			opts2 := applyOptions(WithAttrs([]slog.Attr{attr1}), WithAttrs([]slog.Attr{attr2}))
			want2 := []slog.Attr{attr1, attr2}
			if diff := cmp.Diff(want2, opts2.programmaticAttrs, cmpopts.IgnoreUnexported(slog.Value{})); diff != "" {
				t.Errorf("WithAttrs multiple calls mismatch (-want +got):\n%s", diff)
			}
		})

		t.Run("EmptySlice", func(t *testing.T) {
			t.Parallel()
			optsEmpty := applyOptions(WithAttrs([]slog.Attr{}))
			if len(optsEmpty.programmaticAttrs) != 0 {
				t.Errorf("WithAttrs empty slice: got %d, want 0", len(optsEmpty.programmaticAttrs))
			}
		})

		t.Run("NilSlice", func(t *testing.T) {
			t.Parallel()
			optsNil := applyOptions(WithAttrs(nil))
			if len(optsNil.programmaticAttrs) != 0 {
				t.Errorf("WithAttrs nil slice: got %d, want 0", len(optsNil.programmaticAttrs))
			}
		})

		t.Run("Immutability", func(t *testing.T) {
			t.Parallel()
			original := []slog.Attr{attr1}
			optsCopy := applyOptions(WithAttrs(original))
			original[0] = attr2
			if optsCopy.programmaticAttrs[0].Key != "key1" {
				t.Errorf("WithAttrs immutability: stored slice altered, got key %q, want %q",
					optsCopy.programmaticAttrs[0].Key, "key1")
			}
		})
	})

	// Test WithGroup
	t.Run("WithGroup", func(t *testing.T) {
		t.Parallel()

		t.Run("SingleCall", func(t *testing.T) {
			t.Parallel()
			opts := applyOptions(WithGroup("group1"))
			if opts.programmaticGroup == nil {
				t.Fatal("WithGroup(\"group1\"): programmaticGroup is nil, want non-nil")
			}
			if *opts.programmaticGroup != "group1" {
				t.Errorf("WithGroup(%q): got %q, want %q", "group1", *opts.programmaticGroup, "group1")
			}
		})

		t.Run("MultipleCalls", func(t *testing.T) {
			t.Parallel()
			opts := applyOptions(WithGroup("group1"), WithGroup("group2"))
			if opts.programmaticGroup == nil {
				t.Fatal("WithGroup multiple calls: programmaticGroup is nil, want non-nil")
			}
			if *opts.programmaticGroup != "group2" {
				t.Errorf("WithGroup multiple calls: got %q, want %q", *opts.programmaticGroup, "group2")
			}
		})

		t.Run("EmptyName", func(t *testing.T) {
			t.Parallel()
			opts := applyOptions(WithGroup(""))
			if opts.programmaticGroup == nil {
				t.Fatal("WithGroup(\"\"): programmaticGroup is nil, want non-nil")
			}
			if *opts.programmaticGroup != "" {
				t.Errorf("WithGroup(\"\"): got %q, want empty string", *opts.programmaticGroup)
			}
		})
	})

	// Test WithGCPEntryCountThreshold
	t.Run("WithGCPEntryCountThreshold", func(t *testing.T) {
		t.Parallel()
		wantCount := 500
		opts := applyOptions(WithGCPEntryCountThreshold(wantCount))
		if opts.gcpEntryCountThreshold == nil {
			t.Fatalf("WithGCPEntryCountThreshold(%d): gcpEntryCountThreshold is nil, want pointer to %d",
				wantCount, wantCount)
		}
		if *opts.gcpEntryCountThreshold != wantCount {
			t.Errorf("WithGCPEntryCountThreshold(%d): got %d, want %d",
				wantCount, *opts.gcpEntryCountThreshold, wantCount)
		}
	})

	// Test WithGCPDelayThreshold
	t.Run("WithGCPDelayThreshold", func(t *testing.T) {
		t.Parallel()
		wantDelay := 10 * time.Second
		opts := applyOptions(WithGCPDelayThreshold(wantDelay))
		if opts.gcpDelayThreshold == nil {
			t.Fatalf("WithGCPDelayThreshold(%v): gcpDelayThreshold is nil, want pointer to %v",
				wantDelay, wantDelay)
		}
		if *opts.gcpDelayThreshold != wantDelay {
			t.Errorf("WithGCPDelayThreshold(%v): got %v, want %v",
				wantDelay, *opts.gcpDelayThreshold, wantDelay)
		}
	})

	// Test WithReplaceAttr
	t.Run("WithReplaceAttr", func(t *testing.T) {
		t.Parallel()
		testReplacer := func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == "sensitive" {
				return slog.String("masked", "****")
			}
			return a
		}

		opts := applyOptions(WithReplaceAttr(testReplacer))
		if opts.replaceAttr == nil {
			t.Fatal("WithReplaceAttr(): replaceAttr is nil, want non-nil")
		}

		// Transform a sensitive attribute
		input := slog.String("sensitive", "password123")
		out := opts.replaceAttr(nil, input)
		if out.Key != "masked" || out.Value.String() != "****" {
			t.Errorf("replaceAttr did not mask sensitive key: got %v", out)
		}

		// Non-sensitive should pass through
		normal := slog.String("normal", "value")
		out2 := opts.replaceAttr([]string{"grp"}, normal)
		if out2.Key != "normal" || out2.Value.String() != "value" {
			t.Errorf("replaceAttr modified non-sensitive attribute: got %v, want %v", out2, normal)
		}
	})

	// Test WithMiddleware
	t.Run("WithMiddleware", func(t *testing.T) {
		t.Parallel()

		mw1 := func(h slog.Handler) slog.Handler { return h }
		mw2 := func(h slog.Handler) slog.Handler { return h }

		t.Run("SingleMiddleware", func(t *testing.T) {
			t.Parallel()
			opts := applyOptions(WithMiddleware(mw1))
			if len(opts.middlewares) != 1 {
				t.Fatalf("WithMiddleware single: got %d, want 1", len(opts.middlewares))
			}
			if opts.middlewares[0] == nil {
				t.Fatal("WithMiddleware single: stored middleware is nil")
			}
		})

		t.Run("MultipleMiddlewares", func(t *testing.T) {
			t.Parallel()
			opts := applyOptions(WithMiddleware(mw1), WithMiddleware(mw2))
			if len(opts.middlewares) != 2 {
				t.Fatalf("WithMiddleware multiple: got %d, want 2", len(opts.middlewares))
			}
		})

		t.Run("ApplicationOrder", func(t *testing.T) {
			t.Parallel()
			var ids []string
			m1 := func(h slog.Handler) slog.Handler { ids = append(ids, "m1"); return h }
			m2 := func(h slog.Handler) slog.Handler { ids = append(ids, "m2"); return h }
			opts := applyOptions(WithMiddleware(m1), WithMiddleware(m2))
			for _, mw := range opts.middlewares {
				_ = mw(nil) // trigger side effect
			}
			if !reflect.DeepEqual(ids, []string{"m1", "m2"}) {
				t.Errorf("middleware order incorrect: got %v, want %v", ids, []string{"m1", "m2"})
			}
		})

		t.Run("NilMiddleware", func(t *testing.T) {
			t.Parallel()
			var nilMw Middleware
			opts := applyOptions(WithMiddleware(nilMw))
			if len(opts.middlewares) != 0 {
				t.Errorf("WithMiddleware(nil): got %d, want 0", len(opts.middlewares))
			}
		})
	})
}
