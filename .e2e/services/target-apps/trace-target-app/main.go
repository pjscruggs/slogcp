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
	"fmt"
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

	"cloud.google.com/go/pubsub/v2"
	traceapi "cloud.google.com/go/trace/apiv2"
	grpc_logging "github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp-e2e-internal/services/target-apps/trace-target-app/handlers"
	tracepb "github.com/pjscruggs/slogcp-e2e-internal/services/traceproto"
	slogcpadapter "github.com/pjscruggs/slogcp-grpc-adapter"
	"github.com/pjscruggs/slogcp/slogcpgrpc"
	"github.com/pjscruggs/slogcp/slogcphttp"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

var (
	appVersion = "dev"
	buildTime  = "unknown"
)

const (
	envDisableHTTPTracePropagation   = "TRACE_DISABLE_HTTP_TRACE_PROPAGATION"
	envDisableGRPCTracePropagation   = "TRACE_DISABLE_GRPC_TRACE_PROPAGATION"
	envLegacyDisableGRPCInterceptors = "TRACE_DISABLE_GRPC_CLIENT_INTERCEPTORS"
	envDefaultTraceSampled           = "TRACE_DEFAULT_SAMPLED"
	envPubSubTopic                   = "TRACE_PUBSUB_TOPIC"
)

// main boots the trace target service used by e2e propagation tests.
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	cfg := loadConfig()
	if err := run(ctx, cfg); err != nil {
		log.Printf("trace-target-app failed: %v", err)
		return
	}
}

type config struct {
	Port                        string
	ProjectID                   string
	DownstreamHTTPURL           string
	DownstreamGRPCTarget        string
	DisableHTTPTracePropagation bool
	DisableGRPCTracePropagation bool
	DefaultSample               bool
	PubSubTopic                 string
}

// loadConfig builds runtime config from environment variables.
func loadConfig() config {
	return config{
		Port:                        getEnvOrDefault("PORT", "8080"),
		ProjectID:                   os.Getenv("GOOGLE_CLOUD_PROJECT"),
		DownstreamHTTPURL:           os.Getenv("DOWNSTREAM_HTTP_URL"),
		DownstreamGRPCTarget:        os.Getenv("DOWNSTREAM_GRPC_TARGET"),
		DisableHTTPTracePropagation: getEnvAsBool(envDisableHTTPTracePropagation, false),
		DisableGRPCTracePropagation: getEnvAsBool(envDisableGRPCTracePropagation, false) || getEnvAsBool(envLegacyDisableGRPCInterceptors, false),
		DefaultSample:               getEnvAsBool(envDefaultTraceSampled, true),
		PubSubTopic:                 strings.TrimSpace(os.Getenv(envPubSubTopic)),
	}
}

// run initializes dependencies and serves HTTP traffic until shutdown.
func run(ctx context.Context, cfg config) error {
	if cfg.ProjectID == "" {
		return fmt.Errorf("GOOGLE_CLOUD_PROJECT must be set")
	}
	slogcp.EnsurePropagation()

	handler, err := slogcp.NewHandler(os.Stdout,
		slogcp.WithSourceLocationEnabled(true),
		slogcp.WithAttrs([]slog.Attr{
			slog.Group(slogcp.LabelsGroup,
				slog.String("app", "trace-target-app"),
				slog.String("component", "e2e-test"),
			),
		}),
	)
	if err != nil {
		return fmt.Errorf("initializing logger: %w", err)
	}
	defer closeSlogHandler(handler)
	logger := slog.New(handler)
	baseLogger := logger

	traceClient, err := traceapi.NewClient(ctx)
	if err != nil {
		return fmt.Errorf("creating trace client: %w", err)
	}
	defer closeTraceClient(ctx, logger, traceClient)

	pubSubClient, pubSubPublisher, err := setupPubSubPublisher(ctx, cfg)
	if err != nil {
		return err
	}
	if pubSubPublisher != nil {
		defer pubSubPublisher.Stop()
	}
	if pubSubClient != nil {
		defer closePubSubClient(ctx, logger, pubSubClient)
	}

	httpOpts := []slogcphttp.Option{
		slogcphttp.WithLogger(baseLogger),
		slogcphttp.WithTracePropagation(!cfg.DisableHTTPTracePropagation),
		slogcphttp.WithLegacyXCloudInjection(true),
	}
	transport := slogcphttp.Transport(http.DefaultTransport, httpOpts...)
	transport, err = withCloudRunIDTokenTransport(ctx, transport, cfg.DownstreamHTTPURL)
	if err != nil {
		return fmt.Errorf("configuring downstream http auth: %w", err)
	}
	httpClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	grpcConn := setupDownstreamGRPCConn(ctx, cfg, baseLogger, handler, logger)
	defer closeGRPCConn(ctx, logger, grpcConn)

	var grpcClient tracepb.TraceWorkerClient
	if grpcConn != nil {
		grpcClient = tracepb.NewTraceWorkerClient(grpcConn)
	}

	invoker := &handlers.DownstreamClients{
		HTTPBaseURL: cfg.DownstreamHTTPURL,
		HTTPClient:  httpClient,
		GRPCClient:  grpcClient,
	}

	traceHandler := handlers.NewTraceHandler(baseLogger, traceClient, cfg.ProjectID, invoker, pubSubPublisher)
	traceHandler.SetDefaultSample(cfg.DefaultSample)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", healthHandler(ctx, logger))
	mux.HandleFunc("/trace/simple", traceHandler.TraceSimple)
	mux.HandleFunc("/trace/downstream-http", traceHandler.TraceDownstreamHTTP)
	mux.HandleFunc("/trace/downstream-grpc", traceHandler.TraceDownstreamGRPC)
	mux.HandleFunc("/trace/downstream-pubsub", traceHandler.TraceDownstreamPubSub)
	mux.HandleFunc("/trace/downstream-grpc/server-stream", traceHandler.TraceDownstreamGRPCServerStream)
	mux.HandleFunc("/trace/downstream-grpc/client-stream", traceHandler.TraceDownstreamGRPCClientStream)
	mux.HandleFunc("/trace/downstream-grpc/bidi-stream", traceHandler.TraceDownstreamGRPCBidiStream)

	wrappedHandler := slogcphttp.Middleware(httpOpts...)(mux)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           wrappedHandler,
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}

	logger.InfoContext(ctx, "trace-target-app starting",
		"port", cfg.Port,
		"version", appVersion,
		"build_time", buildTime,
		"downstream_http", cfg.DownstreamHTTPURL,
		"downstream_grpc", cfg.DownstreamGRPCTarget,
		"pubsub_topic", cfg.PubSubTopic,
		"disable_http_trace_propagation", cfg.DisableHTTPTracePropagation,
		"disable_grpc_trace_propagation", cfg.DisableGRPCTracePropagation,
		"default_trace_sampled", cfg.DefaultSample,
	)

	if err := serveUntilShutdown(ctx, server, logger); err != nil {
		return err
	}

	logger.InfoContext(context.Background(), "trace-target-app stopped")
	return nil
}

// setupDownstreamGRPCConn dials the configured downstream gRPC target.
func setupDownstreamGRPCConn(ctx context.Context, cfg config, baseLogger *slog.Logger, handler *slogcp.Handler, logger *slog.Logger) *grpc.ClientConn {
	if cfg.DownstreamGRPCTarget == "" {
		return nil
	}

	dialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewClientTLSFromCert(nil, "")),
	}
	perRPCCreds, usePerRPCCreds, err := newCloudRunPerRPCCredentials(ctx, cfg.DownstreamGRPCTarget)
	if err != nil {
		logger.ErrorContext(context.Background(), "failed to configure downstream gRPC auth", "error", err)
		return nil
	}
	if usePerRPCCreds {
		dialOpts = append(dialOpts, grpc.WithPerRPCCredentials(&perRPCCreds))
	}
	grpcOpts := []slogcpgrpc.Option{
		slogcpgrpc.WithLogger(baseLogger),
		slogcpgrpc.WithTracePropagation(!cfg.DisableGRPCTracePropagation),
		slogcpgrpc.WithLegacyXCloudInjection(true),
	}
	adapterLogger := slogcpadapter.NewLogger(handler, slogcpadapter.WithLogger(baseLogger))
	grpcLoggingOpts := grpcLoggingOptions()
	dialOpts = append(dialOpts, slogcpgrpc.DialOptions(grpcOpts...)...)
	dialOpts = append(dialOpts,
		grpc.WithChainUnaryInterceptor(grpc_logging.UnaryClientInterceptor(adapterLogger, grpcLoggingOpts...)),
		grpc.WithChainStreamInterceptor(grpc_logging.StreamClientInterceptor(adapterLogger, grpcLoggingOpts...)),
	)

	grpcConn, err := grpc.NewClient(cfg.DownstreamGRPCTarget, dialOpts...)
	if err != nil {
		logger.ErrorContext(context.Background(), "failed to dial downstream gRPC target", "error", err)
		return nil
	}

	return grpcConn
}

// closeSlogHandler closes the slog handler and logs close failures.
func closeSlogHandler(handler *slogcp.Handler) {
	if handler == nil {
		return
	}
	if closeErr := handler.Close(); closeErr != nil {
		log.Printf("Failed to close slog handler: %v", closeErr)
	}
}

// closeTraceClient closes the trace client and logs close failures.
func closeTraceClient(ctx context.Context, logger *slog.Logger, traceClient *traceapi.Client) {
	if traceClient == nil {
		return
	}
	if closeErr := traceClient.Close(); closeErr != nil {
		logger.WarnContext(ctx, "Failed to close trace client", "error", closeErr)
	}
}

// closePubSubClient closes the pubsub client and logs close failures.
func closePubSubClient(ctx context.Context, logger *slog.Logger, pubSubClient *pubsub.Client) {
	if pubSubClient == nil {
		return
	}
	if closeErr := pubSubClient.Close(); closeErr != nil {
		logger.WarnContext(ctx, "Failed to close pubsub client", "error", closeErr)
	}
}

// closeGRPCConn closes the downstream gRPC connection and logs failures.
func closeGRPCConn(ctx context.Context, logger *slog.Logger, grpcConn *grpc.ClientConn) {
	if grpcConn == nil {
		return
	}
	if closeErr := grpcConn.Close(); closeErr != nil {
		logger.WarnContext(ctx, "Failed to close downstream grpc client conn", "error", closeErr)
	}
}

// healthHandler returns the /health endpoint implementation.
func healthHandler(ctx context.Context, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, writeErr := w.Write([]byte(`{"status":"healthy"}`)); writeErr != nil {
			logger.WarnContext(ctx, "Failed to write health response", "error", writeErr)
		}
	}
}

// serveUntilShutdown serves HTTP and performs graceful shutdown on cancel.
func serveUntilShutdown(ctx context.Context, server *http.Server, logger *slog.Logger) error {
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

// setupPubSubPublisher creates the optional pubsub client and publisher.
func setupPubSubPublisher(ctx context.Context, cfg config) (*pubsub.Client, *pubsub.Publisher, error) {
	if cfg.PubSubTopic == "" {
		return nil, nil, nil
	}

	pubSubClient, err := pubsub.NewClient(ctx, cfg.ProjectID)
	if err != nil {
		return nil, nil, fmt.Errorf("creating pubsub client: %w", err)
	}

	return pubSubClient, pubSubClient.Publisher(cfg.PubSubTopic), nil
}

// getEnvOrDefault returns the trimmed env value or fallback.
func getEnvOrDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// grpcLoggingOptions returns adapter logging options used by client interceptors.
func grpcLoggingOptions() []grpc_logging.Option {
	return []grpc_logging.Option{
		grpc_logging.WithLogOnEvents(grpc_logging.StartCall, grpc_logging.FinishCall),
		grpc_logging.WithLevels(grpc_logging.DefaultServerCodeToLevel),
	}
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
