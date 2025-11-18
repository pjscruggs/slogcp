package slogcp_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/pjscruggs/slogcp"
)

// TestContextWithLoggerStoresAndRetrievesLogger verifies that ContextWithLogger
// stores custom loggers and Logger retrieves overrides and fallbacks correctly.
func TestContextWithLoggerStoresAndRetrievesLogger(t *testing.T) {
	t.Parallel()

	defaultLogger := slog.Default()
	if got := slogcp.Logger(context.Background()); got != defaultLogger {
		t.Fatalf("Logger(context.Background()) = %v, want default logger %v", got, defaultLogger)
	}

	custom := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := slogcp.ContextWithLogger(context.Background(), custom)
	if got := slogcp.Logger(ctx); got != custom {
		t.Fatalf("Logger(ctx) = %v, want %v", got, custom)
	}

	overridden := slog.New(slog.NewJSONHandler(io.Discard, nil))
	ctx = slogcp.ContextWithLogger(ctx, overridden)
	if got := slogcp.Logger(ctx); got != overridden {
		t.Fatalf("Logger(ctx after override) = %v, want %v", got, overridden)
	}
}
