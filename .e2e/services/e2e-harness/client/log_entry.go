// Copyright 2025-2026 Patrick J. Scruggs
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

package client

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/logging"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	severityKey          = "severity"
	labelsKey            = "logging.googleapis.com/labels"
	operationKey         = "logging.googleapis.com/operation"
	sourceLocationKey    = "logging.googleapis.com/sourceLocation"
	traceKey             = "logging.googleapis.com/trace"
	spanKey              = "logging.googleapis.com/spanId"
	traceSampledKey      = "logging.googleapis.com/trace_sampled"
	otelTraceKey         = "otel.trace_id"
	otelSpanKey          = "otel.span_id"
	otelSampledKey       = "otel.trace_sampled"
	httpRequestFieldName = "httpRequest"
)

// Severity mirrors the Cloud Logging severity field used in harness assertions.
type Severity string

// Cloud Logging severity values recognized by the harness.
const (
	SeverityDefault   Severity = "DEFAULT"
	SeverityDebug     Severity = "DEBUG"
	SeverityInfo      Severity = "INFO"
	SeverityNotice    Severity = "NOTICE"
	SeverityWarning   Severity = "WARNING"
	SeverityError     Severity = "ERROR"
	SeverityCritical  Severity = "CRITICAL"
	SeverityAlert     Severity = "ALERT"
	SeverityEmergency Severity = "EMERGENCY"
)

// String returns the string form of the severity.
func (s Severity) String() string {
	return string(s)
}

// SourceLocation describes the sourceLocation metadata attached to a log entry.
type SourceLocation struct {
	File     string
	Line     int64
	Function string
}

// MonitoredResource describes the monitored resource attached to a log entry.
type MonitoredResource struct {
	Type   string
	Labels map[string]string
}

// LogEntry is the harness-friendly projection of a Cloud Logging entry.
type LogEntry struct {
	Payload       map[string]any
	Severity      Severity
	Timestamp     time.Time
	LogName       string
	Trace         string
	SpanID        string
	TraceSampled  bool
	Labels        map[string]string
	HTTPRequest   map[string]any
	Source        *SourceLocation
	Resource      *MonitoredResource
	InsertID      string
	TextPayload   string
	RawAttributes map[string]any
}

// newLogEntry converts a Cloud Logging entry into the harness log representation.
func newLogEntry(entry *logging.Entry) (*LogEntry, error) {
	if entry == nil {
		return nil, fmt.Errorf("nil logging entry")
	}
	payload, err := normalizedPayload(entry)
	if err != nil {
		return nil, err
	}

	severity := resolveEntrySeverity(entry, payload)
	labels := mergedEntryLabels(entry, payload)
	httpRequestPayload := mergedHTTPRequest(entry, payload)
	source := extractSourceLocation(entry, payload)
	trace := resolveTrace(entry, payload)
	spanID := resolveSpanID(entry, payload)
	sampled := resolveTraceSampled(entry, payload)
	resource := buildResource(entry)
	textPayload := extractTextPayload(entry)
	setOperationPayload(entry, payload)

	return &LogEntry{
		Payload:       payload,
		Severity:      severity,
		Timestamp:     entry.Timestamp,
		LogName:       entry.LogName,
		Trace:         trace,
		SpanID:        spanID,
		TraceSampled:  sampled,
		Labels:        labels,
		HTTPRequest:   httpRequestPayload,
		Source:        source,
		Resource:      resource,
		InsertID:      entry.InsertID,
		TextPayload:   textPayload,
		RawAttributes: payload,
	}, nil
}

// normalizedPayload returns a mutable payload map and validates required invariants.
func normalizedPayload(entry *logging.Entry) (map[string]any, error) {
	payload := extractPayloadMap(entry)
	if payload == nil {
		payload = make(map[string]any)
	}
	if _, exists := payload["time"]; exists {
		return nil, fmt.Errorf("log entry contains disallowed jsonPayload.time field")
	}
	if entry.Timestamp.IsZero() {
		return nil, fmt.Errorf("log entry missing top-level timestamp")
	}
	return payload, nil
}

// resolveEntrySeverity computes severity from payload and entry defaults.
func resolveEntrySeverity(entry *logging.Entry, payload map[string]any) Severity {
	if raw := strings.TrimSpace(asString(payload[severityKey])); raw != "" {
		return normalizeSeverity(raw)
	}
	if raw := strings.TrimSpace(entry.Severity.String()); raw != "" {
		return normalizeSeverity(raw)
	}
	return SeverityDefault
}

// mergedEntryLabels combines top-level and payload labels.
func mergedEntryLabels(entry *logging.Entry, payload map[string]any) map[string]string {
	payloadLabels := extractStringMap(payload[labelsKey])
	labels := mergeStringMaps(copyStringMap(entry.Labels), payloadLabels)
	if payload[labelsKey] == nil && len(entry.Labels) > 0 {
		payload[labelsKey] = stringMapToAnyMap(entry.Labels)
	}
	return labels
}

// mergedHTTPRequest merges top-level and payload httpRequest maps.
func mergedHTTPRequest(entry *logging.Entry, payload map[string]any) map[string]any {
	httpRequestPayload := copyMapAny(extractMap(payload[httpRequestFieldName]))
	if entry.HTTPRequest != nil {
		if converted := httpRequestStructToMap(entry.HTTPRequest); len(converted) > 0 {
			httpRequestPayload = mergeMapAny(converted, httpRequestPayload)
		}
	}
	return httpRequestPayload
}

// extractSourceLocation resolves source location from payload or entry fallback.
func extractSourceLocation(entry *logging.Entry, payload map[string]any) *SourceLocation {
	if srcMap := extractMap(payload[sourceLocationKey]); len(srcMap) > 0 {
		return &SourceLocation{
			File:     asString(srcMap["file"]),
			Function: asString(srcMap["function"]),
			Line:     asInt64(srcMap["line"]),
		}
	}
	if entry.SourceLocation == nil {
		return nil
	}
	return &SourceLocation{
		File:     entry.SourceLocation.File,
		Function: entry.SourceLocation.Function,
		Line:     int64(entry.SourceLocation.Line),
	}
}

// resolveTrace resolves trace value from known payload keys and entry fallback.
func resolveTrace(entry *logging.Entry, payload map[string]any) string {
	trace := asString(payload[traceKey])
	if trace == "" {
		trace = asString(payload[otelTraceKey])
	}
	if trace == "" {
		trace = entry.Trace
	}
	return trace
}

// resolveSpanID resolves span value from known payload keys and entry fallback.
func resolveSpanID(entry *logging.Entry, payload map[string]any) string {
	spanID := asString(payload[spanKey])
	if spanID == "" {
		spanID = asString(payload[otelSpanKey])
	}
	if spanID == "" {
		spanID = entry.SpanID
	}
	return spanID
}

// resolveTraceSampled resolves sampled flag from payload with entry fallback.
func resolveTraceSampled(entry *logging.Entry, payload map[string]any) bool {
	sampled, ok := valueAsBool(payload[traceSampledKey])
	if !ok {
		sampled, ok = valueAsBool(payload[otelSampledKey])
	}
	if !ok {
		sampled = entry.TraceSampled
	}
	entry.TraceSampled = sampled
	return sampled
}

// buildResource copies monitored resource metadata when present.
func buildResource(entry *logging.Entry) *MonitoredResource {
	if entry.Resource == nil {
		return nil
	}
	return &MonitoredResource{
		Type:   entry.Resource.Type,
		Labels: copyStringMap(entry.Resource.Labels),
	}
}

// extractTextPayload returns text payload for string entries.
func extractTextPayload(entry *logging.Entry) string {
	if s, ok := entry.Payload.(string); ok {
		return s
	}
	return ""
}

// setOperationPayload injects operation metadata into payload when present.
func setOperationPayload(entry *logging.Entry, payload map[string]any) {
	if entry.Operation == nil {
		return
	}
	payload[operationKey] = map[string]any{
		"id":       entry.Operation.GetId(),
		"producer": entry.Operation.GetProducer(),
		"first":    entry.Operation.GetFirst(),
		"last":     entry.Operation.GetLast(),
	}
}

// normalizeSeverity canonicalizes a severity string into the harness enum.
func normalizeSeverity(value string) Severity {
	trimmed := strings.ToUpper(strings.TrimSpace(value))
	if trimmed == "" {
		return SeverityDefault
	}
	if canonical, ok := severityByName[trimmed]; ok {
		return canonical
	}
	return Severity(trimmed)
}

var severityByName = map[string]Severity{
	"DEFAULT":   SeverityDefault,
	"DEBUG":     SeverityDebug,
	"INFO":      SeverityInfo,
	"NOTICE":    SeverityNotice,
	"WARNING":   SeverityWarning,
	"ERROR":     SeverityError,
	"CRITICAL":  SeverityCritical,
	"ALERT":     SeverityAlert,
	"EMERGENCY": SeverityEmergency,
}

// extractMap returns a map payload when value can be interpreted as one.
func extractMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case map[string]any:
		return v
	case map[string]string:
		return stringMapToAnyMap(v)
	default:
		return nil
	}
}

// extractStringMap converts a generic map payload into a string map.
func extractStringMap(value any) map[string]string {
	raw := extractMap(value)
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		str := asString(v)
		if str == "" {
			continue
		}
		out[k] = str
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// asString formats common JSON-compatible values as strings.
func asString(v any) string {
	if v == nil {
		return ""
	}
	if value, ok := v.(string); ok {
		return value
	}
	if value, ok := v.(json.Number); ok {
		return value.String()
	}
	if value, ok := v.(fmt.Stringer); ok {
		return value.String()
	}
	switch value := v.(type) {
	case bool:
		if value {
			return "true"
		}
		return "false"
	case float32:
		return formatFloatString(float64(value))
	case float64:
		return formatFloatString(value)
	default:
		return fmt.Sprintf("%v", v)
	}
}

// formatFloatString formats a float while trimming trailing zeroes.
func formatFloatString(value float64) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%f", value), "0"), ".")
}

// asBool reports whether v represents a truthy boolean value.
func asBool(v any) bool {
	switch value := v.(type) {
	case bool:
		return value
	case string:
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "true", "1", "t":
			return true
		default:
			return false
		}
	case float64:
		return value != 0
	case int:
		return value != 0
	default:
		return false
	}
}

// asInt64 converts common JSON-compatible numeric representations to int64.
func asInt64(v any) int64 {
	switch value := v.(type) {
	case float64:
		return int64(value)
	case float32:
		return int64(value)
	case int:
		return int64(value)
	case int64:
		return value
	case uint64:
		if value > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(value)
	case string:
		if n, err := json.Number(value).Int64(); err == nil {
			return n
		}
	}
	return 0
}

// copyStringMap clones a string map.
func copyStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]string, len(src))
	maps.Copy(dst, src)
	return dst
}

// stringMapToAnyMap widens a string map into a generic map.
func stringMapToAnyMap(src map[string]string) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dst := make(map[string]any, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// mergeStringMaps overlays overlay on top of base and returns the result.
func mergeStringMaps(base, overlay map[string]string) map[string]string {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	result := make(map[string]string, len(base)+len(overlay))
	maps.Copy(result, base)
	maps.Copy(result, overlay)
	if len(result) == 0 {
		return nil
	}
	return result
}

// extractPayloadMap decodes the structured payload carried by a logging entry.
func extractPayloadMap(entry *logging.Entry) map[string]any {
	if entry == nil {
		return nil
	}

	if payload, ok := entry.Payload.(map[string]any); ok {
		return payload
	}

	if payload, ok := entry.Payload.(map[string]any); ok {
		return payload
	}

	if protoMsg, ok := entry.Payload.(proto.Message); ok {
		if jsonBytes, err := protojson.Marshal(protoMsg); err == nil {
			var payload map[string]any
			if err := json.Unmarshal(jsonBytes, &payload); err == nil {
				return payload
			}
		}
	}

	if strPayload, ok := entry.Payload.(string); ok {
		var payload map[string]any
		if err := json.Unmarshal([]byte(strPayload), &payload); err == nil {
			return payload
		}
	}

	if payload := structToMap(entry.Payload); payload != nil {
		return payload
	}

	return nil
}

// structToMap reflects a struct value into a JSON-style map.
func structToMap(data any) map[string]any {
	if data == nil {
		return nil
	}

	value := reflect.ValueOf(data)
	if value.Kind() == reflect.Ptr {
		value = value.Elem()
	}

	if !value.IsValid() || value.Kind() != reflect.Struct {
		return nil
	}

	typ := value.Type()
	result := make(map[string]any, value.NumField())

	for i := 0; i < value.NumField(); i++ {
		field := typ.Field(i)
		fieldValue := value.Field(i)
		if !fieldValue.CanInterface() {
			continue
		}
		fieldName, include := jsonFieldName(field)
		if !include {
			continue
		}
		result[fieldName] = fieldValue.Interface()
	}

	if len(result) == 0 {
		return nil
	}

	return result
}

// jsonFieldName resolves struct field output key from json tags.
func jsonFieldName(field reflect.StructField) (string, bool) {
	jsonTag := field.Tag.Get("json")
	if jsonTag == "" {
		return field.Name, true
	}
	parts := strings.Split(jsonTag, ",")
	if len(parts) == 0 || parts[0] == "" {
		return field.Name, true
	}
	if parts[0] == "-" {
		return "", false
	}
	return parts[0], true
}

// copyMapAny clones a generic map.
func copyMapAny(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	dup := make(map[string]any, len(src))
	maps.Copy(dup, src)
	return dup
}

// mergeMapAny overlays overlay on top of base and returns the result.
func mergeMapAny(base, overlay map[string]any) map[string]any {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	result := copyMapAny(base)
	if result == nil {
		result = make(map[string]any, len(overlay))
	}
	maps.Copy(result, overlay)
	if len(result) == 0 {
		return nil
	}
	return result
}

// httpRequestStructToMap normalizes Cloud Logging httpRequest metadata.
func httpRequestStructToMap(req *logging.HTTPRequest) map[string]any {
	if req == nil {
		return nil
	}

	fields := httpRequestFields(req)

	result := map[string]any{
		"requestMethod":                  fields.method,
		"requestUrl":                     fields.request,
		"userAgent":                      fields.userAgent,
		"referer":                        fields.referer,
		"protocol":                       fields.proto,
		"requestSize":                    strconv.FormatInt(req.RequestSize, 10),
		"status":                         req.Status,
		"responseSize":                   strconv.FormatInt(req.ResponseSize, 10),
		"latency":                        fmt.Sprintf("%.9fs", req.Latency.Seconds()),
		"remoteIp":                       fields.remoteIP,
		"serverIp":                       req.LocalIP,
		"cacheHit":                       req.CacheHit,
		"cacheValidatedWithOriginServer": req.CacheValidatedWithOriginServer,
		"cacheFillBytes":                 strconv.FormatInt(req.CacheFillBytes, 10),
		"cacheLookup":                    req.CacheLookup,
	}

	return result
}

type httpRequestFieldValues struct {
	method    string
	request   string
	userAgent string
	referer   string
	proto     string
	remoteIP  string
}

// httpRequestFields extracts request-derived HTTP metadata with defaults.
func httpRequestFields(req *logging.HTTPRequest) httpRequestFieldValues {
	fields := httpRequestFieldValues{}
	if req.Request != nil {
		fields = extractHTTPRequestValues(req.Request)
	}
	if fields.remoteIP == "" {
		fields.remoteIP = req.RemoteIP
	}
	if fields.proto == "" {
		fields.proto = defaultHTTPProtocol(req)
	}
	return fields
}

// extractHTTPRequestValues pulls metadata from net/http.Request.
func extractHTTPRequestValues(httpReq *http.Request) httpRequestFieldValues {
	fields := httpRequestFieldValues{
		method:    httpReq.Method,
		userAgent: httpReq.UserAgent(),
		referer:   httpReq.Referer(),
		proto:     httpReq.Proto,
	}
	if httpReq.URL != nil {
		fields.request = httpReq.URL.String()
	}
	fields.remoteIP = remoteAddrHost(httpReq.RemoteAddr)
	return fields
}

// remoteAddrHost returns the host from RemoteAddr, falling back to the raw value.
func remoteAddrHost(remoteAddr string) string {
	if remoteAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

// defaultHTTPProtocol returns a protocol fallback for HTTP request metadata.
func defaultHTTPProtocol(req *logging.HTTPRequest) string {
	if req.Request != nil && req.Request.Proto != "" {
		return req.Request.Proto
	}
	return "HTTP/1.1"
}

// valueAsBool extracts a boolean while reporting whether conversion succeeded.
func valueAsBool(v any) (bool, bool) {
	switch value := v.(type) {
	case bool:
		return value, true
	case string:
		return parseLooseBool(value)
	case float64:
		return value != 0, true
	case float32:
		return value != 0, true
	case int, int32, int64:
		return asBool(value), true
	case uint, uint32, uint64:
		return asBool(value), true
	default:
		return false, false
	}
}

// parseLooseBool parses accepted textual bool forms used by log payloads.
func parseLooseBool(value string) (bool, bool) {
	trimmed := strings.TrimSpace(strings.ToLower(value))
	if trimmed == "" {
		return false, false
	}
	switch trimmed {
	case "true", "1", "t", "yes":
		return true, true
	case "false", "0", "f", "no":
		return false, true
	default:
		return false, false
	}
}
