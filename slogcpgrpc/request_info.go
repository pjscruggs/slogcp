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

package slogcpgrpc

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
)

// RequestInfo captures per-RPC metadata such as method, sizes, latency, and status.
type RequestInfo struct {
	fullMethod string
	service    string
	method     string
	kind       string
	client     bool
	start      time.Time
	status     atomic.Uint32
	latencyNS  atomic.Int64
	reqBytes   atomic.Int64
	respBytes  atomic.Int64
	reqCount   atomic.Int64
	respCount  atomic.Int64
	peer       atomic.Value
}

const unsetLatencySentinel = int64(-1)

// newRequestInfo constructs a RequestInfo with derived service and method details.
func newRequestInfo(fullMethod, kind string, client bool, start time.Time) *RequestInfo {
	service, method := splitFullMethod(fullMethod)
	info := &RequestInfo{
		fullMethod: fullMethod,
		service:    service,
		method:     method,
		kind:       kind,
		client:     client,
		start:      start,
	}
	info.status.Store(uint32(codes.OK))
	info.latencyNS.Store(unsetLatencySentinel)
	return info
}

// setPeer records the remote peer address for the request.
func (ri *RequestInfo) setPeer(peer string) {
	ri.peer.Store(peer)
}

// Peer returns the recorded remote peer address, if any.
func (ri *RequestInfo) Peer() string {
	if v := ri.peer.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// recordRequest tracks request payload sizes and counts.
func (ri *RequestInfo) recordRequest(msg any) {
	if msg == nil {
		return
	}
	if size := messageSize(msg); size > 0 {
		ri.reqBytes.Add(size)
	}
	ri.reqCount.Add(1)
}

// recordResponse tracks response payload sizes and counts.
func (ri *RequestInfo) recordResponse(msg any) {
	if msg == nil {
		return
	}
	if size := messageSize(msg); size > 0 {
		ri.respBytes.Add(size)
	}
	ri.respCount.Add(1)
}

// finalize stores the terminal status code and latency for the request.
func (ri *RequestInfo) finalize(code codes.Code, duration time.Duration) {
	if duration < 0 {
		duration = 0
	}
	ri.status.Store(uint32(code))
	ri.latencyNS.Store(duration.Nanoseconds())
}

// Status returns the recorded gRPC status code.
func (ri *RequestInfo) Status() codes.Code {
	code := ri.status.Load()
	return codes.Code(code)
}

// Latency returns the recorded latency or the elapsed time if finalization has not occurred.
func (ri *RequestInfo) Latency() time.Duration {
	if ns := ri.latencyNS.Load(); ns != unsetLatencySentinel {
		return time.Duration(ns)
	}
	return time.Since(ri.start)
}

// RequestBytes returns the cumulative size of request payloads.
func (ri *RequestInfo) RequestBytes() int64 {
	return ri.reqBytes.Load()
}

// ResponseBytes returns the cumulative size of response payloads.
func (ri *RequestInfo) ResponseBytes() int64 {
	return ri.respBytes.Load()
}

// RequestCount returns the number of request messages observed.
func (ri *RequestInfo) RequestCount() int64 {
	return ri.reqCount.Load()
}

// ResponseCount returns the number of response messages observed.
func (ri *RequestInfo) ResponseCount() int64 {
	return ri.respCount.Load()
}

// loggerAttrs builds structured logging attributes for the request.
func (ri *RequestInfo) loggerAttrs(cfg *config, traceAttrs []slog.Attr) []slog.Attr {
	capHint := len(traceAttrs) + 8
	if cfg.includeSizes {
		capHint += 4
	}
	if cfg.includePeer {
		capHint++
	}
	attrs := make([]slog.Attr, 0, capHint)
	if len(traceAttrs) > 0 {
		attrs = append(attrs, traceAttrs...)
	}
	attrs = ri.appendBaseRPCAttrs(attrs)
	attrs = append(attrs, ri.statusAttr(), ri.durationAttr())
	attrs = ri.appendSizeAttrs(attrs, cfg)
	attrs = ri.appendPeerAttr(attrs, cfg)
	return attrs
}

// appendBaseRPCAttrs appends the core RPC attributes for the request.
func (ri *RequestInfo) appendBaseRPCAttrs(attrs []slog.Attr) []slog.Attr {
	attrs = append(attrs, slog.String("rpc.system", "grpc"))
	if ri.service != "" {
		attrs = append(attrs, slog.String("rpc.service", ri.service))
	}
	if ri.method != "" {
		attrs = append(attrs, slog.String("rpc.method", ri.method))
	}
	if ri.kind != "" {
		attrs = append(attrs, slog.String("grpc.type", ri.kind))
	}
	return attrs
}

type requestInfoValueKind uint8

const (
	requestInfoStatus requestInfoValueKind = iota
	requestInfoDuration
	requestInfoRequestBytes
	requestInfoResponseBytes
	requestInfoRequestCount
	requestInfoResponseCount
)

type requestInfoValue struct {
	info *RequestInfo
	kind requestInfoValueKind
}

// LogValue implements slog.LogValuer for deferred RequestInfo evaluation.
func (v requestInfoValue) LogValue() slog.Value {
	if v.info == nil {
		return slog.Value{}
	}
	switch v.kind {
	case requestInfoStatus:
		return slog.StringValue(v.info.Status().String())
	case requestInfoDuration:
		return slog.DurationValue(v.info.Latency())
	case requestInfoRequestBytes:
		return slog.Int64Value(v.info.RequestBytes())
	case requestInfoResponseBytes:
		return slog.Int64Value(v.info.ResponseBytes())
	case requestInfoRequestCount:
		return slog.Int64Value(v.info.RequestCount())
	case requestInfoResponseCount:
		return slog.Int64Value(v.info.ResponseCount())
	default:
		return slog.Value{}
	}
}

// statusAttr lazily renders the status code attribute.
func (ri *RequestInfo) statusAttr() slog.Attr {
	return slog.Attr{
		Key:   "grpc.status_code",
		Value: slog.AnyValue(requestInfoValue{info: ri, kind: requestInfoStatus}),
	}
}

// durationAttr lazily renders the RPC duration attribute.
func (ri *RequestInfo) durationAttr() slog.Attr {
	return slog.Attr{
		Key:   "rpc.duration",
		Value: slog.AnyValue(requestInfoValue{info: ri, kind: requestInfoDuration}),
	}
}

// appendSizeAttrs appends size and message count attributes when enabled.
func (ri *RequestInfo) appendSizeAttrs(attrs []slog.Attr, cfg *config) []slog.Attr {
	if !cfg.includeSizes {
		return attrs
	}

	return append(attrs,
		slog.Attr{
			Key:   "rpc.request_size",
			Value: slog.AnyValue(requestInfoValue{info: ri, kind: requestInfoRequestBytes}),
		},
		slog.Attr{
			Key:   "rpc.response_size",
			Value: slog.AnyValue(requestInfoValue{info: ri, kind: requestInfoResponseBytes}),
		},
		slog.Attr{
			Key:   "rpc.request_count",
			Value: slog.AnyValue(requestInfoValue{info: ri, kind: requestInfoRequestCount}),
		},
		slog.Attr{
			Key:   "rpc.response_count",
			Value: slog.AnyValue(requestInfoValue{info: ri, kind: requestInfoResponseCount}),
		},
	)
}

// appendPeerAttr attaches peer IP information when requested.
func (ri *RequestInfo) appendPeerAttr(attrs []slog.Attr, cfg *config) []slog.Attr {
	if !cfg.includePeer {
		return attrs
	}
	if peer := ri.Peer(); peer != "" {
		return append(attrs, slog.String("net.peer.ip", peer))
	}
	return attrs
}

// splitFullMethod parses a gRPC full method string into service and method components.
func splitFullMethod(full string) (service, method string) {
	if !strings.HasPrefix(full, "/") {
		return "", strings.TrimSpace(full)
	}
	full = strings.TrimPrefix(full, "/")
	parts := strings.SplitN(full, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return full, ""
}

// messageSize returns the encoded size of a gRPC message when possible.
func messageSize(msg any) int64 {
	switch m := msg.(type) {
	case proto.Message:
		return int64(proto.Size(m))
	case interface{ Size() int }:
		return int64(m.Size())
	default:
		return 0
	}
}

type logValueFunc func() slog.Value

// LogValue satisfies the slog.LogValuer interface for deferred evaluation.
func (f logValueFunc) LogValue() slog.Value {
	return f()
}

// Service returns the service name component of the method.
func (ri *RequestInfo) Service() string {
	return ri.service
}

// Method returns the method name component.
func (ri *RequestInfo) Method() string {
	return ri.method
}

// FullMethod returns the fully-qualified gRPC method string.
func (ri *RequestInfo) FullMethod() string {
	return ri.fullMethod
}

// Kind returns the RPC kind such as unary or streaming variants.
func (ri *RequestInfo) Kind() string {
	return ri.kind
}

// IsClient reports whether the RequestInfo describes a client-side call.
func (ri *RequestInfo) IsClient() bool {
	return ri.client
}
