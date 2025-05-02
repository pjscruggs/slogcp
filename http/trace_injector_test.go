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

package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestInjectTraceContextMiddleware(t *testing.T) {
	validTraceIDHex := "4bf92f3577b34da6a3ce929d0e0e4736"

	tests := []struct {
		name              string
		headerValue       string
		existingContext   bool
		wantTraceID       string
		wantSpanID        string
		wantSampled       bool
		wantContextChange bool
	}{
		{
			name:              "Valid header with sampling",
			headerValue:       validTraceIDHex + "/1234567890;o=1",
			existingContext:   false,
			wantTraceID:       validTraceIDHex,
			wantContextChange: true,
			wantSampled:       true,
		},
		{
			name:              "Valid header without sampling",
			headerValue:       validTraceIDHex + "/1234567890;o=0",
			existingContext:   false,
			wantTraceID:       validTraceIDHex,
			wantContextChange: true,
			wantSampled:       false,
		},
		{
			name:              "Valid header without options",
			headerValue:       validTraceIDHex + "/1234567890",
			existingContext:   false,
			wantTraceID:       validTraceIDHex,
			wantContextChange: true,
			wantSampled:       false,
		},
		{
			name:              "Invalid header format (missing slash)",
			headerValue:       validTraceIDHex + "1234567890",
			existingContext:   false,
			wantContextChange: false,
		},
		{
			name:              "Invalid trace ID format",
			headerValue:       "invalid-trace-id/1234567890;o=1",
			existingContext:   false,
			wantContextChange: false,
		},
		{
			name:              "Invalid span ID format",
			headerValue:       validTraceIDHex + "/invalid-span-id;o=1",
			existingContext:   false,
			wantTraceID:       validTraceIDHex, // Should still inject a valid trace context with generated span ID
			wantContextChange: true,
			wantSampled:       true,
		},
		{
			name:              "Empty header",
			headerValue:       "",
			existingContext:   false,
			wantContextChange: false,
		},
		{
			name:              "With existing context",
			headerValue:       validTraceIDHex + "/1234567890;o=1",
			existingContext:   true,
			wantContextChange: false, // Should not change existing context
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create test handler that inspects the context
			var gotContext context.Context
			testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotContext = r.Context()
				w.WriteHeader(http.StatusOK)
			})

			// Apply middleware
			middleware := InjectTraceContextMiddleware()
			wrappedHandler := middleware(testHandler)

			// Create test request
			req := httptest.NewRequest("GET", "/test", nil)
			if tt.headerValue != "" {
				req.Header.Set(XCloudTraceContextHeader, tt.headerValue)
			}

			// Inject existing context if needed
			if tt.existingContext {
				existingTraceID, _ := trace.TraceIDFromHex("11111111111111112222222222222222")
				existingSpanID, _ := trace.SpanIDFromHex("3333333333333333")
				sc := trace.NewSpanContext(trace.SpanContextConfig{
					TraceID:    existingTraceID,
					SpanID:     existingSpanID,
					TraceFlags: trace.FlagsSampled,
					Remote:     true,
				})
				req = req.WithContext(trace.ContextWithRemoteSpanContext(req.Context(), sc))
			}

			// Execute request
			recorder := httptest.NewRecorder()
			wrappedHandler.ServeHTTP(recorder, req)

			// Verify results
			if recorder.Code != http.StatusOK {
				t.Errorf("Handler returned wrong status code: got %v want %v", recorder.Code, http.StatusOK)
			}

			spanContext := trace.SpanContextFromContext(gotContext)
			contextChanged := spanContext.IsValid() && (!tt.existingContext || spanContext.TraceID().String() != "11111111111111112222222222222222")

			if contextChanged != tt.wantContextChange {
				t.Errorf("Context change: got %v, want %v", contextChanged, tt.wantContextChange)
			}

			if tt.wantContextChange {
				// Verify trace ID
				if spanContext.TraceID().String() != tt.wantTraceID {
					t.Errorf("TraceID: got %v, want %v", spanContext.TraceID().String(), tt.wantTraceID)
				}

				// Verify sampling flag
				if spanContext.IsSampled() != tt.wantSampled {
					t.Errorf("IsSampled: got %v, want %v", spanContext.IsSampled(), tt.wantSampled)
				}

				// Verify remote flag
				if !spanContext.IsRemote() {
					t.Error("Expected IsRemote() to be true")
				}
			}
		})
	}
}

func TestInjectTraceContextFromHeader(t *testing.T) {
	validTraceIDHex := "4bf92f3577b34da6a3ce929d0e0e4736"
	baseCtx := context.Background()

	tests := []struct {
		name        string
		headerValue string
		wantValid   bool
		wantSampled bool
	}{
		{
			name:        "Valid with sampling",
			headerValue: validTraceIDHex + "/1234567890;o=1",
			wantValid:   true,
			wantSampled: true,
		},
		{
			name:        "Valid without sampling",
			headerValue: validTraceIDHex + "/1234567890;o=0",
			wantValid:   true,
			wantSampled: false,
		},
		{
			name:        "Valid without options",
			headerValue: validTraceIDHex + "/1234567890",
			wantValid:   true,
			wantSampled: false,
		},
		{
			name:        "Invalid format (missing slash)",
			headerValue: validTraceIDHex + "1234567890",
			wantValid:   false,
		},
		{
			name:        "Invalid trace ID",
			headerValue: "invalid-trace-id/1234567890;o=1",
			wantValid:   false,
		},
		{
			name:        "Invalid span ID",
			headerValue: validTraceIDHex + "/invalid-span-id;o=1",
			wantValid:   true, // Should create a valid context with generated span ID
			wantSampled: true,
		},
		{
			name:        "Zero trace ID",
			headerValue: "00000000000000000000000000000000/1234567890;o=1",
			wantValid:   false, // Invalid trace ID (all zeros)
		},
		{
			name:        "Very large span ID",
			headerValue: validTraceIDHex + "/18446744073709551615;o=1", // max uint64
			wantValid:   true,
			wantSampled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resultCtx := injectTraceContextFromHeader(baseCtx, tt.headerValue)
			spanContext := trace.SpanContextFromContext(resultCtx)

			// Check if context is valid as expected
			if spanContext.IsValid() != tt.wantValid {
				t.Errorf("Context validity: got %v, want %v", spanContext.IsValid(), tt.wantValid)
			}

			// If we expect a valid context, verify trace details
			if tt.wantValid {
				if spanContext.TraceID().String() != validTraceIDHex {
					t.Errorf("TraceID: got %v, want %v", spanContext.TraceID().String(), validTraceIDHex)
				}

				if spanContext.IsSampled() != tt.wantSampled {
					t.Errorf("IsSampled: got %v, want %v", spanContext.IsSampled(), tt.wantSampled)
				}

				if spanContext.SpanID().IsValid() == false {
					t.Error("Expected valid SpanID")
				}

				if !spanContext.IsRemote() {
					t.Error("Expected IsRemote() to be true")
				}
			}
		})
	}
}
