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

// Command grpc-adapter logs a local health RPC through grpc-ecosystem middleware.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/logging"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	slogcpadapter "github.com/pjscruggs/slogcp-grpc-adapter/v2"

	"github.com/pjscruggs/slogcp/v2"
)

// main runs the local RPC example with a bounded client context.
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := run(ctx, os.Stdout)
	cancel()
	if err != nil {
		log.Fatal(err)
	}
}

// run owns the handler and local server while a health client makes one RPC.
func run(ctx context.Context, output io.Writer) (result error) {
	handler, err := slogcp.NewHandler(output)
	if err != nil {
		return fmt.Errorf("create slogcp handler %w", err)
	}
	defer func() { result = errors.Join(result, handler.Close()) }()
	adapted := slogcpadapter.NewLogger(handler)
	server := grpc.NewServer(grpc.ChainUnaryInterceptor(
		logging.UnaryServerInterceptor(adapted,
			logging.WithLogOnEvents(logging.FinishCall),
		),
	))
	defer server.Stop()
	healthService := health.NewServer()
	healthService.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, healthService)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open local listener %w", err)
	}
	defer func() { _ = listener.Close() }()
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("create local gRPC client %w", err)
	}
	defer func() { result = errors.Join(result, conn.Close()) }()
	response, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		return fmt.Errorf("check service health %w", err)
	}
	if response.Status != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("unexpected service health %v", response.Status)
	}
	server.GracefulStop()
	if err := <-serveErrors; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("serve local RPC %w", err)
	}
	return nil
}
