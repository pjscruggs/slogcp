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

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	traceapi "cloud.google.com/go/trace/apiv2"
	tracepbapi "cloud.google.com/go/trace/apiv2/tracepb"
	grpc_logging "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"github.com/pjscruggs/slogcp"
	localtracepb "github.com/pjscruggs/slogcp-e2e-internal/services/traceproto"
	slogcpadapter "github.com/pjscruggs/slogcp-grpc-adapter"
	"github.com/pjscruggs/slogcp/slogcpgrpc"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

var (
	appVersion = "dev"
	buildTime  = "unknown"
)

const (
	envLogMetadata = "TRACE_DOWNSTREAM_GRPC_LOG_METADATA"
	envLogPayload  = "TRACE_DOWNSTREAM_GRPC_LOG_PAYLOAD"
)

type config struct {
	Port      string
	ProjectID string
}

type grpcInterceptorConfig struct {
	includePeer  bool
	includeSizes bool
}

// main boots the downstream gRPC service used by trace propagation e2e tests.
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg := config{
		Port:      getEnvOrDefault("PORT", "8080"),
		ProjectID: os.Getenv("GOOGLE_CLOUD_PROJECT"),
	}
	if cfg.ProjectID == "" {
		log.Print("GOOGLE_CLOUD_PROJECT must be set")
		return
	}

	if err := run(ctx, cfg); err != nil {
		log.Printf("trace-downstream-grpc failed: %v", err)
		return
	}
}

// run initializes dependencies and serves gRPC-over-h2c traffic until shutdown.
func run(ctx context.Context, cfg config) error {
	slogcp.EnsurePropagation()

	handler, err := slogcp.NewHandler(os.Stdout,
		slogcp.WithSourceLocationEnabled(true),
		slogcp.WithAttrs([]slog.Attr{
			slog.Group(slogcp.LabelsGroup,
				slog.String("app", "trace-downstream-grpc"),
				slog.String("component", "e2e-test"),
			),
		}),
	)
	if err != nil {
		return fmt.Errorf("initializing handler: %w", err)
	}
	defer func() {
		if closeErr := handler.Close(); closeErr != nil {
			log.Printf("Failed to close slog handler: %v", closeErr)
		}
	}()
	logger := slog.New(handler)

	traceClient, err := traceapi.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("creating trace client: %w", err)
	}
	defer func() {
		if closeErr := traceClient.Close(); closeErr != nil {
			logger.WarnContext(ctx, "Failed to close trace client", "error", closeErr)
		}
	}()

	interceptorCfg := parseGRPCInterceptorConfig()

	serverImpl := &traceWorkerServer{
		logger:      logger,
		traceClient: traceClient,
		projectID:   cfg.ProjectID,
	}

	grpcServer := newGRPCServer(logger, interceptorCfg)
	localtracepb.RegisterTraceWorkerServer(grpcServer, serverImpl)

	h2cHandler := h2c.NewHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		grpcServer.ServeHTTP(w, r)
	}), &http2.Server{})

	httpServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           h2cHandler,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	logger.InfoContext(ctx, "trace-downstream-grpc starting", "port", cfg.Port, "version", appVersion, "build_time", buildTime)

	errCh := make(chan error, 1)
	go func() {
		if serveErr := httpServer.ListenAndServe(); serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- serveErr
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		grpcServer.GracefulStop()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			logger.ErrorContext(shutdownCtx, "gRPC server shutdown error", "error", err)
		}
	case serveErr := <-errCh:
		return fmt.Errorf("grpc server error: %w", serveErr)
	}

	logger.InfoContext(context.Background(), "trace-downstream-grpc stopped")
	return nil
}

// newGRPCServer builds the gRPC server with slogcp and adapter interceptors.
func newGRPCServer(logger *slog.Logger, interceptorCfg grpcInterceptorConfig) *grpc.Server {
	interceptorOpts := []slogcpgrpc.Option{
		slogcpgrpc.WithLogger(logger),
	}
	if !interceptorCfg.includePeer {
		interceptorOpts = append(interceptorOpts, slogcpgrpc.WithPeerInfo(false))
	}
	if !interceptorCfg.includeSizes {
		interceptorOpts = append(interceptorOpts, slogcpgrpc.WithPayloadSizes(false))
	}

	adapterLogger := slogcpadapter.NewLogger(nil, slogcpadapter.WithLogger(logger))
	loggingOpts := grpcLoggingOptions()
	serverOpts := slogcpgrpc.ServerOptions(interceptorOpts...)
	serverOpts = append(serverOpts,
		grpc.ChainUnaryInterceptor(grpc_logging.UnaryServerInterceptor(adapterLogger, loggingOpts...)),
		grpc.ChainStreamInterceptor(grpc_logging.StreamServerInterceptor(adapterLogger, loggingOpts...)),
	)

	return grpc.NewServer(serverOpts...)
}

// grpcLoggingOptions returns adapter logging options used by this service.
func grpcLoggingOptions() []grpc_logging.Option {
	return []grpc_logging.Option{
		grpc_logging.WithLogOnEvents(grpc_logging.StartCall, grpc_logging.FinishCall),
		grpc_logging.WithLevels(grpc_logging.DefaultServerCodeToLevel),
	}
}

// parseGRPCInterceptorConfig resolves gRPC interceptor toggles from env vars.
func parseGRPCInterceptorConfig() grpcInterceptorConfig {
	return grpcInterceptorConfig{
		includePeer:  getEnvAsBool(envLogMetadata, true),
		includeSizes: getEnvAsBool(envLogPayload, true),
	}
}

// parseStatusCode converts textual or numeric status input to a gRPC code.
func parseStatusCode(raw string) codes.Code {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return codes.Internal
	}

	upper := strings.ToUpper(trimmed)
	if code, ok := grpcCodeByName[upper]; ok {
		return code
	}

	if parsed, err := strconv.Atoi(trimmed); err == nil {
		if code, ok := grpcCodeByNumber[parsed]; ok {
			return code
		}
	}

	return codes.Unknown
}

var grpcCodeByName = map[string]codes.Code{
	"OK":                  codes.OK,
	"CANCELLED":           codes.Canceled,
	"CANCELED":            codes.Canceled,
	"UNKNOWN":             codes.Unknown,
	"INVALID_ARGUMENT":    codes.InvalidArgument,
	"DEADLINE_EXCEEDED":   codes.DeadlineExceeded,
	"NOT_FOUND":           codes.NotFound,
	"ALREADY_EXISTS":      codes.AlreadyExists,
	"PERMISSION_DENIED":   codes.PermissionDenied,
	"RESOURCE_EXHAUSTED":  codes.ResourceExhausted,
	"FAILED_PRECONDITION": codes.FailedPrecondition,
	"ABORTED":             codes.Aborted,
	"OUT_OF_RANGE":        codes.OutOfRange,
	"UNIMPLEMENTED":       codes.Unimplemented,
	"INTERNAL":            codes.Internal,
	"UNAVAILABLE":         codes.Unavailable,
	"DATA_LOSS":           codes.DataLoss,
	"UNAUTHENTICATED":     codes.Unauthenticated,
}

var grpcCodeByNumber = map[int]codes.Code{
	0:  codes.OK,
	1:  codes.Canceled,
	2:  codes.Unknown,
	3:  codes.InvalidArgument,
	4:  codes.DeadlineExceeded,
	5:  codes.NotFound,
	6:  codes.AlreadyExists,
	7:  codes.PermissionDenied,
	8:  codes.ResourceExhausted,
	9:  codes.FailedPrecondition,
	10: codes.Aborted,
	11: codes.OutOfRange,
	12: codes.Unimplemented,
	13: codes.Internal,
	14: codes.Unavailable,
	15: codes.DataLoss,
	16: codes.Unauthenticated,
}

type traceWorkerServer struct {
	localtracepb.UnimplementedTraceWorkerServer
	logger      *slog.Logger
	traceClient *traceapi.Client
	projectID   string
}

// DoWork handles unary downstream work requests.
func (s *traceWorkerServer) DoWork(ctx context.Context, req *localtracepb.TraceWorkRequest) (*localtracepb.TraceWorkResponse, error) {
	if req == nil || req.GetTestId() == "" {
		return nil, fmt.Errorf("test_id is required")
	}

	logger := s.logger
	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		return nil, fmt.Errorf("missing trace context")
	}

	spanID := generateSpanID()
	attrs := map[string]string{
		"test_id":   req.GetTestId(),
		"component": "downstream-grpc",
	}
	if s.traceClient != nil {
		if err := writeSpan(ctx, s.traceClient, s.projectID, spanCtx.TraceID().String(), spanCtx.SpanID().String(), spanID, "downstream.grpc.handler", attrs); err != nil {
			logger.WarnContext(ctx, "failed to write downstream grpc span", "error", err)
		}
	}

	logger.InfoContext(ctx, "Downstream gRPC work completed",
		"test_id", req.GetTestId(),
		"trace_id", spanCtx.TraceID().String(),
		"span_id", spanID,
	)

	return &localtracepb.TraceWorkResponse{
		Acknowledgement: "downstream grpc processed",
		Metadata: map[string]string{
			"service": "trace-downstream-grpc",
		},
	}, nil
}

// FailWork returns a caller-selected gRPC error status.
func (s *traceWorkerServer) FailWork(ctx context.Context, req *localtracepb.ErrorWorkRequest) (*localtracepb.TraceWorkResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request is required: %w", status.Error(codes.InvalidArgument, "request is required"))
	}

	logger := s.logger
	code := parseStatusCode(req.GetGrpcStatus())
	message := strings.TrimSpace(req.GetErrorMessage())
	if message == "" {
		message = fmt.Sprintf("%s error from downstream", code.String())
	}

	logger.WarnContext(ctx, "Downstream gRPC error response",
		"grpc_status", code.String(),
		"error_message", message,
	)

	return nil, fmt.Errorf("fail work status %s: %w", code.String(), status.Error(code, message))
}

// PanicWork simulates a panic-path error response.
func (s *traceWorkerServer) PanicWork(ctx context.Context, req *localtracepb.PanicWorkRequest) (*localtracepb.TraceWorkResponse, error) {
	panicMessage := "downstream panic triggered"
	if req != nil && strings.TrimSpace(req.GetPanicMessage()) != "" {
		panicMessage = req.GetPanicMessage()
	}

	logger := s.logger
	logger.ErrorContext(ctx, "panic recovery enabled",
		slog.String("panic_message", panicMessage),
	)
	return nil, fmt.Errorf("panic work: %w", status.Error(codes.Internal, panicMessage))
}

// EchoPayload returns the request payload unchanged.
func (s *traceWorkerServer) EchoPayload(_ context.Context, req *localtracepb.EchoPayloadRequest) (*localtracepb.EchoPayloadResponse, error) {
	if req == nil {
		return nil, fmt.Errorf("request is required: %w", status.Error(codes.InvalidArgument, "request is required"))
	}

	return &localtracepb.EchoPayloadResponse{Data: req.GetData()}, nil
}

// StreamServer emits a configured number of server-streamed responses.
func (s *traceWorkerServer) StreamServer(req *localtracepb.ServerStreamRequest, stream localtracepb.TraceWorker_StreamServerServer) error {
	if req == nil {
		return fmt.Errorf("request is required: %w", status.Error(codes.InvalidArgument, "request is required"))
	}

	testID := strings.TrimSpace(req.GetTestId())
	if testID == "" {
		return fmt.Errorf("test_id is required: %w", status.Error(codes.InvalidArgument, "test_id is required"))
	}

	if req.GetResponseCount() < 0 {
		return fmt.Errorf("response_count must be non-negative: %w", status.Error(codes.InvalidArgument, "response_count must be non-negative"))
	}

	payload := req.GetResponsePayload()
	if payload == "" {
		payload = "downstream stream payload"
	}

	for i := int32(0); i < req.GetResponseCount(); i++ {
		resp := &localtracepb.ServerStreamResponse{
			TestId:   testID,
			Sequence: i + 1,
			Payload:  payload,
		}
		if err := stream.Send(resp); err != nil {
			return fmt.Errorf("send server stream response: %w", err)
		}
	}

	statusRaw := strings.TrimSpace(req.GetFinalStatus())
	if statusRaw == "" {
		return nil
	}

	statusCode := parseStatusCode(statusRaw)
	if statusCode == codes.OK {
		return nil
	}

	message := strings.TrimSpace(req.GetFinalMessage())
	if message == "" {
		message = fmt.Sprintf("%s error from downstream stream", statusCode.String())
	}

	return fmt.Errorf("server stream final status %s: %w", statusCode.String(), status.Error(statusCode, message))
}

// StreamClient consumes client-streamed messages and replies once.
func (s *traceWorkerServer) StreamClient(stream localtracepb.TraceWorker_StreamClientServer) error {
	state := clientStreamState{finalStatus: codes.OK}

	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("receive client stream chunk: %w", err)
		}
		if err := applyClientStreamChunk(&state, req); err != nil {
			return err
		}
	}

	if state.testID == "" {
		return fmt.Errorf("test_id is required: %w", status.Error(codes.InvalidArgument, "test_id is required"))
	}

	if state.finalStatus != codes.OK {
		if state.finalMessage == "" {
			state.finalMessage = fmt.Sprintf("%s error from downstream client stream", state.finalStatus.String())
		}
		return fmt.Errorf("client stream final status %s: %w", state.finalStatus.String(), status.Error(state.finalStatus, state.finalMessage))
	}

	resp := &localtracepb.ClientStreamResponse{
		TestId:         state.testID,
		ReceivedChunks: state.received,
		Concatenated:   state.builder.String(),
	}

	if err := stream.SendAndClose(resp); err != nil {
		return fmt.Errorf("send and close client stream: %w", err)
	}

	return nil
}

type clientStreamState struct {
	testID       string
	received     int32
	builder      strings.Builder
	finalStatus  codes.Code
	finalMessage string
}

// applyClientStreamChunk merges one inbound client-stream chunk into state.
func applyClientStreamChunk(state *clientStreamState, req *localtracepb.ClientStreamRequest) error {
	if req == nil {
		return nil
	}

	if trimmedID := strings.TrimSpace(req.GetTestId()); trimmedID != "" {
		if state.testID == "" {
			state.testID = trimmedID
		} else if state.testID != trimmedID {
			return fmt.Errorf("test_id mismatch in stream: %w", status.Error(codes.InvalidArgument, "test_id mismatch in stream"))
		}
	}

	if payload := req.GetPayload(); payload != "" {
		state.builder.WriteString(payload)
	}
	state.received++

	if statusRaw := strings.TrimSpace(req.GetFinalStatus()); statusRaw != "" {
		state.finalStatus = parseStatusCode(statusRaw)
	}
	if message := strings.TrimSpace(req.GetFinalMessage()); message != "" {
		state.finalMessage = message
	}

	return nil
}

// StreamBidi echoes each request chunk over a bidirectional stream.
func (s *traceWorkerServer) StreamBidi(stream localtracepb.TraceWorker_StreamBidiServer) error {
	state := bidiStreamState{finalStatus: codes.OK}

	for {
		req, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("receive bidi stream chunk: %w", err)
		}

		closeAfter, err := processBidiChunk(stream, &state, req)
		if err != nil {
			return err
		}
		if closeAfter {
			break
		}
	}

	if state.testID == "" {
		return fmt.Errorf("test_id is required: %w", status.Error(codes.InvalidArgument, "test_id is required"))
	}

	if state.finalStatus != codes.OK {
		if state.finalMessage == "" {
			state.finalMessage = fmt.Sprintf("%s error from downstream bidi stream", state.finalStatus.String())
		}
		return fmt.Errorf("bidi stream final status %s: %w", state.finalStatus.String(), status.Error(state.finalStatus, state.finalMessage))
	}

	return nil
}

type bidiStreamState struct {
	testID       string
	finalStatus  codes.Code
	finalMessage string
}

// processBidiChunk validates and forwards one bidi-stream message.
func processBidiChunk(stream localtracepb.TraceWorker_StreamBidiServer, state *bidiStreamState, req *localtracepb.BidiStreamRequest) (bool, error) {
	if req == nil {
		return false, nil
	}

	trimmedID := strings.TrimSpace(req.GetTestId())
	if trimmedID == "" {
		return false, fmt.Errorf("test_id is required: %w", status.Error(codes.InvalidArgument, "test_id is required"))
	}
	if state.testID == "" {
		state.testID = trimmedID
	} else if state.testID != trimmedID {
		return false, fmt.Errorf("test_id mismatch in stream: %w", status.Error(codes.InvalidArgument, "test_id mismatch in stream"))
	}

	resp := &localtracepb.BidiStreamResponse{
		TestId:  state.testID,
		ChunkId: req.GetChunkId(),
		Payload: req.GetPayload(),
	}
	if err := stream.Send(resp); err != nil {
		return false, fmt.Errorf("send bidi stream response: %w", err)
	}

	if statusRaw := strings.TrimSpace(req.GetFinalStatus()); statusRaw != "" {
		state.finalStatus = parseStatusCode(statusRaw)
	}
	if message := strings.TrimSpace(req.GetFinalMessage()); message != "" {
		state.finalMessage = message
	}

	return req.GetCloseAfter(), nil
}

// writeSpan writes a synthetic child span for downstream work.
func writeSpan(ctx context.Context, client *traceapi.Client, projectID, traceID, parentSpanID, spanID, name string, attrs map[string]string) error {
	attrMap := make(map[string]*tracepbapi.AttributeValue, len(attrs))
	for k, v := range attrs {
		attrMap[k] = &tracepbapi.AttributeValue{
			Value: &tracepbapi.AttributeValue_StringValue{
				StringValue: &tracepbapi.TruncatableString{Value: v},
			},
		}
	}

	start := time.Now()
	end := start.Add(5 * time.Millisecond)
	span := &tracepbapi.Span{
		Name:                    fmt.Sprintf("projects/%s/traces/%s/spans/%s", projectID, traceID, spanID),
		SpanId:                  spanID,
		ParentSpanId:            parentSpanID,
		DisplayName:             &tracepbapi.TruncatableString{Value: name},
		StartTime:               timestamppb.New(start),
		EndTime:                 timestamppb.New(end),
		Attributes:              &tracepbapi.Span_Attributes{AttributeMap: attrMap},
		SpanKind:                tracepbapi.Span_SERVER,
		SameProcessAsParentSpan: wrapperspb.Bool(true),
	}

	req := &tracepbapi.BatchWriteSpansRequest{
		Name:  fmt.Sprintf("projects/%s", projectID),
		Spans: []*tracepbapi.Span{span},
	}

	if err := client.BatchWriteSpans(ctx, req); err != nil {
		return fmt.Errorf("batch write spans: %w", err)
	}

	return nil
}

// generateSpanID returns a random 16-character hex span ID.
func generateSpanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		return fmt.Sprintf("%016x", now)
	}
	return hex.EncodeToString(b[:])
}

// getEnvAsBool parses a boolean env var with fallback.
func getEnvAsBool(key string, defaultVal bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return defaultVal
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return defaultVal
	}
	return parsed
}

// getEnvOrDefault returns a trimmed env value or fallback.
func getEnvOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
