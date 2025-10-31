package loggingmock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Special jsonPayload keys (match Ruby defaults)
const (
	keyOp           = "logging.googleapis.com/operation"
	keySrcLoc       = "logging.googleapis.com/sourceLocation"
	keyTrace        = "logging.googleapis.com/trace"
	keySpanID       = "logging.googleapis.com/spanId"
	keyTraceSampled = "logging.googleapis.com/trace_sampled"
)

// TransformLogEntryJSON rewrites a single LogEntry JSON to mimic the backend:
// - Normalize/elevate timestamp (top-level > legacy seconds/nanos > jsonPayload.time > now)
// - Normalize severity (aliases ok; numeric values NOT accepted -> DEFAULT; empty -> DEFAULT)
// - Elevate httpRequest only if entirely canonical (strict)
// - Elevate special fields from jsonPayload: operation, sourceLocation, trace, spanId, traceSampled
// - Never HTML-escape output JSON
func TransformLogEntryJSON(in string, now time.Time) (string, error) {
	var root map[string]interface{}
	dec := json.NewDecoder(strings.NewReader(in))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		return "", fmt.Errorf("invalid JSON: %w", err)
	}

	payload := mapFrom(root["jsonPayload"])

	// 1) Timestamp
	nowUTC := now.UTC()
	ts, hasTimestamp := normalizeTopLevelTimestamp(root)
	if !hasTimestamp && payload != nil {
		if t, ok := payload["time"].(string); ok {
			if parsed, ok := parseRFC3339Flexible(t); ok {
				ts = parsed
				hasTimestamp = true
				delete(payload, "time")
			}
		}
	}
	if !hasTimestamp {
		ts = nowUTC
	}
	ts = adjustTimestampIfInvalid(ts, nowUTC)
	root["timestamp"] = formatRFC3339ZNormalized(ts)

	// 2) Severity (no numeric severities; default to DEFAULT)
	if sevVal, ok := root["severity"]; ok {
		root["severity"] = normalizeSeverity(sevVal)
	} else {
		root["severity"] = "DEFAULT"
	}

	// 3) Elevate canonically-valid httpRequest
	if payload != nil {
		_, topLevelExists := root["httpRequest"]
		if hr, ok := payload["httpRequest"]; ok {
			if hrm := mapFrom(hr); hrm != nil {
				cleaned, ok := extractHttpRequest(hrm)
				if ok && !topLevelExists {
					root["httpRequest"] = cleaned
				}
				if len(hrm) == 0 {
					delete(payload, "httpRequest")
				}
			}
		}
	}

	// 4) Elevate special fields from jsonPayload (operation/sourceLocation/trace/spanId/traceSampled)
	if payload != nil {
		projectID := extractProjectID(root)
		elevateOperation(payload, root)
		elevateSourceLocation(payload, root)
		elevateSimpleTrace(payload, root, projectID)
		elevateSimpleSpanID(payload, root)
		elevateSimpleTraceSampled(payload, root)
		root["jsonPayload"] = payload
	}

	out, err := marshalNoHTMLEscape(root)
	if err != nil {
		return "", err
	}
	return out, nil
}

// TransformRaw rewrites a single LogEntry using the current time.
func TransformRaw(in string) (string, error) {
	return TransformLogEntryJSON(in, time.Now().UTC())
}

// EnsureJSONObject verifies the input is a JSON object.
func EnsureJSONObject(in string) error {
	var v any
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		return err
	}
	if _, ok := v.(map[string]any); !ok {
		return errors.New("input must be a JSON object")
	}
	return nil
}

// mapFrom returns v as a map[string]interface{} when possible, otherwise nil.
func mapFrom(v interface{}) map[string]interface{} {
	if v == nil {
		return nil
	}
	m, _ := v.(map[string]interface{})
	return m
}

// normalizeTopLevelTimestamp lifts timestamp-like fields to a canonical RFC3339 value.
func normalizeTopLevelTimestamp(root map[string]interface{}) (time.Time, bool) {
	if v, ok := root["timestamp"]; ok {
		switch val := v.(type) {
		case string:
			if ts, ok := parseRFC3339Flexible(val); ok {
				root["timestamp"] = formatRFC3339ZNormalized(ts)
				return ts, true
			}
			delete(root, "timestamp")
		case map[string]interface{}:
			delete(root, "timestamp")
			secs, hasSecs := numericToInt64(val["seconds"])
			nanos, hasNanos := numericToInt64(val["nanos"])
			if !hasNanos {
				nanos = 0
			}
			if hasSecs {
				ts := time.Unix(secs, nanos).UTC()
				root["timestamp"] = formatRFC3339ZNormalized(ts)
				return ts, true
			}
		default:
			delete(root, "timestamp")
		}
	}
	if rawSecs, ok := root["timestampSeconds"]; ok {
		delete(root, "timestampSeconds")
		secs, hasSecs := numericToInt64(rawSecs)
		rawNanos, hasNanos := root["timestampNanos"]
		if hasNanos {
			delete(root, "timestampNanos")
		}
		nanos, hasParsedNanos := int64(0), false
		if hasNanos {
			nanos, hasParsedNanos = numericToInt64(rawNanos)
		}
		if !hasParsedNanos {
			nanos = 0
		}
		if hasSecs {
			ts := time.Unix(secs, nanos).UTC()
			root["timestamp"] = formatRFC3339ZNormalized(ts)
			return ts, true
		}
	}
	if rawTimeNanos, ok := root["timeNanos"]; ok {
		delete(root, "timeNanos")
		if nanos, ok := numericToInt64(rawTimeNanos); ok {
			secs := nanos / 1_000_000_000
			rem := nanos % 1_000_000_000
			if rem < 0 {
				secs--
				rem += 1_000_000_000
			}
			ts := time.Unix(secs, rem).UTC()
			root["timestamp"] = formatRFC3339ZNormalized(ts)
			return ts, true
		}
	}
	return time.Time{}, false
}

// adjustTimestampIfInvalid bounds future timestamps and replaces invalid values.
func adjustTimestampIfInvalid(ts time.Time, current time.Time) time.Time {
	if ts.IsZero() {
		return current.UTC()
	}
	tsUTC := ts.UTC()
	currentUTC := current.UTC()
	oneDayLater := currentUTC.Add(24 * time.Hour)
	if !tsUTC.After(oneDayLater) {
		return tsUTC
	}
	nextYear := time.Date(currentUTC.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC)
	if !tsUTC.Before(nextYear) {
		return time.Unix(0, 0).UTC()
	}
	return tsUTC.AddDate(-1, 0, 0)
}

// parseRFC3339Flexible accepts a broader RFC3339 subset, including lowercase Z zone markers.
func parseRFC3339Flexible(s string) (time.Time, bool) {
	str := s
	// Ruby Time.iso8601 accepts lowercase 'z'; Go requires 'Z'
	if strings.HasSuffix(strings.ToLower(str), "z") && !strings.HasSuffix(str, "Z") {
		str = str[:len(str)-1] + "Z"
	}
	tt, err := time.Parse(time.RFC3339Nano, str)
	if err != nil {
		return time.Time{}, false
	}
	return tt.UTC(), true
}

// formatRFC3339ZNormalized emits an RFC3339 string with trimmed fractional precision.
func formatRFC3339ZNormalized(t time.Time) string {
	ns := t.Nanosecond()
	var frac string
	switch {
	case ns == 0:
		frac = ""
	case ns%1_000_000 == 0:
		frac = fmt.Sprintf(".%03d", ns/1_000_000) // millis
	case ns%1_000 == 0:
		frac = fmt.Sprintf(".%06d", ns/1_000) // micros
	default:
		frac = fmt.Sprintf(".%09d", ns) // nanos
	}
	return fmt.Sprintf("%04d-%02d-%02dT%02d:%02d:%02d%sZ",
		t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), frac)
}

var (
	validSeverities = map[string]struct{}{
		"DEFAULT": {}, "DEBUG": {}, "INFO": {}, "NOTICE": {}, "WARNING": {},
		"ERROR": {}, "CRITICAL": {}, "ALERT": {}, "EMERGENCY": {},
	}
	severityTranslations = map[string]string{
		"WARN": "WARNING", "FATAL": "CRITICAL", "TRACE": "DEBUG", "TRACE_INT": "DEBUG",
		"FINE": "DEBUG", "FINER": "DEBUG", "FINEST": "DEBUG", "SEVERE": "ERROR",
		"CONFIG": "DEBUG", "CRIT": "CRITICAL", "EMERG": "EMERGENCY",
		"D": "DEBUG", "I": "INFO", "N": "NOTICE", "W": "WARNING", "E": "ERROR",
		"C": "CRITICAL", "A": "ALERT", "INFORMATION": "INFO", "ERR": "ERROR", "F": "CRITICAL",
	}
)

// normalizeSeverity coerces severity inputs into canonical Cloud Logging severities.
func normalizeSeverity(v interface{}) string {
	switch vv := v.(type) {
	case json.Number:
		return "DEFAULT"
	case float64, float32, int64, int32, int, uint64, uint32, uint:
		return "DEFAULT"
	case string:
		s := strings.TrimSpace(vv)
		if isAllDigits(s) {
			return "DEFAULT"
		}
		up := strings.ToUpper(s)
		if _, ok := validSeverities[up]; ok {
			return up
		}
		if mapped, ok := severityTranslations[up]; ok {
			return mapped
		}
		return "DEFAULT"
	default:
		return "DEFAULT"
	}
}

// isAllDigits reports whether s contains only ASCII digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

var (
	reLatencyFlexible = regexp.MustCompile(`^\s*(\d+)(?:\.(\d+))?\s*s\s*$`)
	reTraceID         = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)
)

// extractHttpRequest canonicalizes a jsonPayload.httpRequest map for elevation.
func extractHttpRequest(m map[string]interface{}) (map[string]interface{}, bool) {
	if m == nil {
		return nil, false
	}

	out := make(map[string]interface{})

	if v, ok := m["requestMethod"]; ok {
		delete(m, "requestMethod")
		out["requestMethod"] = fmt.Sprint(v)
	}
	if v, ok := m["requestUrl"]; ok {
		delete(m, "requestUrl")
		out["requestUrl"] = fmt.Sprint(v)
	}
	if v, ok := m["userAgent"]; ok {
		delete(m, "userAgent")
		out["userAgent"] = fmt.Sprint(v)
	}
	if v, ok := m["remoteIp"]; ok {
		delete(m, "remoteIp")
		out["remoteIp"] = fmt.Sprint(v)
	}
	if v, ok := m["serverIp"]; ok {
		delete(m, "serverIp")
		out["serverIp"] = fmt.Sprint(v)
	}
	if v, ok := m["referer"]; ok {
		delete(m, "referer")
		out["referer"] = fmt.Sprint(v)
	}
	if v, ok := m["protocol"]; ok {
		delete(m, "protocol")
		out["protocol"] = fmt.Sprint(v)
	}
	if v, ok := m["requestSize"]; ok {
		delete(m, "requestSize")
		out["requestSize"] = strconv.FormatInt(parseIntRubyish(v), 10)
	}
	if v, ok := m["responseSize"]; ok {
		delete(m, "responseSize")
		out["responseSize"] = strconv.FormatInt(parseIntRubyish(v), 10)
	}
	if v, ok := m["cacheFillBytes"]; ok {
		delete(m, "cacheFillBytes")
		out["cacheFillBytes"] = strconv.FormatInt(parseIntRubyish(v), 10)
	}
	if v, ok := m["status"]; ok {
		delete(m, "status")
		out["status"] = int(parseIntRubyish(v))
	}
	if v, ok := m["latency"]; ok {
		delete(m, "latency")
		if latency, ok := parseLatencyValue(v); ok {
			out["latency"] = latency
		}
	}
	if v, ok := m["cacheLookup"]; ok {
		delete(m, "cacheLookup")
		out["cacheLookup"] = parseBoolRubyish(v)
	}
	if v, ok := m["cacheHit"]; ok {
		delete(m, "cacheHit")
		out["cacheHit"] = parseBoolRubyish(v)
	}
	if v, ok := m["cacheValidatedWithOriginServer"]; ok {
		delete(m, "cacheValidatedWithOriginServer")
		out["cacheValidatedWithOriginServer"] = parseBoolRubyish(v)
	}

	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

// parseLatencyValue converts latency representations into backend-friendly strings.
func parseLatencyValue(v interface{}) (string, bool) {
	raw := strings.TrimSpace(fmt.Sprint(v))
	if raw == "" {
		return "", false
	}
	matches := reLatencyFlexible.FindStringSubmatch(strings.ToLower(raw))
	if matches == nil {
		return "", false
	}
	intPart := matches[1]
	fraction := matches[2]
	if len(fraction) > 9 {
		fraction = fraction[:9]
	}
	fraction = strings.TrimRight(fraction, "0")
	if fraction == "" {
		return intPart + "s", true
	}
	return intPart + "." + fraction + "s", true
}

// extractProjectID chooses the best-effort project identifier from a log entry.
func extractProjectID(root map[string]interface{}) string {
	logName, ok := root["logName"].(string)
	if !ok || logName == "" {
		return ""
	}
	trimmed := strings.TrimPrefix(strings.TrimSpace(logName), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) >= 4 && parts[0] == "projects" && parts[2] == "logs" {
		return parts[1]
	}
	return ""
}

// formatTraceValue formats trace identifiers, adding a project prefix when available.
func formatTraceValue(v interface{}, projectID string) string {
	trace := strings.TrimSpace(fmt.Sprint(v))
	if trace == "" {
		return trace
	}
	if strings.HasPrefix(trace, "projects/") {
		return trace
	}
	if projectID != "" && reTraceID.MatchString(trace) {
		return fmt.Sprintf("projects/%s/traces/%s", projectID, trace)
	}
	return trace
}

// numericToInt64 converts flexible numeric inputs into an int64.
func numericToInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		if strings.Contains(n.String(), ".") {
			return 0, false
		}
		i, err := n.Int64()
		return i, err == nil
	case float64:
		if math.Trunc(n) != n {
			return 0, false
		}
		return int64(n), true
	case string:
		i, err := strconv.ParseInt(n, 10, 64)
		return i, err == nil
	case int64:
		return n, true
	case int:
		return int64(n), true
	default:
		return 0, false
	}
}

// elevateOperation hoists structured operation information from the payload to the root.
func elevateOperation(payload, root map[string]interface{}) {
	if _, exists := root["operation"]; exists {
		return
	}
	raw := payload[keyOp]
	obj := mapFrom(raw)
	useSimpleKey := false
	if obj == nil {
		obj = mapFrom(payload["operation"])
		if obj == nil {
			return
		}
		useSimpleKey = true
	}

	// Extract known subfields
	extracted := map[string]any{}
	if v, ok := obj["id"]; ok {
		extracted["id"] = fmt.Sprint(v)
		delete(obj, "id")
	}
	if v, ok := obj["producer"]; ok {
		extracted["producer"] = fmt.Sprint(v)
		delete(obj, "producer")
	}
	if v, ok := obj["first"]; ok {
		extracted["first"] = parseBoolRubyish(v)
		delete(obj, "first")
	}
	if v, ok := obj["last"]; ok {
		extracted["last"] = parseBoolRubyish(v)
		delete(obj, "last")
	}

	if len(extracted) > 0 {
		root["operation"] = extracted
	}

	if useSimpleKey {
		if m := mapFrom(payload["operation"]); m != nil && len(m) == 0 {
			delete(payload, "operation")
		}
	} else {
		if m := mapFrom(payload[keyOp]); m != nil && len(m) == 0 {
			delete(payload, keyOp)
		}
	}
}

// elevateSourceLocation moves source location metadata into the expected top-level field.
func elevateSourceLocation(payload, root map[string]interface{}) {
	if _, exists := root["sourceLocation"]; exists {
		return
	}
	raw := payload[keySrcLoc]
	obj := mapFrom(raw)
	useSimpleKey := false
	if obj == nil {
		obj = mapFrom(payload["sourceLocation"])
		if obj == nil {
			return
		}
		useSimpleKey = true
	}

	extracted := map[string]interface{}{}
	if v, ok := obj["file"]; ok {
		extracted["file"] = fmt.Sprint(v)
		delete(obj, "file")
	}
	if v, ok := obj["function"]; ok {
		extracted["function"] = fmt.Sprint(v)
		delete(obj, "function")
	}
	if v, ok := obj["line"]; ok {
		extracted["line"] = parseIntRubyish(v)
		delete(obj, "line")
	}

	if len(extracted) > 0 {
		root["sourceLocation"] = extracted
	}

	if useSimpleKey {
		if m := mapFrom(payload["sourceLocation"]); m != nil && len(m) == 0 {
			delete(payload, "sourceLocation")
		}
	} else {
		if m := mapFrom(payload[keySrcLoc]); m != nil && len(m) == 0 {
			delete(payload, keySrcLoc)
		}
	}
}

// elevateSimpleTrace migrates simple trace values while applying project formatting.
func elevateSimpleTrace(payload, root map[string]interface{}, projectID string) {
	if _, exists := root["trace"]; exists {
		delete(payload, keyTrace)
		delete(payload, "trace")
		return
	}
	if v, ok := payload[keyTrace]; ok {
		root["trace"] = formatTraceValue(v, projectID)
		delete(payload, keyTrace)
		return
	}
	if v, ok := payload["trace"]; ok {
		root["trace"] = formatTraceValue(v, projectID)
		delete(payload, "trace")
	}
}

// elevateSimpleSpanID promotes span identifiers from the payload when present.
func elevateSimpleSpanID(payload, root map[string]interface{}) {
	if _, exists := root["spanId"]; exists {
		delete(payload, keySpanID)
		delete(payload, "spanId")
		return
	}
	if v, ok := payload[keySpanID]; ok {
		root["spanId"] = fmt.Sprint(v)
		delete(payload, keySpanID)
		return
	}
	if v, ok := payload["spanId"]; ok {
		root["spanId"] = fmt.Sprint(v)
		delete(payload, "spanId")
	}
}

// elevateSimpleTraceSampled converts trace sampling hints into canonical booleans.
func elevateSimpleTraceSampled(payload, root map[string]interface{}) {
	if _, exists := root["traceSampled"]; exists {
		delete(payload, keyTraceSampled)
		delete(payload, "traceSampled")
		return
	}
	if v, ok := payload[keyTraceSampled]; ok {
		root["traceSampled"] = parseBoolRubyish(v)
		delete(payload, keyTraceSampled)
		return
	}
	if v, ok := payload["traceSampled"]; ok {
		root["traceSampled"] = parseBoolRubyish(v)
		delete(payload, "traceSampled")
	}
}

// Ruby-like boolean: true if true, "true", or 1; otherwise false.
func parseBoolRubyish(v interface{}) bool {
	switch x := v.(type) {
	case bool:
		return x
	case json.Number:
		i, err := x.Int64()
		return err == nil && i == 1
	case float64:
		return x == 1
	case string:
		return x == "true"
	default:
		return false
	}
}

// Ruby-like int: strings -> leading integer or 0 if non-numeric; floats truncated; ints as-is.
func parseIntRubyish(v interface{}) int64 {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		if f, err := x.Float64(); err == nil {
			return int64(f)
		}
		return 0
	case float64:
		return int64(x)
	case float32:
		return int64(x)
	case int64:
		return x
	case int:
		return int64(x)
	case string:
		s := strings.TrimSpace(x)
		sign := int64(1)
		if strings.HasPrefix(s, "+") {
			s = s[1:]
		} else if strings.HasPrefix(s, "-") {
			sign = -1
			s = s[1:]
		}
		i := 0
		var acc int64
		for i < len(s) && s[i] >= '0' && s[i] <= '9' {
			acc = acc*10 + int64(s[i]-'0')
			i++
		}
		return sign * acc
	default:
		return 0
	}
}

// marshalNoHTMLEscape serializes v without HTML escaping to mirror backend encoding.
func marshalNoHTMLEscape(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "")
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}


// mustUnmarshalMap parses a JSON object string and fails the test on error.
func mustUnmarshalMap(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("unmarshal failed: %v\n%s", err, s)
	}
	return m
}

// TestSeverityNormalization ensures severity aliases resolve to canonical names.
func TestSeverityNormalization(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"AliasWarn", `{"severity":"warn"}`, "WARNING"},
		{"LowercaseCritical", `{"severity":"critical"}`, "CRITICAL"},
		{"NumericString", `{"severity":"400"}`, "DEFAULT"},
		{"NumericValue", `{"severity":400}`, "DEFAULT"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			out, err := TransformLogEntryJSON(tc.input, now)
			if err != nil {
				t.Fatal(err)
			}
			m := mustUnmarshalMap(t, out)
			if got := m["severity"]; got != tc.want {
				t.Fatalf("severity mismatch: got %v want %v", got, tc.want)
			}
		})
	}
}

// TestSeverityDefaultWhenMissing verifies missing severity defaults to DEFAULT.
func TestSeverityDefaultWhenMissing(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	out, err := TransformLogEntryJSON("{}", now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["severity"]; got != "DEFAULT" {
		t.Fatalf("severity mismatch: got %v want DEFAULT", got)
	}
}

// TestTimestampFromStructuredMap confirms timestamp maps become RFC3339 strings.
func TestTimestampFromStructuredMap(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := time.Date(2024, 2, 3, 4, 5, 6, 700000000, time.UTC)
	input := fmt.Sprintf(`{"timestamp":{"seconds":%d,"nanos":%d}}`, want.Unix(), want.Nanosecond())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(want) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(want))
	}
}

// TestTimestampFromLegacyFields checks legacy second/nano fields normalization.
func TestTimestampFromLegacyFields(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := time.Date(2024, 7, 8, 9, 10, 11, 123456000, time.UTC)
	input := fmt.Sprintf(`{"timestampSeconds":"%d","timestampNanos":"%d"}`, want.Unix(), want.Nanosecond())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(want) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(want))
	}
}

// TestTimestampFromPayloadTime confirms payload time strings are promoted.
func TestTimestampFromPayloadTime(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	input := `{
	  "jsonPayload": {
	    "time": "2024-02-03T04:05:06.007z",
	    "other": 1
	  }
	}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != "2024-02-03T04:05:06.007Z" {
		t.Fatalf("timestamp mismatch: got %v want 2024-02-03T04:05:06.007Z", got)
	}
	jp := m["jsonPayload"].(map[string]any)
	if _, ok := jp["time"]; ok {
		t.Fatalf("expected payload time to be removed: %+v", jp)
	}
}

// TestTimestampFromTimeNanos validates timeNanos inputs are converted correctly.
func TestTimestampFromTimeNanos(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	want := time.Date(2024, 5, 6, 7, 8, 9, 123456000, time.UTC)
	input := fmt.Sprintf(`{"timeNanos":%d}`, want.UnixNano())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(want) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(want))
	}
}

// TestTimestampAdjustsFuture reduces near-future timestamps within acceptable bounds.
func TestTimestampAdjustsFuture(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	future := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	input := fmt.Sprintf(`{"timestamp":"%s"}`, future.Format(time.RFC3339))
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	want := future.AddDate(-1, 0, 0)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(want) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(want))
	}
}

// TestTimestampResetsFarFuture rewrites far-future timestamps to the epoch.
func TestTimestampResetsFarFuture(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	future := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	input := fmt.Sprintf(`{"timestamp":"%s"}`, future.Format(time.RFC3339))
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(time.Unix(0, 0).UTC()) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(time.Unix(0, 0).UTC()))
	}
}

// TestHttpRequestExtraction ensures httpRequest payloads are elevated and normalized.
func TestHttpRequestExtraction(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	input := `{
	  "jsonPayload": {
	    "httpRequest": {
	      "requestMethod": "GET",
	      "requestUrl": "/path",
	      "requestSize": 123,
	      "responseSize": "456",
	      "cacheFillBytes": 789,
	      "status": "201",
	      "latency": "1.50 s",
	      "cacheHit": "true",
	      "cacheLookup": 1,
	      "cacheValidatedWithOriginServer": true,
	      "unknown": "keep"
	    }
	  }
	}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	httpReq, ok := m["httpRequest"].(map[string]any)
	if !ok {
		t.Fatalf("httpRequest not elevated: %+v", m)
	}
	if httpReq["requestMethod"] != "GET" || httpReq["requestUrl"] != "/path" {
		t.Fatalf("unexpected httpRequest strings: %+v", httpReq)
	}
	if httpReq["requestSize"] != "123" || httpReq["responseSize"] != "456" || httpReq["cacheFillBytes"] != "789" {
		t.Fatalf("size coercion mismatch: %+v", httpReq)
	}
	if toInt64(httpReq["status"]) != 201 {
		t.Fatalf("status mismatch: %+v", httpReq)
	}
	if httpReq["latency"] != "1.5s" {
		t.Fatalf("latency mismatch: %+v", httpReq)
	}
	if httpReq["cacheHit"] != true || httpReq["cacheLookup"] != true || httpReq["cacheValidatedWithOriginServer"] != true {
		t.Fatalf("cache booleans mismatch: %+v", httpReq)
	}
	jp := m["jsonPayload"].(map[string]any)
	if hr, ok := jp["httpRequest"].(map[string]any); ok {
		if len(hr) != 1 || hr["unknown"] != "keep" {
			t.Fatalf("unexpected residual httpRequest payload: %+v", hr)
		}
	} else {
		t.Fatalf("expected residual httpRequest payload to remain")
	}
}

// TestTraceAutoformatWithProject verifies traces gain project-qualified names.
func TestTraceAutoformatWithProject(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	input := `{
	  "logName": "projects/acme/logs/test",
	  "jsonPayload": {
	    "logging.googleapis.com/trace": "0123456789abcdef0123456789abcdef"
	  }
	}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	want := "projects/acme/traces/0123456789abcdef0123456789abcdef"
	if got := m["trace"]; got != want {
		t.Fatalf("trace mismatch: got %v want %s", got, want)
	}
	if jp, ok := m["jsonPayload"].(map[string]any); ok {
		if _, exists := jp["logging.googleapis.com/trace"]; exists {
			t.Fatalf("expected trace key removed from payload: %+v", jp)
		}
	} else {
		t.Fatalf("expected jsonPayload map in output")
	}
}

// TestTraceAutoformatWithoutProject keeps trace IDs untouched when no project exists.
func TestTraceAutoformatWithoutProject(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	input := `{
	  "jsonPayload": {
	    "logging.googleapis.com/trace": "0123456789abcdef0123456789abcdef"
	  }
	}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	want := "0123456789abcdef0123456789abcdef"
	if got := m["trace"]; got != want {
		t.Fatalf("trace mismatch without project: got %v want %s", got, want)
	}
}

// TestSpecialFieldsElevation checks special payload keys move to top-level fields.
func TestSpecialFieldsElevation(t *testing.T) {
	input := `{
	  "jsonPayload": {
	    "time": "2024-02-03T04:05:06.007Z",
	    "logging.googleapis.com/operation": {
	      "id":"op-123",
	      "producer": "svc.foo",
	      "first": "true",
	      "last": 0,
	      "extra": "keep-me"
	    },
	    "logging.googleapis.com/sourceLocation": {
	      "file": "main.go",
	      "function": "handler",
	      "line": "42",
	      "unknown": "residual"
	    },
	    "logging.googleapis.com/trace": "projects/x/traces/abc",
	    "logging.googleapis.com/spanId": "0123456789abcdef",
	    "logging.googleapis.com/trace_sampled": 1
	  }
	}`
	out, err := TransformLogEntryJSON(input, time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)

	if got, want := m["timestamp"].(string), "2024-02-03T04:05:06.007Z"; got != want {
		t.Fatalf("timestamp mismatch: got %q want %q", got, want)
	}

	op, ok := m["operation"].(map[string]any)
	if !ok {
		t.Fatalf("operation not elevated")
	}
	if op["id"] != "op-123" || op["producer"] != "svc.foo" || op["first"] != true || op["last"] != false {
		t.Fatalf("operation fields not normalized: %+v", op)
	}

	jp := m["jsonPayload"].(map[string]any)
	if opPayload, ok := jp["logging.googleapis.com/operation"].(map[string]any); ok {
		if len(opPayload) != 1 || opPayload["extra"] != "keep-me" {
			t.Fatalf("operation residual not preserved: %+v", opPayload)
		}
	} else {
		t.Fatalf("expected residual operation map to remain in payload")
	}

	src, ok := m["sourceLocation"].(map[string]any)
	if !ok {
		t.Fatalf("sourceLocation not elevated")
	}
	if src["file"] != "main.go" || src["function"] != "handler" || toInt64(src["line"]) != 42 {
		t.Fatalf("sourceLocation fields not normalized: %+v", src)
	}

	if slPayload, ok := jp["logging.googleapis.com/sourceLocation"].(map[string]any); ok {
		if len(slPayload) != 1 || slPayload["unknown"] != "residual" {
			t.Fatalf("sourceLocation residual not preserved: %+v", slPayload)
		}
	} else {
		t.Fatalf("expected residual sourceLocation map to remain in payload")
	}

	if m["trace"] != "projects/x/traces/abc" {
		t.Fatalf("trace not elevated")
	}
	if m["spanId"] != "0123456789abcdef" {
		t.Fatalf("spanId not elevated")
	}
	if m["traceSampled"] != true {
		t.Fatalf("traceSampled not normalized/elevated")
	}
	if _, ok := jp["logging.googleapis.com/trace"]; ok {
		t.Fatalf("trace key should be removed from payload")
	}
	if _, ok := jp["logging.googleapis.com/spanId"]; ok {
		t.Fatalf("spanId key should be removed from payload")
	}
	if _, ok := jp["logging.googleapis.com/trace_sampled"]; ok {
		t.Fatalf("trace_sampled key should be removed from payload")
	}
}

// TestSimpleKeysVariantsAlsoElevated handles simplified key names for special fields.
func TestSimpleKeysVariantsAlsoElevated(t *testing.T) {
	input := `{
	  "jsonPayload": {
	    "operation": {"id": 99},
	    "sourceLocation": {"file": "a.go", "line": 3.9},
	    "trace": "projects/y/traces/t1",
	    "spanId": "aaaaaaaaaaaaaaaa",
	    "traceSampled": "true"
	  }
	}`
	out, err := TransformLogEntryJSON(input, time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	op := m["operation"].(map[string]any)
	if op["id"] != "99" {
		t.Fatalf("operation id not stringified: %+v", op)
	}
	src := m["sourceLocation"].(map[string]any)
	if src["file"] != "a.go" || toInt64(src["line"]) != 3 {
		t.Fatalf("sourceLocation not normalized: %+v", src)
	}
	if m["trace"] != "projects/y/traces/t1" {
		t.Fatalf("trace not elevated")
	}
	if m["spanId"] != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("spanId not elevated")
	}
	if m["traceSampled"] != true {
		t.Fatalf("traceSampled not normalized")
	}
}

// toInt64 coerces JSON number variants to int64 for assertions.
func toInt64(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case json.Number:
		i, _ := x.Int64()
		return i
	default:
		return 0
	}
}
