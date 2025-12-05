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

package loggingmock

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
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
	keyLabels       = "logging.googleapis.com/labels"
)

// TransformLogEntryJSON rewrites a single LogEntry JSON to mimic the backend:
// - Normalize/elevate timestamp (top-level > legacy seconds/nanos > jsonPayload.time > now)
// - Promote severity (valid values go top-level; DEFAULT dropped; invalid payload values remain)
// - Elevate httpRequest only if entirely canonical (strict)
// - Elevate special fields from jsonPayload: operation, sourceLocation, trace, spanId, traceSampled
// - Never HTML-escape output JSON
func TransformLogEntryJSON(in string, now time.Time) (string, error) {
	var root map[string]any
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
		if parsed, ok := extractPayloadTimestamp(payload); ok {
			ts = parsed
			hasTimestamp = true
		}
	}
	if !hasTimestamp {
		ts = nowUTC
	}
	ts = adjustTimestampIfInvalid(ts, nowUTC)
	root["timestamp"] = formatRFC3339ZNormalized(ts)

	// 2) Severity (promote canonical severities; drop DEFAULT; leave invalid payload values)
	severitySet := false
	if sevVal, ok := root["severity"]; ok {
		if canonical, valid := canonicalizeSeverity(sevVal); valid {
			if canonical != "DEFAULT" {
				root["severity"] = canonical
				severitySet = true
			} else {
				delete(root, "severity")
			}
		} else {
			delete(root, "severity")
		}
	}

	// 3) Elevate special fields from jsonPayload
	if payload != nil {
		if rawHTTP, ok := payload["httpRequest"]; ok {
			if httpMap := mapFrom(rawHTTP); httpMap != nil {
				clone := make(map[string]any, len(httpMap))
				maps.Copy(clone, httpMap)
				canonical, ok := extractHttpRequest(clone)
				if ok && len(clone) == 0 {
					if _, exists := root["httpRequest"]; !exists {
						root["httpRequest"] = canonical
					}
					delete(payload, "httpRequest")
				} else {
					payload["httpRequest"] = httpMap
				}
			}
		}
		elevateOperation(payload, root)
		if rawSeverity, ok := payload["severity"]; ok {
			if canonical, valid := canonicalizeSeverity(rawSeverity); valid {
				delete(payload, "severity")
				if canonical != "DEFAULT" && !severitySet {
					root["severity"] = canonical
				}
			}
		}
		elevateLabels(payload, root)
		elevateSourceLocation(payload, root)
		elevateSimpleTrace(payload, root)
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

// mapFrom returns v as a map[string]any when possible, otherwise nil.
func mapFrom(v any) map[string]any {
	if v == nil {
		return nil
	}
	m, _ := v.(map[string]any)
	return m
}

const maxValidNanosecondsExclusive int64 = 1000000000

// extractPayloadTimestamp promotes timestamp-related fields from the payload when valid.
func extractPayloadTimestamp(payload map[string]any) (time.Time, bool) {
	if raw := payload["timestamp"]; raw != nil {
		if obj := mapFrom(raw); obj != nil {
			if ts, ok := parseTimestampStruct(obj); ok {
				delete(payload, "timestamp")
				return ts, true
			}
		}
	}

	if secsVal, hasSecs := payload["timestampSeconds"]; hasSecs {
		if nanosVal, hasNanos := payload["timestampNanos"]; hasNanos {
			if ts, ok := parseSecondsNanosPair(secsVal, nanosVal); ok {
				delete(payload, "timestampSeconds")
				delete(payload, "timestampNanos")
				return ts, true
			}
		}
	}

	if t, ok := payload["time"].(string); ok {
		if parsed, ok := parseRFC3339Flexible(t); ok {
			delete(payload, "time")
			return parsed, true
		}
	}

	return time.Time{}, false
}

// parseTimestampStruct parses a structured {seconds, nanos} timestamp map.
func parseTimestampStruct(obj map[string]any) (time.Time, bool) {
	secs, hasSecs := obj["seconds"]
	nanos, hasNanos := obj["nanos"]
	if !hasSecs || !hasNanos {
		return time.Time{}, false
	}
	return parseSecondsNanosPair(secs, nanos)
}

// parseSecondsNanosPair converts seconds/nanos pairs into a UTC time, validating nanos precision.
func parseSecondsNanosPair(secsVal any, nanosVal any) (time.Time, bool) {
	secs, ok := numericToInt64(secsVal)
	if !ok {
		return time.Time{}, false
	}
	nanos, ok := numericToInt64(nanosVal)
	if !ok {
		return time.Time{}, false
	}
	if nanos < 0 || nanos >= maxValidNanosecondsExclusive {
		return time.Time{}, false
	}
	return time.Unix(secs, nanos).UTC(), true
}

// normalizeTopLevelTimestamp lifts timestamp-like fields to a canonical RFC3339 value.
func normalizeTopLevelTimestamp(root map[string]any) (time.Time, bool) {
	if v, ok := root["timestamp"]; ok {
		switch val := v.(type) {
		case string:
			if ts, ok := parseRFC3339Flexible(val); ok {
				root["timestamp"] = formatRFC3339ZNormalized(ts)
				return ts, true
			}
			delete(root, "timestamp")
		case map[string]any:
			if ts, ok := parseTimestampStruct(val); ok {
				root["timestamp"] = formatRFC3339ZNormalized(ts)
				return ts, true
			}
			delete(root, "timestamp")
		default:
			delete(root, "timestamp")
		}
	}
	if rawSecs, ok := root["timestampSeconds"]; ok {
		if rawNanos, hasNanos := root["timestampNanos"]; hasNanos {
			if ts, ok := parseSecondsNanosPair(rawSecs, rawNanos); ok {
				root["timestamp"] = formatRFC3339ZNormalized(ts)
				delete(root, "timestampSeconds")
				delete(root, "timestampNanos")
				return ts, true
			}
			delete(root, "timestampSeconds")
			delete(root, "timestampNanos")
		} else {
			delete(root, "timestampSeconds")
		}
	}
	delete(root, "timeNanos")
	return time.Time{}, false
}

// adjustTimestampIfInvalid bounds future timestamps and replaces invalid values.
func adjustTimestampIfInvalid(ts time.Time, current time.Time) time.Time {
	currentUTC := current.UTC()
	if ts.IsZero() {
		return currentUTC
	}
	tsUTC := ts.UTC()
	if tsUTC.After(currentUTC.Add(24 * time.Hour)) {
		return currentUTC
	}
	maxPast := currentUTC.Add(-30 * 24 * time.Hour)
	if !tsUTC.After(maxPast) {
		return currentUTC
	}
	return tsUTC
}

// parseRFC3339Flexible accepts a broader RFC3339 subset, including lowercase Z zone markers.
func parseRFC3339Flexible(s string) (time.Time, bool) {
	str := strings.TrimSpace(s)
	if str == "" {
		return time.Time{}, false
	}
	if strings.HasSuffix(strings.ToLower(str), "z") && !strings.HasSuffix(str, "Z") {
		str = str[:len(str)-1] + "Z"
	}
	normalized, ok := normalizeRFC3339String(str)
	if !ok {
		return time.Time{}, false
	}
	tt, err := time.Parse(time.RFC3339Nano, normalized)
	if err != nil {
		return time.Time{}, false
	}
	return tt.UTC(), true
}

// normalizeRFC3339String rewrites offsets/fractions into a layout Go accepts.
func normalizeRFC3339String(str string) (string, bool) {
	if !strings.Contains(str, "T") {
		return "", false
	}
	if strings.HasSuffix(str, "Z") {
		main := str[:len(str)-1]
		m, ok := normalizeFractionalComponent(main)
		if !ok {
			return "", false
		}
		return m + "Z", true
	}
	lastPlus := strings.LastIndex(str, "+")
	lastMinus := strings.LastIndex(str, "-")
	idx := max(lastMinus, lastPlus)
	if idx <= len("2006-01-02") {
		return "", false
	}
	main := str[:idx]
	offset := str[idx:]
	normMain, ok := normalizeFractionalComponent(main)
	if !ok {
		return "", false
	}
	normOffset, ok := normalizeOffset(offset)
	if !ok {
		return "", false
	}
	return normMain + normOffset, true
}

// normalizeFractionalComponent trims fractional seconds longer than 9 digits.
func normalizeFractionalComponent(main string) (string, bool) {
	tIndex := strings.Index(main, "T")
	if tIndex == -1 {
		return "", false
	}
	dotIndex := strings.LastIndex(main, ".")
	if dotIndex == -1 {
		return main, true
	}
	if dotIndex < tIndex {
		return "", false
	}
	frac := main[dotIndex+1:]
	if frac == "" {
		return "", false
	}
	for _, r := range frac {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	return main[:dotIndex+1] + frac, true
}

// normalizeOffset accepts colon-free or short offsets and rewrites them to +/-HH:MM.
func normalizeOffset(offset string) (string, bool) {
	if len(offset) < 2 {
		return "", false
	}
	sign := offset[0]
	if sign != '+' && sign != '-' {
		return "", false
	}
	body := offset[1:]
	if body == "" {
		return "", false
	}
	if strings.Contains(body, ":") {
		parts := strings.Split(body, ":")
		if len(parts) != 2 {
			return "", false
		}
		hours := parts[0]
		mins := parts[1]
		if len(hours) == 1 {
			hours = "0" + hours
		} else if len(hours) != 2 {
			return "", false
		}
		if len(mins) != 2 {
			return "", false
		}
		if !isAllDigits(hours) || !isAllDigits(mins) {
			return "", false
		}
		return fmt.Sprintf("%c%s:%s", sign, hours, mins), true
	}
	if !isAllDigits(body) {
		return "", false
	}
	switch len(body) {
	case 1:
		return fmt.Sprintf("%c0%s:00", sign, body), true
	case 2:
		return fmt.Sprintf("%c%s:00", sign, body), true
	case 4:
		return fmt.Sprintf("%c%s:%s", sign, body[:2], body[2:]), true
	default:
		return "", false
	}
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
		"WARN":        "WARNING",
		"FATAL":       "CRITICAL",
		"TRACE":       "DEBUG",
		"TRACE_INT":   "DEBUG",
		"FINE":        "DEBUG",
		"FINER":       "DEBUG",
		"FINEST":      "DEBUG",
		"SEVERE":      "ERROR",
		"CONFIG":      "DEBUG",
		"CRIT":        "CRITICAL",
		"EMERG":       "EMERGENCY",
		"D":           "DEBUG",
		"I":           "INFO",
		"N":           "NOTICE",
		"W":           "WARNING",
		"E":           "ERROR",
		"C":           "CRITICAL",
		"A":           "ALERT",
		"INFORMATION": "INFO",
		"ERR":         "ERROR",
		"F":           "CRITICAL",
	}
)

// canonicalizeSeverity returns the canonical severity name and whether the input was valid.
func canonicalizeSeverity(v any) (string, bool) {
	switch vv := v.(type) {
	case json.Number, float64, float32, int64, int32, int, uint64, uint32, uint:
		return "", false
	case string:
		if strings.TrimSpace(vv) == "" {
			return "", false
		}
		// Intentionally avoid trimming to preserve whitespace sensitivity.
		up := strings.ToUpper(vv)
		if isAllDigits(up) {
			return "", false
		}
		if _, ok := validSeverities[up]; ok {
			return up, true
		}
		if mapped, ok := severityTranslations[up]; ok {
			return mapped, true
		}
		return "", false
	default:
		return "", false
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

// extractHttpRequest canonicalizes a jsonPayload.httpRequest map for elevation.
func extractHttpRequest(m map[string]any) (map[string]any, bool) {
	if m == nil {
		return nil, false
	}

	rawLatency, hasLatency := m["latency"]
	var latency string
	var latencyValid, latencyWithinRange bool

	if hasLatency {
		latency, latencyValid, latencyWithinRange = parseLatencyValue(rawLatency)
		if !latencyValid {
			return nil, false
		}
		delete(m, "latency")
	}

	promoted := make(map[string]any)

	process := func(key string, handler func(any)) {
		if v, ok := m[key]; ok {
			handler(v)
			delete(m, key)
		}
	}

	process("requestMethod", func(v any) { promoted["requestMethod"] = fmt.Sprint(v) })
	process("requestUrl", func(v any) { promoted["requestUrl"] = fmt.Sprint(v) })
	process("userAgent", func(v any) { promoted["userAgent"] = fmt.Sprint(v) })
	process("remoteIp", func(v any) { promoted["remoteIp"] = fmt.Sprint(v) })
	process("serverIp", func(v any) { promoted["serverIp"] = fmt.Sprint(v) })
	process("referer", func(v any) { promoted["referer"] = fmt.Sprint(v) })
	process("protocol", func(v any) { promoted["protocol"] = fmt.Sprint(v) })

	process("requestSize", func(v any) {
		if size, ok := formatHTTPSize(v); ok {
			promoted["requestSize"] = size
		}
	})
	process("responseSize", func(v any) {
		if size, ok := formatHTTPSize(v); ok {
			promoted["responseSize"] = size
		}
	})
	process("cacheFillBytes", func(v any) {
		if size, ok := formatHTTPSize(v); ok && !isZeroSize(size) {
			promoted["cacheFillBytes"] = size
		}
	})
	process("status", func(v any) {
		if statusVal, ok := normalizeStatusValue(v); ok {
			promoted["status"] = statusVal
		}
	})

	// Cache booleans: invalid encoding blocks promotion; accepted falsy values are dropped.
	cacheBoolKeys := []string{"cacheLookup", "cacheHit", "cacheValidatedWithOriginServer"}
	for _, key := range cacheBoolKeys {
		raw, ok := m[key]
		if !ok {
			continue
		}
		val, valid := parseHTTPBool(raw)
		if !valid {
			return nil, false
		}
		if val {
			promoted[key] = true
		}
		delete(m, key)
	}

	if hasLatency && latencyWithinRange {
		promoted["latency"] = latency
	}

	return promoted, true
}

// parseLatencyValue returns the canonical latency, whether parsing succeeded, and whether it
// falls within the Duration range. Valid formats require a trailing lowercase s, no whitespace,
// up to nine fractional digits, and optionally a leading dot.
func parseLatencyValue(v any) (string, bool, bool) {
	raw := fmt.Sprint(v)
	if raw == "" {
		return "", false, false
	}
	if strings.ContainsAny(raw, " \t") {
		return "", false, false
	}
	if !strings.HasSuffix(raw, "s") {
		return "", false, false
	}

	body := raw[:len(raw)-1]
	if body == "" {
		return "", false, false
	}

	if strings.Contains(body, "S") {
		return "", false, false
	}

	sign := 1
	switch body[0] {
	case '-':
		sign = -1
		body = body[1:]
	case '+':
		body = body[1:]
	}
	if body == "" {
		return "", false, false
	}

	var intPart, fracPart string
	if strings.HasPrefix(body, ".") {
		intPart = "0"
		fracPart = body[1:]
	} else if strings.Contains(body, ".") {
		parts := strings.SplitN(body, ".", 2)
		intPart = parts[0]
		fracPart = parts[1]
	} else {
		intPart = body
	}

	if intPart == "" {
		intPart = "0"
	}
	if !isAllDigits(intPart) {
		return "", false, false
	}
	if fracPart != "" && !isAllDigits(fracPart) {
		return "", false, false
	}
	if len(fracPart) > 9 {
		return "", false, false
	}

	secs, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil {
		return "", false, false
	}
	nanos := int32(0)
	if fracPart != "" {
		scale := int64(math.Pow10(9 - len(fracPart)))
		fracInt, _ := strconv.ParseInt(fracPart, 10, 64)
		nanos = int32(fracInt * scale)
	}

	secs *= int64(sign)
	nanos *= int32(sign)

	withinRange := true
	if abs64(secs) > 315576000000 {
		withinRange = false
	} else if abs64(secs) == 315576000000 && nanos != 0 {
		withinRange = false
	}

	return formatDurationSecondsNanos(secs, nanos), true, withinRange
}

// formatDurationSecondsNanos renders a Duration-like latency with 3/6/9-digit fractions.
func formatDurationSecondsNanos(secs int64, nanos int32) string {
	sign := ""
	if secs < 0 || nanos < 0 {
		sign = "-"
		secs = abs64(secs)
		nanos = int32(abs64(int64(nanos)))
	}

	frac := ""
	if nanos != 0 {
		if nanos%1_000_000 == 0 {
			frac = fmt.Sprintf(".%03d", nanos/1_000_000)
		} else if nanos%1_000 == 0 {
			frac = fmt.Sprintf(".%06d", nanos/1_000)
		} else {
			frac = fmt.Sprintf(".%09d", nanos)
		}
	}

	return fmt.Sprintf("%s%d%s%s", sign, secs, frac, "s")
}

// normalizeStatusValue coerces the status code where possible.
func normalizeStatusValue(v any) (any, bool) {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return float64(i), true
		}
		if f, err := x.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return float64(int64(f)), true
		}
		return nil, false
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, false
		}
		return float64(int64(x)), true
	case float32:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil, false
		}
		return float64(int64(f)), true
	case int64, int32, int, uint64, uint32, uint:
		if i, ok := numericToInt64(x); ok {
			return float64(i), true
		}
		return nil, false
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return nil, false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return float64(int64(f)), true
		}
		return s, true
	default:
		return nil, false
	}
}

// formatHTTPSize normalizes numeric-ish values to string representations.
func formatHTTPSize(v any) (string, bool) {
	if i, ok := numericToInt64(v); ok {
		return strconv.FormatInt(i, 10), true
	}

	switch x := v.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			return strconv.FormatInt(int64(f), 10), true
		}
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return "", false
		}
		return strconv.FormatInt(int64(x), 10), true
	case float32:
		f := float64(x)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "", false
		}
		return strconv.FormatInt(int64(f), 10), true
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return "", false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) {
			if f == math.Trunc(f) {
				return strconv.FormatInt(int64(f), 10), true
			}
			return s, true
		}
		return s, true
	}
	return "", false
}

// parseHTTPBool enforces the accepted cache* encodings.
func parseHTTPBool(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case string:
		if x == "true" {
			return true, true
		}
		if x == "false" {
			return false, true
		}
		return false, false
	default:
		return false, false
	}
}

// isZeroSize checks if a string representation of a number is zero.
func isZeroSize(s string) bool {
	f, err := strconv.ParseFloat(s, 64)
	return err == nil && f == 0
}

// abs64 returns the absolute value of an int64.
func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// formatTraceValue mirrors backend behavior by copying trace identifiers verbatim.
func formatTraceValue(v any) string {
	return strings.TrimSpace(fmt.Sprint(v))
}

// numericToInt64 converts flexible numeric inputs into an int64.
func numericToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		str := n.String()
		if !strings.ContainsAny(str, ".eE") {
			i, err := n.Int64()
			return i, err == nil
		}
		f, err := n.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		trunc := math.Trunc(f)
		if trunc > math.MaxInt64 || trunc < math.MinInt64 {
			return 0, false
		}
		return int64(trunc), true
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false
		}
		trunc := math.Trunc(n)
		if trunc > math.MaxInt64 || trunc < math.MinInt64 {
			return 0, false
		}
		return int64(trunc), true
	case float32:
		f := float64(n)
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false
		}
		trunc := math.Trunc(f)
		if trunc > math.MaxInt64 || trunc < math.MinInt64 {
			return 0, false
		}
		return int64(trunc), true
	case int64:
		return n, true
	case int32:
		return int64(n), true
	case int:
		return int64(n), true
	case uint:
		if uint64(n) > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	case uint32:
		return int64(n), true
	case uint64:
		if n > math.MaxInt64 {
			return 0, false
		}
		return int64(n), true
	default:
		return 0, false
	}
}

// elevateOperation hoists structured operation information from the payload to the root.
func elevateOperation(payload, root map[string]any) {
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
func elevateSourceLocation(payload, root map[string]any) {
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

	extracted := map[string]any{}
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

// elevateSimpleTrace migrates simple trace values without rewriting content.
func elevateSimpleTrace(payload, root map[string]any) {
	if _, exists := root["trace"]; exists {
		delete(payload, keyTrace)
		delete(payload, "trace")
		return
	}
	if v, ok := payload[keyTrace]; ok {
		root["trace"] = formatTraceValue(v)
		delete(payload, keyTrace)
		return
	}
	if v, ok := payload["trace"]; ok {
		root["trace"] = formatTraceValue(v)
		delete(payload, "trace")
	}
}

// elevateSimpleSpanID promotes span identifiers from the payload when present.
func elevateSimpleSpanID(payload, root map[string]any) {
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
func elevateSimpleTraceSampled(payload, root map[string]any) {
	if _, exists := root["traceSampled"]; exists {
		delete(payload, keyTraceSampled)
		delete(payload, "traceSampled")
		return
	}
	if v, ok := payload[keyTraceSampled]; ok {
		if vb, ok := v.(bool); ok {
			root["traceSampled"] = vb
		}
		delete(payload, keyTraceSampled)
		return
	}
	if v, ok := payload["traceSampled"]; ok {
		if vb, ok := v.(bool); ok {
			root["traceSampled"] = vb
		}
		delete(payload, "traceSampled")
	}
}

// elevateLabels promotes payload label maps into the top-level entry labels when valid.
func elevateLabels(payload, root map[string]any) {
	if _, exists := root["labels"]; exists {
		delete(payload, keyLabels)
		delete(payload, "labels")
		return
	}

	var (
		keyUsed string
		raw     any
		ok      bool
	)

	if raw, ok = payload[keyLabels]; ok {
		keyUsed = keyLabels
	} else if raw, ok = payload["labels"]; ok {
		keyUsed = "labels"
	} else {
		return
	}

	labelsMap := mapFrom(raw)
	if labelsMap == nil {
		delete(payload, keyUsed)
		return
	}

	out := make(map[string]string, len(labelsMap))
	for k, v := range labelsMap {
		str, ok := v.(string)
		if !ok {
			delete(payload, keyUsed)
			return
		}
		out[k] = str
	}
	if len(out) == 0 {
		delete(payload, keyUsed)
		return
	}

	root["labels"] = out
	delete(payload, keyUsed)
}

// Ruby-like boolean: true if true, "true", or 1; otherwise false.
func parseBoolRubyish(v any) bool {
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
func parseIntRubyish(v any) int64 {
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
		name        string
		input       string
		want        string
		wantPresent bool
	}{
		{"AliasWarn", `{"severity":"warn"}`, "WARNING", true},
		{"LowercaseCritical", `{"severity":"critical"}`, "CRITICAL", true},
		{"NumericString", `{"severity":"400"}`, "", false},
		{"NumericValue", `{"severity":400}`, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := TransformLogEntryJSON(tc.input, now)
			if err != nil {
				t.Fatal(err)
			}
			m := mustUnmarshalMap(t, out)
			got, ok := m["severity"]
			if tc.wantPresent {
				if !ok {
					t.Fatalf("severity missing, want %q", tc.want)
				}
				if got != tc.want {
					t.Fatalf("severity mismatch: got %v want %v", got, tc.want)
				}
				return
			}
			if ok {
				t.Fatalf("unexpected severity present: %v", got)
			}
		})
	}
}

// TestSeverityDefaultWhenMissing verifies missing severity leaves the entry without a severity field.
func TestSeverityDefaultWhenMissing(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	out, err := TransformLogEntryJSON("{}", now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if _, ok := m["severity"]; ok {
		t.Fatalf("unexpected severity present: %v", m["severity"])
	}
}

// TestPayloadSeverityHandling ensures payload severities follow backend promotion rules.
func TestPayloadSeverityHandling(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []struct {
		name                string
		input               string
		topSeverity         string
		topSeverityPresent  bool
		payloadSeverityKept bool
	}{
		{
			name:               "ValidPromoted",
			input:              `{"jsonPayload":{"severity":"warn","foo":1}}`,
			topSeverity:        "WARNING",
			topSeverityPresent: true,
		},
		{
			name:               "DefaultDropped",
			input:              `{"jsonPayload":{"severity":"DEFAULT","foo":1}}`,
			topSeverityPresent: false,
		},
		{
			name:                "InvalidRetained",
			input:               `{"jsonPayload":{"severity":" warn","foo":1}}`,
			topSeverityPresent:  false,
			payloadSeverityKept: true,
		},
	}

	for _, tc := range tests {
		c := tc
		t.Run(c.name, func(t *testing.T) {
			out, err := TransformLogEntryJSON(c.input, now)
			if err != nil {
				t.Fatalf("TransformLogEntryJSON() returned %v", err)
			}
			m := mustUnmarshalMap(t, out)
			got, have := m["severity"]
			if c.topSeverityPresent {
				if !have {
					t.Fatalf("severity missing, want %q", c.topSeverity)
				}
				if got != c.topSeverity {
					t.Fatalf("severity mismatch: got %v want %v", got, c.topSeverity)
				}
			} else if have {
				t.Fatalf("unexpected severity present: %v", got)
			}

			jp, ok := m["jsonPayload"].(map[string]any)
			if !ok {
				t.Fatalf("jsonPayload missing or wrong type: %T", m["jsonPayload"])
			}
			_, retained := jp["severity"]
			if c.payloadSeverityKept {
				if !retained {
					t.Fatal("expected severity to remain in jsonPayload")
				}
			} else if retained {
				t.Fatalf("severity unexpectedly retained in jsonPayload: %v", jp["severity"])
			}
		})
	}
}

// TestTimestampFromStructuredMap confirms timestamp maps become RFC3339 strings.
func TestTimestampFromStructuredMap(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	want := time.Date(2025, 11, 3, 17, 50, 58, 123456000, time.UTC)
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
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	want := time.Date(2025, 11, 3, 17, 50, 59, 654321000, time.UTC)
	input := fmt.Sprintf(`{"timestampSeconds":%d,"timestampNanos":%d}`, want.Unix(), want.Nanosecond())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(want) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(want))
	}
}

// TestTimestampLegacyFieldsRejectStrings ensures stringified legacy fields are ignored.
func TestTimestampLegacyFieldsRejectStrings(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384250000, time.UTC)
	want := formatRFC3339ZNormalized(now)
	input := `{"timestampSeconds":"1700000001","timestampNanos":"123"}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != want {
		t.Fatalf("timestamp mismatch: got %v want %s", got, want)
	}
	if _, ok := m["timestampSeconds"]; ok {
		t.Fatalf("timestampSeconds should be dropped: %+v", m)
	}
}

// TestTimestampLegacyFieldsFloatSecondsTruncated confirms floating seconds truncate toward zero.
func TestTimestampLegacyFieldsFloatSecondsTruncated(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	want := time.Date(2025, 11, 3, 17, 50, 57, 789000000, time.UTC)
	floatSeconds := float64(want.Unix()) + 0.9876
	input := fmt.Sprintf(`{"timestampSeconds":%s,"timestampNanos":%d}`, strconv.FormatFloat(floatSeconds, 'f', 4, 64), want.Nanosecond())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(want) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(want))
	}
}

// TestTimestampLegacyFieldsRejectStringNanos ensures mixed string nanos are ignored.
func TestTimestampLegacyFieldsRejectStringNanos(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	input := `{"timestampSeconds":1700000001,"timestampNanos":"123"}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(now) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(now))
	}
}

// TestTimestampStructuredNanosTooLargeFallsBack ensures proto-style timestamp maps with
// excessive fractional precision fall back to the ingestion time.
func TestTimestampStructuredNanosTooLargeFallsBack(t *testing.T) {
	now := time.Date(2025, 11, 4, 23, 13, 12, 655392000, time.UTC)
	target := now.Add(2 * time.Hour)
	input := fmt.Sprintf(`{"timestamp":{"seconds":%d,"nanos":1234567891}}`, target.Unix())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	want := formatRFC3339ZNormalized(now)
	if got := m["timestamp"]; got != want {
		t.Fatalf("timestamp mismatch: got %v want %s", got, want)
	}
}

// TestTimestampLegacyFieldsExcessNanosFallsBack verifies timestampSeconds/timestampNanos
// pairs exceeding nanosecond precision revert to the ingestion time and drop the fields.
func TestTimestampLegacyFieldsExcessNanosFallsBack(t *testing.T) {
	now := time.Date(2025, 11, 4, 23, 13, 13, 209024000, time.UTC)
	past := now.Add(-3 * time.Hour)
	input := fmt.Sprintf(`{"timestampSeconds":%d,"timestampNanos":1234567891234}`, past.Unix())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	want := formatRFC3339ZNormalized(now)
	if got := m["timestamp"]; got != want {
		t.Fatalf("timestamp mismatch: got %v want %s", got, want)
	}
	if _, ok := m["timestampSeconds"]; ok {
		t.Fatalf("timestampSeconds should be removed for invalid nanos: %+v", m["timestampSeconds"])
	}
	if _, ok := m["timestampNanos"]; ok {
		t.Fatalf("timestampNanos should be removed for invalid nanos: %+v", m["timestampNanos"])
	}
}

// TestTimestampMapRejectsStringValues ensures proto-style maps with strings are ignored.
func TestTimestampMapRejectsStringValues(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	input := `{"timestamp":{"seconds":"1700000001","nanos":"123"}}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(now) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(now))
	}
	if _, ok := m["timestamp"].(string); !ok {
		t.Fatalf("expected timestamp to remain a string: %+v", m["timestamp"]) // ensures covering decode
	}
}

// TestTimestampFromPayloadTime confirms payload time strings are promoted.
func TestTimestampFromPayloadTime(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	input := `{
	  "jsonPayload": {
	    "time": "2025-11-03T17:51:01.111111z",
	    "other": 1
	  }
	}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != "2025-11-03T17:51:01.111111Z" {
		t.Fatalf("timestamp mismatch: got %v want %s", got, "2025-11-03T17:51:01.111111Z")
	}
	jp := m["jsonPayload"].(map[string]any)
	if _, ok := jp["time"]; ok {
		t.Fatalf("expected payload time to be removed: %+v", jp)
	}
}

// TestPayloadTimeTruncatesAndTrims verifies fractional seconds truncate then trim trailing zeros.
func TestPayloadTimeTruncatesAndTrims(t *testing.T) {
	now := time.Date(2025, 11, 5, 6, 55, 55, 0, time.UTC)
	input := `{
	  "jsonPayload": {
	    "time": "2025-11-05T06:55:55.1200000009Z"
	  }
	}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != "2025-11-05T06:55:55.120Z" {
		t.Fatalf("timestamp = %v, want 2025-11-05T06:55:55.120Z", got)
	}
	jp := m["jsonPayload"].(map[string]any)
	if _, ok := jp["time"]; ok {
		t.Fatalf("expected payload time to be removed after consumption: %+v", jp)
	}
}

// TestPayloadTimestampStructPromotes verifies structured timestamp maps set the entry timestamp.
func TestPayloadTimestampStructPromotes(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	wanted := time.Date(2025, 11, 3, 17, 52, 1, 987654000, time.UTC)
	secondary := "2025-11-03T17:53:02.123456Z"
	input := fmt.Sprintf(`{
	  "jsonPayload": {
	    "timestamp": {
	      "seconds": %d,
	      "nanos": %d
	    },
	    "time": "%s"
	  }
	}`, wanted.Unix(), wanted.Nanosecond(), secondary)
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(wanted) {
		t.Fatalf("timestamp = %v, want %s", got, formatRFC3339ZNormalized(wanted))
	}
	jp := m["jsonPayload"].(map[string]any)
	if _, exists := jp["timestamp"]; exists {
		t.Fatalf("jsonPayload timestamp should be removed when used: %v", jp["timestamp"])
	}
	if _, exists := jp["time"]; !exists {
		t.Fatal("jsonPayload time should remain when not used to set timestamp")
	}
}

// TestPayloadTimestampSecondsPairPromotes validates timestampSeconds/timestampNanos pairs promote.
func TestPayloadTimestampSecondsPairPromotes(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	wanted := time.Date(2025, 11, 3, 18, 0, 1, 246000000, time.UTC)
	input := fmt.Sprintf(`{
	  "jsonPayload": {
	    "timestampSeconds": %d,
	    "timestampNanos": %d
	  }
	}`, wanted.Unix(), wanted.Nanosecond())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(wanted) {
		t.Fatalf("timestamp = %v, want %s", got, formatRFC3339ZNormalized(wanted))
	}
	jp := m["jsonPayload"].(map[string]any)
	if _, exists := jp["timestampSeconds"]; exists {
		t.Fatal("timestampSeconds should be removed when used")
	}
	if _, exists := jp["timestampNanos"]; exists {
		t.Fatal("timestampNanos should be removed when used")
	}
}

// TestPayloadTimestampInvalidNanosFallsBack ensures invalid nanos values are ignored.
func TestPayloadTimestampInvalidNanosFallsBack(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	seconds := now.Unix()
	invalidNanos := int64(2000000000)
	fallback := "2025-11-03T17:55:08.000001Z"
	input := fmt.Sprintf(`{
	  "jsonPayload": {
	    "timestampSeconds": %d,
	    "timestampNanos": %d,
	    "time": "%s"
	  }
	}`, seconds, invalidNanos, fallback)
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != fallback {
		t.Fatalf("timestamp = %v, want %s", got, fallback)
	}
	jp := m["jsonPayload"].(map[string]any)
	if _, exists := jp["timestampSeconds"]; !exists {
		t.Fatal("timestampSeconds should remain when pair invalid")
	}
	if _, exists := jp["timestampNanos"]; !exists {
		t.Fatal("timestampNanos should remain when pair invalid")
	}
	if _, exists := jp["time"]; exists {
		t.Fatal("time should be removed when used to set timestamp")
	}
}

// TestPayloadTimestampStructExcessNanosFallsBack ensures payload timestamp maps with excessive
// fractional precision fall back to the ingestion time while leaving the payload intact.
func TestPayloadTimestampStructExcessNanosFallsBack(t *testing.T) {
	now := time.Date(2025, 11, 4, 23, 13, 12, 655392000, time.UTC)
	desired := now.Add(90 * time.Minute)
	input := fmt.Sprintf(`{
	  "jsonPayload": {
	    "timestamp": {
	      "seconds": %d,
	      "nanos": 1234567891
	    }
	  }
	}`, desired.Unix())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	want := formatRFC3339ZNormalized(now)
	if got := m["timestamp"]; got != want {
		t.Fatalf("timestamp mismatch: got %v want %s", got, want)
	}
	jp := m["jsonPayload"].(map[string]any)
	raw, ok := jp["timestamp"].(map[string]any)
	if !ok {
		t.Fatalf("payload timestamp map should remain: %+v", jp["timestamp"])
	}
	if toInt64(raw["seconds"]) != desired.Unix() {
		t.Fatalf("payload seconds altered: %+v", raw["seconds"])
	}
	if toInt64(raw["nanos"]) != 1234567891 {
		t.Fatalf("payload nanos altered: %+v", raw["nanos"])
	}
}

// TestPayloadTimestampPairExcessNanosFallsBack verifies payload seconds/nanos pairs that exceed
// nanosecond precision fall back to the ingestion time and leave the payload untouched.
func TestPayloadTimestampPairExcessNanosFallsBack(t *testing.T) {
	now := time.Date(2025, 11, 4, 23, 13, 13, 209024000, time.UTC)
	past := now.Add(-45 * time.Minute)
	input := fmt.Sprintf(`{
	  "jsonPayload": {
	    "timestampSeconds": %d,
	    "timestampNanos": 1234567891234
	  }
	}`, past.Unix())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	want := formatRFC3339ZNormalized(now)
	if got := m["timestamp"]; got != want {
		t.Fatalf("timestamp mismatch: got %v want %s", got, want)
	}
	jp := m["jsonPayload"].(map[string]any)
	if toInt64(jp["timestampSeconds"]) != past.Unix() {
		t.Fatalf("payload timestampSeconds altered: %+v", jp["timestampSeconds"])
	}
	if toInt64(jp["timestampNanos"]) != 1234567891234 {
		t.Fatalf("payload timestampNanos altered: %+v", jp["timestampNanos"])
	}
}

// TestTimestampPrecedence verifies the backend preference order for timestamp fields.
func TestTimestampPrecedence(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	structured := time.Date(2025, 11, 3, 17, 50, 58, 123456000, time.UTC)
	legacy := time.Date(2025, 11, 3, 17, 50, 59, 654321000, time.UTC)
	payloadLiteral := "2025-11-03T17:51:01.111111Z"
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name: "StructuredMapWins",
			input: fmt.Sprintf(`{"timestamp":{"seconds":%d,"nanos":%d},"timestampSeconds":%d,"timestampNanos":%d,"jsonPayload":{"time":"%s"}}`,
				structured.Unix(), structured.Nanosecond(), legacy.Unix(), legacy.Nanosecond(), payloadLiteral),
			expected: formatRFC3339ZNormalized(structured),
		},
		{
			name: "LegacySecondsWinWhenMapMissing",
			input: fmt.Sprintf(`{"timestampSeconds":%d,"timestampNanos":%d,"jsonPayload":{"time":"%s"}}`,
				legacy.Unix(), legacy.Nanosecond(), payloadLiteral),
			expected: formatRFC3339ZNormalized(legacy),
		},
		{
			name:     "PayloadTimeUsedWhenOthersAbsent",
			input:    fmt.Sprintf(`{"jsonPayload":{"time":"%s"}}`, payloadLiteral),
			expected: payloadLiteral,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := TransformLogEntryJSON(tc.input, now)
			if err != nil {
				t.Fatalf("TransformLogEntryJSON() error = %v", err)
			}
			m := mustUnmarshalMap(t, out)
			if got := m["timestamp"]; got != tc.expected {
				t.Fatalf("timestamp mismatch: got %v want %s", got, tc.expected)
			}
		})
	}
}

// TestPayloadTimeAlternateFormats exercises accepted RFC3339 variants.
func TestPayloadTimeAlternateFormats(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	wantBase := formatRFC3339ZNormalized(time.Date(2025, 11, 3, 14, 7, 12, 0, time.UTC))
	wantFrac := formatRFC3339ZNormalized(time.Date(2025, 11, 3, 14, 7, 12, 123456789, time.UTC))
	wantNanos := formatRFC3339ZNormalized(time.Date(2025, 11, 3, 14, 7, 12, 123456000, time.UTC))
	tests := []struct {
		name     string
		literal  string
		expected string
	}{
		{"CanonicalZ", "2025-11-03T14:07:12Z", wantBase},
		{"CanonicalNanos", "2025-11-03T14:07:12.123456Z", wantNanos},
		{"LowercaseZ", "2025-11-03T14:07:12z", wantBase},
		{"CanonOffset", "2025-11-03T09:07:12-05:00", wantBase},
		{"ColonFreeOffset", "2025-11-03T17:07:12+0300", wantBase},
		{"ShortOffset", "2025-11-03T17:07:12+3", wantBase},
		{"ColonFreeNegativeOffset", "2025-11-03T09:07:12-0500", wantBase},
		{"LongFractionTruncated", "2025-11-03T14:07:12.123456789123Z", wantFrac},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"jsonPayload":{"time":"%s","other":1}}`, tc.literal)
			out, err := TransformLogEntryJSON(payload, now)
			if err != nil {
				t.Fatal(err)
			}
			m := mustUnmarshalMap(t, out)
			if got := m["timestamp"]; got != tc.expected {
				t.Fatalf("timestamp mismatch: got %v want %s", got, tc.expected)
			}
			jp := m["jsonPayload"].(map[string]any)
			if _, ok := jp["time"]; ok {
				t.Fatalf("expected payload time to be removed: %+v", jp)
			}
		})
	}
}

// TestPayloadTimeFractionTenDigitsTruncatesWithoutRounding confirms fractional components longer
// than nine digits are truncated rather than rounded.
func TestPayloadTimeFractionTenDigitsTruncatesWithoutRounding(t *testing.T) {
	now := time.Date(2025, 11, 4, 21, 6, 57, 0, time.UTC)
	want := "2025-11-04T21:06:57.123456789Z"
	cases := []string{
		"2025-11-04T21:06:57.1234567894Z",
		"2025-11-04T21:06:57.1234567895Z",
		"2025-11-04T21:06:57.1234567896Z",
	}
	for _, literal := range cases {
		payload := fmt.Sprintf(`{"jsonPayload":{"time":"%s","note":1}}`, literal)
		out, err := TransformLogEntryJSON(payload, now)
		if err != nil {
			t.Fatalf("TransformLogEntryJSON() for %s returned %v", literal, err)
		}
		m := mustUnmarshalMap(t, out)
		if got := m["timestamp"]; got != want {
			t.Fatalf("timestamp mismatch for %s: got %v want %s", literal, got, want)
		}
		jp := m["jsonPayload"].(map[string]any)
		if _, ok := jp["time"]; ok {
			t.Fatalf("time should be removed after promotion for %s", literal)
		}
	}
}

// TestPayloadTimeFractionNineDigitsPreserved covers accepted nine-digit fractions, including
// trailing zeros followed by a non-zero digit and maximal precision.
func TestPayloadTimeFractionNineDigitsPreserved(t *testing.T) {
	now := time.Date(2025, 11, 10, 17, 22, 49, 0, time.UTC)
	cases := []struct {
		name     string
		literal  string
		expected string
	}{
		{
			name:     "TrailingZeroNine",
			literal:  "2025-11-10T17:22:49.120000009Z",
			expected: "2025-11-10T17:22:49.120000009Z",
		},
		{
			name:     "MaxNines",
			literal:  "2025-11-04T21:06:57.999999999Z",
			expected: "2025-11-04T21:06:57.999999999Z",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"jsonPayload":{"time":"%s","note":2}}`, tc.literal)
			out, err := TransformLogEntryJSON(payload, now)
			if err != nil {
				t.Fatalf("TransformLogEntryJSON() for %s returned %v", tc.literal, err)
			}
			m := mustUnmarshalMap(t, out)
			if got := m["timestamp"]; got != tc.expected {
				t.Fatalf("timestamp mismatch for %s: got %v want %s", tc.literal, got, tc.expected)
			}
			jp := m["jsonPayload"].(map[string]any)
			if _, ok := jp["time"]; ok {
				t.Fatalf("time should be removed after promotion for %s", tc.literal)
			}
		})
	}
}

// TestPayloadTimeFractionTrailingZerosNormalized ensures nine-digit fractions composed entirely
// of trailing zeros compress down to millisecond precision.
func TestPayloadTimeFractionTrailingZerosNormalized(t *testing.T) {
	now := time.Date(2025, 11, 4, 21, 6, 57, 0, time.UTC)
	payload := `{"jsonPayload":{"time":"2025-11-04T21:06:57.120000000Z"}}`
	out, err := TransformLogEntryJSON(payload, now)
	if err != nil {
		t.Fatalf("TransformLogEntryJSON() returned %v", err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != "2025-11-04T21:06:57.120Z" {
		t.Fatalf("timestamp mismatch: got %v want %s", got, "2025-11-04T21:06:57.120Z")
	}
	jp := m["jsonPayload"].(map[string]any)
	if _, ok := jp["time"]; ok {
		t.Fatal("time should be removed after promotion")
	}
}

// TestPayloadTimeInvalidFormatsFallback ensures unsupported formats fall back to ingestion.
func TestPayloadTimeInvalidFormatsFallback(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	invalids := []string{
		"2025-11-03 14:07:12Z",
		"2025-11-03T14:07Z",
		"2025-11-03",
		"2025-11-03T14:07:12",
		"2025-12-31T23:59:60Z",
		"2025-11-03T24:01:01Z",
		"2025-W45-1T12:00:00Z",
		"2025-307T12:00:00Z",
		"2025-11-03T14:07:12PST",
		"2025-11-03T14:07:12UTC",
	}

	for _, literal := range invalids {
		payload := fmt.Sprintf(`{"jsonPayload":{"time":"%s","other":1}}`, literal)
		out, err := TransformLogEntryJSON(payload, now)
		if err != nil {
			t.Fatalf("TransformLogEntryJSON() returned error for %q: %v", literal, err)
		}
		m := mustUnmarshalMap(t, out)
		if got := m["timestamp"]; got != formatRFC3339ZNormalized(now) {
			t.Fatalf("timestamp mismatch for %q: got %v want %s", literal, got, formatRFC3339ZNormalized(now))
		}
		jp := m["jsonPayload"].(map[string]any)
		if _, ok := jp["time"]; !ok {
			t.Fatalf("invalid time %q should remain in payload: %+v", literal, jp)
		}
	}
}

// TestTimestampIgnoresTimeNanos validates timeNanos inputs fall back to ingestion time.
func TestTimestampIgnoresTimeNanos(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	want := formatRFC3339ZNormalized(now)
	input := fmt.Sprintf(`{"timeNanos":%d}`, time.Date(2024, 5, 6, 7, 8, 9, 123456000, time.UTC).UnixNano())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != want {
		t.Fatalf("timestamp mismatch: got %v want %s", got, want)
	}
}

// TestTimestampAcceptsFutureWithinWindow keeps timestamps up to 24h ahead.
func TestTimestampAcceptsFutureWithinWindow(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	input := fmt.Sprintf(`{"timestamp":"%s"}`, future.Format(time.RFC3339))
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(future) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(future))
	}
}

// TestTimestampClampsFutureBeyondWindow clamps timestamps more than 24h ahead.
func TestTimestampClampsFutureBeyondWindow(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 123000, time.UTC)
	future := now.Add(24*time.Hour + time.Minute)
	input := fmt.Sprintf(`{"timestamp":"%s"}`, future.Format(time.RFC3339))
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(now) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(now))
	}
}

// TestTimestampLegacyFieldsClampFutureBeyondWindow ensures legacy fields respect the future window.
func TestTimestampLegacyFieldsClampFutureBeyondWindow(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 500000, time.UTC)
	future := now.Add(24*time.Hour + 2*time.Minute)
	input := fmt.Sprintf(`{"timestampSeconds":%d,"timestampNanos":%d}`, future.Unix(), future.Nanosecond())
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(now) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(now))
	}
}

// TestPayloadTimeClampFutureBeyondWindow ensures payload time strings respect the future window.
func TestPayloadTimeClampFutureBeyondWindow(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 750000, time.UTC)
	future := now.Add(24*time.Hour + 3*time.Minute)
	input := fmt.Sprintf(`{"jsonPayload":{"time":"%s"}}`, future.Format(time.RFC3339Nano))
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(now) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(now))
	}
}

// TestTimestampAcceptsPastWithinWindow honors timestamps up to 21 days old.
func TestTimestampAcceptsPastWithinWindow(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	past := now.Add(-21 * 24 * time.Hour)
	input := fmt.Sprintf(`{"timestamp":"%s"}`, past.Format(time.RFC3339))
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(past) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(past))
	}
}

// TestTimestampClampsFarPast clamps timestamps 30 days or older.
func TestTimestampClampsFarPast(t *testing.T) {
	now := time.Date(2025, 1, 31, 3, 4, 5, 800000, time.UTC)
	past := now.Add(-30 * 24 * time.Hour)
	input := fmt.Sprintf(`{"timestamp":"%s"}`, past.Format(time.RFC3339))
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if got := m["timestamp"]; got != formatRFC3339ZNormalized(now) {
		t.Fatalf("timestamp mismatch: got %v want %s", got, formatRFC3339ZNormalized(now))
	}
}

// TestHttpRequestElevatedWhenCanonical ensures canonical httpRequest promotes to root.
func TestHttpRequestElevatedWhenCanonical(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	input := `{
	  "jsonPayload": {
	    "httpRequest": {
	      "requestMethod": "POST",
	      "requestUrl": "https://example.com/upload",
	      "requestSize": 256,
	      "responseSize": "512",
	      "cacheFillBytes": 1024,
	      "status": 200,
	      "latency": "2.500s",
	      "cacheHit": "true",
	      "cacheLookup": true,
	      "cacheValidatedWithOriginServer": true
	    }
	  }
	}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatalf("TransformLogEntryJSON() returned %v", err)
	}
	m := mustUnmarshalMap(t, out)
	rootHTTP, ok := m["httpRequest"].(map[string]any)
	if !ok {
		t.Fatalf("expected httpRequest to be elevated: %+v", m)
	}
	if rootHTTP["requestMethod"] != "POST" || rootHTTP["requestUrl"] != "https://example.com/upload" {
		t.Fatalf("unexpected httpRequest strings: %+v", rootHTTP)
	}
	if rootHTTP["requestSize"] != "256" || rootHTTP["responseSize"] != "512" || rootHTTP["cacheFillBytes"] != "1024" {
		t.Fatalf("size conversions mismatch: %+v", rootHTTP)
	}
	if status, ok := rootHTTP["status"].(float64); !ok || status != 200 {
		t.Fatalf("status mismatch: %+v", rootHTTP["status"])
	}
	if latency, ok := rootHTTP["latency"].(string); !ok || latency != "2.500s" {
		t.Fatalf("latency mismatch: %+v", rootHTTP["latency"])
	}
	if lookup, ok := rootHTTP["cacheLookup"].(bool); !ok || !lookup {
		t.Fatalf("cacheLookup bool mismatch: %+v", rootHTTP)
	}
	if hit, ok := rootHTTP["cacheHit"].(bool); !ok || !hit {
		t.Fatalf("cacheHit bool mismatch: %+v", rootHTTP)
	}
	if validated, ok := rootHTTP["cacheValidatedWithOriginServer"].(bool); !ok || !validated {
		t.Fatalf("cacheValidatedWithOriginServer bool mismatch: %+v", rootHTTP)
	}
	jp := m["jsonPayload"].(map[string]any)
	if _, exists := jp["httpRequest"]; exists {
		t.Fatalf("httpRequest should be removed from payload after elevation: %+v", jp)
	}
}

// TestHttpRequestRemainsInPayload ensures httpRequest stays under jsonPayload.
func TestHttpRequestRemainsInPayload(t *testing.T) {
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
	if _, exists := m["httpRequest"]; exists {
		t.Fatalf("httpRequest should not be elevated: %+v", m)
	}
	jp := m["jsonPayload"].(map[string]any)
	hr, ok := jp["httpRequest"].(map[string]any)
	if !ok {
		t.Fatalf("payload httpRequest missing: %+v", jp)
	}
	// Expect full map because invalid fields block promotion/consumption
	if len(hr) < 10 {
		t.Fatalf("expected httpRequest to remain intact: %+v", hr)
	}
}

// TestTraceCopiedVerbatimWithProject ensures traces promote without rewriting content.
func TestTraceCopiedVerbatimWithProject(t *testing.T) {
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
	want := "0123456789abcdef0123456789abcdef"
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

// TestTraceCopiedVerbatimWithoutProject keeps trace IDs untouched when no project exists.
func TestTraceCopiedVerbatimWithoutProject(t *testing.T) {
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

// TestTraceSampledRejectsNonBoolean validates only literal booleans are promoted.
func TestTraceSampledRejectsNonBoolean(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	tests := []string{
		`{"jsonPayload":{"logging.googleapis.com/trace_sampled":"true"}}`,
		`{"jsonPayload":{"logging.googleapis.com/trace_sampled":1}}`,
		`{"jsonPayload":{"traceSampled":"true"}}`,
	}

	for _, input := range tests {
		out, err := TransformLogEntryJSON(input, now)
		if err != nil {
			t.Fatalf("TransformLogEntryJSON() returned error for %s: %v", input, err)
		}
		m := mustUnmarshalMap(t, out)
		if _, ok := m["traceSampled"]; ok {
			t.Fatalf("traceSampled should not be promoted for %s", input)
		}
		jp := m["jsonPayload"].(map[string]any)
		if _, ok := jp["logging.googleapis.com/trace_sampled"]; ok {
			t.Fatalf("trace_sampled key should be removed for %s", input)
		}
		if _, ok := jp["traceSampled"]; ok {
			t.Fatalf("traceSampled key should be removed for %s", input)
		}
	}
}

// TestTraceSampledPromotesForBoolean ensures literal booleans elevate regardless of key variant.
func TestTraceSampledPromotesForBoolean(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	input := `{"jsonPayload":{"traceSampled":true}}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	if m["traceSampled"] != true {
		t.Fatalf("traceSampled not promoted for literal boolean: %+v", m)
	}
	jp := m["jsonPayload"].(map[string]any)
	if _, ok := jp["traceSampled"]; ok {
		t.Fatalf("traceSampled key should be removed from payload")
	}
}

// TestSpecialFieldsHandling covers selective promotion of structured payload keys.
func TestSpecialFieldsHandling(t *testing.T) {
	input := `{
	  "jsonPayload": {
	    "time": "2025-11-03T17:51:01.111111Z",
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
	    "logging.googleapis.com/trace_sampled": true
	  }
	}`
	now := time.Date(2025, 11, 3, 17, 50, 57, 384000000, time.UTC)
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)

	if got, want := m["timestamp"].(string), "2025-11-03T17:51:01.111111Z"; got != want {
		t.Fatalf("timestamp mismatch: got %q want %q", got, want)
	}

	op, ok := m["operation"].(map[string]any)
	if !ok {
		t.Fatalf("operation not elevated: %+v", m)
	}
	if op["id"] != "op-123" || op["producer"] != "svc.foo" {
		t.Fatalf("operation fields not normalized: %+v", op)
	}
	if first, ok := op["first"].(bool); !ok || !first {
		t.Fatalf("operation first flag mismatch: %+v", op["first"])
	}
	if last, ok := op["last"].(bool); !ok || last {
		t.Fatalf("operation last flag mismatch: %+v", op["last"])
	}

	jp := m["jsonPayload"].(map[string]any)
	if opPayload, ok := jp["logging.googleapis.com/operation"].(map[string]any); ok {
		if len(opPayload) != 1 || opPayload["extra"] != "keep-me" {
			t.Fatalf("operation residual not preserved: %+v", opPayload)
		}
	} else {
		t.Fatalf("expected operation map inside payload")
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
		t.Fatalf("trace not copied verbatim: %+v", m["trace"])
	}
	if m["spanId"] != "0123456789abcdef" {
		t.Fatalf("spanId not elevated")
	}
	if m["traceSampled"] != true {
		t.Fatalf("traceSampled not elevated for boolean input")
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

// TestLabelsElevatedForStringMap verifies string-only maps become entry labels.
func TestLabelsElevatedForStringMap(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 0, time.UTC)
	input := `{
	  "jsonPayload": {
	    "logging.googleapis.com/labels": {
	      "component": "payments",
	      "region": "us-central1"
	    }
	  }
	}`
	out, err := TransformLogEntryJSON(input, now)
	if err != nil {
		t.Fatal(err)
	}
	m := mustUnmarshalMap(t, out)
	labels, ok := m["labels"].(map[string]any)
	if !ok {
		t.Fatalf("labels not elevated: %+v", m)
	}
	if labels["component"] != "payments" || labels["region"] != "us-central1" {
		t.Fatalf("labels mismatch: %+v", labels)
	}
	jp := m["jsonPayload"].(map[string]any)
	if _, ok := jp["logging.googleapis.com/labels"]; ok {
		t.Fatalf("labels key should be removed from payload: %+v", jp)
	}
}

// TestLabelsDroppedWhenInvalid ensures non-string entries remove the label block.
func TestLabelsDroppedWhenInvalid(t *testing.T) {
	now := time.Date(2025, 11, 3, 17, 50, 57, 0, time.UTC)
	tests := []string{
		`{"jsonPayload":{"logging.googleapis.com/labels":{"component":1}}}`,
		`{"jsonPayload":{"logging.googleapis.com/labels":["bad"]}}`,
		`{"jsonPayload":{"labels":{"component":1}}}`,
	}
	for _, input := range tests {
		out, err := TransformLogEntryJSON(input, now)
		if err != nil {
			t.Fatalf("TransformLogEntryJSON() returned error for %s: %v", input, err)
		}
		m := mustUnmarshalMap(t, out)
		if _, ok := m["labels"]; ok {
			t.Fatalf("labels should not be elevated for %s", input)
		}
		jp := m["jsonPayload"].(map[string]any)
		if _, ok := jp["logging.googleapis.com/labels"]; ok {
			t.Fatalf("invalid labels block should be removed for %s", input)
		}
		if _, ok := jp["labels"]; ok {
			t.Fatalf("invalid labels block should be removed for %s", input)
		}
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
	op, ok := m["operation"].(map[string]any)
	if !ok {
		t.Fatalf("operation not elevated for simple key: %+v", m)
	}
	if op["id"] != "99" {
		t.Fatalf("operation id not normalized: %+v", op)
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
	if _, ok := m["traceSampled"]; ok {
		t.Fatalf("traceSampled should not elevate for string literal")
	}
	jp := m["jsonPayload"].(map[string]any)
	if _, ok := jp["operation"]; ok {
		t.Fatalf("operation key should be removed from payload")
	}
	if _, ok := jp["traceSampled"]; ok {
		t.Fatalf("traceSampled string should be removed from payload")
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

// TestHttpRequestPromotionOutcomes covers specific promotion rules for httpRequest.
func TestHttpRequestPromotionOutcomes(t *testing.T) {
	now := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)

	tests := []struct {
		name             string
		httpRequest      map[string]any
		shouldPromote    bool
		expectedLatency  string // empty if dropped or not promoted
		expectedCacheHit any    // nil if dropped
	}{
		// Latency Absent (still promotes)
		{
			name: "LatencyMissing_Promotes",
			httpRequest: map[string]any{
				"requestMethod": "GET",
				"requestUrl":    "https://example.com/items",
				"status":        200,
				"userAgent":     "ua",
			},
			shouldPromote: true,
		},

		// Latency Valid
		{
			name:            "LatencyValid_0.321s",
			httpRequest:     map[string]any{"latency": "0.321s"},
			shouldPromote:   true,
			expectedLatency: "0.321s",
		},
		{
			name:            "LatencyValid_1.234s",
			httpRequest:     map[string]any{"latency": "1.234s"},
			shouldPromote:   true,
			expectedLatency: "1.234s",
		},
		{
			name:            "LatencyValid_3s",
			httpRequest:     map[string]any{"latency": "3s"},
			shouldPromote:   true,
			expectedLatency: "3s",
		},
		{
			name:            "LatencyValid_.5s",
			httpRequest:     map[string]any{"latency": ".5s"},
			shouldPromote:   true,
			expectedLatency: "0.500s",
		},
		{
			name:            "LatencyValid_Trimmed",
			httpRequest:     map[string]any{"latency": "0.1200000s"},
			shouldPromote:   true,
			expectedLatency: "0.120s",
		},
		{
			name:            "LatencyValid_9Digits",
			httpRequest:     map[string]any{"latency": "0.123456789s"},
			shouldPromote:   true,
			expectedLatency: "0.123456789s",
		},
		{
			name:            "LatencyValid_42s",
			httpRequest:     map[string]any{"latency": "42s"},
			shouldPromote:   true,
			expectedLatency: "42s",
		},
		{
			name:            "LatencyValid_0s",
			httpRequest:     map[string]any{"latency": "0s"},
			shouldPromote:   true,
			expectedLatency: "0s",
		},
		{
			name:            "LatencyValid_Negative",
			httpRequest:     map[string]any{"latency": "-1s"},
			shouldPromote:   true,
			expectedLatency: "-1s",
		},
		{
			name:            "LatencyValid_LeadingZero",
			httpRequest:     map[string]any{"latency": "0000123s"},
			shouldPromote:   true,
			expectedLatency: "123s",
		},

		// Latency Invalid / Blocking
		{
			name:          "LatencyInvalid_Whitespace",
			httpRequest:   map[string]any{"latency": "0.123 s"},
			shouldPromote: false,
		},
		{
			name:          "LatencyInvalid_Tab",
			httpRequest:   map[string]any{"latency": "1\ts"},
			shouldPromote: false,
		},
		{
			name:          "LatencyInvalid_MissingSuffix",
			httpRequest:   map[string]any{"latency": "0.789"},
			shouldPromote: false,
		},
		{
			name:          "LatencyInvalid_UppercaseS",
			httpRequest:   map[string]any{"latency": "1.5S"},
			shouldPromote: false,
		},
		{
			name:          "LatencyInvalid_OtherUnits",
			httpRequest:   map[string]any{"latency": "150ms"},
			shouldPromote: false,
		},
		{
			name:          "LatencyInvalid_RawNumeric",
			httpRequest:   map[string]any{"latency": 1.5},
			shouldPromote: false,
		},
		{
			name:          "LatencyInvalid_10Digits",
			httpRequest:   map[string]any{"latency": "0.1234567891s"},
			shouldPromote: false,
		},
		{
			name:          "LatencyInvalid_LeadingDot10Digits",
			httpRequest:   map[string]any{"latency": ".1234567899s"},
			shouldPromote: false,
		},

		// Latency Range (Promotes but drops field)
		{
			name:            "LatencyRange_TooLarge",
			httpRequest:     map[string]any{"latency": "999999999999s"},
			shouldPromote:   true,
			expectedLatency: "", // Dropped
		},

		// Cache Fields
		{
			name:             "Cache_TrueLiteral",
			httpRequest:      map[string]any{"cacheHit": true},
			shouldPromote:    true,
			expectedCacheHit: true,
		},
		{
			name:             "Cache_TrueString",
			httpRequest:      map[string]any{"cacheHit": "true"},
			shouldPromote:    true,
			expectedCacheHit: true,
		},
		{
			name:             "Cache_FalseLiteral",
			httpRequest:      map[string]any{"cacheHit": false},
			shouldPromote:    true,
			expectedCacheHit: nil, // Dropped
		},
		{
			name:             "Cache_FalseString",
			httpRequest:      map[string]any{"cacheHit": "false"},
			shouldPromote:    true,
			expectedCacheHit: nil, // Dropped
		},
		{
			name:          "Cache_Invalid_Int",
			httpRequest:   map[string]any{"cacheHit": 1},
			shouldPromote: false,
		},
		{
			name:          "Cache_Invalid_StringInt",
			httpRequest:   map[string]any{"cacheHit": "1"},
			shouldPromote: false,
		},
		{
			name:          "Cache_Invalid_Uppercase",
			httpRequest:   map[string]any{"cacheHit": "TRUE"},
			shouldPromote: false,
		},
		{
			name:          "Cache_Invalid_MixedCase",
			httpRequest:   map[string]any{"cacheHit": "True"},
			shouldPromote: false,
		},
		{
			name:          "Cache_Invalid_Yes",
			httpRequest:   map[string]any{"cacheHit": "yes"},
			shouldPromote: false,
		},
		{
			name:          "Cache_Invalid_Empty",
			httpRequest:   map[string]any{"cacheHit": ""},
			shouldPromote: false,
		},

		// Other Fields
		{
			name:          "Status_String",
			httpRequest:   map[string]any{"status": "404"},
			shouldPromote: true,
		},
		{
			name:          "Status_Negative",
			httpRequest:   map[string]any{"status": -1},
			shouldPromote: true,
		},
		{
			name:          "Status_DecimalString",
			httpRequest:   map[string]any{"status": "200.0"},
			shouldPromote: true,
		},
		{
			name:          "Size_Float",
			httpRequest:   map[string]any{"requestSize": 1.0},
			shouldPromote: true,
		},
		{
			name:          "CacheFillBytes_Zero",
			httpRequest:   map[string]any{"cacheFillBytes": 0},
			shouldPromote: true,
			// Check logic manually for dropped field
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"httpRequest": tc.httpRequest}
			root := map[string]any{"jsonPayload": payload}
			inputBytes, _ := json.Marshal(root)
			out, err := TransformLogEntryJSON(string(inputBytes), now)
			if err != nil {
				t.Fatalf("TransformLogEntryJSON failed: %v", err)
			}
			m := mustUnmarshalMap(t, out)

			if tc.shouldPromote {
				if _, ok := m["httpRequest"]; !ok {
					t.Fatal("expected httpRequest to be promoted")
				}
				if _, ok := m["jsonPayload"].(map[string]any)["httpRequest"]; ok {
					t.Fatal("expected httpRequest to be removed from jsonPayload")
				}

				promoted := m["httpRequest"].(map[string]any)

				// Check Latency
				if tc.expectedLatency != "" {
					if got := promoted["latency"]; got != tc.expectedLatency {
						t.Fatalf("latency mismatch: got %v, want %v", got, tc.expectedLatency)
					}
				} else if _, ok := promoted["latency"]; ok {
					// If we expected empty (dropped) but got something, fail unless we didn't specify expectation (e.g. status tests)
					// But for latency tests we set expectedLatency to "" for dropped.
					// For other tests (Cache, Status), latency is missing from input, so it won't be there.
					if _, hadLatency := tc.httpRequest["latency"]; hadLatency {
						t.Fatalf("expected latency to be dropped, got %v", promoted["latency"])
					}
				}

				// Check CacheHit
				if _, hasCache := tc.httpRequest["cacheHit"]; hasCache {
					if tc.expectedCacheHit != nil {
						if got := promoted["cacheHit"]; got != tc.expectedCacheHit {
							t.Fatalf("cacheHit mismatch: got %v, want %v", got, tc.expectedCacheHit)
						}
					} else {
						if _, ok := promoted["cacheHit"]; ok {
							t.Fatalf("expected cacheHit to be dropped, got %v", promoted["cacheHit"])
						}
					}
				}

			} else {
				if _, ok := m["httpRequest"]; ok {
					t.Fatal("expected httpRequest NOT to be promoted")
				}
				if _, ok := m["jsonPayload"].(map[string]any)["httpRequest"]; !ok {
					t.Fatal("expected httpRequest to remain in jsonPayload")
				}
			}
		})
	}
}
