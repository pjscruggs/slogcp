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

package http

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLoadMiddlewareOptionsFromEnv(t *testing.T) {
	t.Setenv("SLOGCP_HTTP_SKIP_PATH_SUBSTRINGS", "healthz, metrics , ")
	t.Setenv("SLOGCP_HTTP_SUPPRESS_UNSAMPLED_BELOW", "WARNING")
	t.Setenv("SLOGCP_HTTP_LOG_REQUEST_HEADER_KEYS", "x-real-ip, x-forwarded-for")
	t.Setenv("SLOGCP_HTTP_LOG_RESPONSE_HEADER_KEYS", "retry-after, Content-Type")
	t.Setenv("SLOGCP_HTTP_REQUEST_BODY_LIMIT", "128")
	t.Setenv("SLOGCP_HTTP_RESPONSE_BODY_LIMIT", "256")
	t.Setenv("SLOGCP_HTTP_RECOVER_PANICS", "YeS")
	t.Setenv("SLOGCP_HTTP_TRUST_PROXY_HEADERS", "on")

	opts := loadMiddlewareOptionsFromEnv()

	if got, want := opts.SkipPathSubstrings, []string{"healthz", "metrics"}; !slicesEqual(got, want) {
		t.Fatalf("SkipPathSubstrings = %v, want %v", got, want)
	}

	if opts.SuppressUnsampledBelow == nil {
		t.Fatalf("SuppressUnsampledBelow = nil, want level")
	}
	if got, want := *opts.SuppressUnsampledBelow, slog.LevelWarn; got != want {
		t.Fatalf("SuppressUnsampledBelow = %v, want %v", got, want)
	}

	if got, want := opts.LogRequestHeaderKeys, []string{"X-Real-Ip", "X-Forwarded-For"}; !slicesEqual(got, want) {
		t.Fatalf("LogRequestHeaderKeys = %v, want %v", got, want)
	}

	if got, want := opts.LogResponseHeaderKeys, []string{"Retry-After", "Content-Type"}; !slicesEqual(got, want) {
		t.Fatalf("LogResponseHeaderKeys = %v, want %v", got, want)
	}

	if got, want := opts.RequestBodyLimit, int64(128); got != want {
		t.Fatalf("RequestBodyLimit = %d, want %d", got, want)
	}

	if got, want := opts.ResponseBodyLimit, int64(256); got != want {
		t.Fatalf("ResponseBodyLimit = %d, want %d", got, want)
	}

	if !opts.RecoverPanics {
		t.Fatal("RecoverPanics = false, want true")
	}

	if !opts.TrustProxyHeaders {
		t.Fatal("TrustProxyHeaders = false, want true")
	}
}

func TestLoadMiddlewareOptionsFromEnvIgnoresInvalid(t *testing.T) {
	t.Setenv("SLOGCP_HTTP_SUPPRESS_UNSAMPLED_BELOW", "bogus")
	t.Setenv("SLOGCP_HTTP_REQUEST_BODY_LIMIT", "-10")
	t.Setenv("SLOGCP_HTTP_RESPONSE_BODY_LIMIT", "not-a-number")
	t.Setenv("SLOGCP_HTTP_RECOVER_PANICS", "not-bool")
	t.Setenv("SLOGCP_HTTP_TRUST_PROXY_HEADERS", "perhaps")

	opts := loadMiddlewareOptionsFromEnv()

	if opts.SuppressUnsampledBelow != nil {
		t.Fatalf("SuppressUnsampledBelow = %v, want nil", *opts.SuppressUnsampledBelow)
	}
	if opts.RequestBodyLimit != 0 {
		t.Fatalf("RequestBodyLimit = %d, want 0", opts.RequestBodyLimit)
	}
	if opts.ResponseBodyLimit != 0 {
		t.Fatalf("ResponseBodyLimit = %d, want 0", opts.ResponseBodyLimit)
	}
	if opts.RecoverPanics {
		t.Fatal("RecoverPanics = true, want false")
	}
}

func TestParseBoolFlag(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		in   string
		want bool
		ok   bool
	}{
		"true":        {in: "true", want: true, ok: true},
		"true spaced": {in: "  TRUE  ", want: true, ok: true},
		"one":         {in: "1", want: true, ok: true},
		"yes":         {in: "YeS", want: true, ok: true},
		"on":          {in: "on", want: true, ok: true},
		"false":       {in: "false", want: false, ok: true},
		"zero":        {in: "0", want: false, ok: true},
		"no":          {in: "NO", want: false, ok: true},
		"off":         {in: "Off", want: false, ok: true},
		"invalid":     {in: "maybe", want: false, ok: false},
		"empty":       {in: "   ", want: false, ok: false},
	}

	for name, tc := range tests {
		tc := tc
		t.Run(name, func(t *testing.T) {
			got, ok := parseBoolFlag(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if got != tc.want {
				t.Fatalf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestMiddlewareOptionHelpers(t *testing.T) {
	opts := defaultMiddlewareOptions()

	var shouldLogCalled bool
	WithShouldLog(func(ctx context.Context, r *http.Request) bool {
		shouldLogCalled = true
		return false
	})(&opts)
	if opts.ShouldLog == nil {
		t.Fatal("ShouldLog not set")
	}
	if opts.ShouldLog(context.Background(), httptest.NewRequest(http.MethodGet, "http://example.com", nil)) {
		t.Fatal("ShouldLog should return false")
	}
	if !shouldLogCalled {
		t.Fatal("ShouldLog predicate never invoked")
	}

	WithSkipPathSubstrings("  foo ", "", "bar")(&opts)
	if got, want := opts.SkipPathSubstrings, []string{"foo", "bar"}; !slicesEqual(got, want) {
		t.Fatalf("SkipPathSubstrings = %v, want %v", got, want)
	}

	WithSuppressUnsampledBelow(slog.LevelWarn)(&opts)
	if opts.SuppressUnsampledBelow == nil || *opts.SuppressUnsampledBelow != slog.LevelWarn {
		t.Fatalf("SuppressUnsampledBelow = %v, want %v", opts.SuppressUnsampledBelow, slog.LevelWarn)
	}

	WithLogRequestHeaderKeys("x-custom", "X-Second")(&opts)
	if got, want := opts.LogRequestHeaderKeys, []string{"X-Custom", "X-Second"}; !slicesEqual(got, want) {
		t.Fatalf("LogRequestHeaderKeys = %v, want %v", got, want)
	}

	WithLogResponseHeaderKeys("retry-after", "content-type")(&opts)
	if got, want := opts.LogResponseHeaderKeys, []string{"Retry-After", "Content-Type"}; !slicesEqual(got, want) {
		t.Fatalf("LogResponseHeaderKeys = %v, want %v", got, want)
	}

	WithRequestBodyLimit(128)(&opts)
	if opts.RequestBodyLimit != 128 {
		t.Fatalf("RequestBodyLimit = %d, want 128", opts.RequestBodyLimit)
	}
	WithRequestBodyLimit(-1)(&opts)
	if opts.RequestBodyLimit != 0 {
		t.Fatalf("negative RequestBodyLimit should clamp to 0, got %d", opts.RequestBodyLimit)
	}

	WithResponseBodyLimit(256)(&opts)
	if opts.ResponseBodyLimit != 256 {
		t.Fatalf("ResponseBodyLimit = %d, want 256", opts.ResponseBodyLimit)
	}
	WithResponseBodyLimit(-5)(&opts)
	if opts.ResponseBodyLimit != 0 {
		t.Fatalf("negative ResponseBodyLimit should clamp to 0, got %d", opts.ResponseBodyLimit)
	}

	WithRecoverPanics(true)(&opts)
	if !opts.RecoverPanics {
		t.Fatal("RecoverPanics not set to true")
	}
	WithRecoverPanics(false)(&opts)
	if opts.RecoverPanics {
		t.Fatal("RecoverPanics not reset to false")
	}

	WithTrustProxyHeaders(true)(&opts)
	if !opts.TrustProxyHeaders {
		t.Fatal("TrustProxyHeaders not set to true")
	}
	WithTrustProxyHeaders(false)(&opts)
	if opts.TrustProxyHeaders {
		t.Fatal("TrustProxyHeaders not reset to false")
	}
}

func slicesEqual[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
