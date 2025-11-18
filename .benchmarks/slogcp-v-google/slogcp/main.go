package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"time"

	slogcp "github.com/pjscruggs/slogcp"
	slogcphttp "github.com/pjscruggs/slogcp/http"
)

const maxConnectionsPerHost = 50

type clientScenario struct {
	server       *httptest.Server
	client       *http.Client
	transport    *http.Transport
	activeLogger atomic.Pointer[slog.Logger]
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatalf("slogcp example failed: %v", err)
	}
}

func run(ctx context.Context, w io.Writer) error {
	scenario, err := newClientScenario()
	if err != nil {
		return err
	}
	defer scenario.Close()

	return scenario.Run(ctx, w)
}

func newClientScenario() (*clientScenario, error) {
	var base *http.Transport
	if transport, ok := http.DefaultTransport.(*http.Transport); ok {
		base = transport.Clone()
	} else {
		base = &http.Transport{}
	}

	base.MaxConnsPerHost = maxConnectionsPerHost
	base.MaxIdleConns = maxConnectionsPerHost
	base.MaxIdleConnsPerHost = maxConnectionsPerHost

	scenario := &clientScenario{
		transport: base,
		client:    &http.Client{Transport: slogcphttp.Transport(base)},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := scenario.currentLogger()
		start := time.Now()

		logger.InfoContext(r.Context(), "received request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("traceparent", r.Header.Get("Traceparent")),
		)
		w.Header().Set("X-Received-Trace", "true")
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, strings.NewReader("ok")); err != nil {
			logger.ErrorContext(r.Context(), "write response", slog.String("error", err.Error()))
			return
		}
		logger.InfoContext(r.Context(), "handled request",
			slog.Duration("latency", time.Since(start)),
		)
	}))

	scenario.server = server
	return scenario, nil
}

func (c *clientScenario) Close() {
	if c == nil {
		return
	}
	if c.server != nil {
		c.server.CloseClientConnections()
		c.server.Close()
	}
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	c.activeLogger.Store(nil)
}

func (c *clientScenario) Run(ctx context.Context, w io.Writer) error {
	handler, err := slogcp.NewHandler(w,
		slogcp.WithSourceLocationEnabled(true),
		slogcp.WithTime(true),
	)
	if err != nil {
		return fmt.Errorf("new handler: %w", err)
	}
	defer handler.Close()

	logger := slog.New(handler).With(
		slog.String("app", "slogcp-client"),
	)

	c.activeLogger.Store(logger)
	defer c.activeLogger.Store(nil)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server.URL, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	logger.InfoContext(ctx, "sending request", slog.String("url", req.URL.String()))

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("client do: %w", err)
	}
	defer resp.Body.Close()

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("drain body: %w", err)
	}

	logger.InfoContext(ctx, "response received",
		slog.Int("status", resp.StatusCode),
		slog.String("trace", resp.Header.Get("X-Received-Trace")),
	)

	return nil
}

func (c *clientScenario) currentLogger() *slog.Logger {
	if c == nil {
		return slog.Default()
	}
	if logger := c.activeLogger.Load(); logger != nil {
		return logger
	}
	return slog.Default()
}
