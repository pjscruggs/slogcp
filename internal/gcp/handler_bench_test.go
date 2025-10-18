package gcp

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"runtime"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	loggingpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/protobuf/types/known/structpb"
)

var (
	benchPayload        map[string]any
	benchProtoPayload   *structpb.Struct
	benchHTTPRequest    *logging.HTTPRequest
	benchDynamicLabels  map[string]string
	benchSourceLocation *loggingpb.LogEntrySourceLocation
	benchStrings        [3]string
	benchBool           bool
	benchErr            error
	benchLabelResult    string
)

func newBenchmarkHandler() *gcpHandler {
	return &gcpHandler{
		cfg: Config{
			AddSource:         true,
			StackTraceEnabled: false,
			StackTraceLevel:   slog.LevelError,
			GCPCommonLabels: map[string]string{
				"app":     "bench",
				"version": "v1.2.3",
			},
		},
		groupedAttrs: []groupedAttr{
			{
				groups: []string{"logging.googleapis.com/labels"},
				attr:   slog.String("static_label", "static_value"),
			},
			{
				groups: []string{"environment"},
				attr:   slog.String("cluster", "cluster-a"),
			},
		},
		groups:                []string{"environment"},
		redirectWriter:        io.Discard,
		runtimeLabels:         map[string]string{"runtime": "cloud-run", "region": "us-central1"},
		runtimeServiceContext: map[string]string{"service": "bench-service", "version": "2025-10-18"},
	}
}

func newBenchmarkRecord(pc uintptr) slog.Record {
	now := time.Unix(0, 0)
	rec := slog.NewRecord(now, slog.LevelInfo, "benchmark payload build", pc)
	req := &http.Request{
		Method: "GET",
		URL: &url.URL{
			Scheme: "https",
			Host:   "example.com",
			Path:   "/v1/resource",
		},
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header: http.Header{
			"User-Agent": []string{"benchmark-client/1.0"},
			"Accept":     []string{"application/json"},
		},
	}
	rec.AddAttrs(
		slog.String("user", "benchmark-user"),
		slog.Int("attempt", 3),
		slog.Float64("load_factor", 0.76),
		slog.Time("observed_at", now),
		slog.Any("error_obj", errors.New("synthetic failure")),
		slog.Any(httpRequestKey, &logging.HTTPRequest{
			Request:      req,
			RequestSize:  512,
			ResponseSize: 2048,
			Status:       http.StatusOK,
			Latency:      42 * time.Millisecond,
			RemoteIP:     "203.0.113.8",
			LocalIP:      "10.1.2.3",
		}),
		slog.Group("logging.googleapis.com/labels",
			slog.String("tenant", "acme"),
			slog.Int("shard", 12),
			slog.Duration("elapsed", 37*time.Millisecond),
		),
		slog.Group("http",
			slog.String("method", "GET"),
			slog.String("route", "/v1/resource"),
			slog.Group("headers",
				slog.String("user-agent", "benchmark-client/1.0"),
				slog.String("accept", "application/json"),
			),
		),
		slog.Any("payload_metadata", map[string]any{
			"ids":   []int{1, 2, 3, 4, 5},
			"flags": map[string]bool{"cold": false, "warm": true},
			"notes": "additional metadata payload for struct conversion",
		}),
	)
	return rec
}

type customLabelValue struct {
	a int
	b string
}

func capturePC() uintptr {
	var pcs [1]uintptr
	n := runtime.Callers(2, pcs[:])
	if n == 0 {
		return 0
	}
	return pcs[0]
}

func BenchmarkBuildPayload(b *testing.B) {
	handler := newBenchmarkHandler()
	rec := newBenchmarkRecord(0)

	b.Run("MapOnly", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			local := rec
			payload, protoPayload, httpReq, errType, errMsg, stackStr, labels := handler.buildPayload(local, false, nil)
			if len(payload) == 0 {
				b.Fatalf("empty payload at iteration %d", i)
			}
			benchPayload = payload
			benchProtoPayload = protoPayload
			benchHTTPRequest = httpReq
			benchDynamicLabels = labels
			benchStrings = [3]string{errType, errMsg, stackStr}
		}
	})

	b.Run("ProtoStruct", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			local := rec
			payload, protoPayload, httpReq, errType, errMsg, stackStr, labels := handler.buildPayload(local, true, nil)
			if protoPayload == nil {
				b.Fatalf("nil proto payload at iteration %d", i)
			}
			benchPayload = payload
			benchProtoPayload = protoPayload
			benchHTTPRequest = httpReq
			benchDynamicLabels = labels
			benchStrings = [3]string{errType, errMsg, stackStr}
		}
	})

	b.Run("StateReuse", func(b *testing.B) {
		state := &payloadState{}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			local := rec
			payload, protoPayload, httpReq, errType, errMsg, stackStr, labels := handler.buildPayload(local, false, state)
			if len(payload) == 0 {
				b.Fatalf("empty payload with state at iteration %d", i)
			}
			benchPayload = payload
			benchProtoPayload = protoPayload
			benchHTTPRequest = httpReq
			benchDynamicLabels = labels
			benchStrings = [3]string{errType, errMsg, stackStr}
			state.recycle()
		}
	})
}

func BenchmarkEmitRedirectJSON(b *testing.B) {
	handler := newBenchmarkHandler()
	rec := newBenchmarkRecord(0)
	state := &payloadState{}

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		local := rec
		payload, _, httpReq, errType, errMsg, stackStr, labels := handler.buildPayload(local, false, state)
		benchErr = handler.emitRedirectJSON(local, payload, httpReq, nil, "", "", "", false, errType, errMsg, stackStr, labels)
		if benchErr != nil {
			b.Fatalf("emitRedirectJSON failed: %v", benchErr)
		}
		state.recycle()
	}
}

func BenchmarkResolveSourceLocation(b *testing.B) {
	handler := newBenchmarkHandler()
	handler.cfg.AddSource = true
	pc := capturePC()
	if pc == 0 {
		b.Fatal("failed to capture program counter")
	}
	rec := slog.NewRecord(time.Now(), slog.LevelInfo, "source", pc)

	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		benchSourceLocation = handler.resolveSourceLocation(rec)
		if benchSourceLocation == nil {
			b.Fatal("source location resolution returned nil")
		}
	}
}

func BenchmarkLabelValueToString(b *testing.B) {
	values := []slog.Value{
		slog.StringValue("string value"),
		slog.Int64Value(123456789),
		slog.Uint64Value(987654321),
		slog.Float64Value(3.14159),
		slog.BoolValue(true),
		slog.DurationValue(150 * time.Millisecond),
		slog.TimeValue(time.Date(2025, time.October, 18, 12, 34, 56, 0, time.UTC)),
		slog.AnyValue(time.Duration(45 * time.Second)),
		slog.AnyValue(time.Date(2025, time.September, 5, 8, 9, 10, 0, time.UTC)),
		slog.AnyValue([]byte("binary label value")),
		slog.AnyValue(customLabelValue{a: 42, b: "custom"}),
	}

	b.ReportAllocs()
	idx := 0
	for i := 0; i < b.N; i++ {
		s, ok := labelValueToString(values[idx])
		benchLabelResult = s
		benchBool = ok
		idx++
		if idx == len(values) {
			idx = 0
		}
	}
}
