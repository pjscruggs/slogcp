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
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/api/idtoken"
)

// newTargetHTTPClient returns an ID-token authenticated client for HTTPS
// Cloud Run endpoints and a plain client for local HTTP targets.
func newTargetHTTPClient(ctx context.Context, baseURL string) (*http.Client, error) {
	audience, secure, err := cloudRunAudienceFromURL(baseURL)
	if err != nil {
		return nil, err
	}
	if !secure {
		return &http.Client{Timeout: 30 * time.Second}, nil
	}

	client, err := idtoken.NewClient(ctx, audience)
	if err != nil {
		return nil, fmt.Errorf("creating id-token client for %s: %w", audience, err)
	}
	client.Timeout = 30 * time.Second
	return client, nil
}

// cloudRunAudienceFromURL derives the Cloud Run audience from a service URL.
func cloudRunAudienceFromURL(raw string) (string, bool, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", false, fmt.Errorf("parsing target url %q: %w", raw, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", false, fmt.Errorf("target url %q must include scheme and host", raw)
	}
	if !strings.EqualFold(parsed.Scheme, "https") {
		return "", false, nil
	}
	return parsed.Scheme + "://" + parsed.Host, true, nil
}
