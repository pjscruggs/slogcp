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
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/pjscruggs/slogcp-e2e-internal/services/e2e-harness/client"
	"github.com/pjscruggs/slogcp-e2e-internal/services/e2e-harness/validation"

	"github.com/google/uuid"
)

const (
	traceWaitTimeout        = 180 * time.Second
	traceStartupLogLookback = 15 * time.Minute
)

// TraceTestSuite contains trace propagation scenarios.
type TraceTestSuite struct {
	targetClient           *client.TargetClient
	loggingClient          *client.LoggingClient
	traceClient            *client.TraceClient
	validator              *validation.LogValidator
	projectID              string
	testRunID              string
	scenarioName           string
	serviceName            string
	expectedDownstreamHTTP string
	expectedDownstreamGRPC string
	expectedTraceProject   string
	disableHTTPPropagation bool
	disableGRPCPropagation bool
	defaultTraceSampled    bool
}

// NewTraceTestSuite constructs the trace suite.
func NewTraceTestSuite(targetClient *client.TargetClient, loggingClient *client.LoggingClient, traceClient *client.TraceClient, projectID string, cfg TraceScenarioConfig) *TraceTestSuite {
	expectedProject := cfg.ExpectedTraceProjectID
	if expectedProject == "" {
		expectedProject = projectID
	}

	disableHTTPPropagation := false
	if cfg.DisableHTTPTracePropagation != nil {
		disableHTTPPropagation = *cfg.DisableHTTPTracePropagation
	}

	disableGRPCPropagation := false
	if cfg.DisableGRPCTracePropagation != nil {
		disableGRPCPropagation = *cfg.DisableGRPCTracePropagation
	}

	defaultTraceSampled := true
	if cfg.DefaultTraceSampled != nil {
		defaultTraceSampled = *cfg.DefaultTraceSampled
	}

	return &TraceTestSuite{
		targetClient:           targetClient,
		loggingClient:          loggingClient,
		traceClient:            traceClient,
		validator:              validation.NewLogValidator(projectID),
		projectID:              projectID,
		testRunID:              uuid.New().String(),
		scenarioName:           cfg.ScenarioName,
		serviceName:            cfg.ServiceName,
		expectedDownstreamHTTP: cfg.ExpectedDownstreamHTTP,
		expectedDownstreamGRPC: cfg.ExpectedDownstreamGRPC,
		expectedTraceProject:   expectedProject,
		disableHTTPPropagation: disableHTTPPropagation,
		disableGRPCPropagation: disableGRPCPropagation,
		defaultTraceSampled:    defaultTraceSampled,
	}
}

// TraceScenarioConfig captures scenario expectations for the trace suite.
type TraceScenarioConfig struct {
	ScenarioName                string
	ServiceName                 string
	ExpectedDownstreamHTTP      string
	ExpectedDownstreamGRPC      string
	ExpectedTraceProjectID      string
	DisableHTTPTracePropagation *bool
	DisableGRPCTracePropagation *bool
	DefaultTraceSampled         *bool
}

// GetTests returns the trace scenarios.
func (s *TraceTestSuite) GetTests() []TestCase {
	tests := []TestCase{
		{
			Name:        "TraceStartupConfiguration",
			Description: "Validate startup configuration reflects downstream targets",
			Execute:     s.testStartupConfiguration,
		},
		{
			Name:        "TraceSimple",
			Description: "Verify trace IDs are recorded for simple trace endpoint",
			Execute:     s.testSimpleTrace,
		},
		{
			Name:        "TraceDownstreamHTTP",
			Description: "Ensure trace context flows to downstream HTTP service",
			Execute:     s.testDownstreamHTTP,
		},
		{
			Name:        "TraceDownstreamGRPC",
			Description: "Ensure trace context flows to downstream gRPC service",
			Execute:     s.testDownstreamGRPC,
		},
		{
			Name:        "TraceDownstreamPubSub",
			Description: "Ensure trace context flows to downstream Pub/Sub subscriber",
			Execute:     s.testDownstreamPubSub,
		},
		{
			Name:        "TraceGRPCAdapterLogging",
			Description: "Ensure go-grpc-middleware logging via slogcp adapter retains trace correlation",
			Execute:     s.testGRPCAdapterLogging,
		},
	}

	tests = append(tests, s.GetStreamingTests()...)
	return tests
}

// testSimpleTrace verifies baseline trace correlation for a single request.
func (s *TraceTestSuite) testSimpleTrace(ctx context.Context) error {
	testID := fmt.Sprintf("%s-simple-%d", s.testRunID, time.Now().UnixNano())
	req := client.TraceRequest{
		TestID:  testID,
		Message: "trace simple test",
	}

	resp, err := s.targetClient.TraceSimple(ctx, req)
	if err != nil {
		return fmt.Errorf("trace simple request failed: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("trace simple failed: %s", resp.Message)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-1 * time.Minute),
		MaxResults: 10,
	}, logWaitTimeout)
	if err != nil {
		return fmt.Errorf("waiting for logs: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no trace logs found for test_id %s", testID)
	}

	if err := s.validateTraceFields(entries, resp.TraceID); err != nil {
		return fmt.Errorf("trace field validation failed: %w", err)
	}

	if _, err := s.traceClient.WaitForSpans(ctx, resp.TraceID, []string{"trace.simple"}, traceWaitTimeout); err != nil {
		return fmt.Errorf("waiting for trace spans: %w", err)
	}

	return nil
}

// testDownstreamHTTP verifies trace propagation through downstream HTTP calls.
func (s *TraceTestSuite) testDownstreamHTTP(ctx context.Context) error {
	testID := fmt.Sprintf("%s-http-%d", s.testRunID, time.Now().UnixNano())
	req := client.TraceRequest{
		TestID:  testID,
		Message: "trace downstream http",
	}

	resp, err := s.targetClient.TraceDownstreamHTTP(ctx, req)
	if err != nil {
		return fmt.Errorf("downstream http trace request failed: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("downstream http trace failed: %s", resp.Message)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.waitForLogsFromServices(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-2 * time.Minute),
		MaxResults: 20,
	}, []string{"trace-target-app", "trace-downstream-http"})
	if err != nil {
		return fmt.Errorf("waiting for downstream http logs: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no logs found for downstream http trace")
	}

	if err := s.validateLogsCoverServices(entries, []string{"trace-target-app", "trace-downstream-http"}); err != nil {
		return err
	}

	if err := s.validateTraceFields(entries, resp.TraceID); err != nil {
		return fmt.Errorf("trace field validation failed: %w", err)
	}

	if _, err := s.traceClient.WaitForSpans(ctx, resp.TraceID, []string{
		"trace.downstream.http",
		"downstream.http.call",
		"downstream.http.handler",
	}, traceWaitTimeout); err != nil {
		return fmt.Errorf("waiting for downstream http spans: %w", err)
	}

	return nil
}

// testDownstreamGRPC verifies trace propagation through downstream unary gRPC.
func (s *TraceTestSuite) testDownstreamGRPC(ctx context.Context) error {
	testID := fmt.Sprintf("%s-grpc-%d", s.testRunID, time.Now().UnixNano())
	req := client.TraceRequest{
		TestID:  testID,
		Message: "trace downstream grpc",
	}

	resp, err := s.targetClient.TraceDownstreamGRPC(ctx, req)
	if err != nil {
		return fmt.Errorf("downstream grpc trace request failed: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("downstream grpc trace failed: %s", resp.Message)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.waitForLogsFromServices(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-2 * time.Minute),
		MaxResults: 20,
	}, []string{"trace-target-app", "trace-downstream-grpc"})
	if err != nil {
		return fmt.Errorf("waiting for downstream grpc logs: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no logs found for downstream grpc trace")
	}

	if err := s.validateLogsCoverServices(entries, []string{"trace-target-app", "trace-downstream-grpc"}); err != nil {
		return err
	}

	if err := s.validateTraceFields(entries, resp.TraceID); err != nil {
		return fmt.Errorf("trace field validation failed: %w", err)
	}

	if _, err := s.traceClient.WaitForSpans(ctx, resp.TraceID, []string{
		"trace.downstream.grpc",
		"downstream.grpc.call",
		"downstream.grpc.handler",
	}, traceWaitTimeout); err != nil {
		return fmt.Errorf("waiting for downstream grpc spans: %w", err)
	}

	return nil
}

// testDownstreamPubSub verifies trace propagation through Pub/Sub publish/consume.
func (s *TraceTestSuite) testDownstreamPubSub(ctx context.Context) error {
	testID := fmt.Sprintf("%s-pubsub-%d", s.testRunID, time.Now().UnixNano())
	req := client.TraceRequest{
		TestID:  testID,
		Message: "trace downstream pubsub",
	}

	resp, err := s.targetClient.TraceDownstreamPubSub(ctx, req)
	if err != nil {
		return fmt.Errorf("downstream pubsub trace request failed: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("downstream pubsub trace failed: %s", resp.Message)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.waitForLogsFromServices(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-2 * time.Minute),
		MaxResults: 20,
	}, []string{"trace-target-app", "trace-downstream-http"})
	if err != nil {
		return fmt.Errorf("waiting for downstream pubsub logs: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no logs found for downstream pubsub trace")
	}

	if err := s.validateLogsCoverServices(entries, []string{"trace-target-app", "trace-downstream-http"}); err != nil {
		return err
	}

	if err := s.validateTraceFields(entries, resp.TraceID); err != nil {
		return fmt.Errorf("trace field validation failed: %w", err)
	}

	return nil
}

// testGRPCAdapterLogging validates grpc adapter log fields and severity mapping.
func (s *TraceTestSuite) testGRPCAdapterLogging(ctx context.Context) error {
	testID := fmt.Sprintf("%s-grpc-adapter-%d", s.testRunID, time.Now().UnixNano())
	req := client.TraceRequest{
		TestID:  testID,
		Message: "trace grpc adapter logging",
	}

	resp, err := s.targetClient.TraceDownstreamGRPC(ctx, req)
	if err != nil {
		return fmt.Errorf("grpc adapter trace request failed: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("grpc adapter trace failed: %s", resp.Message)
	}

	traceName := s.expectedTraceName(resp.TraceID)
	filter := fmt.Sprintf(
		`trace="%s" AND jsonPayload.message="finished call" AND jsonPayload."grpc.service"="tracegrpc.TraceWorker" AND jsonPayload."grpc.method"="DoWork"`,
		traceName,
	)
	entries, err := s.waitForLogsFromServices(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-2 * time.Minute),
		MaxResults: 20,
	}, []string{"trace-target-app", "trace-downstream-grpc"})
	if err != nil {
		return fmt.Errorf("waiting for grpc adapter logs: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no adapter interceptor logs found for trace %s", resp.TraceID)
	}

	if err := s.validateTraceFields(entries, resp.TraceID); err != nil {
		return fmt.Errorf("adapter interceptor trace field validation failed: %w", err)
	}

	componentSeen := collectGRPCComponents(entries, s.validator)

	if err := s.validateLogsCoverServices(entries, []string{"trace-target-app", "trace-downstream-grpc"}); err != nil {
		return err
	}

	if _, ok := componentSeen["client"]; !ok {
		return fmt.Errorf("missing client-side adapter logs for trace %s", resp.TraceID)
	}
	if _, ok := componentSeen["server"]; !ok {
		return fmt.Errorf("missing server-side adapter logs for trace %s", resp.TraceID)
	}

	return nil
}

// collectGRPCComponents extracts non-empty grpc.component values from entries.
func collectGRPCComponents(entries []*client.LogEntry, validator *validation.LogValidator) map[string]struct{} {
	componentSeen := map[string]struct{}{}
	for _, entry := range entries {
		payload := validator.ExtractPayloadMap(entry)
		if payload == nil {
			continue
		}
		component, ok := payload["grpc.component"].(string)
		if !ok {
			continue
		}
		component = strings.TrimSpace(component)
		if component != "" {
			componentSeen[component] = struct{}{}
		}
	}
	return componentSeen
}

// testStartupConfiguration validates startup metadata logs across services.
func (s *TraceTestSuite) testStartupConfiguration(ctx context.Context) error {
	if s.serviceName == "" {
		return fmt.Errorf("trace scenario %s missing service name", s.scenarioName)
	}

	filter := fmt.Sprintf(`resource.labels.service_name="%s" AND jsonPayload.message="trace-target-app starting"`, s.serviceName)
	entries, err := s.loggingClient.WaitForLogs(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-traceStartupLogLookback),
		MaxResults: 5,
	}, logWaitTimeout)
	if err != nil {
		return fmt.Errorf("waiting for trace startup log: %w", err)
	}
	if len(entries) == 0 {
		return fmt.Errorf("no startup logs found for trace service %s", s.serviceName)
	}

	payload := s.validator.ExtractPayloadMap(entries[0])
	httpValue, grpcValue, err := s.validateStartupDownstreamTargets(payload)
	if err != nil {
		return err
	}

	if err := s.expectStartupBoolean(payload, "disable_http_trace_propagation", s.disableHTTPPropagation); err != nil {
		return err
	}
	if err := s.expectStartupBoolean(payload, "disable_grpc_trace_propagation", s.disableGRPCPropagation); err != nil {
		return err
	}
	if err := s.expectStartupBoolean(payload, "default_trace_sampled", s.defaultTraceSampled); err != nil {
		return err
	}

	log.Printf("Trace startup configuration validated (HTTP=%s, gRPC=%s)", httpValue, grpcValue)
	return nil
}

// validateStartupDownstreamTargets checks startup downstream endpoint fields.
func (s *TraceTestSuite) validateStartupDownstreamTargets(payload map[string]any) (string, string, error) {
	httpValue, ok := payload["downstream_http"].(string)
	if !ok {
		return "", "", fmt.Errorf("startup log missing downstream_http field (payload: %v)", payload)
	}
	if httpValue != s.expectedDownstreamHTTP {
		return "", "", fmt.Errorf("unexpected downstream http url, got %s want %s", httpValue, s.expectedDownstreamHTTP)
	}

	grpcValue, ok := payload["downstream_grpc"].(string)
	if !ok {
		return "", "", fmt.Errorf("startup log missing downstream_grpc field (payload: %v)", payload)
	}
	if grpcValue != s.expectedDownstreamGRPC {
		return "", "", fmt.Errorf("unexpected downstream grpc target, got %s want %s", grpcValue, s.expectedDownstreamGRPC)
	}

	return httpValue, grpcValue, nil
}

// validateTraceField validates one entry's trace field against traceID.
func (s *TraceTestSuite) validateTraceField(entry *client.LogEntry, traceID string) error {
	expected := s.expectedTraceName(traceID)
	if entry.Trace == "" {
		return fmt.Errorf("log entry missing trace field")
	}
	if entry.Trace != expected {
		service := ""
		if entry.Labels != nil {
			service = entry.Labels["app"]
		}
		return fmt.Errorf("unexpected trace field for service %s, got %s want %s", service, entry.Trace, expected)
	}
	return nil
}

// validateTraceFields validates trace fields for a set of entries.
func (s *TraceTestSuite) validateTraceFields(entries []*client.LogEntry, traceID string) error {
	for _, entry := range entries {
		if err := s.validateTraceField(entry, traceID); err != nil {
			return err
		}
	}
	return nil
}

// expectedTraceName builds the full Cloud Trace resource name.
func (s *TraceTestSuite) expectedTraceName(traceID string) string {
	return fmt.Sprintf("projects/%s/traces/%s", s.expectedTraceProject, traceID)
}

// validateLogsCoverServices ensures logs include every expected service.
func (s *TraceTestSuite) validateLogsCoverServices(entries []*client.LogEntry, services []string) error {
	found := make(map[string]struct{})
	for _, entry := range entries {
		for _, svc := range services {
			if entry.Labels != nil && entry.Labels["app"] == svc {
				found[svc] = struct{}{}
			}
		}
	}
	if len(found) != len(services) {
		return fmt.Errorf("expected logs from services %v, found %v", services, found)
	}
	return nil
}

// expectStartupBoolean validates one boolean startup field value.
func (s *TraceTestSuite) expectStartupBoolean(payload map[string]any, field string, want bool) error {
	value, ok := payload[field]
	if !ok {
		return fmt.Errorf("startup log missing %s field (payload: %v)", field, payload)
	}
	boolValue, ok := value.(bool)
	if !ok {
		return fmt.Errorf("startup log field %s has unexpected type %T (payload: %v)", field, value, payload)
	}
	if boolValue != want {
		return fmt.Errorf("unexpected %s value, got %t want %t", field, boolValue, want)
	}
	return nil
}

// waitForLogsFromServices polls until logs contain all expected services.
func (s *TraceTestSuite) waitForLogsFromServices(ctx context.Context, opts client.QueryOptions, services []string) ([]*client.LogEntry, error) {
	deadline := time.Now().Add(logWaitTimeout)
	expected := make(map[string]struct{}, len(services))
	for _, svc := range services {
		expected[svc] = struct{}{}
	}

	found := make(map[string]struct{})
	seenEntries := make(map[string]struct{})
	var aggregated []*client.LogEntry

	backoff := 1 * time.Second
	maxBackoff := 10 * time.Second

	for time.Now().Before(deadline) {
		entries, err := s.loggingClient.QueryLogs(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("querying logs: %w", err)
		}

		aggregated = appendUniqueEntries(aggregated, entries, seenEntries)
		markFoundServices(found, expected, entries)

		if len(found) == len(expected) {
			return aggregated, nil
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for service logs canceled: %w", ctx.Err())
		case <-time.After(backoff):
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}

	return nil, fmt.Errorf("timeout waiting for logs from services %v, found %v", services, found)
}

// appendUniqueEntries appends entries that have not been seen before.
func appendUniqueEntries(aggregated, entries []*client.LogEntry, seenEntries map[string]struct{}) []*client.LogEntry {
	for _, entry := range entries {
		key := logEntryKey(entry)
		if _, seen := seenEntries[key]; seen {
			continue
		}
		aggregated = append(aggregated, entry)
		seenEntries[key] = struct{}{}
	}
	return aggregated
}

// markFoundServices records expected services observed in entry labels.
func markFoundServices(found, expected map[string]struct{}, entries []*client.LogEntry) {
	for _, entry := range entries {
		if entry.Labels == nil {
			continue
		}
		svc := entry.Labels["app"]
		if svc == "" {
			continue
		}
		if _, ok := expected[svc]; ok {
			found[svc] = struct{}{}
		}
	}
}

// logEntryKey returns a deterministic key for de-duplicating fetched entries.
func logEntryKey(entry *client.LogEntry) string {
	if entry.InsertID != "" {
		return entry.InsertID
	}
	return fmt.Sprintf("%p-%s-%s", entry, entry.LogName, entry.Timestamp.Format(time.RFC3339Nano))
}
