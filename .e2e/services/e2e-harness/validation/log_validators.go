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

package validation

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/pjscruggs/slogcp-e2e-internal/services/e2e-harness/client"
)

// Result captures the outcome of a log validation check.
type Result struct {
	Valid   bool
	Message string
	Details map[string]any
}

// LogValidator validates Cloud Logging entries emitted by E2E target services.
type LogValidator struct {
	expectedProjectID string
}

// NewLogValidator constructs a validator scoped to the provided GCP project.
func NewLogValidator(projectID string) *LogValidator {
	return &LogValidator{expectedProjectID: projectID}
}

// OperationExpectation describes the expected Cloud Logging operation metadata.
type OperationExpectation struct {
	ID       string
	Producer string
	First    bool
	Last     bool
}

// ExtractPayloadMap returns the structured payload carried by entry.
func (v *LogValidator) ExtractPayloadMap(entry *client.LogEntry) map[string]any {
	if entry == nil {
		return nil
	}
	return entry.Payload
}

// ValidateSeverity confirms that entry carries the expected severity.
func (v *LogValidator) ValidateSeverity(entry *client.LogEntry, expected client.Severity) Result {
	if entry == nil {
		return Result{Valid: false, Message: "log entry is nil"}
	}
	if entry.Severity != expected {
		return Result{
			Valid:   false,
			Message: fmt.Sprintf("severity mismatch: got %s, want %s", entry.Severity, expected),
			Details: map[string]any{
				"got":  entry.Severity.String(),
				"want": expected.String(),
			},
		}
	}
	return Result{Valid: true, Message: "severity matches"}
}

// ValidatePayload confirms that entry contains the expected structured fields.
func (v *LogValidator) ValidatePayload(entry *client.LogEntry, expectedContent map[string]any) Result {
	if entry == nil {
		return Result{Valid: false, Message: "log entry is nil"}
	}
	if entry.Payload == nil {
		return Result{Valid: false, Message: "payload is nil"}
	}

	missing := make([]string, 0, len(expectedContent))
	for key, expectedValue := range expectedContent {
		actual, ok := entry.Payload[key]
		if !ok {
			missing = append(missing, key)
			continue
		}
		if fmt.Sprintf("%v", actual) != fmt.Sprintf("%v", expectedValue) {
			return Result{
				Valid:   false,
				Message: fmt.Sprintf("field %s mismatch: got %v, want %v", key, actual, expectedValue),
			}
		}
	}

	if len(missing) > 0 {
		return Result{
			Valid:   false,
			Message: fmt.Sprintf("missing fields: %v", missing),
			Details: map[string]any{"payload": entry.Payload},
		}
	}

	return Result{Valid: true, Message: "payload matches expected content"}
}

// ValidateOperation confirms that operation metadata matches expected.
func (v *LogValidator) ValidateOperation(entry *client.LogEntry, expected OperationExpectation) Result {
	if entry == nil {
		return Result{Valid: false, Message: "log entry is nil"}
	}

	opMap := extractMap(entry.Payload["logging.googleapis.com/operation"])
	if len(opMap) == 0 {
		return Result{Valid: false, Message: "operation metadata is missing"}
	}

	id := valueToString(opMap["id"])
	producer := valueToString(opMap["producer"])
	first := valueToBool(opMap["first"])
	last := valueToBool(opMap["last"])

	details := map[string]any{
		"id":       id,
		"producer": producer,
		"first":    first,
		"last":     last,
	}

	if expected.ID != "" && id != expected.ID {
		return Result{Valid: false, Message: fmt.Sprintf("operation id mismatch: got %s, want %s", id, expected.ID), Details: details}
	}
	if expected.Producer != "" && producer != expected.Producer {
		return Result{Valid: false, Message: fmt.Sprintf("operation producer mismatch: got %s, want %s", producer, expected.Producer), Details: details}
	}
	if first != expected.First {
		return Result{Valid: false, Message: fmt.Sprintf("operation first mismatch: got %t, want %t", first, expected.First), Details: details}
	}
	if last != expected.Last {
		return Result{Valid: false, Message: fmt.Sprintf("operation last mismatch: got %t, want %t", last, expected.Last), Details: details}
	}

	return Result{Valid: true, Message: "operation metadata matches", Details: details}
}

// ValidateSourceLocation confirms that source location metadata is populated and plausible.
func (v *LogValidator) ValidateSourceLocation(entry *client.LogEntry, expectedFileFragment, expectedFuncSuffix string) Result {
	if entry == nil {
		return Result{Valid: false, Message: "log entry is nil"}
	}
	if entry.Source == nil {
		return Result{Valid: false, Message: "source location is missing"}
	}

	src := entry.Source
	details := map[string]any{
		"file":     src.File,
		"line":     src.Line,
		"function": src.Function,
	}

	if src.Line <= 0 {
		return Result{Valid: false, Message: fmt.Sprintf("invalid source line: %d", src.Line), Details: details}
	}
	if expectedFileFragment != "" && !strings.Contains(src.File, expectedFileFragment) {
		return Result{Valid: false, Message: fmt.Sprintf("unexpected source file: %s", src.File), Details: details}
	}
	if expectedFuncSuffix != "" && !strings.HasSuffix(src.Function, expectedFuncSuffix) {
		return Result{Valid: false, Message: fmt.Sprintf("unexpected source function: %s", src.Function), Details: details}
	}

	return Result{Valid: true, Message: "source location is valid", Details: details}
}

// ValidateTimestamp confirms that the log timestamp is recent and non-zero.
func (v *LogValidator) ValidateTimestamp(entry *client.LogEntry, maxAge time.Duration) Result {
	if entry == nil {
		return Result{Valid: false, Message: "log entry is nil"}
	}
	if entry.Timestamp.IsZero() {
		return Result{Valid: false, Message: "timestamp is zero"}
	}

	age := time.Since(entry.Timestamp)
	if age > maxAge {
		return Result{
			Valid:   false,
			Message: fmt.Sprintf("timestamp too old: %v ago", age),
			Details: map[string]any{
				"timestamp": entry.Timestamp,
				"age":       age.String(),
			},
		}
	}
	if age < 0 {
		return Result{Valid: false, Message: "timestamp is in the future", Details: map[string]any{"timestamp": entry.Timestamp}}
	}

	return Result{Valid: true, Message: "timestamp is valid"}
}

// ValidateResourceLabels confirms that Cloud Run resource metadata is internally consistent.
func (v *LogValidator) ValidateResourceLabels(entry *client.LogEntry) Result {
	if entry == nil {
		return Result{Valid: false, Message: "log entry is nil"}
	}

	resource := entry.Resource
	if resource == nil || len(resource.Labels) == 0 {
		return Result{Valid: false, Message: "resource labels are missing"}
	}

	serviceCtx := extractMap(entry.Payload["serviceContext"])
	serviceName := valueToString(serviceCtx["service"])
	revision := valueToString(serviceCtx["version"])

	labels := entry.Labels
	details := map[string]any{
		"labels":         labels,
		"serviceContext": serviceCtx,
		"resource":       resource,
		"resourceLabels": resource.Labels,
	}

	if mismatch := validateCoreResourceLabelConsistency(labels, resource.Labels, serviceName, revision); mismatch != "" {
		return Result{Valid: false, Message: mismatch, Details: details}
	}
	if mismatch := validateRegionAndProject(labels, resource.Labels, v.expectedProjectID); mismatch != "" {
		return Result{Valid: false, Message: mismatch, Details: details}
	}

	return Result{Valid: true, Message: "resource labels are valid", Details: details}
}

// validateServiceNameLabels verifies service labels agree with service context.
func validateServiceNameLabels(labels, resourceLabels map[string]string, serviceName string) string {
	if lbl := labels["cloud_run.service"]; lbl != "" && lbl != serviceName {
		return fmt.Sprintf("cloud_run.service mismatch: got %s, want %s", lbl, serviceName)
	}
	if lbl := resourceLabels["service_name"]; lbl != "" && lbl != serviceName {
		return fmt.Sprintf("resource service_name mismatch: got %s, want %s", lbl, serviceName)
	}
	return ""
}

// validateRevisionLabels verifies revision labels agree with service context.
func validateRevisionLabels(labels, resourceLabels map[string]string, revision string) string {
	if revision == "" {
		return ""
	}
	if lbl := labels["cloud_run.revision"]; lbl != "" && lbl != revision {
		return fmt.Sprintf("cloud_run.revision mismatch: got %s, want %s", lbl, revision)
	}
	if lbl := resourceLabels["revision_name"]; lbl != "" && lbl != revision {
		return fmt.Sprintf("resource revision_name mismatch: got %s, want %s", lbl, revision)
	}
	return ""
}

// validateCoreResourceLabelConsistency validates service/revision context mappings.
func validateCoreResourceLabelConsistency(labels, resourceLabels map[string]string, serviceName, revision string) string {
	if serviceName == "" {
		return "serviceContext missing service"
	}
	if mismatch := validateServiceNameLabels(labels, resourceLabels, serviceName); mismatch != "" {
		return mismatch
	}
	return validateRevisionLabels(labels, resourceLabels, revision)
}

// validateRegionAndProject validates required region and expected project IDs.
func validateRegionAndProject(labels, resourceLabels map[string]string, expectedProjectID string) string {
	region := labels["cloud_run.region"]
	if region == "" {
		region = resourceLabels["location"]
	}
	if region == "" {
		return "region label missing"
	}
	if mismatch := validateProjectID("project_id", labels["project_id"], expectedProjectID); mismatch != "" {
		return mismatch
	}
	if mismatch := validateProjectID("resource project_id", resourceLabels["project_id"], expectedProjectID); mismatch != "" {
		return mismatch
	}
	return ""
}

// validateProjectID validates optional project labels against the expected project.
func validateProjectID(field, value, expected string) string {
	if value == "" || value == expected {
		return ""
	}
	return fmt.Sprintf("%s mismatch: got %s, want %s", field, value, expected)
}

// ValidateLogName confirms that entry uses the expected Cloud Logging log name.
func (v *LogValidator) ValidateLogName(entry *client.LogEntry, expectedLogName string) Result {
	if entry == nil {
		return Result{Valid: false, Message: "log entry is nil"}
	}
	if entry.LogName == "" {
		return Result{Valid: false, Message: "log name is empty"}
	}

	expectedPrefix := fmt.Sprintf("projects/%s/logs/", v.expectedProjectID)
	if !strings.HasPrefix(entry.LogName, expectedPrefix) {
		return Result{Valid: false, Message: fmt.Sprintf("log name has wrong prefix: %s", entry.LogName)}
	}

	if expectedLogName != "" && entry.LogName != expectedLogName {
		actualID := strings.TrimPrefix(entry.LogName, expectedPrefix)
		expectedID := strings.TrimPrefix(expectedLogName, expectedPrefix)
		actualDecoded, _ := url.PathUnescape(actualID)
		expectedDecoded, _ := url.PathUnescape(expectedID)
		if actualDecoded != expectedDecoded {
			return Result{
				Valid:   false,
				Message: fmt.Sprintf("log name mismatch: got %s (decoded: %s), want %s (decoded: %s)", entry.LogName, actualDecoded, expectedLogName, expectedDecoded),
			}
		}
	}

	return Result{Valid: true, Message: "log name is valid", Details: map[string]any{"logName": entry.LogName}}
}

// ValidateLabels confirms that entry carries the expected labels.
func (v *LogValidator) ValidateLabels(entry *client.LogEntry, expectedLabels map[string]string) Result {
	if len(expectedLabels) == 0 {
		return Result{Valid: true, Message: "no labels to validate"}
	}
	if entry == nil {
		return Result{Valid: false, Message: "log entry is nil"}
	}
	if len(entry.Labels) == 0 {
		return Result{Valid: false, Message: "labels are empty"}
	}

	mismatches := []string{}
	for key, expectedValue := range expectedLabels {
		actualValue, ok := entry.Labels[key]
		if !ok {
			mismatches = append(mismatches, fmt.Sprintf("missing label %s", key))
			continue
		}
		if actualValue != expectedValue {
			mismatches = append(mismatches, fmt.Sprintf("label %s: got %s, want %s", key, actualValue, expectedValue))
		}
	}

	if len(mismatches) > 0 {
		return Result{Valid: false, Message: fmt.Sprintf("label mismatches: %v", mismatches), Details: map[string]any{"labels": entry.Labels}}
	}

	return Result{Valid: true, Message: "labels match"}
}

// ValidateStructuredPayload confirms that entry contains a non-empty structured payload.
func (v *LogValidator) ValidateStructuredPayload(entry *client.LogEntry) Result {
	if entry == nil {
		return Result{Valid: false, Message: "log entry is nil"}
	}
	if len(entry.Payload) == 0 {
		return Result{Valid: false, Message: "structured payload is empty"}
	}
	if _, ok := entry.Payload["message"]; !ok {
		return Result{Valid: false, Message: "structured payload missing 'message' field"}
	}

	return Result{Valid: true, Message: "structured payload is valid", Details: map[string]any{"fieldCount": len(entry.Payload)}}
}

// ValidateHTTPRequest provides coarse semantic validation only. It intentionally
// does not assert raw field presence/absence because client.LogEntry merges
// payload and top-level projections. Strict key presence checks should use
// raw entries from Logging API entries.list in the core test suite.
func (v *LogValidator) ValidateHTTPRequest(entry *client.LogEntry) Result {
	if entry == nil {
		return Result{Valid: false, Message: "log entry is nil"}
	}

	httpPayload := extractMap(entry.Payload["httpRequest"])
	if len(httpPayload) == 0 {
		httpPayload = extractMap(entry.HTTPRequest)
	}
	if len(httpPayload) == 0 {
		return Result{Valid: false, Message: "httpRequest metadata is missing"}
	}

	method := valueToString(httpPayload["requestMethod"])
	requestURL := valueToString(httpPayload["requestUrl"])
	status, _ := valueToInt(httpPayload["status"])

	details := map[string]any{
		"method": method,
		"url":    requestURL,
		"status": status,
	}

	if method == "" || requestURL == "" {
		return Result{Valid: false, Message: "httpRequest metadata missing method or url", Details: details}
	}

	return Result{Valid: true, Message: "httpRequest metadata is valid", Details: details}
}

// ValidateNestedStructure confirms that nested JSON structures survive logging.
func (v *LogValidator) ValidateNestedStructure(entry *client.LogEntry, _ map[string]any) Result {
	if entry == nil {
		return Result{Valid: false, Message: "log entry is nil"}
	}

	payload := entry.Payload
	if payload == nil {
		return Result{Valid: false, Message: "payload is nil"}
	}

	level1 := extractMap(extractMap(payload["attributes"])["level1"])
	if len(level1) == 0 {
		level1 = extractMap(payload["level1"])
	}
	if len(level1) == 0 {
		return Result{Valid: false, Message: "level1 not found in payload", Details: map[string]any{"payload": payload}}
	}

	level2 := extractMap(level1["level2"])
	if len(level2) == 0 {
		return Result{Valid: false, Message: "level2 not found or not a map", Details: map[string]any{"level1": level1}}
	}

	if val := valueToString(level2["level3"]); val != "deep-value" {
		return Result{Valid: false, Message: fmt.Sprintf("level3 not preserved correctly: got %v, want 'deep-value'", val), Details: map[string]any{"level2": level2}}
	}

	array := extractSlice(level2["array"])
	if len(array) != 2 {
		return Result{Valid: false, Message: fmt.Sprintf("array not preserved correctly: got %v", array), Details: map[string]any{"level2": level2}}
	}

	if fmt.Sprintf("%v", array[0]) != "item1" || fmt.Sprintf("%v", array[1]) != "item2" {
		return Result{Valid: false, Message: fmt.Sprintf("array contents incorrect: got [%v, %v]", array[0], array[1])}
	}

	return Result{Valid: true, Message: "nested structure validated successfully", Details: map[string]any{"depth": 3, "arrayLength": len(array)}}
}

// extractMap returns value when it is already a map payload.
func extractMap(value any) map[string]any {
	if value == nil {
		return nil
	}
	switch v := value.(type) {
	case map[string]any:
		return v
	default:
		return nil
	}
}

// extractSlice returns value when it is already a generic slice payload.
func extractSlice(value any) []any {
	switch v := value.(type) {
	case []any:
		return v
	default:
		return nil
	}
}

// valueToString renders common payload values as strings.
func valueToString(v any) string {
	if v == nil {
		return ""
	}
	switch value := v.(type) {
	case string:
		return value
	case fmt.Stringer:
		return value.String()
	case bool:
		if value {
			return "true"
		}
		return "false"
	case int, int32, int64, uint, uint32, uint64:
		return fmt.Sprintf("%v", value)
	case float32, float64:
		return fmt.Sprintf("%v", value)
	default:
		return fmt.Sprintf("%v", value)
	}
}

// valueToBool extracts a boolean while reporting whether conversion succeeded.
func valueToBool(v any) bool {
	switch value := v.(type) {
	case bool:
		return value
	case string:
		parsed, ok := parseStringBool(value)
		return ok && parsed
	case float64, float32:
		return parseNumericBoolString(fmt.Sprintf("%v", value))
	case int, int32, int64, uint, uint32, uint64:
		return parseNumericBoolString(fmt.Sprintf("%v", value))
	default:
		return false
	}
}

// parseStringBool parses accepted string bool representations.
func parseStringBool(value string) (bool, bool) {
	lower := strings.ToLower(strings.TrimSpace(value))
	if lower == "" {
		return false, false
	}
	switch lower {
	case "true", "1", "t":
		return true, true
	case "false", "0", "f":
		return false, true
	default:
		return false, false
	}
}

// parseNumericBoolString treats non-zero numeric text as true.
func parseNumericBoolString(value string) bool {
	return strings.TrimSpace(value) != "0"
}

// valueToInt extracts an int while guarding against overflow.
func valueToInt(v any) (int, bool) {
	const maxInt = int(^uint(0) >> 1)
	const minInt = -maxInt - 1
	maxIntUint64 := uint64(maxInt)

	if parsed, ok := signedValueToInt(v, maxInt, minInt); ok {
		return parsed, true
	}
	if parsed, ok := unsignedValueToInt(v, maxIntUint64); ok {
		return parsed, true
	}
	if parsed, ok := floatValueToInt(v, float64(maxInt), float64(minInt)); ok {
		return parsed, true
	}

	if parsed, ok := parseStringInt(v); ok {
		return parsed, true
	}
	return 0, false
}

// signedValueToInt converts signed integer values to int when in range.
func signedValueToInt(v any, maxInt, minInt int) (int, bool) {
	switch value := v.(type) {
	case int:
		return value, true
	case int32:
		return int(value), true
	case int64:
		return int64ToInt(value, maxInt, minInt)
	default:
		return 0, false
	}
}

// unsignedValueToInt converts unsigned integer values to int when in range.
func unsignedValueToInt(v any, limit uint64) (int, bool) {
	switch value := v.(type) {
	case uint:
		return parseUintToInt(uint64(value), limit)
	case uint32:
		return parseUintToInt(uint64(value), limit)
	case uint64:
		return parseUintToInt(value, limit)
	default:
		return 0, false
	}
}

// floatValueToInt converts float values to int when in range.
func floatValueToInt(v any, maxValue, minValue float64) (int, bool) {
	switch value := v.(type) {
	case float32:
		return float64ToInt(float64(value), maxValue, minValue)
	case float64:
		return float64ToInt(value, maxValue, minValue)
	default:
		return 0, false
	}
}

// parseStringInt parses string payload values as integers.
func parseStringInt(v any) (int, bool) {
	value, ok := v.(string)
	if !ok {
		return 0, false
	}
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	n, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, false
	}
	return n, true
}

// int64ToInt safely converts int64 to int.
func int64ToInt(value int64, maxInt, minInt int) (int, bool) {
	if value > int64(maxInt) || value < int64(minInt) {
		return 0, false
	}
	return int(value), true
}

// float64ToInt safely converts float64 to int.
func float64ToInt(value, maxValue, minValue float64) (int, bool) {
	if value > maxValue || value < minValue {
		return 0, false
	}
	return int(value), true
}

// parseUintToInt converts an unsigned integer to int after a range check.
func parseUintToInt(value uint64, limit uint64) (int, bool) {
	if value > limit {
		return 0, false
	}
	parsed, err := strconv.Atoi(strconv.FormatUint(value, 10))
	if err != nil {
		return 0, false
	}
	return parsed, true
}
