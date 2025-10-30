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
	EnsurePropagation()
}

// EnsurePropagation configures a composite OpenTelemetry text map propagator that
// prefers the W3C Trace Context headers while accepting Google Cloud's legacy
// X-Cloud-Trace-Context header on ingress. The configuration is applied exactly
// once per process unless the SLOGCP_DISABLE_PROPAGATOR_AUTOSET environment
// variable is set to a truthy value.
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
		if disableAutoSet() {
			return
		}

		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			gcppropagator.CloudTraceOneWayPropagator{},
			propagation.TraceContext{},
			propagation.Baggage{},
		))
	})
}

// disableAutoSet reports whether automatic propagator installation is disabled
// via the SLOGCP_DISABLE_PROPAGATOR_AUTOSET environment variable.
func disableAutoSet() bool {
	raw := strings.TrimSpace(os.Getenv("SLOGCP_DISABLE_PROPAGATOR_AUTOSET"))
	if raw == "" {
		return false
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false
	}
	return b
}
