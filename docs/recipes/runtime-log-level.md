# Change the log level without rebuilding loggers

Use this recipe when an application's existing configuration watcher or
administrative control needs to adjust verbosity while the process runs. Keep
one slogcp handler and update its level variable so loggers already derived from
it observe the change. Go's [`slog.LevelVar`][levelvar] supports concurrent
reads and updates.

## Retain the handler's level control

This source-file excerpt includes its imports. Call `newControlledLogger` at
startup, handle its error, and keep both returned objects. Pass the logger to
application components as usual. Invoke `setLogLevel` from your existing
configuration mechanism and handle validation errors there.

```go
package app

import (
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/pjscruggs/slogcp"
)

func newControlledLogger(
	out io.Writer, projectID string,
) (*slog.Logger, *slogcp.Handler, error) {
	handler, err := slogcp.NewHandler(out,
		slogcp.WithRedirectWriter(out),
		slogcp.WithTraceProjectID(projectID),
		slogcp.WithLevel(slog.LevelInfo),
	)
	if err != nil {
		return nil, nil, err
	}
	return slog.New(handler).With("component", "worker"), handler, nil
}

func setLogLevel(handler *slogcp.Handler, value string) error {
	var level slog.Level
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "DEBUG":
		level = slog.LevelDebug
	case "INFO":
		level = slog.LevelInfo
	case "WARN":
		level = slog.LevelWarn
	case "ERROR":
		level = slog.LevelError
	default:
		return fmt.Errorf("log level must be DEBUG, INFO, WARN, or ERROR")
	}
	if handler == nil || handler.LevelVar() == nil {
		return fmt.Errorf("log handler is unavailable")
	}
	handler.LevelVar().Set(level)
	return nil
}
```

The four accepted names are this application's control policy. Invalid input
leaves the current level unchanged. The helper intentionally accepts only those
names; slogcp's [severity reference](../CONFIGURATION.md#severity-levels)
documents its additional levels and environment parsing separately.

The startup helper explicitly chooses INFO, overriding level environment
settings. Omit `WithLevel` if those settings should choose the initial level.
Environment variables are read during construction; changing one later is not a
live update mechanism. `handler.Level()` reports the current threshold.

An application that already owns a `*slog.LevelVar` can supply it with
`WithLevelVar`. Handler construction initializes that variable from the resolved
startup level, so presetting the variable is not a substitute for `WithLevel`.
See the [handler option contract](../../handler.go).

Changing the threshold affects base and derived loggers sharing this handler.
Existing outer filtering wrappers or additional destinations can impose other
thresholds. A DEBUG event still has DEBUG severity; lowering the threshold
changes whether it is accepted, not its severity. An update does not recover
previously discarded records, and concurrent in-flight records need not switch
at a single boundary across all outputs.

Use the application's existing authorization and configuration ownership for
runtime changes. If debug mode has an expiry, manage it in that same control
path so an old timer cannot overwrite a newer setting. Stop the watcher and
logging producers before closing the handler; writer ownership stays with the
application. See the [shutdown
guide](../USAGE.md#manage-configuration-and-shutdown).

## Verify changes on an existing child logger

Create a child with `logger.With("queue", "orders")` before changing the level.
With an in-memory writer, verify DEBUG is absent at startup, present after
setting DEBUG, and absent again after restoring INFO. The emitted DEBUG record
should retain both `component` and `queue`.

Try an invalid value and verify the threshold does not change. Exercise
concurrent logging and level updates with `go test -race ./...` in your
application module. This checks runtime behavior without adding a network
administration endpoint to the example.

[levelvar]: https://pkg.go.dev/log/slog#LevelVar
