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
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cloud.google.com/go/pubsub/v2"
	traceapi "cloud.google.com/go/trace/apiv2"
	tracepb "cloud.google.com/go/trace/apiv2/tracepb"
	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcphttp"
	"github.com/pjscruggs/slogcp/slogcppubsub"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

var (
	appVersion = "dev"
	buildTime  = "unknown"
)

type config struct {
	Port               string
	ProjectID          string
	PubSubTopic        string
	PubSubSubscription string
}

type workRequest struct {
	TestID  string `json:"test_id"`
	Message string `json:"message"`
}

type pubSubWorkRequest struct {
	TestID  string `json:"test_id"`
	Message string `json:"message"`
}

type workResponse struct {
	Acknowledgement string            `json:"acknowledgement"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// main boots the downstream HTTP service used by trace propagation e2e tests.
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg := config{
		Port:               getEnvOrDefault("PORT", "8080"),
		ProjectID:          os.Getenv("GOOGLE_CLOUD_PROJECT"),
		PubSubTopic:        strings.TrimSpace(os.Getenv("TRACE_PUBSUB_TOPIC")),
		PubSubSubscription: strings.TrimSpace(os.Getenv("TRACE_PUBSUB_SUBSCRIPTION")),
	}
	if cfg.ProjectID == "" {
		log.Print("GOOGLE_CLOUD_PROJECT must be set")
		return
	}

	if err := run(ctx, cfg); err != nil {
		log.Printf("trace-downstream-http failed: %v", err)
		return
	}
}

// run initializes dependencies and serves HTTP traffic until shutdown.
func run(ctx context.Context, cfg config) error {
	slogcp.EnsurePropagation()

	handler, err := slogcp.NewHandler(os.Stdout,
		slogcp.WithSourceLocationEnabled(true),
		slogcp.WithAttrs([]slog.Attr{
			slog.Group(slogcp.LabelsGroup,
				slog.String("app", "trace-downstream-http"),
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

	pubSubClient, hasSubscription, err := startPubSubReceiver(ctx, cfg, logger)
	if err != nil {
		return err
	}
	if hasSubscription {
		defer func() {
			if closeErr := pubSubClient.Close(); closeErr != nil {
				logger.WarnContext(ctx, "Failed to close pubsub client", "error", closeErr)
			}
		}()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, writeErr := w.Write([]byte(`{"status":"healthy"}`)); writeErr != nil {
			logger.WarnContext(ctx, "Failed to write health response", "error", writeErr)
		}
	})
	mux.HandleFunc("/work", func(w http.ResponseWriter, r *http.Request) {
		handleWork(w, r, logger, traceClient, cfg.ProjectID)
	})

	wrappedHandler := slogcphttp.Middleware(
		slogcphttp.WithLogger(logger),
	)(mux)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           wrappedHandler,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	logger.InfoContext(ctx, "trace-downstream-http starting",
		"port", cfg.Port,
		"version", appVersion,
		"build_time", buildTime,
		"pubsub_topic", cfg.PubSubTopic,
		"pubsub_subscription", cfg.PubSubSubscription,
	)

	if err := runHTTPServer(ctx, server, logger); err != nil {
		return err
	}

	logger.InfoContext(context.Background(), "trace-downstream-http stopped")
	return nil
}

// startPubSubReceiver starts optional Pub/Sub subscription processing.
func startPubSubReceiver(ctx context.Context, cfg config, logger *slog.Logger) (*pubsub.Client, bool, error) {
	if cfg.PubSubSubscription == "" {
		return nil, false, nil
	}

	pubSubClient, err := pubsub.NewClient(ctx, cfg.ProjectID)
	if err != nil {
		return nil, true, fmt.Errorf("creating pubsub client: %w", err)
	}

	sub := pubSubClient.Subscriber(cfg.PubSubSubscription)
	go func() {
		recvErr := sub.Receive(ctx, slogcppubsub.WrapReceiveHandler(
			handlePubSubMessage,
			slogcppubsub.WithLogger(logger),
			slogcppubsub.WithProjectID(cfg.ProjectID),
			slogcppubsub.WithSubscriptionID(cfg.PubSubSubscription),
			slogcppubsub.WithTopicID(cfg.PubSubTopic),
			slogcppubsub.WithOTel(false),
		))
		if recvErr != nil {
			logger.ErrorContext(ctx, "pubsub receive stopped", "error", recvErr)
		}
	}()

	return pubSubClient, true, nil
}

// runHTTPServer serves HTTP traffic and handles graceful shutdown.
func runHTTPServer(ctx context.Context, server *http.Server, logger *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		if serveErr := server.ListenAndServe(); serveErr != nil && serveErr != http.ErrServerClosed {
			errCh <- serveErr
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.ErrorContext(shutdownCtx, "HTTP server shutdown error", "error", err)
		}
		return nil
	case serveErr := <-errCh:
		return fmt.Errorf("http server error: %w", serveErr)
	}
}

// handleWork serves HTTP requests used by downstream trace propagation tests.
func handleWork(w http.ResponseWriter, r *http.Request, logger *slog.Logger, traceClient *traceapi.Client, projectID string) {
	ctx := r.Context()

	var req workRequest
	defer func() {
		if err := r.Body.Close(); err != nil {
			logger.WarnContext(ctx, "Failed to close work request body", "error", err)
		}
	}()
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.TestID == "" {
		http.Error(w, "test_id is required", http.StatusBadRequest)
		return
	}

	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		http.Error(w, "missing trace context", http.StatusBadRequest)
		return
	}

	spanID := generateSpanID()
	attrs := map[string]string{
		"test_id":   req.TestID,
		"component": "downstream-http",
	}
	if err := writeSpan(ctx, traceClient, projectID, spanCtx.TraceID().String(), spanCtx.SpanID().String(), spanID, "downstream.http.handler", attrs); err != nil {
		logger.WarnContext(ctx, "failed to write downstream span", "error", err)
	}

	logger.InfoContext(ctx, "Downstream HTTP work completed",
		"test_id", req.TestID,
		"trace_id", spanCtx.TraceID().String(),
		"span_id", spanID,
	)

	resp := workResponse{
		Acknowledgement: "downstream http processed",
		Metadata: map[string]string{
			"service": "trace-downstream-http",
		},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.WarnContext(ctx, "Failed to write work response", "error", err)
	}
}

// handlePubSubMessage processes one Pub/Sub message in the downstream service.
func handlePubSubMessage(ctx context.Context, msg *pubsub.Message) {
	if msg == nil {
		return
	}

	var payload pubSubWorkRequest
	if len(msg.Data) > 0 {
		if err := json.Unmarshal(msg.Data, &payload); err != nil {
			slogcp.Logger(ctx).Warn("Failed to decode pubsub payload", "error", err)
		}
	}

	testID := strings.TrimSpace(payload.TestID)
	if testID == "" && msg.Attributes != nil {
		testID = strings.TrimSpace(msg.Attributes["test_id"])
	}

	logger := slogcp.Logger(ctx)
	if testID == "" {
		logger.Info("Processed pubsub message", "payload", payload.Message)
	} else {
		logger.Info("Processed pubsub message", "test_id", testID, "payload", payload.Message)
	}

	msg.Ack()
}

// writeSpan writes a synthetic child span to Cloud Trace for correlation checks.
func writeSpan(ctx context.Context, client *traceapi.Client, projectID, traceID, parentSpanID, spanID, name string, attrs map[string]string) error {
	attrMap := make(map[string]*tracepb.AttributeValue, len(attrs))
	for k, v := range attrs {
		attrMap[k] = &tracepb.AttributeValue{
			Value: &tracepb.AttributeValue_StringValue{
				StringValue: &tracepb.TruncatableString{Value: v},
			},
		}
	}

	start := time.Now()
	end := start.Add(5 * time.Millisecond)
	span := &tracepb.Span{
		Name:                    fmt.Sprintf("projects/%s/traces/%s/spans/%s", projectID, traceID, spanID),
		SpanId:                  spanID,
		ParentSpanId:            parentSpanID,
		DisplayName:             &tracepb.TruncatableString{Value: name},
		StartTime:               timestamppb.New(start),
		EndTime:                 timestamppb.New(end),
		Attributes:              &tracepb.Span_Attributes{AttributeMap: attrMap},
		SpanKind:                tracepb.Span_SERVER,
		SameProcessAsParentSpan: wrapperspb.Bool(true),
	}

	req := &tracepb.BatchWriteSpansRequest{
		Name:  fmt.Sprintf("projects/%s", projectID),
		Spans: []*tracepb.Span{span},
	}

	if err := client.BatchWriteSpans(ctx, req); err != nil {
		return fmt.Errorf("batch write spans: %w", err)
	}

	return nil
}

// generateSpanID returns a random 16-char hex span ID.
func generateSpanID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		now := time.Now().UnixNano()
		return fmt.Sprintf("%016x", now)
	}
	return hex.EncodeToString(b[:])
}

// getEnvOrDefault returns the trimmed environment value or fallback.
func getEnvOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
