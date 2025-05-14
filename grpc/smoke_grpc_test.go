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

// Package grpc_test contains smoke tests for slogcp’s gRPC interceptors.
// The tests call the unary‑server interceptor directly, passing a dummy
// handler and minimal grpc.UnaryServerInfo, to confirm that happy‑path
// invocations complete without error and propagate the response value.
package grpc_test

import (
	"context"
	"testing"

	"google.golang.org/grpc"

	"github.com/pjscruggs/slogcp"
	slogcpgrpc "github.com/pjscruggs/slogcp/grpc"
)

func newTestLogger(t *testing.T) *slogcp.Logger {
	t.Helper()

	lg, err := slogcp.New(slogcp.WithRedirectToStdout())
	if err != nil {
		t.Fatalf("slogcp.New() returned %v, want nil", err)
	}
	return lg
}

// TestUnaryServerInterceptorSmoke invokes the unary‑server interceptor with a
// dummy handler and asserts that it forwards the call and returns nil error.
func TestUnaryServerInterceptorSmoke(t *testing.T) {
	t.Parallel()

	lg := newTestLogger(t)
	defer func() {
		if err := lg.Close(); err != nil {
			t.Errorf("Logger.Close() returned %v, want nil", err)
		}
	}()

	interceptor := slogcpgrpc.UnaryServerInterceptor(lg)

	info := &grpc.UnaryServerInfo{FullMethod: "/pkg.Service/Method"}
	handler := func(context.Context, interface{}) (interface{}, error) { return "ok", nil }

	got, err := interceptor(context.Background(), "req", info, handler)
	if err != nil {
		t.Fatalf("UnaryServerInterceptor() returned %v, want nil", err)
	}
	if got != "ok" {
		t.Errorf("UnaryServerInterceptor() response = %v, want %q", got, "ok")
	}
}
