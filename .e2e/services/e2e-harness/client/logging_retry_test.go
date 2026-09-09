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

package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	logpb "cloud.google.com/go/logging/apiv2/loggingpb"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type logTransport func(*http.Request) (*http.Response, error)

func (f logTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func logResponse(code int, body string) *http.Response {
	return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestJSONQuotaRetryPreservesPageAndEmptyContinuation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var tokens []string
		responses := []*http.Response{
			logResponse(200, `{"entries":[{"insertId":"one"}],"nextPageToken":"two"}`),
			logResponse(429, `quota exceeded`),
			logResponse(200, `{"entries":[],"nextPageToken":"three"}`),
			logResponse(200, `{"entries":[{"insertId":"two"}]}`),
		}
		c := &LoggingClient{projectID: "fixture", httpClient: &http.Client{Transport: logTransport(func(r *http.Request) (*http.Response, error) {
			var request listEntriesRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			tokens = append(tokens, request.PageToken)
			if len(tokens) > len(responses) {
				t.Fatal("unexpected request")
			}
			return responses[len(tokens)-1], nil
		})}}
		start := time.Now()
		entries, err := c.QueryLogsJSON(t.Context(), QueryOptions{})
		if err != nil || len(entries) != 2 {
			t.Fatalf("entries=%v err=%v", entries, err)
		}
		if !reflect.DeepEqual(tokens, []string{"", "two", "two", "three"}) {
			t.Fatal(tokens)
		}
		if elapsed := time.Since(start); elapsed < 30*time.Second || elapsed >= 35*time.Second {
			t.Fatal(elapsed)
		}
	})
}

func TestLogQuotaDoesNotHidePermanentErrors(t *testing.T) {
	for _, code := range []int{400, 401, 403} {
		calls := 0
		c := &LoggingClient{httpClient: &http.Client{Transport: logTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return logResponse(code, "not permitted"), nil
		})}}
		if _, err := c.QueryLogsJSON(t.Context(), QueryOptions{}); err == nil || calls != 1 {
			t.Fatalf("code=%d calls=%d err=%v", code, calls, err)
		}
	}
	for _, code := range []codes.Code{codes.PermissionDenied, codes.Unauthenticated, codes.InvalidArgument} {
		calls := 0
		err := retryLogQuota(t.Context(), func() error { calls++; return status.Error(code, "permanent") })
		if status.Code(err) != code || calls != 1 {
			t.Fatalf("code=%v calls=%d err=%v", code, calls, err)
		}
	}
}

func TestLogQuotaDeadlineAndAttemptLimit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		c := &LoggingClient{httpClient: &http.Client{Transport: logTransport(func(*http.Request) (*http.Response, error) {
			calls++
			return logResponse(429, "quota"), nil
		})}}
		start := time.Now()
		_, err := c.WaitForLogsJSON(t.Context(), QueryOptions{}, 40*time.Second)
		if !errors.Is(err, context.DeadlineExceeded) || calls != 2 || time.Since(start) != 40*time.Second {
			t.Fatalf("calls=%d elapsed=%v err=%v", calls, time.Since(start), err)
		}
		calls = 0
		_, err = c.QueryLogsJSON(t.Context(), QueryOptions{})
		if err == nil || calls != 4 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
}

func TestLogQuotaCancellationAndMalformedResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		go func() { time.Sleep(time.Second); cancel() }()
		err := retryLogQuota(ctx, func() error { calls++; return status.Error(codes.ResourceExhausted, "quota") })
		if !errors.Is(err, context.Canceled) || calls != 1 {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
	calls := 0
	c := &LoggingClient{httpClient: &http.Client{Transport: logTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return logResponse(200, "not JSON"), nil
	})}}
	if _, err := c.QueryLogsJSON(t.Context(), QueryOptions{}); err == nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

type quotaLogServer struct {
	logpb.UnimplementedLoggingServiceV2Server
	tokens      []string
	alwaysQuota bool
}

func (s *quotaLogServer) ListLogEntries(_ context.Context, r *logpb.ListLogEntriesRequest) (*logpb.ListLogEntriesResponse, error) {
	s.tokens = append(s.tokens, r.PageToken)
	if s.alwaysQuota {
		return nil, status.Error(codes.ResourceExhausted, "quota")
	}
	switch len(s.tokens) {
	case 1:
		return &logpb.ListLogEntriesResponse{Entries: []*logpb.LogEntry{{InsertId: "one", Timestamp: timestamppb.Now()}}, NextPageToken: "next"}, nil
	case 2:
		return nil, status.Error(codes.ResourceExhausted, "quota")
	default:
		return &logpb.ListLogEntriesResponse{Entries: []*logpb.LogEntry{{InsertId: "two", Timestamp: timestamppb.Now()}}}, nil
	}
}

func TestLogadminRetriesThrottledPage(t *testing.T) {
	for _, persistent := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			listener := bufconn.Listen(1 << 20)
			defer listener.Close()
			server := grpc.NewServer()
			fixture := &quotaLogServer{alwaysQuota: persistent}
			logpb.RegisterLoggingServiceV2Server(server, fixture)
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			c, err := NewLoggingClient(t.Context(), "fixture", option.WithoutAuthentication(),
				option.WithEndpoint("passthrough:///fixture"),
				option.WithGRPCDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())),
				option.WithGRPCDialOption(grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() })))
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			start := time.Now()
			entries, err := c.WaitForLogCount(t.Context(), QueryOptions{}, 2, 40*time.Second)
			if persistent {
				if err == nil || time.Since(start) != 40*time.Second || len(fixture.tokens) != 2 {
					t.Fatalf("tokens=%v elapsed=%v err=%v", fixture.tokens, time.Since(start), err)
				}
				return
			}
			if err != nil || len(entries) != 2 {
				t.Fatalf("entries=%v err=%v", entries, err)
			}
			if !reflect.DeepEqual(fixture.tokens, []string{"", "next", "next"}) {
				t.Fatal(fixture.tokens)
			}
		})
	}
}

func TestMissingLogsStillFailWithinDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := &LoggingClient{httpClient: &http.Client{Transport: logTransport(func(*http.Request) (*http.Response, error) {
			return logResponse(200, `{"entries":[]}`), nil
		})}}
		start := time.Now()
		entries, err := c.WaitForLogsJSON(t.Context(), QueryOptions{}, 5*time.Second)
		if err == nil || len(entries) != 0 || time.Since(start) != 5*time.Second {
			t.Fatalf("entries=%v elapsed=%v err=%v", entries, time.Since(start), err)
		}
	})
}
