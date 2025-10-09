package http

import (
	"log/slog"
	"testing"
)

func TestLoadMiddlewareOptionsFromEnv(t *testing.T) {
	t.Setenv("SLOGCP_HTTP_SKIP_PATH_SUBSTRINGS", "healthz, metrics , ")
	t.Setenv("SLOGCP_HTTP_SUPPRESS_UNSAMPLED_BELOW", "WARNING")
	t.Setenv("SLOGCP_HTTP_LOG_REQUEST_HEADER_KEYS", "x-real-ip, x-forwarded-for")
	t.Setenv("SLOGCP_HTTP_LOG_RESPONSE_HEADER_KEYS", "retry-after, Content-Type")
	t.Setenv("SLOGCP_HTTP_REQUEST_BODY_LIMIT", "128")
	t.Setenv("SLOGCP_HTTP_RESPONSE_BODY_LIMIT", "256")
	t.Setenv("SLOGCP_HTTP_RECOVER_PANICS", "true")
	t.Setenv("SLOGCP_HTTP_TRUST_PROXY_HEADERS", "1")

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
