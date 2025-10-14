package gcp

import (
	"log/slog"
	"net/http"
	"testing"
	"time"

	"cloud.google.com/go/logging"
	"google.golang.org/protobuf/types/known/structpb"
)

const labelsGroupKey = "logging.googleapis.com/labels"

type recordingEntryLogger struct {
	entries []logging.Entry
}

func (r *recordingEntryLogger) Log(entry logging.Entry) {
	r.entries = append(r.entries, entry)
}

func (r *recordingEntryLogger) Flush() error {
	return nil
}

func TestBuildPayloadPopulatesProto(t *testing.T) {
	h := &gcpHandler{
		cfg:      Config{},
		entryLog: &recordingEntryLogger{},
	}

	record := slog.NewRecord(time.Date(2025, 1, 2, 3, 4, 5, 600000000, time.UTC), slog.LevelInfo, "test", 0)
	record.AddAttrs(
		slog.String("user", "alice"),
		slog.Int("count", 42),
		slog.Duration("elapsed", time.Second),
		slog.Group("nested",
			slog.Bool("flag", true),
			slog.Time("when", time.Date(2024, 8, 9, 10, 11, 12, 0, time.FixedZone("CST", -6*3600))),
		),
		slog.Group("logging.googleapis.com/labels", slog.String("request_id", "abc123")),
	)

	payload, protoPayload, httpReq, errType, errMsg, stack, labels := h.buildPayload(record, true, nil)
	if httpReq != nil || errType != "" || errMsg != "" || stack != "" {
		t.Fatalf("unexpected extras: httpReq=%v errType=%q errMsg=%q stack=%q", httpReq, errType, errMsg, stack)
	}
	if labels == nil || labels["request_id"] != "abc123" {
		t.Fatalf("dynamic labels not captured: %#v", labels)
	}
	if _, exists := payload[labelsGroupKey]; exists {
		t.Fatalf("payload unexpectedly includes labels group: %#v", payload[labelsGroupKey])
	}

	if got := payload["user"]; got != "alice" {
		t.Fatalf("payload[user] = %#v, want %q", got, "alice")
	}
	if got := payload["count"]; got != int64(42) {
		t.Fatalf("payload[count] = %#v, want %d", got, 42)
	}
	if _, ok := payload["nested"]; !ok {
		t.Fatalf("payload missing nested group")
	}
	nested, ok := payload["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested payload has type %T, want map[string]any", payload["nested"])
	}
	if got := nested["flag"]; got != true {
		t.Fatalf("nested[flag] = %#v, want true", got)
	}
	expectedTime := time.Date(2024, 8, 9, 16, 11, 12, 0, time.UTC).Format(time.RFC3339Nano)
	if got := nested["when"]; got != expectedTime {
		t.Fatalf("nested[when] = %#v, want %q", got, expectedTime)
	}

	if protoPayload == nil {
		t.Fatalf("protoPayload is nil")
	}
	fields := protoPayload.GetFields()
	if len(fields) == 0 {
		t.Fatalf("protoPayload fields empty")
	}
	if got := fields["user"].GetStringValue(); got != "alice" {
		t.Fatalf("proto user = %q, want %q", got, "alice")
	}
	if got := int(fields["count"].GetNumberValue()); got != 42 {
		t.Fatalf("proto count = %d, want 42", got)
	}
	nestedProto := fields["nested"].GetStructValue()
	if nestedProto == nil {
		t.Fatalf("proto nested struct is nil")
	}
	if got := nestedProto.Fields["flag"].GetBoolValue(); !got {
		t.Fatalf("proto nested flag = %v, want true", got)
	}
	if got := nestedProto.Fields["when"].GetStringValue(); got != expectedTime {
		t.Fatalf("proto nested when = %q, want %q", got, expectedTime)
	}
}

func TestBuildPayloadProtoDisabledOnUnsupportedValue(t *testing.T) {
	h := &gcpHandler{
		cfg:      Config{},
		entryLog: &recordingEntryLogger{},
	}

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "bad", 0)
	record.AddAttrs(slog.Any("bad_map", map[int]string{1: "one"}))

	payload, protoPayload, _, _, _, _, _ := h.buildPayload(record, true, nil)

	if payload["bad_map"] == nil {
		t.Fatalf("payload missing bad_map entry")
	}
	if protoPayload != nil {
		t.Fatalf("expected protoPayload to be nil for unsupported value, got %#v", protoPayload)
	}
}

func TestEmitGCPEntryUsesProtoPayload(t *testing.T) {
	logger := &recordingEntryLogger{}
	h := &gcpHandler{
		cfg: Config{
			LogTarget:       LogTargetGCP,
			GCPCommonLabels: map[string]string{"env": "dev"},
		},
		entryLog: logger,
	}

	record := slog.NewRecord(time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC), slog.LevelInfo, "hello", 0)
	record.AddAttrs(slog.String("user", "alice"))

	payload, protoPayload, httpReq, _, _, _, dynamicLabels := h.buildPayload(record, true, nil)
	if protoPayload == nil {
		t.Fatalf("protoPayload is nil")
	}

	err := h.emitGCPEntry(record, payload, protoPayload, httpReq, nil, "", "", false, dynamicLabels)
	if err != nil {
		t.Fatalf("emitGCPEntry returned error: %v", err)
	}

	if len(logger.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(logger.entries))
	}

	entry := logger.entries[0]
	structPayload, ok := entry.Payload.(*structpb.Struct)
	if !ok {
		t.Fatalf("entry payload has type %T, want *structpb.Struct", entry.Payload)
	}

	if got := structPayload.Fields["user"].GetStringValue(); got != "alice" {
		t.Fatalf("struct payload user = %q, want %q", got, "alice")
	}
	if got := structPayload.Fields["message"].GetStringValue(); got != "hello" {
		t.Fatalf("struct payload message = %q, want %q", got, "hello")
	}

	labelStruct := structPayload.Fields["logging.googleapis.com/labels"].GetStructValue()
	if labelStruct == nil {
		t.Fatalf("proto labels struct is nil")
	}
	if got := labelStruct.Fields["env"].GetStringValue(); got != "dev" {
		t.Fatalf("proto label env = %q, want %q", got, "dev")
	}
}

func TestBuildPayloadExtractsHTTPRequest(t *testing.T) {
	req := &http.Request{
		Method:     http.MethodPost,
		Proto:      "HTTP/1.1",
		RequestURI: "/api",
	}
	httpPayload := &logging.HTTPRequest{
		Request:      req,
		Status:       http.StatusCreated,
		RequestSize:  123,
		ResponseSize: 456,
		RemoteIP:     "203.0.113.9",
		LocalIP:      "10.0.0.5",
	}

	h := &gcpHandler{
		cfg:      Config{},
		entryLog: &recordingEntryLogger{},
	}

	record := slog.NewRecord(time.Now(), slog.LevelInfo, "http-test", 0)
	record.AddAttrs(
		slog.String("component", "api"),
		slog.Any(httpRequestKey, httpPayload),
	)

	payload, protoPayload, httpReq, _, _, _, _ := h.buildPayload(record, true, nil)
	if httpReq != httpPayload {
		t.Fatalf("http request pointer mismatch: got %p want %p", httpReq, httpPayload)
	}
	if _, exists := payload[httpRequestKey]; exists {
		t.Fatalf("payload unexpectedly contains httpRequest entry: %#v", payload[httpRequestKey])
	}
	if protoPayload == nil {
		t.Fatalf("proto payload should not be nil")
	}
	if _, exists := protoPayload.Fields[httpRequestKey]; exists {
		t.Fatalf("proto payload unexpectedly contains httpRequest field")
	}
	// Ensure other fields still present
	if payload["component"] != "api" {
		t.Fatalf("payload component mismatch: %#v", payload["component"])
	}
}

func TestEmitGCPEntryFallsBackToMapPayload(t *testing.T) {
	logger := &recordingEntryLogger{}
	h := &gcpHandler{
		cfg: Config{
			LogTarget:       LogTargetGCP,
			GCPCommonLabels: map[string]string{"service": "billing"},
		},
		entryLog: logger,
	}

	record := slog.NewRecord(time.Now(), slog.LevelError, "oops", 0)
	record.AddAttrs(
		slog.Any("bad", map[int]string{1: "one"}), // forces proto fallback
		slog.String("ok", "value"),
	)

	payload, protoPayload, httpReq, _, _, _, dynamicLabels := h.buildPayload(record, true, nil)
	if protoPayload != nil {
		t.Fatalf("expected proto payload to be nil due to unsupported value, got %#v", protoPayload)
	}

	err := h.emitGCPEntry(record, payload, protoPayload, httpReq, nil, "", "", false, dynamicLabels)
	if err != nil {
		t.Fatalf("emitGCPEntry returned error: %v", err)
	}

	if len(logger.entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(logger.entries))
	}

	entry := logger.entries[0]
	payloadMap, ok := entry.Payload.(map[string]any)
	if !ok {
		t.Fatalf("entry payload type = %T, want map[string]any", entry.Payload)
	}

	if payloadMap[messageKey] != "oops" {
		t.Fatalf("payload message = %#v, want %q", payloadMap[messageKey], "oops")
	}
	if payloadMap["ok"] != "value" {
		t.Fatalf("payload ok field missing, got %#v", payloadMap["ok"])
	}
	if entry.Labels["service"] != "billing" {
		t.Fatalf("entry labels missing service, got %#v", entry.Labels)
	}
}
