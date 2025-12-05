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

package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"cloud.google.com/go/logging"
	logpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

const (
	bufSize               = 1024 * 1024
	loggingParent         = "projects/local-dev"
	loggingLogID          = "slogcp-v-google"
	traceHeaderName       = "Traceparent"
	maxConnectionsPerHost = 50
)

type fakeLoggingServer struct {
	logpb.UnimplementedLoggingServiceV2Server
}

func main() {
	if err := run(context.Background(), os.Stdout); err != nil {
		log.Fatalf("google logging example failed: %v", err)
	}
}

func run(ctx context.Context, out io.Writer) error {
	scenario, err := newClientScenario(ctx)
	if err != nil {
		return err
	}
	defer scenario.Close()

	return scenario.Run(ctx, out)
}

type loggingHarness struct {
	listener *bufconn.Listener
	server   *grpc.Server
	conn     *grpc.ClientConn
	client   *logging.Client
}

type clientScenario struct {
	harness      *loggingHarness
	server       *httptest.Server
	transport    *http.Transport
	httpClient   *http.Client
	activeLogger atomic.Pointer[logging.Logger]
}

func newClientScenario(ctx context.Context) (*clientScenario, error) {
	harness, err := newLoggingHarness(ctx)
	if err != nil {
		return nil, fmt.Errorf("new logging harness: %w", err)
	}

	scenario := &clientScenario{harness: harness}

	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.MaxConnsPerHost = maxConnectionsPerHost
	transport.MaxIdleConns = maxConnectionsPerHost
	transport.MaxIdleConnsPerHost = maxConnectionsPerHost

	scenario.transport = transport
	scenario.httpClient = &http.Client{Transport: transport}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger := scenario.currentLogger()
		if logger == nil {
			http.Error(w, "logger unavailable", http.StatusServiceUnavailable)
			return
		}

		start := time.Now()
		received := logging.Entry{
			Severity: logging.Info,
			Payload: map[string]any{
				"message": "received request",
				"method":  r.Method,
				"path":    r.URL.Path,
				"trace":   r.Header.Get(traceHeaderName),
			},
			HTTPRequest: &logging.HTTPRequest{Request: r, Status: http.StatusOK},
		}
		if err := logger.LogSync(r.Context(), received); err != nil {
			log.Printf("log request: %v", err)
		}

		w.Header().Set("X-Received-Trace", "true")
		w.WriteHeader(http.StatusOK)
		if _, err := io.Copy(w, strings.NewReader("ok")); err != nil {
			log.Printf("write response: %v", err)
			return
		}

		completed := logging.Entry{
			Severity: logging.Info,
			Payload: map[string]any{
				"message": "handled request",
				"latency": time.Since(start).String(),
			},
		}
		if err := logger.LogSync(r.Context(), completed); err != nil {
			log.Printf("log response: %v", err)
		}
	}))

	scenario.server = server
	return scenario, nil
}

func (c *clientScenario) Run(ctx context.Context, out io.Writer) error {
	logger, cleanup, err := c.harness.newLogger(out)
	if err != nil {
		return fmt.Errorf("new redirect logger: %w", err)
	}
	defer cleanup()

	c.activeLogger.Store(logger)
	defer c.activeLogger.Store(nil)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.server.URL, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	if err := logger.LogSync(ctx, logging.Entry{
		Severity: logging.Info,
		Payload: map[string]any{
			"message": "sending request",
			"url":     req.URL.String(),
		},
	}); err != nil {
		return fmt.Errorf("log request start: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("drain body: %w", err)
	}

	if err := logger.LogSync(ctx, logging.Entry{
		Severity: logging.Info,
		Payload: map[string]any{
			"message": "response received",
			"status":  resp.Status,
			"trace":   resp.Header.Get("X-Received-Trace"),
		},
	}); err != nil {
		return fmt.Errorf("log response: %w", err)
	}

	return nil
}

func (c *clientScenario) Close() {
	if c == nil {
		return
	}
	c.activeLogger.Store(nil)
	if c.server != nil {
		c.server.CloseClientConnections()
		c.server.Close()
	}
	if c.transport != nil {
		c.transport.CloseIdleConnections()
	}
	if c.harness != nil {
		c.harness.Close()
	}
}

func (c *clientScenario) currentLogger() *logging.Logger {
	if c == nil {
		return nil
	}
	return c.activeLogger.Load()
}

func newLoggingHarness(ctx context.Context) (*loggingHarness, error) {
	listener := bufconn.Listen(bufSize)
	server := grpc.NewServer()
	logpb.RegisterLoggingServiceV2Server(server, &fakeLoggingServer{})

	go func() {
		if err := server.Serve(listener); err != nil {
			log.Printf("grpc serve: %v", err)
		}
	}()

	dialer := func(ctx context.Context, _ string) (net.Conn, error) {
		return listener.Dial()
	}

	conn, err := grpc.NewClient("bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(dialer),
	)
	if err != nil {
		listener.Close()
		server.Stop()
		return nil, fmt.Errorf("create bufconn client: %w", err)
	}

	client, err := logging.NewClient(ctx, loggingParent, option.WithGRPCConn(conn))
	if err != nil {
		conn.Close()
		listener.Close()
		server.Stop()
		return nil, fmt.Errorf("new client: %w", err)
	}

	return &loggingHarness{
		listener: listener,
		server:   server,
		conn:     conn,
		client:   client,
	}, nil
}

func (h *loggingHarness) newLogger(out io.Writer) (*logging.Logger, func(), error) {
	if h == nil {
		return nil, nil, fmt.Errorf("nil logging harness")
	}
	logger := h.client.Logger(loggingLogID, logging.RedirectAsJSON(out))
	cleanup := func() {
		if err := logger.Flush(); err != nil {
			log.Printf("flush logger: %v", err)
		}
	}
	return logger, cleanup, nil
}

func (h *loggingHarness) Close() {
	if h == nil {
		return
	}
	if err := h.client.Close(); err != nil {
		log.Printf("close client: %v", err)
	}
	h.conn.Close()
	h.server.Stop()
	h.listener.Close()
}
