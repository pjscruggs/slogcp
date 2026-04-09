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
	"errors"
	"fmt"
	"net/http"
	"time"

	"google.golang.org/api/cloudtrace/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// TraceClient wraps the Cloud Trace v1 REST API for polling spans that were
// written using the v2 API.
type TraceClient struct {
	projectID string
	service   *cloudtrace.Service
}

// NewTraceClient instantiates the Cloud Trace v1 client.
func NewTraceClient(ctx context.Context, projectID string, opts ...option.ClientOption) (*TraceClient, error) {
	if projectID == "" {
		return nil, errors.New("projectID is required")
	}

	opts = append([]option.ClientOption{option.WithScopes(cloudtrace.TraceReadonlyScope)}, opts...)
	service, err := cloudtrace.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating Cloud Trace service: %w", err)
	}

	return &TraceClient{
		projectID: projectID,
		service:   service,
	}, nil
}

// Close is a no-op for the underlying REST client, provided for symmetry.
func (c *TraceClient) Close() error {
	return nil
}

// GetTrace retrieves a trace by ID. It returns an error if the trace does not
// exist yet.
func (c *TraceClient) GetTrace(ctx context.Context, traceID string) (*cloudtrace.Trace, error) {
	call := c.service.Projects.Traces.Get(c.projectID, traceID)
	call.Context(ctx)
	trace, err := call.Do()
	if err != nil {
		return nil, fmt.Errorf("fetching trace %s: %w", traceID, err)
	}
	return trace, nil
}

// WaitForSpans polls Cloud Trace until all expected span names are observed or
// the timeout elapses.
func (c *TraceClient) WaitForSpans(ctx context.Context, traceID string, expected []string, timeout time.Duration) (*cloudtrace.Trace, error) {
	if len(expected) == 0 {
		return nil, errors.New("expected span names required")
	}

	deadline := time.Now().Add(timeout)
	backoff := time.Second
	maxBackoff := 10 * time.Second

	expectedSet := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		expectedSet[name] = struct{}{}
	}

	for time.Now().Before(deadline) {
		trace, err := c.GetTrace(ctx, traceID)
		if err == nil {
			if spansCover(trace, expectedSet) {
				return trace, nil
			}
		} else if !isTraceNotFound(err) {
			return nil, fmt.Errorf("waiting for trace %s: %w", traceID, err)
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for trace %s canceled: %w", traceID, ctx.Err())
		case <-time.After(backoff):
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}

	return nil, fmt.Errorf("timed out waiting for spans for trace %s", traceID)
}

// spansCover reports whether trace contains every expected span name.
func spansCover(trace *cloudtrace.Trace, expected map[string]struct{}) bool {
	if trace == nil {
		return false
	}

	found := make(map[string]struct{})
	for _, span := range trace.Spans {
		if span == nil {
			continue
		}
		if _, ok := expected[span.Name]; ok {
			found[span.Name] = struct{}{}
		}
	}

	return len(found) == len(expected)
}

// isTraceNotFound reports whether err represents a missing trace.
func isTraceNotFound(err error) bool {
	if err == nil {
		return false
	}
	if apiErr := new(googleapi.Error); errors.As(err, &apiErr) {
		return apiErr.Code == http.StatusNotFound
	}
	return false
}
