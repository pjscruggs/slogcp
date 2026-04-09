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

package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pjscruggs/slogcp-e2e-internal/services/e2e-harness/client"
	"github.com/pjscruggs/slogcp-e2e-internal/services/e2e-harness/validation"

	"github.com/google/uuid"
)

// logWaitTimeout defines how long to wait for logs to appear
const logWaitTimeout = 120 * time.Second

// TestCase represents a single test case
type TestCase struct {
	Name        string
	Description string
	Execute     func(ctx context.Context) error
}

// CoreLoggingTestSuite contains all core logging integration tests
type CoreLoggingTestSuite struct {
	targetClient       *client.TargetClient
	loggingClient      *client.LoggingClient
	validator          *validation.LogValidator
	testRunID          string
	projectID          string
	scenarioName       string
	serviceName        string
	expectedAppVersion string
	expectedLogID      string
	severityOnce       sync.Once
	severityOnceErr    error
	severityLogs       map[string]severityLogMetadata
}

// CoreScenarioConfig carries scenario specific expectations for the core logging suite.
type CoreScenarioConfig struct {
	ScenarioName       string
	ServiceName        string
	ExpectedAppVersion string
	ExpectedLogID      string
}

type severityLogMetadata struct {
	expected client.Severity
	testID   string
}

// NewCoreLoggingTestSuite creates a new test suite
func NewCoreLoggingTestSuite(targetClient *client.TargetClient, loggingClient *client.LoggingClient, projectID string, cfg CoreScenarioConfig) *CoreLoggingTestSuite {
	expectedVersion := cfg.ExpectedAppVersion
	if expectedVersion == "" {
		expectedVersion = "dev"
	}

	expectedLogID := cfg.ExpectedLogID
	if expectedLogID == "" {
		expectedLogID = "run.googleapis.com/stdout"
	}

	return &CoreLoggingTestSuite{
		targetClient:       targetClient,
		loggingClient:      loggingClient,
		validator:          validation.NewLogValidator(projectID),
		testRunID:          uuid.New().String(),
		projectID:          projectID,
		scenarioName:       cfg.ScenarioName,
		serviceName:        cfg.ServiceName,
		expectedAppVersion: expectedVersion,
		expectedLogID:      expectedLogID,
		severityLogs:       make(map[string]severityLogMetadata),
	}
}

// GetTests returns all test cases in the suite
func (s *CoreLoggingTestSuite) GetTests() []TestCase {
	tests := []TestCase{
		{
			Name:        "TestScenarioStartupMetadata",
			Description: "Validate scenario specific metadata (service name and version) appear in startup logs",
			Execute:     s.testStartupMetadata,
		},
		{
			Name:        "TestDefaultSeverity",
			Description: "Verify DEFAULT severity level maps correctly",
			Execute:     s.testDefaultSeverity,
		},
		{
			Name:        "TestDefaultSeverityBypassesMinimum",
			Description: "Ensure DEFAULT severity logs bypass handler minimum level filters",
			Execute:     s.testDefaultSeverityBypassesMinimum,
		},
		{
			Name:        "TestDebugSeverity",
			Description: "Verify DEBUG severity level maps correctly",
			Execute:     s.testDebugSeverity,
		},
		{
			Name:        "TestInfoSeverity",
			Description: "Verify INFO severity level maps correctly",
			Execute:     s.testInfoSeverity,
		},
		{
			Name:        "TestNoticeSeverity",
			Description: "Verify NOTICE severity level maps correctly",
			Execute:     s.testNoticeSeverity,
		},
		{
			Name:        "TestWarningSeverity",
			Description: "Verify WARNING severity level maps correctly",
			Execute:     s.testWarningSeverity,
		},
		{
			Name:        "TestErrorSeverity",
			Description: "Verify ERROR severity level maps correctly",
			Execute:     s.testErrorSeverity,
		},
		{
			Name:        "TestCriticalSeverity",
			Description: "Verify CRITICAL severity level maps correctly",
			Execute:     s.testCriticalSeverity,
		},
		{
			Name:        "TestAlertSeverity",
			Description: "Verify ALERT severity level maps correctly",
			Execute:     s.testAlertSeverity,
		},
		{
			Name:        "TestEmergencySeverity",
			Description: "Verify EMERGENCY severity level maps correctly",
			Execute:     s.testEmergencySeverity,
		},
		{
			Name:        "TestStructuredPayload",
			Description: "Ensure structured JSON payloads are properly parsed",
			Execute:     s.testStructuredPayload,
		},
		{
			Name:        "TestOperationGrouping",
			Description: "Verify Cloud Logging operation metadata is captured",
			Execute:     s.testOperationGrouping,
		},
		{
			Name:        "TestNestedStructure",
			Description: "Verify nested JSON structures are preserved",
			Execute:     s.testNestedStructure,
		},
		{
			Name:        "TestTimestamps",
			Description: "Validate log entries have proper timestamps",
			Execute:     s.testTimestamps,
		},
		{
			Name:        "TestSourceLocation",
			Description: "Verify source location metadata is populated",
			Execute:     s.testSourceLocation,
		},
		{
			Name:        "TestResourceLabels",
			Description: "Ensure resource labels are correctly populated",
			Execute:     s.testResourceLabels,
		},
		{
			Name:        "TestCustomLabels",
			Description: "Verify custom labels are properly attached",
			Execute:     s.testCustomLabels,
		},
		{
			Name:        "TestHTTPRequestPayload",
			Description: "Verify finalized httpRequest payloads include response metrics and are promoted to top-level",
			Execute:     s.testHTTPRequestPayload,
		},
		{
			Name:        "TestHTTPRequestInflightOmission",
			Description: "Verify in-flight httpRequest payloads omit response metrics while retaining request metadata",
			Execute:     s.testHTTPRequestInflightOmission,
		},
		{
			Name:        "TestHTTPRequestBackendResidualPromotionProbe",
			Description: "Diagnostic probe for backend promotion + residual handling when httpRequest contains unknown fields",
			Execute:     s.testHTTPRequestBackendResidualPromotionProbe,
		},
		{
			Name:        "TestHTTPRequestBackendStatusZeroProbe",
			Description: "Diagnostic probe for backend handling of httpRequest.status=0",
			Execute:     s.testHTTPRequestBackendStatusZeroProbe,
		},
		{
			Name:        "TestLogName",
			Description: "Ensure log entries land in the expected Cloud Logging stdout log",
			Execute:     s.testLogName,
		},
		{
			Name:        "TestBatchLogging",
			Description: "Ensure multiple logs from batch endpoint appear correctly",
			Execute:     s.testBatchLogging,
		},
	}

	return tests
}

// testDefaultSeverity verifies DEFAULT severity level
func (s *CoreLoggingTestSuite) testDefaultSeverity(ctx context.Context) error {
	// GCP strips the severity field when it is DEFAULT/unspecified, but querying severity=DEFAULT
	// still matches those entries. Validation here relies on our client mapping that absence to DEFAULT.
	return s.testSeverityLevel(ctx, "default", client.SeverityDefault)
}

// testDefaultSeverityBypassesMinimum verifies DEFAULT logs bypass high minimum levels.
func (s *CoreLoggingTestSuite) testDefaultSeverityBypassesMinimum(ctx context.Context) error {
	// Even though the backend removes the severity field for DEFAULT, it must still pass handler
	// filtering and come back as DEFAULT when read from Cloud Logging.
	log.Println("Testing DEFAULT severity bypassing minimum level filtering")

	testID := fmt.Sprintf("%s-default-bypass-%d", s.testRunID, time.Now().UnixNano())
	req := client.LogRequest{
		Message: "Default severity bypass minimum level test",
		TestID:  testID,
	}

	if _, err := s.targetClient.LogWithSeverity(ctx, "default", req); err != nil {
		return fmt.Errorf("failed to send default severity bypass log: %w", err)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-1 * time.Minute),
		MaxResults: 5,
	}, logWaitTimeout)
	if err != nil {
		return fmt.Errorf("failed to find default severity bypass log: %w", err)
	}

	if len(entries) != 1 {
		return fmt.Errorf("expected exactly 1 log entry for DEFAULT severity bypass test, got %d", len(entries))
	}

	entry := entries[0]
	result := s.validator.ValidateSeverity(entry, client.SeverityDefault)
	if !result.Valid {
		return fmt.Errorf("severity validation failed: %s", result.Message)
	}

	log.Println("DEFAULT severity bypassed minimum level filtering successfully")
	return nil
}

// testStartupMetadata verifies startup log contains scenario configuration data.
func (s *CoreLoggingTestSuite) testStartupMetadata(ctx context.Context) error {
	if s.serviceName == "" {
		return fmt.Errorf("service name not provided for scenario %s", s.scenarioName)
	}

	filter := fmt.Sprintf(`resource.labels.service_name="%s" AND jsonPayload.message="Target app starting"`, s.serviceName)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-5 * time.Minute),
		MaxResults: 5,
	}, logWaitTimeout)
	if err != nil {
		return fmt.Errorf("waiting for startup log: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no startup log entries found for service %s", s.serviceName)
	}

	payload := s.validator.ExtractPayloadMap(entries[0])
	version, ok := payload["version"].(string)
	if !ok {
		return fmt.Errorf("startup log missing version field (payload: %v)", payload)
	}
	if version != s.expectedAppVersion {
		return fmt.Errorf("unexpected app version, got %s want %s", version, s.expectedAppVersion)
	}

	log.Printf("Startup metadata validated for service %s (version=%s)", s.serviceName, version)
	return nil
}

// testDebugSeverity verifies DEBUG severity level
func (s *CoreLoggingTestSuite) testDebugSeverity(ctx context.Context) error {
	return s.testSeverityLevel(ctx, "debug", client.SeverityDebug)
}

// testInfoSeverity verifies INFO severity level
func (s *CoreLoggingTestSuite) testInfoSeverity(ctx context.Context) error {
	return s.testSeverityLevel(ctx, "info", client.SeverityInfo)
}

// testNoticeSeverity verifies NOTICE severity level
func (s *CoreLoggingTestSuite) testNoticeSeverity(ctx context.Context) error {
	return s.testSeverityLevel(ctx, "notice", client.SeverityNotice)
}

// testWarningSeverity verifies WARNING severity level
func (s *CoreLoggingTestSuite) testWarningSeverity(ctx context.Context) error {
	return s.testSeverityLevel(ctx, "warning", client.SeverityWarning)
}

// testErrorSeverity verifies ERROR severity level
func (s *CoreLoggingTestSuite) testErrorSeverity(ctx context.Context) error {
	return s.testSeverityLevel(ctx, "error", client.SeverityError)
}

// testCriticalSeverity verifies CRITICAL severity level
func (s *CoreLoggingTestSuite) testCriticalSeverity(ctx context.Context) error {
	return s.testSeverityLevel(ctx, "critical", client.SeverityCritical)
}

// testAlertSeverity verifies ALERT severity level
func (s *CoreLoggingTestSuite) testAlertSeverity(ctx context.Context) error {
	return s.testSeverityLevel(ctx, "alert", client.SeverityAlert)
}

// testEmergencySeverity verifies EMERGENCY severity level
func (s *CoreLoggingTestSuite) testEmergencySeverity(ctx context.Context) error {
	return s.testSeverityLevel(ctx, "emergency", client.SeverityEmergency)
}

// testSeverityLevel is a helper that tests a specific severity level
func (s *CoreLoggingTestSuite) testSeverityLevel(ctx context.Context, endpoint string, expected client.Severity) error {
	log.Printf("Testing severity level: %s", endpoint)

	metadata, err := s.severityMetadata(ctx, endpoint, expected)
	if err != nil {
		return err
	}

	// Wait for log to appear
	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, metadata.testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-5 * time.Minute),
		MaxResults: 10,
	}, logWaitTimeout)

	if err != nil {
		return fmt.Errorf("failed to find log for %s severity: %w", endpoint, err)
	}

	if len(entries) == 0 {
		return fmt.Errorf("no log entries found for %s severity", endpoint)
	}

	// Validate severity
	entry := entries[0]
	result := s.validator.ValidateSeverity(entry, metadata.expected)
	if !result.Valid {
		return fmt.Errorf("severity validation failed for %s: %s", endpoint, result.Message)
	}

	log.Printf("Severity %s validated successfully", endpoint)
	return nil
}

// severityMetadata returns normalized metadata for one severity endpoint call.
func (s *CoreLoggingTestSuite) severityMetadata(ctx context.Context, endpoint string, expected client.Severity) (severityLogMetadata, error) {
	if err := s.ensureSeverityLogs(ctx); err != nil {
		return severityLogMetadata{}, err
	}

	metadata, ok := s.severityLogs[endpoint]
	if !ok {
		return severityLogMetadata{}, fmt.Errorf("severity metadata not found for endpoint %s", endpoint)
	}

	if metadata.expected != expected {
		return severityLogMetadata{}, fmt.Errorf("unexpected expected severity for %s: got %s want %s", endpoint, metadata.expected, expected)
	}

	return metadata, nil
}

// ensureSeverityLogs emits and validates the configured severity matrix.
func (s *CoreLoggingTestSuite) ensureSeverityLogs(ctx context.Context) error {
	s.severityOnce.Do(func() {
		s.severityOnceErr = s.generateSeverityLogs(ctx)
	})

	return s.severityOnceErr
}

// generateSeverityLogs triggers all severity endpoints for the active test id.
func (s *CoreLoggingTestSuite) generateSeverityLogs(ctx context.Context) error {
	definitions := []struct {
		endpoint string
		expected client.Severity
	}{
		{endpoint: "default", expected: client.SeverityDefault},
		{endpoint: "debug", expected: client.SeverityDebug},
		{endpoint: "info", expected: client.SeverityInfo},
		{endpoint: "notice", expected: client.SeverityNotice},
		{endpoint: "warning", expected: client.SeverityWarning},
		{endpoint: "error", expected: client.SeverityError},
		{endpoint: "critical", expected: client.SeverityCritical},
		{endpoint: "alert", expected: client.SeverityAlert},
		{endpoint: "emergency", expected: client.SeverityEmergency},
	}

	for _, definition := range definitions {
		testID := fmt.Sprintf("%s-%s-%d", s.testRunID, definition.endpoint, time.Now().UnixNano())
		req := client.LogRequest{
			Message: fmt.Sprintf("Testing %s severity level", definition.endpoint),
			TestID:  testID,
		}

		if _, err := s.targetClient.LogWithSeverity(ctx, definition.endpoint, req); err != nil {
			s.severityOnceErr = fmt.Errorf("failed to log %s severity: %w", definition.endpoint, err)
			return s.severityOnceErr
		}

		s.severityLogs[definition.endpoint] = severityLogMetadata{
			expected: definition.expected,
			testID:   testID,
		}
	}

	log.Printf("Generated log requests for %d severity levels", len(definitions))
	return nil
}

// testStructuredPayload verifies structured JSON logging
func (s *CoreLoggingTestSuite) testStructuredPayload(ctx context.Context) error {
	log.Println("Testing structured payload logging")

	testID := fmt.Sprintf("%s-structured-%d", s.testRunID, time.Now().UnixNano())

	req := client.LogRequest{
		Message: "Structured payload test",
		TestID:  testID,
		Attributes: map[string]any{
			"user_id":    "test-user-123",
			"request_id": "req-456",
			"metadata": map[string]any{
				"source":  "e2e-test",
				"version": "1.0",
			},
		},
	}

	_, err := s.targetClient.LogStructured(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to send structured log: %w", err)
	}

	// Query logs
	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-1 * time.Minute),
		MaxResults: 10,
	}, logWaitTimeout)

	if err != nil {
		return fmt.Errorf("failed to find structured log: %w", err)
	}

	if len(entries) == 0 {
		return fmt.Errorf("no structured log entries found")
	}

	// Validate it's structured
	result := s.validator.ValidateStructuredPayload(entries[0])
	if !result.Valid {
		return fmt.Errorf("structured payload validation failed: %s", result.Message)
	}

	// Validate specific fields
	expectedContent := map[string]any{
		"message": "Structured payload test",
		"test_id": testID,
	}

	result = s.validator.ValidatePayload(entries[0], expectedContent)
	if !result.Valid {
		return fmt.Errorf("payload content validation failed: %s", result.Message)
	}

	log.Println("Structured payload validated successfully")
	return nil
}

// testOperationGrouping verifies Cloud Logging operation metadata is preserved
func (s *CoreLoggingTestSuite) testOperationGrouping(ctx context.Context) error {
	log.Println("Testing operation grouping logging")

	testID := fmt.Sprintf("%s-operation-%d", s.testRunID, time.Now().UnixNano())
	op := &client.Operation{
		ID:       fmt.Sprintf("operation-%s", testID),
		Producer: "core-logging-target", // Arbitrary producer for validation
		First:    true,
		Last:     false,
	}

	req := client.LogRequest{
		Message:   "Operation grouping test",
		TestID:    testID,
		Operation: op,
	}

	if _, err := s.targetClient.LogWithOperation(ctx, req); err != nil {
		return fmt.Errorf("failed to send operation log: %w", err)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-1 * time.Minute),
		MaxResults: 10,
	}, logWaitTimeout)
	if err != nil {
		return fmt.Errorf("failed to find operation log: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no operation log entries found")
	}

	entry := entries[0]
	result := s.validator.ValidateOperation(entry, validation.OperationExpectation{
		ID:       op.ID,
		Producer: op.Producer,
		First:    op.First,
		Last:     op.Last,
	})
	if !result.Valid {
		return fmt.Errorf("operation validation failed: %s", result.Message)
	}

	log.Println("Operation grouping validated successfully")
	return nil
}

// testNestedStructure verifies nested JSON structures
func (s *CoreLoggingTestSuite) testNestedStructure(ctx context.Context) error {
	log.Println("Testing nested structure logging")

	testID := fmt.Sprintf("%s-nested-%d", s.testRunID, time.Now().UnixNano())

	req := client.LogRequest{
		Message: "Nested structure test",
		TestID:  testID,
		Attributes: map[string]any{
			"level1": map[string]any{
				"level2": map[string]any{
					"level3": "deep-value",
					"array":  []string{"item1", "item2"},
				},
			},
		},
	}

	_, err := s.targetClient.LogNested(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to send nested log: %w", err)
	}

	// Query logs
	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-1 * time.Minute),
		MaxResults: 10,
	}, logWaitTimeout)

	if err != nil {
		return fmt.Errorf("failed to find nested log: %w", err)
	}

	if len(entries) == 0 {
		return fmt.Errorf("no nested log entries found")
	}

	// Validate nested structure
	entry := entries[0]
	expectedStructure := map[string]any{
		"level1": map[string]any{
			"level2": map[string]any{
				"level3": "deep-value",
				"array":  []any{"item1", "item2"},
			},
		},
	}

	result := s.validator.ValidateNestedStructure(entry, expectedStructure)
	if !result.Valid {
		return fmt.Errorf("nested structure validation failed: %s", result.Message)
	}

	log.Println("Nested structure validated successfully")
	return nil
}

// testTimestamps verifies timestamps are properly set
func (s *CoreLoggingTestSuite) testTimestamps(ctx context.Context) error {
	log.Println("Testing timestamp validation")

	testID := fmt.Sprintf("%s-timestamp-%d", s.testRunID, time.Now().UnixNano())

	beforeLog := time.Now()
	req := client.LogRequest{
		Message: "Timestamp test",
		TestID:  testID,
	}

	_, err := s.targetClient.LogWithSeverity(ctx, "info", req)
	if err != nil {
		return fmt.Errorf("failed to send log: %w", err)
	}
	afterLog := time.Now()

	// Query logs
	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  beforeLog.Add(-10 * time.Second),
		MaxResults: 10,
	}, logWaitTimeout)

	if err != nil {
		return fmt.Errorf("failed to find log: %w", err)
	}

	if len(entries) == 0 {
		return fmt.Errorf("no log entries found")
	}

	// Validate timestamp
	entry := entries[0]
	result := s.validator.ValidateTimestamp(entry, 5*time.Minute)
	if !result.Valid {
		return fmt.Errorf("timestamp validation failed: %s", result.Message)
	}

	// Check timestamp is within expected range
	if entry.Timestamp.Before(beforeLog) || entry.Timestamp.After(afterLog.Add(30*time.Second)) {
		return fmt.Errorf("timestamp %v not in expected range [%v, %v]",
			entry.Timestamp, beforeLog, afterLog.Add(30*time.Second))
	}

	log.Println("Timestamp validated successfully")
	return nil
}

// testSourceLocation verifies source location metadata is included with logs
func (s *CoreLoggingTestSuite) testSourceLocation(ctx context.Context) error {
	log.Println("Testing source location metadata")

	testID := fmt.Sprintf("%s-source-%d", s.testRunID, time.Now().UnixNano())

	req := client.LogRequest{
		Message: "Source location test",
		TestID:  testID,
	}

	if _, err := s.targetClient.LogWithSeverity(ctx, "info", req); err != nil {
		return fmt.Errorf("failed to send log: %w", err)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-1 * time.Minute),
		MaxResults: 10,
	}, logWaitTimeout)
	if err != nil {
		return fmt.Errorf("failed to find source location log: %w", err)
	}

	if len(entries) == 0 {
		return fmt.Errorf("no log entries found for source location test")
	}

	entry := entries[0]
	result := s.validator.ValidateSourceLocation(entry,
		"handlers/core_logging.go",
		"(*CoreLoggingHandler).LogInfo",
	)
	if !result.Valid {
		return fmt.Errorf("source location validation failed: %s", result.Message)
	}

	log.Println("Source location validated successfully")
	return nil
}

// testResourceLabels verifies resource labels are correct
func (s *CoreLoggingTestSuite) testResourceLabels(ctx context.Context) error {
	log.Println("Testing resource labels")

	testID := fmt.Sprintf("%s-resource-%d", s.testRunID, time.Now().UnixNano())

	req := client.LogRequest{
		Message: "Resource labels test",
		TestID:  testID,
	}

	_, err := s.targetClient.LogWithSeverity(ctx, "info", req)
	if err != nil {
		return fmt.Errorf("failed to send log: %w", err)
	}

	// Query logs
	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:       filter,
		StartTime:    time.Now().Add(-1 * time.Minute),
		MaxResults:   10,
		ResourceType: "cloud_run_revision",
	}, logWaitTimeout)

	if err != nil {
		return fmt.Errorf("failed to find log: %w", err)
	}

	if len(entries) == 0 {
		return fmt.Errorf("no log entries found")
	}

	// Validate resource labels
	result := s.validator.ValidateResourceLabels(entries[0])
	if !result.Valid {
		return fmt.Errorf("resource label validation failed: %s", result.Message)
	}

	log.Println("Resource labels validated successfully")
	return nil
}

// testCustomLabels verifies custom labels work correctly
func (s *CoreLoggingTestSuite) testCustomLabels(ctx context.Context) error {
	log.Println("Testing custom labels")

	testID := fmt.Sprintf("%s-labels-%d", s.testRunID, time.Now().UnixNano())
	overrideComponent := fmt.Sprintf("dynamic-component-%s", testID)

	req := client.LogRequest{
		Message: "Custom labels test",
		TestID:  testID,
		Labels: map[string]string{
			"component":       overrideComponent,
			"dynamic_test_id": testID,
		},
	}

	_, err := s.targetClient.LogWithLabels(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to send log with labels: %w", err)
	}

	// Query logs - the common labels should be present
	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-1 * time.Minute),
		MaxResults: 10,
	}, logWaitTimeout)

	if err != nil {
		return fmt.Errorf("failed to find log with labels: %w", err)
	}

	if len(entries) == 0 {
		return fmt.Errorf("no log entries found with custom labels")
	}

	// Validate that the common labels configured at startup are present
	expectedLabels := map[string]string{
		"app":             "slogcp-test-target",
		"component":       overrideComponent,
		"dynamic_test_id": testID,
	}

	result := s.validator.ValidateLabels(entries[0], expectedLabels)
	if !result.Valid {
		return fmt.Errorf("label validation failed: %s", result.Message)
	}

	payload := s.validator.ExtractPayloadMap(entries[0])
	if payload == nil {
		return fmt.Errorf("custom labels log missing payload")
	}

	if err := validatePayloadLabelGroup(payload, expectedLabels); err != nil {
		return err
	}

	log.Println("Custom labels validated successfully")
	return nil
}

// validatePayloadLabelGroup validates selected labels under logging.googleapis.com/labels.
func validatePayloadLabelGroup(payload map[string]any, expectedLabels map[string]string) error {
	const labelsGroupKey = "logging.googleapis.com/labels"
	rawLabelGroup, ok := payload[labelsGroupKey]
	if !ok {
		return fmt.Errorf("payload missing %q group", labelsGroupKey)
	}

	labelGroup, ok := rawLabelGroup.(map[string]any)
	if !ok {
		return fmt.Errorf("%s group has unexpected type %T", labelsGroupKey, rawLabelGroup)
	}

	for key, expected := range expectedLabels {
		raw, found := labelGroup[key]
		if !found {
			return fmt.Errorf("payload label %s missing from %s group", key, labelsGroupKey)
		}
		if fmt.Sprint(raw) != expected {
			return fmt.Errorf("payload label %s mismatch: got %v want %s", key, raw, expected)
		}
	}

	return nil
}

// testHTTPRequestPayload verifies promoted httpRequest metadata fields.
func (s *CoreLoggingTestSuite) testHTTPRequestPayload(ctx context.Context) error {
	testID := fmt.Sprintf("%s-http-request-%d", s.testRunID, time.Now().UnixNano())
	req := client.LogRequest{
		Message: "http request log",
		TestID:  testID,
	}
	startTime := time.Now().Add(-1 * time.Minute)

	if _, err := s.targetClient.LogHTTPRequest(ctx, req); err != nil {
		return fmt.Errorf("failed to send http request log: %w", err)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s" AND jsonPayload.message="%s"`, testID, req.Message)
	rawEntry, err := s.waitForRawLogEntry(ctx, filter, startTime)
	if err != nil {
		return fmt.Errorf("waiting for raw http request log: %w", err)
	}
	httpReq := rawEntryHTTPRequest(rawEntry)
	if len(httpReq) == 0 {
		return fmt.Errorf("top-level httpRequest metadata missing in raw entry: %s", formatRawEntryForLog(rawEntry))
	}

	if err := validateHTTPRequestPayloadShape(rawEntry, httpReq); err != nil {
		return err
	}
	if err := validateHTTPRequestPayloadValues(rawEntry, httpReq); err != nil {
		return err
	}
	if err := ensureRequestLogName(rawEntry, "http request log should come from application stdout log"); err != nil {
		return err
	}

	return nil
}

// validateHTTPRequestPayloadShape verifies required httpRequest fields and payload placement.
func validateHTTPRequestPayloadShape(rawEntry map[string]any, httpReq map[string]any) error {
	required := []string{"requestMethod", "requestUrl", "status", "responseSize", "latency"}
	if missing := missingKeys(httpReq, required...); len(missing) > 0 {
		return fmt.Errorf("top-level httpRequest missing required keys %v: %s", missing, formatRawEntryForLog(rawEntry))
	}
	if forbidden := presentKeys(rawEntryJSONPayload(rawEntry), "httpRequest"); len(forbidden) > 0 {
		return fmt.Errorf("jsonPayload.httpRequest should be absent for known-only finalized payloads: %s", formatRawEntryForLog(rawEntry))
	}
	return nil
}

// validateHTTPRequestPayloadValues verifies status/size/latency value constraints.
func validateHTTPRequestPayloadValues(rawEntry map[string]any, httpReq map[string]any) error {
	status, ok := valueToInt(httpReq["status"])
	if !ok {
		return fmt.Errorf("httpRequest.status is not numeric (%T): %s", httpReq["status"], formatRawEntryForLog(rawEntry))
	}
	if status != http.StatusOK {
		return fmt.Errorf("unexpected httpRequest.status: got %d want %d", status, http.StatusOK)
	}

	responseSize, ok := valueToInt(httpReq["responseSize"])
	if !ok {
		return fmt.Errorf("httpRequest.responseSize is not numeric (%T): %s", httpReq["responseSize"], formatRawEntryForLog(rawEntry))
	}
	if responseSize <= 0 {
		return fmt.Errorf("httpRequest.responseSize must be > 0, got %d", responseSize)
	}

	latency := strings.TrimSpace(fmt.Sprint(httpReq["latency"]))
	if latency == "" {
		return fmt.Errorf("httpRequest.latency should be non-empty: %s", formatRawEntryForLog(rawEntry))
	}

	return nil
}

// ensureRequestLogName asserts request logs were emitted to stdout logger.
func ensureRequestLogName(rawEntry map[string]any, reason string) error {
	logName := strings.TrimSpace(fmt.Sprint(rawEntry["logName"]))
	if strings.Contains(strings.ToLower(logName), "requests") {
		return fmt.Errorf("%s, got log name %s", reason, logName)
	}
	return nil
}

// testHTTPRequestInflightOmission verifies in-flight logs omit final response fields.
func (s *CoreLoggingTestSuite) testHTTPRequestInflightOmission(ctx context.Context) error {
	testID := fmt.Sprintf("%s-http-request-inflight-%d", s.testRunID, time.Now().UnixNano())
	req := client.LogRequest{
		Message: "inflight http request log",
		TestID:  testID,
	}
	startTime := time.Now().Add(-1 * time.Minute)

	if _, err := s.targetClient.LogHTTPRequestInflight(ctx, req); err != nil {
		return fmt.Errorf("failed to send in-flight http request log: %w", err)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s" AND jsonPayload.message="%s"`, testID, req.Message)
	rawEntry, err := s.waitForRawLogEntry(ctx, filter, startTime)
	if err != nil {
		return fmt.Errorf("waiting for in-flight raw http request log: %w", err)
	}

	httpReq := rawEntryHTTPRequest(rawEntry)
	if len(httpReq) == 0 {
		return fmt.Errorf("top-level httpRequest metadata missing for in-flight log: %s", formatRawEntryForLog(rawEntry))
	}

	if missing := missingKeys(httpReq, "requestMethod", "requestUrl"); len(missing) > 0 {
		return fmt.Errorf("in-flight httpRequest missing required keys %v: %s", missing, formatRawEntryForLog(rawEntry))
	}

	if forbidden := presentKeys(httpReq, "status", "responseSize", "latency"); len(forbidden) > 0 {
		return fmt.Errorf("in-flight httpRequest should omit response metrics, found %v: %s", forbidden, formatRawEntryForLog(rawEntry))
	}

	if forbidden := presentKeys(rawEntryJSONPayload(rawEntry), "httpRequest"); len(forbidden) > 0 {
		return fmt.Errorf("jsonPayload.httpRequest should be absent for known-only in-flight payloads: %s", formatRawEntryForLog(rawEntry))
	}

	logName := strings.TrimSpace(fmt.Sprint(rawEntry["logName"]))
	if strings.Contains(strings.ToLower(logName), "requests") {
		return fmt.Errorf("in-flight httpRequest log should come from application stdout log, got log name %s", logName)
	}

	return nil
}

// testHTTPRequestBackendResidualPromotionProbe checks residual parser key behavior.
func (s *CoreLoggingTestSuite) testHTTPRequestBackendResidualPromotionProbe(ctx context.Context) error {
	testID := fmt.Sprintf("%s-http-request-residual-%d", s.testRunID, time.Now().UnixNano())
	req := client.LogRequest{
		Message: "http request residual parser probe",
		TestID:  testID,
	}
	startTime := time.Now().Add(-1 * time.Minute)

	if _, err := s.targetClient.LogHTTPRequestParserResidual(ctx, req); err != nil {
		return fmt.Errorf("failed to send residual parser probe log: %w", err)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s" AND jsonPayload.message="%s"`, testID, req.Message)
	rawEntry, err := s.waitForRawLogEntry(ctx, filter, startTime)
	if err != nil {
		return fmt.Errorf("waiting for residual parser probe log: %w", err)
	}

	httpReq := rawEntryHTTPRequest(rawEntry)
	payload := rawEntryJSONPayload(rawEntry)
	payloadHTTP := nestedMap(payload, "httpRequest")

	var mismatches []string
	if len(httpReq) == 0 {
		mismatches = append(mismatches, "top-level httpRequest missing")
	}
	if missing := missingKeys(httpReq, "requestMethod", "requestUrl", "status"); len(missing) > 0 {
		mismatches = append(mismatches, fmt.Sprintf("top-level httpRequest missing %v", missing))
	}
	if present := presentKeys(httpReq, "customField", "anotherCustom"); len(present) > 0 {
		mismatches = append(mismatches, fmt.Sprintf("unknown keys promoted unexpectedly: %v", present))
	}
	if len(payloadHTTP) == 0 {
		mismatches = append(mismatches, "jsonPayload.httpRequest residual map missing")
	} else {
		if got := strings.TrimSpace(fmt.Sprint(payloadHTTP["customField"])); got != "keep-me" {
			mismatches = append(mismatches, fmt.Sprintf("jsonPayload.httpRequest.customField mismatch: got %q want %q", got, "keep-me"))
		}
		if got, ok := valueToInt(payloadHTTP["anotherCustom"]); !ok || got != 42 {
			mismatches = append(mismatches, fmt.Sprintf("jsonPayload.httpRequest.anotherCustom mismatch: got %v want 42", payloadHTTP["anotherCustom"]))
		}
	}

	return s.reportBackendProbeResult("residual-promotion", rawEntry, mismatches)
}

// testHTTPRequestBackendStatusZeroProbe checks backend handling of status=0 probes.
func (s *CoreLoggingTestSuite) testHTTPRequestBackendStatusZeroProbe(ctx context.Context) error {
	testID := fmt.Sprintf("%s-http-request-status-zero-%d", s.testRunID, time.Now().UnixNano())
	req := client.LogRequest{
		Message: "http request status-zero parser probe",
		TestID:  testID,
	}
	startTime := time.Now().Add(-1 * time.Minute)

	if _, err := s.targetClient.LogHTTPRequestParserStatusZero(ctx, req); err != nil {
		return fmt.Errorf("failed to send status-zero parser probe log: %w", err)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s" AND jsonPayload.message="%s"`, testID, req.Message)
	rawEntry, err := s.waitForRawLogEntry(ctx, filter, startTime)
	if err != nil {
		return fmt.Errorf("waiting for status-zero parser probe log: %w", err)
	}

	httpReq := rawEntryHTTPRequest(rawEntry)
	var mismatches []string
	if len(httpReq) == 0 {
		mismatches = append(mismatches, "top-level httpRequest missing")
	}
	if missing := missingKeys(httpReq, "requestMethod", "requestUrl"); len(missing) > 0 {
		mismatches = append(mismatches, fmt.Sprintf("top-level httpRequest missing %v", missing))
	}
	if present := presentKeys(httpReq, "status"); len(present) > 0 {
		mismatches = append(mismatches, "status should be omitted for status=0 probe")
	}

	return s.reportBackendProbeResult("status-zero", rawEntry, mismatches)
}

// waitForRawLogEntry polls Cloud Logging for one matching raw entry.
func (s *CoreLoggingTestSuite) waitForRawLogEntry(ctx context.Context, filter string, startTime time.Time) (map[string]any, error) {
	entries, err := s.loggingClient.WaitForLogsJSON(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  startTime,
		MaxResults: 10,
	}, logWaitTimeout)
	if err != nil {
		return nil, fmt.Errorf("waiting for raw log entry: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no entries returned for filter %q", filter)
	}
	return entries[0], nil
}

// rawEntryJSONPayload extracts jsonPayload from a raw logging entry map.
func rawEntryJSONPayload(raw map[string]any) map[string]any {
	return valueAsMap(raw["jsonPayload"])
}

// rawEntryHTTPRequest extracts httpRequest from a raw logging entry map.
func rawEntryHTTPRequest(raw map[string]any) map[string]any {
	return valueAsMap(raw["httpRequest"])
}

// nestedMap safely extracts one nested map value by key.
func nestedMap(raw map[string]any, key string) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	return valueAsMap(raw[key])
}

// missingKeys returns keys not present in m.
func missingKeys(m map[string]any, keys ...string) []string {
	if len(keys) == 0 {
		return nil
	}
	missing := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := m[key]; !ok {
			missing = append(missing, key)
		}
	}
	return missing
}

// presentKeys returns keys present in m.
func presentKeys(m map[string]any, keys ...string) []string {
	if len(keys) == 0 {
		return nil
	}
	present := make([]string, 0, len(keys))
	for _, key := range keys {
		if _, ok := m[key]; ok {
			present = append(present, key)
		}
	}
	return present
}

// valueAsMap converts dynamic map-like values into map[string]any.
func valueAsMap(v any) map[string]any {
	switch value := v.(type) {
	case map[string]any:
		return value
	default:
		return nil
	}
}

// valueToInt converts dynamic numeric values into int when possible.
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
	if parsed, ok := jsonNumberValueToInt(v); ok {
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

// jsonNumberValueToInt converts json.Number payload values to int.
func jsonNumberValueToInt(v any) (int, bool) {
	value, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	return jsonNumberToInt(value)
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

// jsonNumberToInt converts json.Number to int through int64 or float64 parsing.
func jsonNumberToInt(value json.Number) (int, bool) {
	if i, err := value.Int64(); err == nil {
		return int(i), true
	}
	if f, err := value.Float64(); err == nil {
		return int(f), true
	}
	return 0, false
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

// reportBackendProbeResult logs backend probe mismatches with raw payload details.
func (s *CoreLoggingTestSuite) reportBackendProbeResult(name string, raw map[string]any, mismatches []string) error {
	if len(mismatches) == 0 {
		log.Printf("Backend httpRequest parser probe %q matched expected behavior", name)
		return nil
	}

	msg := fmt.Sprintf("Backend httpRequest parser probe %q mismatches: %s", name, strings.Join(mismatches, "; "))
	if backendParserProbeStrict() {
		return fmt.Errorf("%s. raw_entry=%s", msg, formatRawEntryForLog(raw))
	}
	log.Printf("WARNING: %s. raw_entry=%s", msg, formatRawEntryForLog(raw))
	return nil
}

// backendParserProbeStrict reports whether strict backend probe checks are enabled.
func backendParserProbeStrict() bool {
	raw := strings.TrimSpace(strings.ToLower(os.Getenv("E2E_HTTPREQUEST_BACKEND_PROBE_STRICT")))
	switch raw {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// formatRawEntryForLog pretty-prints raw entries for diagnostics.
func formatRawEntryForLog(raw map[string]any) string {
	if len(raw) == 0 {
		return "{}"
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return fmt.Sprintf("%v", raw)
	}
	const maxLen = 2000
	if len(b) > maxLen {
		return string(b[:maxLen]) + "...(truncated)"
	}
	return string(b)
}

// testLogName verifies log entries land in the expected Cloud Logging stdout log.
func (s *CoreLoggingTestSuite) testLogName(ctx context.Context) error {
	if strings.TrimSpace(s.expectedLogID) == "" {
		log.Println("Skipping log name validation because no expectation was provided")
		return nil
	}

	testID := fmt.Sprintf("%s-logid-%d", s.testRunID, time.Now().UnixNano())
	req := client.LogRequest{
		Message: "Log ID validation",
		TestID:  testID,
	}

	if _, err := s.targetClient.LogWithSeverity(ctx, "info", req); err != nil {
		return fmt.Errorf("failed to send log: %w", err)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-1 * time.Minute),
		MaxResults: 10,
	}, logWaitTimeout)
	if err != nil {
		return fmt.Errorf("failed to find log: %w", err)
	}

	if len(entries) == 0 {
		return fmt.Errorf("no log entries found for log ID validation")
	}

	expectedName := fmt.Sprintf("projects/%s/logs/%s", s.projectID, url.PathEscape(s.expectedLogID))
	result := s.validator.ValidateLogName(entries[0], expectedName)
	if !result.Valid {
		return fmt.Errorf("log name validation failed: %s", result.Message)
	}

	log.Printf("Log name validated successfully (expected %s)", expectedName)
	return nil
}

// testBatchLogging verifies multiple logs are created correctly
func (s *CoreLoggingTestSuite) testBatchLogging(ctx context.Context) error {
	log.Println("Testing batch logging")

	testID := fmt.Sprintf("%s-batch-%d", s.testRunID, time.Now().UnixNano())

	req := client.LogRequest{
		Message: "Batch logging test",
		TestID:  testID,
		Attributes: map[string]any{
			"batch_size": 5,
		},
	}

	_, err := s.targetClient.LogBatch(ctx, req)
	if err != nil {
		return fmt.Errorf("failed to send batch logs: %w", err)
	}

	// Query logs - should find multiple entries
	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-1 * time.Minute),
		MaxResults: 10,
	}, logWaitTimeout)

	if err != nil {
		return fmt.Errorf("failed to find batch logs: %w", err)
	}

	// We expect at least 5 logs from the batch
	if len(entries) < 5 {
		return fmt.Errorf("expected at least 5 logs from batch, got %d", len(entries))
	}

	// Validate each has proper structure
	for i, entry := range entries {
		result := s.validator.ValidateTimestamp(entry, 5*time.Minute)
		if !result.Valid {
			return fmt.Errorf("batch log %d timestamp validation failed: %s", i, result.Message)
		}
	}

	log.Printf("Batch logging validated successfully (found %d logs)", len(entries))
	return nil
}
