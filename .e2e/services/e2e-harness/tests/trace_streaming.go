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
	"strconv"
	"time"

	"github.com/pjscruggs/slogcp-e2e-internal/services/e2e-harness/client"
)

// GetStreamingTests exposes the streaming-specific trace scenarios.
func (s *TraceTestSuite) GetStreamingTests() []TestCase {
	return []TestCase{
		{
			Name:        "TraceDownstreamGRPCServerStream",
			Description: "Ensure the trace target coordinates downstream gRPC server streams",
			Execute:     s.testDownstreamGRPCServerStream,
		},
		{
			Name:        "TraceDownstreamGRPCClientStream",
			Description: "Ensure the trace target coordinates downstream gRPC client streams",
			Execute:     s.testDownstreamGRPCClientStream,
		},
		{
			Name:        "TraceDownstreamGRPCBidiStream",
			Description: "Ensure the trace target coordinates downstream gRPC bidirectional streams",
			Execute:     s.testDownstreamGRPCBidiStream,
		},
	}
}

// testDownstreamGRPCServerStream verifies downstream server-stream trace behavior.
func (s *TraceTestSuite) testDownstreamGRPCServerStream(ctx context.Context) error {
	const (
		responseCount   = 3
		responsePayload = "server-stream-payload"
	)

	testID := fmt.Sprintf("%s-grpc-server-stream-%d", s.testRunID, time.Now().UnixNano())
	req := client.TraceServerStreamRequest{
		TraceRequest: client.TraceRequest{
			TestID:  testID,
			Message: "trace downstream grpc server stream",
		},
		ResponseCount:   responseCount,
		ResponsePayload: responsePayload,
	}

	resp, err := s.targetClient.TraceDownstreamGRPCServerStream(ctx, req)
	if err != nil {
		return fmt.Errorf("server stream trace request failed: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("server stream trace failed: %s", resp.Message)
	}

	if resp.Details == nil {
		return fmt.Errorf("server stream trace missing response details")
	}
	if got := resp.Details["response_count"]; got != strconv.Itoa(responseCount) {
		return fmt.Errorf("unexpected response_count detail: got %s want %d", got, responseCount)
	}
	if got := resp.Details["last_payload"]; got != responsePayload {
		return fmt.Errorf("unexpected last_payload detail: got %s want %s", got, responsePayload)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.waitForLogsFromServices(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-2 * time.Minute),
		MaxResults: 20,
	}, []string{"trace-target-app"})
	if err != nil {
		return fmt.Errorf("waiting for server stream logs: %w", err)
	}

	if err := s.validateTraceFields(entries, resp.TraceID); err != nil {
		return fmt.Errorf("server stream trace field validation failed: %w", err)
	}

	if err := s.expectLogDetails(entries, "trace-target-app", map[string]any{
		"message":   "Completed downstream gRPC server stream",
		"responses": float64(responseCount),
	}); err != nil {
		return err
	}

	expectedSpans := []string{
		"trace.downstream.grpc.stream_server",
		"downstream.grpc.stream_server.call",
	}
	if _, err := s.traceClient.WaitForSpans(ctx, resp.TraceID, expectedSpans, traceWaitTimeout); err != nil {
		return fmt.Errorf("waiting for server stream spans: %w", err)
	}

	log.Printf("Trace downstream gRPC server stream validated (responses=%d)", responseCount)
	return nil
}

// testDownstreamGRPCClientStream verifies downstream client-stream trace behavior.
func (s *TraceTestSuite) testDownstreamGRPCClientStream(ctx context.Context) error {
	chunks := []client.TraceClientStreamChunk{
		{Payload: "alpha"},
		{Payload: "beta"},
		{Payload: "gamma"},
	}
	concatenated := "alphabetagamma"

	testID := fmt.Sprintf("%s-grpc-client-stream-%d", s.testRunID, time.Now().UnixNano())
	req := client.TraceClientStreamRequest{
		TraceRequest: client.TraceRequest{
			TestID:  testID,
			Message: "trace downstream grpc client stream",
		},
		Chunks: chunks,
	}

	resp, err := s.targetClient.TraceDownstreamGRPCClientStream(ctx, req)
	if err != nil {
		return fmt.Errorf("client stream trace request failed: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("client stream trace failed: %s", resp.Message)
	}

	if resp.Details == nil {
		return fmt.Errorf("client stream trace missing response details")
	}
	if got := resp.Details["received_chunks"]; got != strconv.Itoa(len(chunks)) {
		return fmt.Errorf("unexpected received_chunks detail: got %s want %d", got, len(chunks))
	}
	if got := resp.Details["concatenated"]; got != concatenated {
		return fmt.Errorf("unexpected concatenated detail: got %s want %s", got, concatenated)
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.waitForLogsFromServices(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-2 * time.Minute),
		MaxResults: 20,
	}, []string{"trace-target-app"})
	if err != nil {
		return fmt.Errorf("waiting for client stream logs: %w", err)
	}

	if err := s.validateTraceFields(entries, resp.TraceID); err != nil {
		return fmt.Errorf("client stream trace field validation failed: %w", err)
	}

	if err := s.expectLogDetails(entries, "trace-target-app", map[string]any{
		"message":         "Completed downstream gRPC client stream",
		"received_chunks": float64(len(chunks)),
	}); err != nil {
		return err
	}

	expectedSpans := []string{
		"trace.downstream.grpc.stream_client",
		"downstream.grpc.stream_client.call",
	}
	if _, err := s.traceClient.WaitForSpans(ctx, resp.TraceID, expectedSpans, traceWaitTimeout); err != nil {
		return fmt.Errorf("waiting for client stream spans: %w", err)
	}

	log.Printf("Trace downstream gRPC client stream validated (chunks=%d)", len(chunks))
	return nil
}

// testDownstreamGRPCBidiStream verifies downstream bidi-stream trace behavior.
func (s *TraceTestSuite) testDownstreamGRPCBidiStream(ctx context.Context) error {
	messages := []client.TraceBidiStreamMessage{
		{Payload: "one"},
		{Payload: "two", CloseAfter: true},
	}

	testID := fmt.Sprintf("%s-grpc-bidi-stream-%d", s.testRunID, time.Now().UnixNano())
	req := client.TraceBidiStreamRequest{
		TraceRequest: client.TraceRequest{
			TestID:  testID,
			Message: "trace downstream grpc bidi stream",
		},
		Messages: messages,
	}

	resp, err := s.targetClient.TraceDownstreamGRPCBidiStream(ctx, req)
	if err != nil {
		return fmt.Errorf("bidi stream trace request failed: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("bidi stream trace failed: %s", resp.Message)
	}

	if err := validateBidiResponseDetails(resp.Details, len(messages)); err != nil {
		return err
	}

	filter := fmt.Sprintf(`jsonPayload.test_id="%s"`, testID)
	entries, err := s.waitForLogsFromServices(ctx, client.QueryOptions{
		Filter:     filter,
		StartTime:  time.Now().Add(-2 * time.Minute),
		MaxResults: 20,
	}, []string{"trace-target-app"})
	if err != nil {
		return fmt.Errorf("waiting for bidi stream logs: %w", err)
	}

	if err := s.validateTraceFields(entries, resp.TraceID); err != nil {
		return fmt.Errorf("bidi stream trace field validation failed: %w", err)
	}

	if err := s.expectLogDetails(entries, "trace-target-app", map[string]any{
		"message":        "Completed downstream gRPC bidi stream",
		"response_count": float64(len(messages)),
	}); err != nil {
		return err
	}

	expectedSpans := []string{
		"trace.downstream.grpc.stream_bidi",
		"downstream.grpc.stream_bidi.call",
	}
	if _, err := s.traceClient.WaitForSpans(ctx, resp.TraceID, expectedSpans, traceWaitTimeout); err != nil {
		return fmt.Errorf("waiting for bidi stream spans: %w", err)
	}

	log.Printf("Trace downstream gRPC bidi stream validated (messages=%d)", len(messages))
	return nil
}

// validateBidiResponseDetails validates expected response metadata from bidi stream endpoint.
func validateBidiResponseDetails(details map[string]string, expectedCount int) error {
	if details == nil {
		return fmt.Errorf("bidi stream trace missing response details")
	}
	if got := details["response_count"]; got != strconv.Itoa(expectedCount) {
		return fmt.Errorf("unexpected response_count detail: got %s want %d", got, expectedCount)
	}
	if got := details["last_payload"]; got != "two" {
		return fmt.Errorf("unexpected last_payload detail: got %s want two", got)
	}
	if got := details["last_chunk_id"]; got != "chunk-2" {
		return fmt.Errorf("unexpected last_chunk_id detail: got %s want chunk-2", got)
	}
	return nil
}

// expectLogDetails validates selected key/value pairs in service logs.
func (s *TraceTestSuite) expectLogDetails(entries []*client.LogEntry, service string, expected map[string]any) error {
	for _, entry := range entries {
		if entry == nil || entry.Labels == nil || entry.Labels["app"] != service {
			continue
		}
		payload := s.validator.ExtractPayloadMap(entry)
		if payload == nil {
			continue
		}

		if payloadMatchesExpected(payload, expected) {
			return nil
		}
	}

	return fmt.Errorf("expected log from service %s with details %v not found", service, expected)
}

// payloadMatchesExpected reports whether payload contains all expected key/value pairs.
func payloadMatchesExpected(payload map[string]any, expected map[string]any) bool {
	for key, want := range expected {
		got, ok := payload[key]
		if !ok {
			return false
		}
		if !payloadValueMatches(got, want) {
			return false
		}
	}
	return true
}

// payloadValueMatches compares dynamic payload values with expected test values.
func payloadValueMatches(got, want any) bool {
	switch wantTyped := want.(type) {
	case string:
		return fmt.Sprintf("%v", got) == wantTyped
	case float64:
		numeric, ok := got.(float64)
		return ok && numeric == wantTyped
	default:
		return fmt.Sprintf("%v", got) == fmt.Sprintf("%v", wantTyped)
	}
}
