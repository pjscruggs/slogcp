# slogcphttp

`github.com/pjscruggs/slogcp/slogcphttp` provides `net/http` helpers for slogcp:

- `Middleware` derives a request-scoped `*slog.Logger`, stores it on the request `context.Context`, and enriches logs with request and trace metadata.
- `Transport` wraps an `http.RoundTripper` to inject trace headers on outbound requests and annotate the outbound request context with request attributes.

It enriches application logs; it does not emit access logs by itself.

## Install

```bash
go get github.com/pjscruggs/slogcp
```

Import: `github.com/pjscruggs/slogcp/slogcphttp`

## Server middleware

```go
handler, err := slogcp.NewHandler(os.Stdout)
if err != nil {
	log.Fatal(err)
}
logger := slog.New(handler)

mux := http.NewServeMux()
mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
	slogcp.Logger(r.Context()).Info("health probe")
	w.WriteHeader(http.StatusNoContent)
})

wrapped := slogcphttp.Middleware(
	slogcphttp.WithLogger(logger),
	// If your router can provide a low-cardinality template (recommended), set it:
	// slogcphttp.WithRouteGetter(func(r *http.Request) string { return "/widgets/:id" }),
)(mux)

_ = http.ListenAndServe(":8080", wrapped)
```

`Middleware` wraps the handler with `otelhttp.NewHandler` by default so spans are created automatically and `slogcp` can correlate logs with traces when trace context exists.

By default:

- The derived logger includes trace correlation fields (when a span is present) plus request metadata like `http.method`, `http.target`, `http.scheme`, `http.host`, `http.status_code`, and `http.response_size`.
- `http.route` is omitted unless you provide `WithRouteGetter`.
- Query strings are omitted (`WithIncludeQuery(true)` opts in).
- `User-Agent` is omitted (`WithUserAgent(true)` opts in).
- Client IP (`network.peer.ip`) is included when available (`WithClientIP(false)` disables it).

You can inspect request metrics (status, sizes, latency) from handlers via:

```go
scope, ok := slogcphttp.ScopeFromContext(r.Context())
_ = scope
_ = ok
```

## OpenTelemetry and trace propagation

By default, `Middleware`:

- creates server spans via `otelhttp.NewHandler`, and
- extracts/injects trace context using the configured (or global) OpenTelemetry propagator.

Options:

- `WithOTel(false)` disables the `otelhttp` wrapper (no spans are created by this middleware).
- `WithTracePropagation(false)` disables reading/writing trace context headers. When `WithOTel(true)`, spans are still created, but they start new root traces instead of continuing any incoming trace.
- `WithPublicEndpoint(true)` tells `otelhttp` to start a new root span and link any inbound context instead of parenting (useful for untrusted/public edges). When `WithOTel(false)`, logs do not correlate to inbound trace headers by default; opt in with `WithPublicEndpointCorrelateLogsToRemote(true)`.
- `WithPropagators(...)` supplies a specific propagator for header extraction/injection.

## Proxy-aware metadata (Cloud Run / GCLB)

Cloud Run, Cloud Functions, and App Engine terminate TLS before requests reach your code, so `r.TLS` is not a reliable way to determine whether the *original* client request used HTTPS.

By default, slogcphttp trusts `X-Forwarded-Proto` for **scheme only** when it detects one of those serverless environments (so `http.scheme` and derived URLs remain correct). If you need to disable this, set `WithTrustXForwardedProto(false)`.

Client IP parsing remains opt-in: if you're behind Google Cloud Load Balancing and want to derive `network.peer.ip` from `X-Forwarded-For`, enable proxy mode:

```go
wrapped := slogcphttp.Middleware(
	slogcphttp.WithLogger(logger),
	slogcphttp.WithProxyMode(slogcphttp.ProxyModeGCLB),
)(mux)
```

Proxy mode is off by default because forwarded headers are trivial to spoof if requests can bypass your load balancer/proxy.

In `ProxyModeGCLB`, client IP is derived from `X-Forwarded-For` using the "count from the right" convention (default is 2 = "second from right"). This avoids trusting client-supplied prefixes in `X-Forwarded-For` when a load balancer appends `,<client-ip>,<lb-ip>`.

If your infrastructure appends additional trusted hops to the right, adjust with `WithXForwardedForClientIPFromRight(n)`.

## Outbound HTTP client

```go
client := &http.Client{
	Transport: slogcphttp.Transport(nil),
}

req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://example.com", nil)
resp, err := client.Do(req)
_ = resp
_ = err
```

`Transport` injects W3C trace context headers using the configured (or global) OpenTelemetry propagator when an active span is present in the request context. Use `WithTracePropagation(false)` to disable injection, or `WithLegacyXCloudInjection(true)` if you also need the legacy `X-Cloud-Trace-Context` header on outbound requests.

The derived outbound logger includes request metadata such as `http.method`, `http.target`, `http.scheme`, `http.host`, and `server.address`.

## Custom attributes

- `WithAttrEnricher(func(*http.Request, *RequestScope) []slog.Attr)` appends custom fields to the derived logger.
- `WithAttrTransformer(func([]slog.Attr, *http.Request, *RequestScope) []slog.Attr)` can redact or reshape the derived attributes before they are applied.

## Optional: Cloud Logging `httpRequest`

On Cloud Run, you'll typically rely on the platform's own request logs and use trace correlation to pivot between those request logs and your application logs.

If you still need to emit a Cloud Logging `httpRequest` payload (for custom access logs or non-Cloud Run deployments), enable it explicitly on the derived logger:

```go
wrapped := slogcphttp.Middleware(
	slogcphttp.WithLogger(logger),
	slogcphttp.WithHTTPRequestAttr(true),
)(mux)
```

To add the payload to a single log entry, use `HTTPRequestAttrFromContext(ctx, r)`.

## More

- Configuration reference: `../docs/CONFIGURATION.md` (see "HTTP Integration")
- Runnable examples: `../.examples/http-server`, `../.examples/http-client`, `../.examples/http-otel`
