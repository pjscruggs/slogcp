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

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	localtracepb "github.com/pjscruggs/slogcp-e2e-internal/services/traceproto"
)

// DownstreamHTTPRequest carries details required to invoke the downstream HTTP
// service while maintaining trace propagation.
type DownstreamHTTPRequest struct {
	TestID  string
	Message string
}

// DownstreamHTTPResponse is the subset of fields the handler cares about in
// downstream replies.
type DownstreamHTTPResponse struct {
	Acknowledgement string            `json:"acknowledgement"`
	Metadata        map[string]string `json:"metadata,omitempty"`
}

// DownstreamGRPCRequest wraps the protobuf payload with trace identifiers.
type DownstreamGRPCRequest struct {
	Payload *localtracepb.TraceWorkRequest
}

// DownstreamGRPCServerStreamRequest wraps a server-streaming request payload.
type DownstreamGRPCServerStreamRequest struct {
	Payload *localtracepb.ServerStreamRequest
}

// DownstreamGRPCClientStreamRequest wraps client-streaming request payloads.
type DownstreamGRPCClientStreamRequest struct {
	Payloads []*localtracepb.ClientStreamRequest
}

// DownstreamGRPCBidiStreamRequest wraps bidi-streaming request payloads.
type DownstreamGRPCBidiStreamRequest struct {
	Payloads []*localtracepb.BidiStreamRequest
}

// DownstreamClients implements DownstreamInvoker backed by HTTP and gRPC
// clients.
type DownstreamClients struct {
	HTTPBaseURL string
	HTTPClient  *http.Client
	GRPCClient  localtracepb.TraceWorkerClient
}

// CallDownstreamHTTP invokes the downstream HTTP work endpoint.
func (c *DownstreamClients) CallDownstreamHTTP(ctx context.Context, req *DownstreamHTTPRequest) (*DownstreamHTTPResponse, error) {
	if err := validateDownstreamHTTPInputs(c, req); err != nil {
		return nil, err
	}

	body := map[string]any{
		"test_id": req.TestID,
		"message": req.Message,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshaling downstream request: %w", err)
	}

	target := strings.TrimRight(c.HTTPBaseURL, "/") + "/work"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("creating downstream request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("calling downstream http: %w", err)
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("closing downstream http response body: %w", closeErr))
		}
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downstream http status %d", resp.StatusCode)
	}

	var downstreamResp DownstreamHTTPResponse
	if err := json.NewDecoder(resp.Body).Decode(&downstreamResp); err != nil {
		return nil, fmt.Errorf("decoding downstream response: %w", err)
	}
	return &downstreamResp, nil
}

// validateDownstreamHTTPInputs validates required HTTP client prerequisites.
func validateDownstreamHTTPInputs(c *DownstreamClients, req *DownstreamHTTPRequest) error {
	if c == nil {
		return errors.New("downstream clients not configured")
	}
	if req == nil {
		return errors.New("downstream request is nil")
	}
	if c.HTTPBaseURL == "" || c.HTTPClient == nil {
		return errors.New("downstream HTTP client not configured")
	}
	return nil
}

// CallDownstreamGRPC invokes the downstream unary gRPC work endpoint.
func (c *DownstreamClients) CallDownstreamGRPC(ctx context.Context, req *DownstreamGRPCRequest) (*localtracepb.TraceWorkResponse, error) {
	if c == nil {
		return nil, errors.New("downstream clients not configured")
	}
	if req == nil || req.Payload == nil {
		return nil, errors.New("downstream grpc request is nil")
	}
	if c.GRPCClient == nil {
		return nil, errors.New("downstream grpc client not configured")
	}

	resp, err := c.GRPCClient.DoWork(ctx, req.Payload)
	if err != nil {
		return nil, fmt.Errorf("calling downstream grpc do_work: %w", err)
	}

	return resp, nil
}

// CallDownstreamGRPCServerStream invokes downstream server-streaming gRPC work.
func (c *DownstreamClients) CallDownstreamGRPCServerStream(ctx context.Context, req *DownstreamGRPCServerStreamRequest) ([]*localtracepb.ServerStreamResponse, error) {
	if c == nil {
		return nil, errors.New("downstream clients not configured")
	}
	if req == nil || req.Payload == nil {
		return nil, errors.New("downstream grpc server stream request is nil")
	}
	if c.GRPCClient == nil {
		return nil, errors.New("downstream grpc client not configured")
	}

	stream, err := c.GRPCClient.StreamServer(ctx, req.Payload)
	if err != nil {
		return nil, fmt.Errorf("opening downstream grpc server stream: %w", err)
	}

	responses := make([]*localtracepb.ServerStreamResponse, 0, req.Payload.GetResponseCount())
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("receiving downstream grpc server stream: %w", err)
		}
		responses = append(responses, msg)
	}

	return responses, nil
}

// CallDownstreamGRPCClientStream invokes downstream client-streaming gRPC work.
func (c *DownstreamClients) CallDownstreamGRPCClientStream(ctx context.Context, req *DownstreamGRPCClientStreamRequest) (*localtracepb.ClientStreamResponse, error) {
	if c == nil {
		return nil, errors.New("downstream clients not configured")
	}
	if req == nil || len(req.Payloads) == 0 {
		return nil, errors.New("downstream grpc client stream request is nil")
	}
	if c.GRPCClient == nil {
		return nil, errors.New("downstream grpc client not configured")
	}

	stream, err := c.GRPCClient.StreamClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening downstream grpc client stream: %w", err)
	}

	for _, payload := range req.Payloads {
		if payload == nil {
			continue
		}
		if err := stream.Send(payload); err != nil {
			return nil, fmt.Errorf("sending downstream grpc client stream chunk: %w", err)
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return nil, fmt.Errorf("closing downstream grpc client stream: %w", err)
	}

	return resp, nil
}

// CallDownstreamGRPCBidiStream invokes downstream bidirectional gRPC work.
func (c *DownstreamClients) CallDownstreamGRPCBidiStream(ctx context.Context, req *DownstreamGRPCBidiStreamRequest) ([]*localtracepb.BidiStreamResponse, error) {
	if c == nil {
		return nil, errors.New("downstream clients not configured")
	}
	if req == nil || len(req.Payloads) == 0 {
		return nil, errors.New("downstream grpc bidi stream request is nil")
	}
	if c.GRPCClient == nil {
		return nil, errors.New("downstream grpc client not configured")
	}

	stream, err := c.GRPCClient.StreamBidi(ctx)
	if err != nil {
		return nil, fmt.Errorf("opening downstream grpc bidi stream: %w", err)
	}

	responses, err := exchangeBidiStreamMessages(stream, req.Payloads)
	if err != nil {
		return nil, err
	}

	return drainBidiStreamTail(stream, responses)
}

// exchangeBidiStreamMessages sends request chunks and collects immediate replies.
func exchangeBidiStreamMessages(stream localtracepb.TraceWorker_StreamBidiClient, payloads []*localtracepb.BidiStreamRequest) ([]*localtracepb.BidiStreamResponse, error) {
	responses := make([]*localtracepb.BidiStreamResponse, 0, len(payloads))
	for _, payload := range payloads {
		if payload == nil {
			continue
		}
		if err := stream.Send(payload); err != nil {
			_ = stream.CloseSend()
			return nil, fmt.Errorf("sending downstream grpc bidi stream chunk: %w", err)
		}

		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			_ = stream.CloseSend()
			return nil, fmt.Errorf("receiving downstream grpc bidi stream response: %w", err)
		}
		responses = append(responses, resp)
	}

	return responses, nil
}

// drainBidiStreamTail drains any remaining responses after close-send.
func drainBidiStreamTail(stream localtracepb.TraceWorker_StreamBidiClient, responses []*localtracepb.BidiStreamResponse) ([]*localtracepb.BidiStreamResponse, error) {
	if err := stream.CloseSend(); err != nil && !errors.Is(err, io.EOF) {
		return responses, fmt.Errorf("closing downstream grpc bidi stream: %w", err)
	}

	for {
		resp, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("receiving downstream grpc bidi stream tail: %w", err)
		}
		responses = append(responses, resp)
	}

	return responses, nil
}
