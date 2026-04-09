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

package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2"
	traceapi "cloud.google.com/go/trace/apiv2"
	tracepb "cloud.google.com/go/trace/apiv2/tracepb"
	localtracepb "github.com/pjscruggs/slogcp-e2e-internal/services/traceproto"
	"github.com/pjscruggs/slogcp/slogcppubsub"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// DownstreamInvoker defines the interface the trace handler uses to reach
// downstream services. It allows the HTTP and gRPC integrations to be mocked
// during unit tests if desired.
type DownstreamInvoker interface {
	CallDownstreamHTTP(ctx context.Context, req *DownstreamHTTPRequest) (*DownstreamHTTPResponse, error)
	CallDownstreamGRPC(ctx context.Context, req *DownstreamGRPCRequest) (*localtracepb.TraceWorkResponse, error)
	CallDownstreamGRPCServerStream(ctx context.Context, req *DownstreamGRPCServerStreamRequest) ([]*localtracepb.ServerStreamResponse, error)
	CallDownstreamGRPCClientStream(ctx context.Context, req *DownstreamGRPCClientStreamRequest) (*localtracepb.ClientStreamResponse, error)
	CallDownstreamGRPCBidiStream(ctx context.Context, req *DownstreamGRPCBidiStreamRequest) ([]*localtracepb.BidiStreamResponse, error)
}

// TraceRequest is the request payload for trace endpoints.
type TraceRequest struct {
	TestID  string         `json:"test_id"`
	Message string         `json:"message"`
	Options map[string]any `json:"options,omitempty"`
}

// TraceResponse is the response payload returned by trace endpoints.
type TraceResponse struct {
	Success bool              `json:"success"`
	Message string            `json:"message"`
	TraceID string            `json:"trace_id,omitempty"`
	SpanID  string            `json:"spanId,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

type pubSubWorkRequest struct {
	TestID  string `json:"test_id"`
	Message string `json:"message"`
}

type serverStreamTraceRequest struct {
	TraceRequest    *TraceRequest `json:"trace_request"`
	ResponseCount   int           `json:"response_count"`
	ResponsePayload string        `json:"response_payload"`
	FinalStatus     string        `json:"final_status"`
	FinalMessage    string        `json:"final_message"`
}

type clientStreamTraceRequest struct {
	TraceRequest *TraceRequest       `json:"trace_request"`
	Chunks       []clientStreamChunk `json:"chunks"`
}

type clientStreamChunk struct {
	TestID       string `json:"test_id,omitempty"`
	ChunkID      string `json:"chunk_id,omitempty"`
	Payload      string `json:"payload,omitempty"`
	FinalStatus  string `json:"final_status,omitempty"`
	FinalMessage string `json:"final_message,omitempty"`
}

type bidiStreamTraceRequest struct {
	TraceRequest *TraceRequest     `json:"trace_request"`
	Messages     []bidiStreamChunk `json:"messages"`
}

type bidiStreamChunk struct {
	TestID       string `json:"test_id,omitempty"`
	ChunkID      string `json:"chunk_id,omitempty"`
	Payload      string `json:"payload,omitempty"`
	CloseAfter   bool   `json:"close_after,omitempty"`
	FinalStatus  string `json:"final_status,omitempty"`
	FinalMessage string `json:"final_message,omitempty"`
}

// TraceHandler owns the end-to-end trace test endpoints.
type TraceHandler struct {
	logger        *slog.Logger
	traceClient   *traceapi.Client
	projectID     string
	invoker       DownstreamInvoker
	defaultSample bool
	pubsubTopic   *pubsub.Publisher
}

// NewTraceHandler builds a TraceHandler.
func NewTraceHandler(logger *slog.Logger, traceClient *traceapi.Client, projectID string, invoker DownstreamInvoker, pubsubTopic *pubsub.Publisher) *TraceHandler {
	return &TraceHandler{
		logger:        logger,
		traceClient:   traceClient,
		projectID:     projectID,
		invoker:       invoker,
		defaultSample: true,
		pubsubTopic:   pubsubTopic,
	}
}

// SetDefaultSample toggles whether outgoing trace headers default to sampled.
func (h *TraceHandler) SetDefaultSample(sample bool) {
	if h == nil {
		return
	}
	h.defaultSample = sample
}

// TraceSimple handles the /trace/simple endpoint. It records a span for the
// inbound request and returns the identifiers used so tests can look them up.
func (h *TraceHandler) TraceSimple(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	traceReq, err := decodeTraceRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		http.Error(w, "missing or invalid trace context", http.StatusBadRequest)
		return
	}

	span := newSpanBuilder(spanCtx.TraceID().String(), spanCtx.SpanID().String(), "trace.simple", map[string]string{
		"test_id":  traceReq.TestID,
		"endpoint": "/trace/simple",
	})
	if err := h.writeSpan(ctx, span); err != nil {
		h.logger.ErrorContext(ctx, "failed to write span", "error", err)
		http.Error(w, "failed to write span", http.StatusInternalServerError)
		return
	}

	h.logger.InfoContext(ctx, "Handled trace simple request", "test_id", traceReq.TestID, "trace_id", spanCtx.TraceID().String(), "span_id", span.spanID)

	writeTraceResponse(w, &TraceResponse{
		Success: true,
		Message: "simple trace recorded",
		TraceID: spanCtx.TraceID().String(),
		SpanID:  span.spanID,
	})
}

// TraceDownstreamHTTP creates a span, calls the downstream HTTP service (which
// should log the same trace context), and records a child span for that call.
func (h *TraceHandler) TraceDownstreamHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	traceReq, err := decodeTraceRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		http.Error(w, "missing or invalid trace context", http.StatusBadRequest)
		return
	}

	parentSpan := newSpanBuilder(spanCtx.TraceID().String(), spanCtx.SpanID().String(), "trace.downstream.http", map[string]string{
		"test_id":  traceReq.TestID,
		"endpoint": "/trace/downstream-http",
	})

	// Start child span for the outbound HTTP request before invoking downstream.
	outgoingSpan := newSpanBuilder(spanCtx.TraceID().String(), parentSpan.spanID, "downstream.http.call", map[string]string{
		"test_id": traceReq.TestID,
	})

	resp, callErr := h.invoker.CallDownstreamHTTP(ctx, &DownstreamHTTPRequest{
		TestID:  traceReq.TestID,
		Message: traceReq.Message,
	})

	// Finish spans after downstream interaction.
	if writeErr := h.writeSpan(ctx, outgoingSpan); writeErr != nil {
		h.logger.ErrorContext(ctx, "failed to write downstream HTTP span", "error", writeErr)
	}
	if writeErr := h.writeSpan(ctx, parentSpan); writeErr != nil {
		h.logger.ErrorContext(ctx, "failed to write parent span", "error", writeErr)
	}

	if callErr != nil {
		h.logger.ErrorContext(ctx, "downstream HTTP call failed", "error", callErr)
		http.Error(w, "downstream HTTP call failed", http.StatusBadGateway)
		return
	}

	h.logger.InfoContext(ctx, "Completed downstream HTTP trace", "test_id", traceReq.TestID, "trace_id", spanCtx.TraceID().String(), "span_id", parentSpan.spanID, "downstream_span_id", outgoingSpan.spanID)

	writeTraceResponse(w, &TraceResponse{
		Success: true,
		Message: resp.Acknowledgement,
		TraceID: spanCtx.TraceID().String(),
		SpanID:  parentSpan.spanID,
		Details: map[string]string{
			"downstream_span_id": outgoingSpan.spanID,
		},
	})
}

// TraceDownstreamGRPC exercises the gRPC downstream worker.
func (h *TraceHandler) TraceDownstreamGRPC(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	traceReq, err := decodeTraceRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		http.Error(w, "missing or invalid trace context", http.StatusBadRequest)
		return
	}

	parentSpan := newSpanBuilder(spanCtx.TraceID().String(), spanCtx.SpanID().String(), "trace.downstream.grpc", map[string]string{
		"test_id":  traceReq.TestID,
		"endpoint": "/trace/downstream-grpc",
	})

	outgoingSpan := newSpanBuilder(spanCtx.TraceID().String(), parentSpan.spanID, "downstream.grpc.call", map[string]string{
		"test_id": traceReq.TestID,
	})

	resp, callErr := h.invoker.CallDownstreamGRPC(ctx, &DownstreamGRPCRequest{
		Payload: &localtracepb.TraceWorkRequest{
			TestId:  traceReq.TestID,
			Message: traceReq.Message,
		},
	})

	if writeErr := h.writeSpan(ctx, outgoingSpan); writeErr != nil {
		h.logger.ErrorContext(ctx, "failed to write downstream gRPC span", "error", writeErr)
	}
	if writeErr := h.writeSpan(ctx, parentSpan); writeErr != nil {
		h.logger.ErrorContext(ctx, "failed to write parent span", "error", writeErr)
	}

	if callErr != nil {
		h.logger.ErrorContext(ctx, "downstream gRPC call failed", "error", callErr)
		http.Error(w, "downstream gRPC call failed", http.StatusBadGateway)
		return
	}

	h.logger.InfoContext(ctx, "Completed downstream gRPC trace", "test_id", traceReq.TestID, "trace_id", spanCtx.TraceID().String(), "span_id", parentSpan.spanID, "downstream_span_id", outgoingSpan.spanID)

	writeTraceResponse(w, &TraceResponse{
		Success: true,
		Message: resp.GetAcknowledgement(),
		TraceID: spanCtx.TraceID().String(),
		SpanID:  parentSpan.spanID,
		Details: map[string]string{
			"downstream_span_id": outgoingSpan.spanID,
		},
	})
}

// TraceDownstreamPubSub publishes a message and relies on the subscriber to log with the same trace context.
func (h *TraceHandler) TraceDownstreamPubSub(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	traceReq, err := decodeTraceRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.pubsubTopic == nil {
		http.Error(w, "pubsub publisher not configured", http.StatusServiceUnavailable)
		return
	}

	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		http.Error(w, "missing or invalid trace context", http.StatusBadRequest)
		return
	}

	payload, err := json.Marshal(pubSubWorkRequest{
		TestID:  traceReq.TestID,
		Message: traceReq.Message,
	})
	if err != nil {
		http.Error(w, "failed to encode pubsub payload", http.StatusInternalServerError)
		return
	}

	msg := &pubsub.Message{
		Data: payload,
		Attributes: map[string]string{
			"test_id": traceReq.TestID,
		},
	}
	slogcppubsub.Inject(ctx, msg)

	result := h.pubsubTopic.Publish(ctx, msg)
	msgID, err := result.Get(ctx)
	if err != nil {
		h.logger.ErrorContext(ctx, "pubsub publish failed", "error", err, "test_id", traceReq.TestID)
		http.Error(w, "pubsub publish failed", http.StatusBadGateway)
		return
	}

	h.logger.InfoContext(ctx, "Published pubsub message",
		"test_id", traceReq.TestID,
		"trace_id", spanCtx.TraceID().String(),
		"pubsub_message_id", msgID,
	)

	writeTraceResponse(w, &TraceResponse{
		Success: true,
		Message: "downstream pubsub published",
		TraceID: spanCtx.TraceID().String(),
		SpanID:  spanCtx.SpanID().String(),
		Details: map[string]string{
			"pubsub_message_id": msgID,
		},
	})
}

// TraceDownstreamGRPCServerStream exercises the downstream server-streaming RPC.
func (h *TraceHandler) TraceDownstreamGRPCServerStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	payload, err := decodeServerStreamTraceRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	traceReq := payload.TraceRequest

	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		http.Error(w, "missing or invalid trace context", http.StatusBadRequest)
		return
	}

	parentSpan := newSpanBuilder(spanCtx.TraceID().String(), spanCtx.SpanID().String(), "trace.downstream.grpc.stream_server", map[string]string{
		"test_id":  traceReq.TestID,
		"endpoint": "/trace/downstream-grpc/server-stream",
	})
	outgoingSpan := newSpanBuilder(spanCtx.TraceID().String(), parentSpan.spanID, "downstream.grpc.stream_server.call", map[string]string{
		"test_id": traceReq.TestID,
	})

	responses, callErr := h.invoker.CallDownstreamGRPCServerStream(ctx, &DownstreamGRPCServerStreamRequest{
		Payload: &localtracepb.ServerStreamRequest{
			TestId:          traceReq.TestID,
			ResponseCount:   int32FromInt(payload.ResponseCount),
			ResponsePayload: payload.ResponsePayload,
			FinalStatus:     payload.FinalStatus,
			FinalMessage:    payload.FinalMessage,
		},
	})

	if writeErr := h.writeSpan(ctx, outgoingSpan); writeErr != nil {
		h.logger.ErrorContext(ctx, "failed to write downstream gRPC server stream span", "error", writeErr)
	}
	if writeErr := h.writeSpan(ctx, parentSpan); writeErr != nil {
		h.logger.ErrorContext(ctx, "failed to write parent span", "error", writeErr)
	}

	if callErr != nil {
		h.logger.ErrorContext(ctx, "downstream gRPC server stream failed", "error", callErr)
		http.Error(w, "downstream gRPC server stream failed", http.StatusBadGateway)
		return
	}

	details := map[string]string{
		"downstream_span_id": outgoingSpan.spanID,
		"response_count":     strconv.Itoa(len(responses)),
	}
	if len(responses) > 0 {
		last := responses[len(responses)-1]
		if seq := last.GetSequence(); seq > 0 {
			details["last_sequence"] = strconv.Itoa(int(seq))
		}
		if payload := last.GetPayload(); payload != "" {
			details["last_payload"] = payload
		}
	}

	h.logger.InfoContext(ctx, "Completed downstream gRPC server stream", "test_id", traceReq.TestID, "trace_id", spanCtx.TraceID().String(), "span_id", parentSpan.spanID, "downstream_span_id", outgoingSpan.spanID, "responses", len(responses))

	writeTraceResponse(w, &TraceResponse{
		Success: true,
		Message: "downstream gRPC server stream completed",
		TraceID: spanCtx.TraceID().String(),
		SpanID:  parentSpan.spanID,
		Details: details,
	})
}

// TraceDownstreamGRPCClientStream exercises the downstream client-streaming RPC.
func (h *TraceHandler) TraceDownstreamGRPCClientStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	payload, err := decodeClientStreamTraceRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	traceReq := payload.TraceRequest

	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		http.Error(w, "missing or invalid trace context", http.StatusBadRequest)
		return
	}

	parentSpan := newSpanBuilder(spanCtx.TraceID().String(), spanCtx.SpanID().String(), "trace.downstream.grpc.stream_client", map[string]string{
		"test_id":  traceReq.TestID,
		"endpoint": "/trace/downstream-grpc/client-stream",
	})
	outgoingSpan := newSpanBuilder(spanCtx.TraceID().String(), parentSpan.spanID, "downstream.grpc.stream_client.call", map[string]string{
		"test_id": traceReq.TestID,
	})

	chunks := make([]*localtracepb.ClientStreamRequest, 0, len(payload.Chunks))
	for idx, chunk := range payload.Chunks {
		testID := strings.TrimSpace(chunk.TestID)
		if testID == "" {
			testID = traceReq.TestID
		}
		chunkID := strings.TrimSpace(chunk.ChunkID)
		if chunkID == "" {
			chunkID = fmt.Sprintf("chunk-%d", idx+1)
		}
		chunks = append(chunks, &localtracepb.ClientStreamRequest{
			TestId:       testID,
			ChunkId:      chunkID,
			Payload:      chunk.Payload,
			FinalStatus:  chunk.FinalStatus,
			FinalMessage: chunk.FinalMessage,
		})
	}

	resp, callErr := h.invoker.CallDownstreamGRPCClientStream(ctx, &DownstreamGRPCClientStreamRequest{
		Payloads: chunks,
	})

	if writeErr := h.writeSpan(ctx, outgoingSpan); writeErr != nil {
		h.logger.ErrorContext(ctx, "failed to write downstream gRPC client stream span", "error", writeErr)
	}
	if writeErr := h.writeSpan(ctx, parentSpan); writeErr != nil {
		h.logger.ErrorContext(ctx, "failed to write parent span", "error", writeErr)
	}

	if callErr != nil {
		h.logger.ErrorContext(ctx, "downstream gRPC client stream failed", "error", callErr)
		http.Error(w, "downstream gRPC client stream failed", http.StatusBadGateway)
		return
	}

	details := map[string]string{
		"downstream_span_id": outgoingSpan.spanID,
		"received_chunks":    strconv.Itoa(int(resp.GetReceivedChunks())),
	}
	if concat := resp.GetConcatenated(); concat != "" {
		details["concatenated"] = concat
	}

	h.logger.InfoContext(ctx, "Completed downstream gRPC client stream", "test_id", traceReq.TestID, "trace_id", spanCtx.TraceID().String(), "span_id", parentSpan.spanID, "downstream_span_id", outgoingSpan.spanID, "received_chunks", resp.GetReceivedChunks())

	writeTraceResponse(w, &TraceResponse{
		Success: true,
		Message: "downstream gRPC client stream completed",
		TraceID: spanCtx.TraceID().String(),
		SpanID:  parentSpan.spanID,
		Details: details,
	})
}

// TraceDownstreamGRPCBidiStream exercises the downstream bidirectional streaming RPC.
func (h *TraceHandler) TraceDownstreamGRPCBidiStream(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	payload, err := decodeBidiStreamTraceRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	traceReq := payload.TraceRequest

	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		http.Error(w, "missing or invalid trace context", http.StatusBadRequest)
		return
	}

	parentSpan := newSpanBuilder(spanCtx.TraceID().String(), spanCtx.SpanID().String(), "trace.downstream.grpc.stream_bidi", map[string]string{
		"test_id":  traceReq.TestID,
		"endpoint": "/trace/downstream-grpc/bidi-stream",
	})
	outgoingSpan := newSpanBuilder(spanCtx.TraceID().String(), parentSpan.spanID, "downstream.grpc.stream_bidi.call", map[string]string{
		"test_id": traceReq.TestID,
	})

	messages := buildBidiStreamMessages(traceReq.TestID, payload.Messages)

	responses, callErr := h.invoker.CallDownstreamGRPCBidiStream(ctx, &DownstreamGRPCBidiStreamRequest{
		Payloads: messages,
	})

	if writeErr := h.writeSpan(ctx, outgoingSpan); writeErr != nil {
		h.logger.ErrorContext(ctx, "failed to write downstream gRPC bidi stream span", "error", writeErr)
	}
	if writeErr := h.writeSpan(ctx, parentSpan); writeErr != nil {
		h.logger.ErrorContext(ctx, "failed to write parent span", "error", writeErr)
	}

	if callErr != nil {
		h.logger.ErrorContext(ctx, "downstream gRPC bidi stream failed", "error", callErr)
		http.Error(w, "downstream gRPC bidi stream failed", http.StatusBadGateway)
		return
	}

	details := map[string]string{
		"downstream_span_id": outgoingSpan.spanID,
		"response_count":     strconv.Itoa(len(responses)),
	}
	if len(responses) > 0 {
		last := responses[len(responses)-1]
		if chunkID := last.GetChunkId(); chunkID != "" {
			details["last_chunk_id"] = chunkID
		}
		if payload := last.GetPayload(); payload != "" {
			details["last_payload"] = payload
		}
	}

	responseCount := len(responses)
	h.logger.InfoContext(
		ctx,
		"Completed downstream gRPC bidi stream",
		"test_id", traceReq.TestID,
		"trace_id", spanCtx.TraceID().String(),
		"span_id", parentSpan.spanID,
		"downstream_span_id", outgoingSpan.spanID,
		"response_count", responseCount,
		"responses", responseCount,
	)

	writeTraceResponse(w, &TraceResponse{
		Success: true,
		Message: "downstream gRPC bidi stream completed",
		TraceID: spanCtx.TraceID().String(),
		SpanID:  parentSpan.spanID,
		Details: details,
	})
}

// decodeServerStreamTraceRequest parses and validates server stream input.
func decodeServerStreamTraceRequest(r *http.Request) (*serverStreamTraceRequest, error) {
	defer closeRequestBody(r)
	var req serverStreamTraceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, fmt.Errorf("failed to decode request: %w", err)
	}
	if req.TraceRequest == nil {
		return nil, errors.New("trace request is required")
	}
	req.TraceRequest.TestID = strings.TrimSpace(req.TraceRequest.TestID)
	if req.TraceRequest.TestID == "" {
		return nil, errors.New("test_id is required")
	}
	if req.ResponseCount < 0 {
		return nil, errors.New("response_count must be non-negative")
	}
	return &req, nil
}

// decodeClientStreamTraceRequest parses and validates client stream input.
func decodeClientStreamTraceRequest(r *http.Request) (*clientStreamTraceRequest, error) {
	defer closeRequestBody(r)
	var req clientStreamTraceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, fmt.Errorf("failed to decode request: %w", err)
	}
	if req.TraceRequest == nil {
		return nil, errors.New("trace request is required")
	}
	req.TraceRequest.TestID = strings.TrimSpace(req.TraceRequest.TestID)
	if req.TraceRequest.TestID == "" {
		return nil, errors.New("test_id is required")
	}
	if len(req.Chunks) == 0 {
		return nil, errors.New("chunks are required")
	}
	return &req, nil
}

// decodeBidiStreamTraceRequest parses and validates bidi stream input.
func decodeBidiStreamTraceRequest(r *http.Request) (*bidiStreamTraceRequest, error) {
	defer closeRequestBody(r)
	var req bidiStreamTraceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, fmt.Errorf("failed to decode request: %w", err)
	}
	if req.TraceRequest == nil {
		return nil, errors.New("trace request is required")
	}
	req.TraceRequest.TestID = strings.TrimSpace(req.TraceRequest.TestID)
	if req.TraceRequest.TestID == "" {
		return nil, errors.New("test_id is required")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("messages are required")
	}
	return &req, nil
}

// decodeTraceRequest parses and validates the common trace request input.
func decodeTraceRequest(r *http.Request) (*TraceRequest, error) {
	defer closeRequestBody(r)
	var req TraceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, fmt.Errorf("failed to decode request: %w", err)
	}
	req.TestID = strings.TrimSpace(req.TestID)
	if req.TestID == "" {
		return nil, errors.New("test_id is required")
	}
	return &req, nil
}

// writeTraceResponse writes a JSON trace response body.
func writeTraceResponse(w http.ResponseWriter, resp *TraceResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

type spanBuilder struct {
	traceID      string
	parentSpanID string
	spanID       string
	name         string
	start        time.Time
	attrs        map[string]string
}

// newSpanBuilder initializes span metadata for delayed trace writes.
func newSpanBuilder(traceID, parentSpanID, name string, attrs map[string]string) *spanBuilder {
	return &spanBuilder{
		traceID:      traceID,
		parentSpanID: parentSpanID,
		spanID:       generateSpanID(),
		name:         name,
		start:        time.Now(),
		attrs:        attrs,
	}
}

// writeSpan emits the configured span to Cloud Trace.
func (h *TraceHandler) writeSpan(ctx context.Context, builder *spanBuilder) error {
	if builder == nil {
		return errors.New("span builder is nil")
	}

	end := time.Now()
	attrMap := make(map[string]*tracepb.AttributeValue, len(builder.attrs))
	for k, v := range builder.attrs {
		attrMap[k] = &tracepb.AttributeValue{
			Value: &tracepb.AttributeValue_StringValue{
				StringValue: &tracepb.TruncatableString{Value: v},
			},
		}
	}

	span := &tracepb.Span{
		Name:                    fmt.Sprintf("projects/%s/traces/%s/spans/%s", h.projectID, builder.traceID, builder.spanID),
		SpanId:                  builder.spanID,
		DisplayName:             &tracepb.TruncatableString{Value: builder.name},
		StartTime:               timestamppb.New(builder.start),
		EndTime:                 timestamppb.New(end),
		Attributes:              &tracepb.Span_Attributes{AttributeMap: attrMap},
		SpanKind:                tracepb.Span_SERVER,
		SameProcessAsParentSpan: wrapperspb.Bool(true),
	}

	if builder.parentSpanID != "" {
		span.ParentSpanId = builder.parentSpanID
	}

	req := &tracepb.BatchWriteSpansRequest{
		Name:  fmt.Sprintf("projects/%s", h.projectID),
		Spans: []*tracepb.Span{span},
	}

	if err := h.traceClient.BatchWriteSpans(ctx, req); err != nil {
		return fmt.Errorf("batch write spans: %w", err)
	}

	return nil
}

// generateSpanID returns a random 16-character hex span ID.
func generateSpanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fall back to timestamp-derived value; tracing tolerates collisions in tests.
		now := time.Now().UnixNano()
		return fmt.Sprintf("%016x", now)
	}
	return hex.EncodeToString(b[:])
}

// int32FromInt saturates int values to the int32 range.
func int32FromInt(v int) int32 {
	if v > int(^uint32(0)>>1) {
		return int32(^uint32(0) >> 1)
	}
	if v < -int(^uint32(0)>>1)-1 {
		return -int32(^uint32(0)>>1) - 1
	}
	return int32(v)
}

// closeRequestBody closes request bodies and logs close failures.
func closeRequestBody(r *http.Request) {
	if r == nil || r.Body == nil {
		return
	}
	if err := r.Body.Close(); err != nil {
		slog.Warn("Failed to close request body", "error", err)
	}
}

// buildBidiStreamMessages normalizes chunk defaults for downstream bidi calls.
func buildBidiStreamMessages(defaultTestID string, chunks []bidiStreamChunk) []*localtracepb.BidiStreamRequest {
	messages := make([]*localtracepb.BidiStreamRequest, 0, len(chunks))
	for idx, msg := range chunks {
		testID := strings.TrimSpace(msg.TestID)
		if testID == "" {
			testID = defaultTestID
		}
		chunkID := strings.TrimSpace(msg.ChunkID)
		if chunkID == "" {
			chunkID = fmt.Sprintf("chunk-%d", idx+1)
		}
		messages = append(messages, &localtracepb.BidiStreamRequest{
			TestId:       testID,
			ChunkId:      chunkID,
			Payload:      msg.Payload,
			CloseAfter:   msg.CloseAfter,
			FinalStatus:  msg.FinalStatus,
			FinalMessage: msg.FinalMessage,
		})
	}

	return messages
}
