# slogcpasync

`github.com/pjscruggs/slogcp/slogcpasync` provides an opt-in async wrapper for `log/slog` handlers, including `slogcp`'s handler:

- `Wrap` queues `slog.Record`s on a bounded channel and drains them with worker goroutines.
- `Middleware` returns a `func(slog.Handler) slog.Handler` for use with `slogcp.WithMiddleware(...)`.
- Configurable overload behavior: block, drop newest, or drop oldest.
- `Close()` flushes queued records (optionally with a timeout) and calls `Close` on the wrapped handler when available.

It is used automatically by `slogcp` when logging to file targets, but remains opt-in for `stdout`/`stderr`.

## Install

```bash
go get github.com/pjscruggs/slogcp
```

Import: `github.com/pjscruggs/slogcp/slogcpasync`

## Wrap a handler

```go
base := slog.NewJSONHandler(os.Stdout, nil)
async := slogcpasync.Wrap(base,
	slogcpasync.WithQueueSize(4096),
	slogcpasync.WithDropMode(slogcpasync.DropModeDropNewest),
)
if closer, ok := async.(interface{ Close() error }); ok {
	defer closer.Close()
}

logger := slog.New(async)
logger.Info("hello")
```

## Use with `slogcp`

Enable async buffering for non-file targets via `slogcp.WithAsync(...)`:

```go
handler, err := slogcp.NewHandler(os.Stdout,
	slogcp.WithAsync(
		slogcpasync.WithQueueSize(4096),
		slogcpasync.WithDropMode(slogcpasync.DropModeDropNewest),
	),
)
if err != nil {
	log.Fatal(err)
}
defer handler.Close()
```

Tune (or disable) buffering on file targets via `slogcp.WithAsyncOnFileTargets(...)`:

```go
handler, err := slogcp.NewHandler(nil,
	slogcp.WithRedirectToFile("app.json"),
	slogcp.WithAsyncOnFileTargets(
		slogcpasync.WithEnabled(false),
	),
)
if err != nil {
	log.Fatal(err)
}
defer handler.Close()
```

## Environment-driven opt-in

To keep services synchronous by default but enable async via `SLOGCP_ASYNC_*` environment variables, combine `WithEnabled(false)` with `WithEnv()`:

```go
handler, err := slogcp.NewHandler(os.Stdout,
	slogcp.WithMiddleware(slogcpasync.Middleware(
		slogcpasync.WithEnabled(false),
		slogcpasync.WithEnv(),
	)),
)
if err != nil {
	log.Fatal(err)
}
defer handler.Close()
```

## Options

| Option | Environment variable | Default | Description |
| --- | --- | --- | --- |
| `WithEnabled(bool)` | `SLOGCP_ASYNC` | `true` once added | Enables or disables the wrapper (disabled returns the original handler). |
| `WithQueueSize(int)` | `SLOGCP_ASYNC_QUEUE_SIZE` | `block: 2048`, `drop_newest: 512`, `drop_oldest: 1024` | Queue capacity; `0` makes the queue unbuffered. |
| `WithWorkerCount(int)` | `SLOGCP_ASYNC_WORKERS` | `1` | Number of goroutines draining the queue. |
| `WithBatchSize(int)` | (none) | `1` | Records a worker drains per wake-up; values `< 1` clamp to the mode default. |
| `WithDropMode(DropMode)` | `SLOGCP_ASYNC_DROP_MODE` | `block` | Overflow policy: `block`, `drop_newest`, or `drop_oldest` (hyphenated forms are also accepted). |
| `WithFlushTimeout(time.Duration)` | `SLOGCP_ASYNC_FLUSH_TIMEOUT` | (none) | Optional timeout for `Close`; returns `ErrFlushTimeout` if workers never finish. |
| `WithOnDrop(func(context.Context, slog.Record))` | (none) | (none) | Callback invoked when a record is dropped (or logged after `Close`). |
| `WithErrorWriter(io.Writer)` | (none) | `os.Stderr` | Destination for worker errors/panic reports; `nil` silences them. |
| `WithEnv()` | (reads all of the above) | (none) | Overlays any `SLOGCP_ASYNC_*` values onto the config. |

## Notes

- `DropModeBlock` can still block callers under sustained log throughput if the queue fills.
- Worker errors and panics are not returned from `Handle`; they are reported via `WithErrorWriter` to avoid silent failure.
- Whenever the async wrapper is in play, call `Close()` on shutdown to flush queued records.

## More

- Configuration reference: `../docs/CONFIGURATION.md` (see "Async logging (`slogcpasync`)")
- Runnable example: `../.examples/async`
