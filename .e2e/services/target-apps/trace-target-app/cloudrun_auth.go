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

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/oauth2"
	"google.golang.org/api/idtoken"
	grpcoauth "google.golang.org/grpc/credentials/oauth"
)

// withCloudRunIDTokenTransport wraps the provided transport with ID-token auth
// when the downstream target is an HTTPS Cloud Run URL.
func withCloudRunIDTokenTransport(ctx context.Context, base http.RoundTripper, targetURL string) (http.RoundTripper, error) {
	audience, secure, err := cloudRunAudienceFromURL(targetURL)
	if err != nil {
		return nil, err
	}
	if !secure {
		return base, nil
	}

	tokenSource, err := idtoken.NewTokenSource(ctx, audience)
	if err != nil {
		return nil, fmt.Errorf("creating id-token source for %s: %w", audience, err)
	}

	return &oauth2.Transport{
		Source: tokenSource,
		Base:   base,
	}, nil
}

// newCloudRunPerRPCCredentials returns per-RPC credentials backed by an ID
// token when the downstream target points at a managed Cloud Run HTTPS
// endpoint. The boolean return value reports whether credentials should be
// attached for the given target.
func newCloudRunPerRPCCredentials(ctx context.Context, target string) (grpcoauth.TokenSource, bool, error) {
	audience, secure, err := cloudRunAudienceFromGRPCTarget(target)
	if err != nil {
		return grpcoauth.TokenSource{}, false, err
	}
	if !secure {
		return grpcoauth.TokenSource{}, false, nil
	}

	tokenSource, err := idtoken.NewTokenSource(ctx, audience)
	if err != nil {
		return grpcoauth.TokenSource{}, false, fmt.Errorf("creating grpc id-token source for %s: %w", audience, err)
	}

	creds := grpcoauth.TokenSource{TokenSource: tokenSource}
	return creds, true, nil
}

// cloudRunAudienceFromURL derives the audience from an HTTPS service URL.
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

// cloudRunAudienceFromGRPCTarget derives the audience from a gRPC host:port.
func cloudRunAudienceFromGRPCTarget(target string) (string, bool, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return "", false, nil
	}
	if strings.Contains(trimmed, "://") {
		return cloudRunAudienceFromURL(trimmed)
	}

	host := strings.TrimSuffix(strings.TrimSuffix(trimmed, "/"), ":443")
	if host == "" {
		return "", false, fmt.Errorf("grpc target %q does not include a host", target)
	}
	return "https://" + host, true, nil
}
