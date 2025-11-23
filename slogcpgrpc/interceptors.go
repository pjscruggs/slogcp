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
	"net"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/pjscruggs/slogcp"
)

type requestInfoKey struct{}

// InfoFromContext retrieves the RequestInfo attached by slogcp interceptors.
func InfoFromContext(ctx context.Context) (*RequestInfo, bool) {
	if ctx == nil {
		return nil, false
	}
	info, ok := ctx.Value(requestInfoKey{}).(*RequestInfo)
	return info, ok && info != nil
}

// UnaryServerInterceptor derives a request-scoped logger for unary RPCs.
func UnaryServerInterceptor(opts ...Option) grpc.UnaryServerInterceptor {
	cfg := applyOptions(opts)
	projectID := resolveProjectID(cfg.projectID)

	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := time.Now()

		md, _ := metadata.FromIncomingContext(ctx)
		ctx, _ = ensureServerSpanContext(ctx, md, cfg)

		requestInfo := newRequestInfo(info.FullMethod, "unary", false, start)
		if cfg.includePeer {
			if peerAddr, ok := peerAddress(ctx); ok {
				requestInfo.setPeer(peerAddr)
			}
		}
		if cfg.includeSizes {
			requestInfo.recordRequest(req)
		}

		ctx = attachLogger(ctx, cfg, requestInfo, projectID)

		resp, err := handler(ctx, req)
		if cfg.includeSizes {
			requestInfo.recordResponse(resp)
		}
		requestInfo.finalize(status.Code(err), time.Since(start))
		return resp, err
	}
}

// StreamServerInterceptor derives a request-scoped logger for streaming RPCs.
func StreamServerInterceptor(opts ...Option) grpc.StreamServerInterceptor {
	cfg := applyOptions(opts)
	projectID := resolveProjectID(cfg.projectID)

	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		start := time.Now()
		ctx := ss.Context()

		md, _ := metadata.FromIncomingContext(ctx)
		ctx, _ = ensureServerSpanContext(ctx, md, cfg)

		kind := streamKind(info)
		requestInfo := newRequestInfo(info.FullMethod, kind, false, start)
		if cfg.includePeer {
			if peerAddr, ok := peerAddress(ctx); ok {
				requestInfo.setPeer(peerAddr)
			}
		}

		ctx = attachLogger(ctx, cfg, requestInfo, projectID)

		wrapped := &serverStream{
			ServerStream: ss,
			ctx:          ctx,
			info:         requestInfo,
			cfg:          cfg,
		}

		err := handler(srv, wrapped)
		requestInfo.finalize(status.Code(err), time.Since(start))
		return err
	}
}

// UnaryClientInterceptor derives a logger per outgoing unary RPC and injects trace metadata.
func UnaryClientInterceptor(opts ...Option) grpc.UnaryClientInterceptor {
	cfg := applyOptions(opts)
	projectID := resolveProjectID(cfg.projectID)

	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, callOpts ...grpc.CallOption) error {
		start := time.Now()

		requestInfo := newRequestInfo(method, "unary", true, start)
		if cfg.includeSizes {
			requestInfo.recordRequest(req)
		}

		ctx = attachLogger(ctx, cfg, requestInfo, projectID)

		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			md = metadata.New(nil)
		} else {
			md = md.Copy()
		}
		injectClientTrace(ctx, md, cfg)
		ctx = metadata.NewOutgoingContext(ctx, md)

		err := invoker(ctx, method, req, reply, cc, callOpts...)
		if cfg.includeSizes && err == nil {
			requestInfo.recordResponse(reply)
		}
		requestInfo.finalize(status.Code(err), time.Since(start))
		return err
	}
}

// StreamClientInterceptor derives a logger per outgoing streaming RPC and injects trace metadata.
func StreamClientInterceptor(opts ...Option) grpc.StreamClientInterceptor {
	cfg := applyOptions(opts)
	projectID := resolveProjectID(cfg.projectID)

	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, callOpts ...grpc.CallOption) (grpc.ClientStream, error) {
		start := time.Now()

		kind := clientStreamKind(desc)
		requestInfo := newRequestInfo(method, kind, true, start)

		ctx = attachLogger(ctx, cfg, requestInfo, projectID)

		md, ok := metadata.FromOutgoingContext(ctx)
		if !ok {
			md = metadata.New(nil)
		} else {
			md = md.Copy()
		}
		injectClientTrace(ctx, md, cfg)
		ctx = metadata.NewOutgoingContext(ctx, md)

		cs, err := streamer(ctx, desc, cc, method, callOpts...)
		if err != nil {
			requestInfo.finalize(status.Code(err), time.Since(start))
			return nil, err
		}

		wrapped := &clientStreamWrapper{
			ClientStream: cs,
			cfg:          cfg,
			info:         requestInfo,
			start:        start,
		}
		return wrapped, nil
	}
}

// ServerOptions returns grpc.ServerOptions that install otelgrpc StatsHandlers
// and slogcp interceptors.
func ServerOptions(opts ...Option) []grpc.ServerOption {
	cfg := applyOptions(opts)
	var serverOpts []grpc.ServerOption

	if cfg.enableOTel {
		serverOpts = append(serverOpts, grpc.StatsHandler(otelgrpc.NewServerHandler(statsHandlerOptions(cfg)...)))
	}

	serverOpts = append(serverOpts,
		grpc.ChainUnaryInterceptor(UnaryServerInterceptor(opts...)),
		grpc.ChainStreamInterceptor(StreamServerInterceptor(opts...)),
	)
	return serverOpts
}

// DialOptions returns grpc.DialOptions that install otelgrpc StatsHandlers and interceptors.
func DialOptions(opts ...Option) []grpc.DialOption {
	cfg := applyOptions(opts)
	var dialOpts []grpc.DialOption

	if cfg.enableOTel {
		dialOpts = append(dialOpts, grpc.WithStatsHandler(otelgrpc.NewClientHandler(statsHandlerOptions(cfg)...)))
	}

	dialOpts = append(dialOpts,
		grpc.WithChainUnaryInterceptor(UnaryClientInterceptor(opts...)),
		grpc.WithChainStreamInterceptor(StreamClientInterceptor(opts...)),
	)
	return dialOpts
}

// statsHandlerOptions configures otelgrpc instrumentation based on the provided configuration.
func statsHandlerOptions(cfg *config) []otelgrpc.Option {
	var opts []otelgrpc.Option
	if cfg.tracerProvider != nil {
		opts = append(opts, otelgrpc.WithTracerProvider(cfg.tracerProvider))
	}
	if cfg.propagatorsSet && cfg.propagators != nil {
		opts = append(opts, otelgrpc.WithPropagators(cfg.propagators))
	}
	return opts
}

// attachLogger adds a request-scoped logger and RequestInfo to the context.
func attachLogger(ctx context.Context, cfg *config, info *RequestInfo, projectID string) context.Context {
	traceAttrs, _ := slogcp.TraceAttributes(ctx, projectID)
	attrs := info.loggerAttrs(cfg, traceAttrs)
	for _, enricher := range cfg.attrEnrichers {
		if enricher == nil {
			continue
		}
		if extra := enricher(ctx, info); len(extra) > 0 {
			attrs = append(attrs, extra...)
		}
	}
	for _, transformer := range cfg.attrTransformers {
		if transformer == nil {
			continue
		}
		attrs = transformer(ctx, attrs, info)
	}

	baseLogger := cfg.logger
	if baseLogger == nil {
		baseLogger = slogcp.Logger(ctx)
	}
	requestLogger := loggerWithAttrs(baseLogger, attrs)

	ctx = slogcp.ContextWithLogger(ctx, requestLogger)
	ctx = context.WithValue(ctx, requestInfoKey{}, info)
	return ctx
}

// resolveProjectID selects the explicit ID if provided, otherwise falling back to runtime detection.
func resolveProjectID(explicit string) string {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		return explicit
	}
	return strings.TrimSpace(slogcp.DetectRuntimeInfo().ProjectID)
}

// peerAddress extracts the remote host portion of the peer address in the context.
func peerAddress(ctx context.Context) (string, bool) {
	pr, ok := peer.FromContext(ctx)
	if !ok || pr == nil || pr.Addr == nil {
		return "", false
	}
	addr := pr.Addr.String()
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host, true
	}
	return addr, true
}

// streamKind converts gRPC stream information into a canonical kind string.
func streamKind(info *grpc.StreamServerInfo) string {
	switch {
	case info.IsClientStream && info.IsServerStream:
		return "bidi_stream"
	case info.IsClientStream:
		return "client_stream"
	case info.IsServerStream:
		return "server_stream"
	default:
		return "unary"
	}
}

// clientStreamKind converts a StreamDesc into the kind string used for logging.
func clientStreamKind(desc *grpc.StreamDesc) string {
	switch {
	case desc.ClientStreams && desc.ServerStreams:
		return "bidi_stream"
	case desc.ClientStreams:
		return "client_stream"
	case desc.ServerStreams:
		return "server_stream"
	default:
		return "unary"
	}
}

type serverStream struct {
	grpc.ServerStream
	ctx  context.Context
	info *RequestInfo
	cfg  *config
}

// Context returns the request context for the wrapped server stream.
func (s *serverStream) Context() context.Context {
	return s.ctx
}

// RecvMsg records inbound payload sizes before delegating to the underlying stream.
func (s *serverStream) RecvMsg(m any) error {
	err := s.ServerStream.RecvMsg(m)
	if err == nil && s.cfg.includeSizes {
		s.info.recordRequest(m)
	}
	return err
}

// SendMsg records outbound payload sizes before delegating to the underlying stream.
func (s *serverStream) SendMsg(m any) error {
	if s.cfg.includeSizes {
		s.info.recordResponse(m)
	}
	return s.ServerStream.SendMsg(m)
}

type clientStreamWrapper struct {
	grpc.ClientStream
	cfg   *config
	info  *RequestInfo
	start time.Time
	once  sync.Once
}

// Context returns the context for the wrapped client stream.
func (c *clientStreamWrapper) Context() context.Context {
	return c.ClientStream.Context()
}

// SendMsg records outbound payload sizes and finalizes the request on error.
func (c *clientStreamWrapper) SendMsg(m any) error {
	if c.cfg.includeSizes {
		c.info.recordRequest(m)
	}
	err := c.ClientStream.SendMsg(m)
	if err != nil {
		c.finish(status.Code(err))
	}
	return err
}

// RecvMsg records inbound payload sizes and finalizes the request when the stream ends.
func (c *clientStreamWrapper) RecvMsg(m any) error {
	err := c.ClientStream.RecvMsg(m)
	if err == nil {
		if c.cfg.includeSizes {
			c.info.recordResponse(m)
		}
		return nil
	}
	if err == io.EOF {
		c.finish(codes.OK)
	} else {
		c.finish(status.Code(err))
	}
	return err
}

// CloseSend closes the client stream and finalizes the request on error.
func (c *clientStreamWrapper) CloseSend() error {
	err := c.ClientStream.CloseSend()
	if err != nil {
		c.finish(status.Code(err))
	}
	return err
}

// finish finalizes the RequestInfo exactly once with the provided gRPC status code.
func (c *clientStreamWrapper) finish(code codes.Code) {
	c.once.Do(func() {
		c.info.finalize(code, time.Since(c.start))
	})
}

// loggerWithAttrs returns a logger augmented with the supplied attributes.
func loggerWithAttrs(base *slog.Logger, attrs []slog.Attr) *slog.Logger {
	if base == nil {
		base = slog.Default()
	}
	if len(attrs) == 0 {
		return base
	}
	args := make([]any, len(attrs))
	for i, attr := range attrs {
		args[i] = attr
	}
	return base.With(args...)
}
