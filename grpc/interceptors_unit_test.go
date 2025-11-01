package grpc

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/pjscruggs/slogcp/chatter"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type recordingState struct {
	mu      sync.Mutex
	records []slog.Record
}

type recordingHandler struct {
	state *recordingState
}

// Enabled reports whether the handler is enabled for the provided log level.
func (h *recordingHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

// Handle records the provided slog record for later inspection.
func (h *recordingHandler) Handle(_ context.Context, record slog.Record) error {
	clone := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clone.AddAttrs(attr)
		return true
	})
	h.state.mu.Lock()
	h.state.records = append(h.state.records, clone)
	h.state.mu.Unlock()
	return nil
}

// WithAttrs implements slog.Handler by returning the handler unchanged.
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler {
	return h
}

// WithGroup implements slog.Handler by returning the handler unchanged.
func (h *recordingHandler) WithGroup(string) slog.Handler {
	return h
}

// Snapshot returns a copy of the recorded slog records.
func (s *recordingState) Snapshot() []slog.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]slog.Record, len(s.records))
	copy(out, s.records)
	return out
}

// Count returns the number of recorded slog records.
func (s *recordingState) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.records)
}

// TestUnaryServerInterceptorRecoversWhenSuppressed verifies panic recovery when logging is suppressed.
func TestUnaryServerInterceptorRecoversWhenSuppressed(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{}))

	interceptor := UnaryServerInterceptor(logger, WithShouldLog(func(context.Context, string) bool {
		return false
	}))

	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Panic"}

	didPanic := false
	invoke := func() {
		defer func() {
			if r := recover(); r != nil {
				didPanic = true
			}
		}()

		_, err := interceptor(context.Background(), nil, info, func(context.Context, any) (any, error) {
			panic("kaboom")
		})
		if err == nil {
			t.Fatalf("interceptor returned nil error after panic")
		}
		if status.Code(err) != codes.Internal {
			t.Fatalf("error code = %v, want %v", status.Code(err), codes.Internal)
		}
	}
	invoke()

	if didPanic {
		t.Fatalf("interceptor should recover from panic even when logging is suppressed")
	}
}

// TestUnaryServerInterceptorForcesLogOnErrorWhenSuppressed ensures forced error logging under suppression.
func TestUnaryServerInterceptorForcesLogOnErrorWhenSuppressed(t *testing.T) {
	state := &recordingState{}
	logger := slog.New(&recordingHandler{state: state})

	cfg := chatter.DefaultConfig()
	cfg.Mode = chatter.ModeOn
	cfg.Action = chatter.ActionDrop
	cfg.GRPC.IgnoreMethods = []string{"/test.Service/Error"}

	interceptor := UnaryServerInterceptor(
		logger,
		WithShouldLog(func(context.Context, string) bool { return false }),
		WithChatterConfig(cfg),
	)

	info := &grpc.UnaryServerInfo{FullMethod: "/test.Service/Error"}

	_, err := interceptor(context.Background(), nil, info, func(context.Context, any) (any, error) {
		return nil, status.Error(codes.Internal, "boom")
	})
	if err == nil {
		t.Fatalf("interceptor returned nil error")
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("error code = %v, want %v", status.Code(err), codes.Internal)
	}

	records := state.Snapshot()
	if len(records) == 0 {
		t.Fatalf("expected forced log for error, got none")
	}

	found := false
	for _, rec := range records {
		if rec.Message != "Finished gRPC call" {
			continue
		}
		if rec.Level != slog.LevelError {
			t.Fatalf("finish log level = %v, want %v", rec.Level, slog.LevelError)
		}
		rec.Attrs(func(attr slog.Attr) bool {
			if attr.Key == grpcCodeKey && attr.Value.String() == codes.Internal.String() {
				found = true
				return false
			}
			return true
		})
		if found {
			break
		}
	}
	if !found {
		t.Fatalf("forced finish log missing grpc code for internal error")
	}
}
