package grpc

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/stats"
)

// TestOptionsMutateConfig verifies each Option toggles the matching config knob.
func TestOptionsMutateConfig(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	prop := propagation.TraceContext{}
	tp := trace.NewNoopTracerProvider()
	filterInvoked := false
	filter := otelgrpc.Filter(func(info *stats.RPCTagInfo) bool {
		filterInvoked = true
		return info != nil
	})

	enrichCalled := false
	transformCalled := false

	cfg := applyOptions([]Option{
		WithLogger(logger),
		WithProjectID("custom-proj"),
		WithPropagators(prop),
		WithTracerProvider(tp),
		WithPublicEndpoint(true),
		WithOTel(false),
		WithSpanAttributes(attribute.String("rpc.system", "grpc")),
		WithFilter(filter),
		WithAttrEnricher(func(ctx context.Context, info *RequestInfo) []slog.Attr {
			enrichCalled = true
			return nil
		}),
		WithAttrTransformer(func(ctx context.Context, attrs []slog.Attr, info *RequestInfo) []slog.Attr {
			transformCalled = true
			return attrs
		}),
		WithPeerInfo(false),
		WithPayloadSizes(false),
		WithLegacyXCloudInjection(true),
	})

	if cfg.logger != logger {
		t.Fatalf("cfg.logger = %v, want %v", cfg.logger, logger)
	}
	if cfg.projectID != "custom-proj" {
		t.Fatalf("cfg.projectID = %q, want custom-proj", cfg.projectID)
	}
	if !cfg.propagatorsSet || cfg.propagators != prop {
		t.Fatalf("propagators not applied: %+v", cfg)
	}
	if cfg.tracerProvider != tp {
		t.Fatalf("cfg.tracerProvider = %v, want %v", cfg.tracerProvider, tp)
	}
	if !cfg.publicEndpoint {
		t.Fatalf("cfg.publicEndpoint = false, want true")
	}
	if cfg.enableOTel {
		t.Fatalf("cfg.enableOTel = true, want false")
	}
	if len(cfg.spanAttributes) != 1 {
		t.Fatalf("cfg.spanAttributes = %+v, want 1 entry", cfg.spanAttributes)
	}
	if len(cfg.filters) != 1 {
		t.Fatalf("cfg.filters = %+v, want filter registered", cfg.filters)
	}

	attrs := cfg.attrEnrichers[0](context.Background(), newRequestInfo("/svc/Method", "unary", false, time.Now()))
	if !enrichCalled {
		t.Fatalf("attr enricher not invoked")
	}
	_ = attrs
	if !cfg.filters[0](&stats.RPCTagInfo{FullMethodName: "/svc/Method"}) || !filterInvoked {
		t.Fatalf("filter should be invoked and return true")
	}
	if cfg.includePeer || cfg.includeSizes {
		t.Fatalf("includePeer=%v includeSizes=%v, want both false", cfg.includePeer, cfg.includeSizes)
	}
	if !cfg.injectLegacyXCTC {
		t.Fatalf("expected legacy XCTC injection enabled")
	}

	base := []slog.Attr{slog.String("keep", "yes")}
	got := cfg.attrTransformers[0](context.Background(), base, newRequestInfo("/svc/Method", "unary", false, time.Now()))
	if !transformCalled || len(got) != len(base) {
		t.Fatalf("attr transformer not invoked correctly: %v len=%d", got, len(got))
	}
}

// TestWithLoggerHandlesNil ensures the option falls back to slog.Default when nil is provided.
func TestWithLoggerHandlesNil(t *testing.T) {
	t.Parallel()

	custom := slog.New(slog.NewJSONHandler(io.Discard, nil))
	cfg := &config{logger: custom}

	WithLogger(nil)(cfg)
	if cfg.logger != slog.Default() {
		t.Fatalf("WithLogger(nil) should fall back to slog.Default(), got %v", cfg.logger)
	}

	alt := slog.New(slog.NewJSONHandler(io.Discard, nil))
	WithLogger(alt)(cfg)
	if cfg.logger != alt {
		t.Fatalf("WithLogger(custom) = %v, want %v", cfg.logger, alt)
	}
}
