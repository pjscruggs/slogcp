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

package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	run "google.golang.org/api/run/v1"
)

// readyService constructs a reconciled service response.
func readyService() *run.Service {
	return &run.Service{Metadata: &run.ObjectMeta{Uid: "test-uid", Generation: 1}, Status: &run.ServiceStatus{ObservedGeneration: 1, Url: "https://service.invalid", Conditions: []*run.GoogleCloudRunV1Condition{{Type: "Ready", Status: "True"}}}}
}

// TestAcceptedDeploymentRetainsCleanup verifies evidence and ownership on failure.
func TestAcceptedDeploymentRetainsCleanup(t *testing.T) {
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method)
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			_, _ = w.Write([]byte(`{"metadata":{"uid":"accepted","generation":1}}`))
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"metadata":{"uid":"accepted","generation":1},"status":{"observedGeneration":1,"latestCreatedRevisionName":"failed-revision","conditions":[{"type":"Ready","status":"False","reason":"ContainerMissing","message":"image unavailable"}]}}`))
		case http.MethodDelete:
			http.Error(w, "already deleted", http.StatusNotFound)
		}
	}))
	defer server.Close()
	api, err := run.NewService(context.Background(), option.WithEndpoint(server.URL+"/"), option.WithoutAuthentication())
	if err != nil {
		t.Fatal(err)
	}
	manager := &CloudRunManager{projectID: "test", region: "region", servicesAPI: run.NewProjectsLocationsServicesService(api)}
	instance, err := manager.DeployService(context.Background(), ServiceConfig{Name: "service", Image: "image"})
	if instance == nil || err == nil || !strings.Contains(err.Error(), "failed-revision") || !strings.Contains(err.Error(), "ContainerMissing") {
		t.Fatalf("ownership/evidence missing: %v %v", instance, err)
	}
	if strings.Join(calls, ",") != "POST,GET" {
		t.Fatalf("cleanup preceded error capture: %v", calls)
	}
	if err := instance.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(calls, ",") != "POST,GET,DELETE" {
		t.Fatalf("cleanup missing: %v", calls)
	}
}

// TestReadinessProgress accepts a reconciled transition after an unknown state.
func TestReadinessProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	calls := 0
	_, err := waitForServiceReady(ctx, nil, func(context.Context) (*run.Service, error) {
		calls++
		s := readyService()
		if calls == 1 {
			s.Status.Conditions[0].Status = "Unknown"
		}
		return s, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("transition failed: calls=%d err=%v", calls, err)
	}
}

// TestServiceReadiness checks current-generation conditions and deletion state.
func TestServiceReadiness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		edit    func(*run.Service)
		ready   bool
		failure bool
	}{
		{"ready", func(*run.Service) {}, true, false},
		{"unknown", func(s *run.Service) { s.Status.Conditions[0].Status = "Unknown" }, false, false},
		{"terminal", func(s *run.Service) { s.Status.Conditions[0].Status = "False" }, false, true},
		{"stale", func(s *run.Service) { s.Status.ObservedGeneration = 0 }, false, false},
		{"no-url", func(s *run.Service) { s.Status.Url = "" }, false, false},
		{"deleted", func(s *run.Service) { s.Metadata.DeletionTimestamp = "2026-01-01T00:00:00Z" }, false, true},
		{"replaced", func(s *run.Service) { s.Metadata.Uid = "other" }, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := readyService()
			tc.edit(s)
			got, err := serviceReadiness(s, &run.ObjectMeta{Uid: "test-uid", Generation: 1})
			if got != tc.ready || (err != nil) != tc.failure {
				t.Fatalf("ready=%v error=%v", got, err)
			}
		})
	}
}

// TestReadinessCancellationPreservesEvidence rejects late responses and retains state.
func TestReadinessCancellationPreservesEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := waitForServiceReady(ctx, nil, func(context.Context) (*run.Service, error) { cancel(); return readyService(), nil })
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "test-uid") {
		t.Fatalf("late success accepted or evidence lost: %v", err)
	}
}

// TestReadinessHangingRequest receives the polling deadline, not an unbounded parent.
func TestReadinessHangingRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := waitForServiceReady(ctx, nil, func(ctx context.Context) (*run.Service, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline lost: %v", err)
	}
}

// TestReadinessErrors keeps API failures distinct from polling timeouts.
func TestReadinessErrors(t *testing.T) {
	for _, code := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
		_, err := waitForServiceReady(ctx, nil, func(context.Context) (*run.Service, error) { return nil, &googleapi.Error{Code: code} })
		cancel()
		if err == nil {
			t.Fatal("API failure accepted")
		}
		if (code == http.StatusServiceUnavailable) != errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("wrong failure classification: %v", err)
		}
	}
}
