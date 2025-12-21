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
	"os"
	"strconv"
	"strings"
	"sync"

	gcppropagator "github.com/GoogleCloudPlatform/opentelemetry-operations-go/propagator"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

var installPropagatorOnce sync.Once

// init triggers default propagator installation when the package is imported.
func init() {
	autoSetPropagation()
}

// autoSetPropagation applies the default propagator when import-time auto-set is enabled.
func autoSetPropagation() {
	if !propagatorAutoSetEnabled() {
		return
	}
	EnsurePropagation()
}

// EnsurePropagation configures a composite OpenTelemetry text map propagator that
// prefers the W3C Trace Context headers while accepting Google Cloud's legacy
// X-Cloud-Trace-Context header on ingress. The configuration is applied exactly
// once per process. Import-time auto-set can be disabled via
// SLOGCP_PROPAGATOR_AUTOSET, but explicit calls to EnsurePropagation are always honored.
//
// The installed propagator order is:
//  1. CloudTraceOneWayPropagator (extracts X-Cloud-Trace-Context only)
//  2. TraceContext (W3C traceparent/tracestate)
//  3. Baggage
//
// Applications remain free to override the global propagator afterwards by
// calling otel.SetTextMapPropagator with their own implementation.
func EnsurePropagation() {
	installPropagatorOnce.Do(func() {
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			gcppropagator.CloudTraceOneWayPropagator{},
			propagation.TraceContext{},
			propagation.Baggage{},
		))
	})
}

// propagatorAutoSetEnabled reports whether automatic propagator installation is enabled
// via the SLOGCP_PROPAGATOR_AUTOSET environment variable.
func propagatorAutoSetEnabled() bool {
	raw := strings.TrimSpace(os.Getenv("SLOGCP_PROPAGATOR_AUTOSET"))
	if raw == "" {
		return true
	}
	b, err := strconv.ParseBool(raw)
	if err == nil {
		return b
	}
	return true
}
