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

package slogcpgrpc

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/pjscruggs/slogcp"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"
)

// BenchmarkUnaryServerInterceptor measures interceptor overhead with logging in the handler.
func BenchmarkUnaryServerInterceptor(b *testing.B) {
	handler, err := slogcp.NewHandler(io.Discard)
	if err != nil {
		b.Fatalf("NewHandler returned error: %v", err)
	}
	b.Cleanup(func() { _ = handler.Close() })

	logger := slog.New(handler)

	interceptor := UnaryServerInterceptor(
		WithLogger(logger),
		WithProjectID("bench-project"),
		WithOTel(false),
	)

	info := &grpc.UnaryServerInfo{FullMethod: "/bench.Service/Method"}
	req := &emptypb.Empty{}
	resp := &emptypb.Empty{}

	baseHandler := func(ctx context.Context, _ any) (any, error) {
		slogcp.Logger(ctx).Info("bench", slog.String("rpc", "ok"))
		return resp, nil
	}

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		if _, err := interceptor(ctx, req, info, baseHandler); err != nil {
			b.Fatalf("interceptor returned error: %v", err)
		}
	}
}
