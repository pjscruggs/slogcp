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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/logging/logadmin"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	"google.golang.org/api/transport"
)

// LoggingClient wraps the Google Cloud Logging API clients
type LoggingClient struct {
	projectID   string
	adminClient *logadmin.Client
	httpClient  *http.Client
}

// NewLoggingClient creates a new Cloud Logging client
func NewLoggingClient(ctx context.Context, projectID string, opts ...option.ClientOption) (*LoggingClient, error) {
	adminClient, err := logadmin.NewClient(ctx, projectID, opts...)
	if err != nil {
		return nil, fmt.Errorf("creating log admin client: %w", err)
	}

	httpClient, _, err := transport.NewHTTPClient(ctx, opts...)
	if err != nil {
		_ = adminClient.Close()
		return nil, fmt.Errorf("creating logging http client: %w", err)
	}

	return &LoggingClient{
		projectID:   projectID,
		adminClient: adminClient,
		httpClient:  httpClient,
	}, nil
}

// Close closes the logging clients
func (c *LoggingClient) Close() error {
	if err := c.adminClient.Close(); err != nil {
		return fmt.Errorf("closing admin client: %w", err)
	}
	return nil
}

// QueryOptions configures a log query
type QueryOptions struct {
	Filter       string
	StartTime    time.Time
	EndTime      time.Time
	MaxResults   int
	ResourceType string
}

// buildLogFilter assembles a Cloud Logging filter string from query options.
func buildLogFilter(opts QueryOptions) string {
	filterParts := []string{}

	if opts.Filter != "" {
		filterParts = append(filterParts, opts.Filter)
	}

	if opts.ResourceType != "" {
		filterParts = append(filterParts, fmt.Sprintf(`resource.type="%s"`, opts.ResourceType))
	}

	if !opts.StartTime.IsZero() {
		filterParts = append(filterParts, fmt.Sprintf(`timestamp>="%s"`, opts.StartTime.Format(time.RFC3339)))
	}
	if !opts.EndTime.IsZero() {
		filterParts = append(filterParts, fmt.Sprintf(`timestamp<="%s"`, opts.EndTime.Format(time.RFC3339)))
	}

	return strings.Join(filterParts, " AND ")
}

// QueryLogs queries Cloud Logging and returns matching entries
func (c *LoggingClient) QueryLogs(ctx context.Context, opts QueryOptions) ([]*LogEntry, error) {
	filter := buildLogFilter(opts)

	// Create iterator with filter
	iter := c.adminClient.Entries(ctx, logadmin.Filter(filter))

	// Collect entries
	var entries []*LogEntry
	count := 0
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 1000 // Default max
	}

	for {
		entry, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return entries, fmt.Errorf("iterating logs: %w", err)
		}

		converted, convErr := newLogEntry(entry)
		if convErr != nil {
			return entries, fmt.Errorf("converting log entry: %w", convErr)
		}

		entries = append(entries, converted)
		count++

		if count >= maxResults {
			break
		}
	}

	return entries, nil
}

// listEntriesRequest mirrors the Logging entries.list request payload.
type listEntriesRequest struct {
	ResourceNames []string `json:"resourceNames"`
	Filter        string   `json:"filter,omitempty"`
	OrderBy       string   `json:"orderBy,omitempty"`
	PageSize      int      `json:"pageSize,omitempty"`
	PageToken     string   `json:"pageToken,omitempty"`
}

// listEntriesResponse mirrors the Logging entries.list response payload.
type listEntriesResponse struct {
	Entries       []map[string]any `json:"entries"`
	NextPageToken string           `json:"nextPageToken"`
}

// QueryLogsJSON queries Cloud Logging via the REST API and returns raw JSON entries,
// preserving which fields were present (as opposed to defaulted by proto3 decoding).
func (c *LoggingClient) QueryLogsJSON(ctx context.Context, opts QueryOptions) ([]map[string]any, error) {
	if c.httpClient == nil {
		return nil, fmt.Errorf("logging http client is not configured")
	}

	maxResults, reqBody := buildEntriesListRequest(c.projectID, opts)
	return c.queryLogsJSONPages(ctx, reqBody, maxResults)
}

// buildEntriesListRequest builds request defaults for entries.list.
func buildEntriesListRequest(projectID string, opts QueryOptions) (int, listEntriesRequest) {
	maxResults := opts.MaxResults
	if maxResults <= 0 {
		maxResults = 1000
	}
	pageSize := min(maxResults, 1000)

	return maxResults, listEntriesRequest{
		ResourceNames: []string{fmt.Sprintf("projects/%s", projectID)},
		Filter:        buildLogFilter(opts),
		OrderBy:       "timestamp desc",
		PageSize:      pageSize,
	}
}

// queryLogsJSONPages fetches entries.list pages until exhausted or max reached.
func (c *LoggingClient) queryLogsJSONPages(ctx context.Context, reqBody listEntriesRequest, maxResults int) ([]map[string]any, error) {
	var out []map[string]any
	for {
		decoded, err := c.fetchEntriesListPage(ctx, reqBody)
		if err != nil {
			return nil, err
		}

		out = append(out, decoded.Entries...)
		if len(out) >= maxResults {
			return out[:maxResults], nil
		}
		if decoded.NextPageToken == "" || len(decoded.Entries) == 0 {
			return out, nil
		}
		reqBody.PageToken = decoded.NextPageToken
	}
}

// fetchEntriesListPage executes one entries.list request.
func (c *LoggingClient) fetchEntriesListPage(ctx context.Context, reqBody listEntriesRequest) (*listEntriesResponse, error) {
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("encoding entries.list request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://logging.googleapis.com/v2/entries:list", bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("building entries.list request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling entries.list: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		return nil, fmt.Errorf("entries.list failed: %s: %s", resp.Status, strings.TrimSpace(string(payload)))
	}

	var decoded listEntriesResponse
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return nil, fmt.Errorf("decoding entries.list response: %w", err)
	}
	return &decoded, nil
}

// WaitForLogs queries logs with retries until entries are found or timeout
func (c *LoggingClient) WaitForLogs(ctx context.Context, opts QueryOptions, timeout time.Duration) ([]*LogEntry, error) {
	deadline := time.Now().Add(timeout)
	backoff := 1 * time.Second
	maxBackoff := 10 * time.Second

	for time.Now().Before(deadline) {
		entries, err := c.QueryLogs(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("querying logs: %w", err)
		}

		if len(entries) > 0 {
			return entries, nil
		}

		// Wait before retrying
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for logs canceled: %w", ctx.Err())
		case <-time.After(backoff):
			// Exponential backoff
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}

	return nil, fmt.Errorf("timeout waiting for logs after %v", timeout)
}

// WaitForLogsJSON queries Cloud Logging via the REST API with retries until entries
// are found or timeout.
func (c *LoggingClient) WaitForLogsJSON(ctx context.Context, opts QueryOptions, timeout time.Duration) ([]map[string]any, error) {
	deadline := time.Now().Add(timeout)
	backoff := 1 * time.Second
	maxBackoff := 10 * time.Second

	for time.Now().Before(deadline) {
		entries, err := c.QueryLogsJSON(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("querying logs: %w", err)
		}

		if len(entries) > 0 {
			return entries, nil
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for raw logs canceled: %w", ctx.Err())
		case <-time.After(backoff):
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}

	return nil, fmt.Errorf("timeout waiting for logs after %v", timeout)
}

// GetLogMetadata returns metadata about available logs
func (c *LoggingClient) GetLogMetadata(ctx context.Context) ([]string, error) {
	iter := c.adminClient.Logs(ctx)

	var logNames []string
	for {
		logName, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return logNames, fmt.Errorf("iterating log names: %w", err)
		}
		logNames = append(logNames, logName)
	}

	return logNames, nil
}
