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

// Command grpc demonstrates slogcp's gRPC helpers by wiring interceptors into a
// small Greeter service and exercising a client call.
//
// This example is both documentation, and a test for `slogcp`.
// Our Github workflow tests if any changes to `slogcp` break the example.
package main

import (
	"context"
	"errors"
	"log"
	"log/slog"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	pb "google.golang.org/grpc/examples/helloworld/helloworld"

	"github.com/pjscruggs/slogcp"
	"github.com/pjscruggs/slogcp/slogcpgrpc"
)

type greeterServer struct {
	pb.UnimplementedGreeterServer
	logger *slog.Logger
}

// SayHello implements the Greeter service and logs the request name.
func (s *greeterServer) SayHello(ctx context.Context, req *pb.HelloRequest) (*pb.HelloReply, error) {
	s.logger.InfoContext(ctx, "say hello", slog.String("name", req.GetName()))
	return &pb.HelloReply{Message: "Hello " + req.GetName()}, nil
}

// main starts a gRPC Greeter server and invokes it with slogcp logging.
func main() {
	handler, err := slogcp.NewHandler(os.Stdout)
	if err != nil {
		log.Fatalf("failed to create handler: %v", err)
	}
	defer handler.Close()

	logger := slog.New(handler)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listener error: %v", err)
	}

	server := grpc.NewServer(slogcpgrpc.ServerOptions(
		slogcpgrpc.WithLogger(logger),
	)...)
	pb.RegisterGreeterServer(server, &greeterServer{logger: logger})

	go func() {
		if serveErr := server.Serve(lis); serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			logger.Error("grpc server error", slog.String("error", serveErr.Error()))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialOptions := append([]grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())},
		slogcpgrpc.DialOptions(slogcpgrpc.WithLogger(logger))...,
	)

	conn, err := grpc.DialContext(ctx, lis.Addr().String(), dialOptions...)
	if err != nil {
		log.Fatalf("dial error: %v", err)
	}
	defer conn.Close()

	client := pb.NewGreeterClient(conn)
	resp, err := client.SayHello(ctx, &pb.HelloRequest{Name: "slogcp"})
	if err != nil {
		log.Fatalf("SayHello error: %v", err)
	}
	logger.Info("gRPC response", slog.String("message", resp.Message))

	server.GracefulStop()
}
