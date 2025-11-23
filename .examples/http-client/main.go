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

// Command http-client creates an HTTP client transport that forwards trace
// context on outbound requests. When run, it exercises the client against a
// local test server to demonstrate propagation.
//
// This example is both documentation, and a test for `slogcp`.
// Our Github workflow tests if any changes to `slogcp` break the example.
package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"

	slogcphttp "github.com/pjscruggs/slogcp/slogcphttp"
)

func main() {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Traceparent"]; ok {
			w.Header().Set("X-Received-Trace", "true")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := &http.Client{Transport: slogcphttp.Transport(nil)}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, server.URL, nil)
	if err != nil {
		log.Fatalf("new request: %v", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Fatalf("client request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	log.Printf("response status: %s", resp.Status)
	if trace := resp.Header.Get("X-Received-Trace"); trace != "" {
		log.Printf("trace context forwarded: %s", trace)
	}
}
