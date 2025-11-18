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

package slogcp

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// resetRuntimeInfoCache clears cached runtime inspection state for isolated tests.
func resetRuntimeInfoCache() {
	runtimeInfoOnce = sync.Once{}
	runtimeInfo = RuntimeInfo{}
	resetHandlerConfigCache()
}

type stubMetadataClient struct {
	onGCE  bool
	values map[string]string
}

// OnGCE reports whether the stub considers metadata available.
func (s *stubMetadataClient) OnGCE() bool {
	return s.onGCE
}

// Get returns the stubbed metadata value or signals absence.
func (s *stubMetadataClient) Get(path string) (string, error) {
	if v, ok := s.values[path]; ok {
		return v, nil
	}
	return "", errors.New("metadata not found")
}

// withMetadataClient installs a temporary metadata client factory for the test scope.
func withMetadataClient(t *testing.T, client metadataClient) {
	original := metadataClientFactory
	metadataClientFactory = func() metadataClient {
		return client
	}
	t.Cleanup(func() {
		metadataClientFactory = original
	})
}

type countingMetadataClient struct {
	onGCE bool
	calls int
}

// OnGCE increments the number of availability checks.
func (c *countingMetadataClient) OnGCE() bool {
	c.calls++
	return c.onGCE
}

// Get always reports missing metadata for availability tests.
func (c *countingMetadataClient) Get(string) (string, error) {
	return "", errors.New("metadata not available")
}

// TestDetectKubernetesUsesMetadata verifies metadata-derived clusters populate labels.
func TestDetectKubernetesUsesMetadata(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("POD_NAME", "api-0")
	t.Setenv("CONTAINER_NAME", "edge")

	tmp := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(tmp, []byte("prod\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) = %v", tmp, err)
	}
	prevPath := kubernetesNamespacePath
	kubernetesNamespacePath = tmp
	t.Cleanup(func() { kubernetesNamespacePath = prevPath })

	info := RuntimeInfo{}
	lookup := newMetadataLookup(&stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"instance/attributes/cluster-name":     "gke-orders",
			"instance/attributes/cluster-location": "us-central1-a",
			"project/project-id":                   "meta-project",
		},
	})

	if !detectKubernetes(&info, lookup) {
		t.Fatalf("detectKubernetes returned false")
	}
	if info.Environment != RuntimeEnvKubernetes {
		t.Fatalf("Environment = %v, want %v", info.Environment, RuntimeEnvKubernetes)
	}
	if got := info.ProjectID; got != "meta-project" {
		t.Fatalf("ProjectID = %q, want %q", got, "meta-project")
	}
	if got := info.Labels["k8s.cluster.name"]; got != "gke-orders" {
		t.Fatalf("k8s.cluster.name = %q, want %q", got, "gke-orders")
	}
	if got := info.Labels["k8s.location"]; got != "us-central1-a" {
		t.Fatalf("k8s.location = %q, want %q", got, "us-central1-a")
	}
	if got := info.Labels["k8s.namespace.name"]; got != "prod" {
		t.Fatalf("k8s.namespace.name = %q, want %q", got, "prod")
	}
	if got := info.Labels["k8s.pod.name"]; got != "api-0" {
		t.Fatalf("k8s.pod.name = %q, want %q", got, "api-0")
	}
	if got := info.Labels["k8s.container.name"]; got != "edge" {
		t.Fatalf("k8s.container.name = %q, want %q", got, "edge")
	}
}

// TestDetectKubernetesEnvFallback exercises the environment fallback paths.
func TestDetectKubernetesEnvFallback(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("CLUSTER_NAME", "env-cluster")
	t.Setenv("CLUSTER_LOCATION", "europe-west4")
	t.Setenv("NAMESPACE_NAME", "payments")
	t.Setenv("POD_NAME", "payments-57df9")
	t.Setenv("CONTAINER_NAME", "payments-api")

	info := RuntimeInfo{}
	lookup := newMetadataLookup(&stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"project/project-id": "env-project",
		},
	})

	if !detectKubernetes(&info, lookup) {
		t.Fatalf("detectKubernetes returned false")
	}
	if got := info.ProjectID; got != "env-project" {
		t.Fatalf("ProjectID = %q, want %q", got, "env-project")
	}
	if got := info.Labels["k8s.cluster.name"]; got != "env-cluster" {
		t.Fatalf("k8s.cluster.name = %q, want %q", got, "env-cluster")
	}
	if got := info.Labels["k8s.location"]; got != "europe-west4" {
		t.Fatalf("k8s.location = %q, want %q", got, "europe-west4")
	}
	if got := info.Labels["k8s.namespace.name"]; got != "payments" {
		t.Fatalf("k8s.namespace.name = %q, want %q", got, "payments")
	}
	if got := info.Labels["k8s.pod.name"]; got != "payments-57df9" {
		t.Fatalf("k8s.pod.name = %q, want %q", got, "payments-57df9")
	}
	if got := info.Labels["k8s.container.name"]; got != "payments-api" {
		t.Fatalf("k8s.container.name = %q, want %q", got, "payments-api")
	}
}

// TestReadNamespaceUsesOverride ensures readNamespace honours the configurable path.
func TestReadNamespaceUsesOverride(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(tmp, []byte("observability\n"), 0o644); err != nil {
		t.Fatalf("os.WriteFile(%q) = %v", tmp, err)
	}
	prev := kubernetesNamespacePath
	kubernetesNamespacePath = tmp
	t.Cleanup(func() { kubernetesNamespacePath = prev })

	if got := readNamespace(); got != "observability" {
		t.Fatalf("readNamespace() = %q, want %q", got, "observability")
	}
}

// TestDetectRuntimeInfoCloudRunService verifies Cloud Run environment variables populate runtime metadata.
func TestDetectRuntimeInfoCloudRunService(t *testing.T) {
	resetRuntimeInfoCache()
	t.Setenv("K_SERVICE", "svc")
	t.Setenv("K_REVISION", "rev")
	t.Setenv("K_CONFIGURATION", "cfg")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "my-project")

	info := detectRuntimeInfo()
	if got := info.ProjectID; got != "my-project" {
		t.Fatalf("ProjectID = %q, want %q", got, "my-project")
	}
	if info.ServiceContext["service"] != "svc" {
		t.Fatalf("service context service = %q, want %q", info.ServiceContext["service"], "svc")
	}
	if info.ServiceContext["version"] != "rev" {
		t.Fatalf("service context version = %q, want %q", info.ServiceContext["version"], "rev")
	}
	if info.Labels["cloud_run.service"] != "svc" {
		t.Fatalf("label cloud_run.service = %q, want %q", info.Labels["cloud_run.service"], "svc")
	}
	if info.Environment != RuntimeEnvCloudRunService {
		t.Fatalf("Environment = %v, want %v", info.Environment, RuntimeEnvCloudRunService)
	}
}

// TestDetectRuntimeInfoMetadataFallback ensures metadata server fallback supplies project ID.
func TestDetectRuntimeInfoMetadataFallback(t *testing.T) {
	resetRuntimeInfoCache()
	withMetadataClient(t, &stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"project/project-id": "meta-project",
		},
	})

	info := detectRuntimeInfo()
	if got := info.ProjectID; got != "meta-project" {
		t.Fatalf("ProjectID = %q, want %q", got, "meta-project")
	}
}

// TestNewHandlerUsesRuntimeProjectID confirms handler defaults trace project ID from runtime discovery.
func TestNewHandlerUsesRuntimeProjectID(t *testing.T) {
	resetRuntimeInfoCache()
	withMetadataClient(t, &stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"project/project-id": "meta-project",
		},
	})

	t.Setenv("SLOGCP_TRACE_PROJECT_ID", "")
	t.Setenv("SLOGCP_PROJECT_ID", "")
	t.Setenv("SLOGCP_GCP_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GCLOUD_PROJECT", "")
	t.Setenv("GCP_PROJECT", "")
	t.Setenv("PROJECT_ID", "")

	h, err := NewHandler(io.Discard)
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}
	if got := h.cfg.TraceProjectID; got != "meta-project" {
		t.Fatalf("TraceProjectID = %q, want %q", got, "meta-project")
	}
}

// TestDetectRuntimeInfoVariants covers the major environments slogcp supports.
func TestDetectRuntimeInfoVariants(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T)
		wantEnv    RuntimeEnvironment
		wantProj   string
		wantLabels map[string]string
		wantSvc    map[string]string
	}{
		{
			name: "cloud_run_job",
			setup: func(t *testing.T) {
				t.Setenv("CLOUD_RUN_JOB", "job")
				t.Setenv("CLOUD_RUN_EXECUTION", "exec")
				t.Setenv("CLOUD_RUN_TASK_INDEX", "1")
				t.Setenv("CLOUD_RUN_TASK_ATTEMPT", "2")
				t.Setenv("CLOUD_RUN_REGION", "us-central1")
				t.Setenv("GOOGLE_CLOUD_PROJECT", "run-project")
			},
			wantEnv:  RuntimeEnvCloudRunJob,
			wantProj: "run-project",
			wantSvc: map[string]string{
				"service": "job",
				"version": "exec",
			},
			wantLabels: map[string]string{
				"cloud_run.job":        "job",
				"cloud_run.execution":  "exec",
				"cloud_run.task_index": "1",
				"cloud_run.region":     "us-central1",
			},
		},
		{
			name: "cloud_functions",
			setup: func(t *testing.T) {
				t.Setenv("K_SERVICE", "func")
				t.Setenv("FUNCTION_TARGET", "Target")
				t.Setenv("FUNCTION_SIGNATURE_TYPE", "http")
				t.Setenv("GOOGLE_CLOUD_PROJECT", "functions-project")
				t.Setenv("FUNCTION_REGION", "europe-west1")
				t.Setenv("K_REVISION", "rev-1")
			},
			wantEnv:  RuntimeEnvCloudFunctions,
			wantProj: "functions-project",
			wantSvc: map[string]string{
				"service": "func",
				"version": "rev-1",
			},
			wantLabels: map[string]string{
				"cloud_function.name":   "func",
				"cloud_function.target": "Target",
				"cloud_function.region": "europe-west1",
			},
		},
		{
			name: "app_engine_standard",
			setup: func(t *testing.T) {
				withMetadataClient(t, &stubMetadataClient{
					onGCE: true,
					values: map[string]string{
						"instance/zone": "projects/p/zones/us-central1-b",
					},
				})
				t.Setenv("GAE_SERVICE", "default")
				t.Setenv("GAE_VERSION", "20191111t111111")
				t.Setenv("GAE_INSTANCE", "instance-1")
				t.Setenv("GAE_APPLICATION", "proj")
			},
			wantEnv:  RuntimeEnvAppEngineStandard,
			wantProj: "proj",
			wantSvc: map[string]string{
				"service": "default",
				"version": "20191111t111111",
			},
			wantLabels: map[string]string{
				"appengine.service":  "default",
				"appengine.version":  "20191111t111111",
				"appengine.instance": "instance-1",
				"appengine.zone":     "us-central1-b",
			},
		},
		{
			name: "app_engine_flexible",
			setup: func(t *testing.T) {
				withMetadataClient(t, &stubMetadataClient{
					onGCE: true,
					values: map[string]string{
						"instance/zone": "projects/p/zones/europe-west1-b",
					},
				})
				t.Setenv("GAE_ENV", "flex")
				t.Setenv("GAE_SERVICE", "service-flex")
				t.Setenv("GAE_VERSION", "v1")
				t.Setenv("GAE_INSTANCE", "instance-flex")
				t.Setenv("GOOGLE_CLOUD_PROJECT", "flex-proj")
			},
			wantEnv:  RuntimeEnvAppEngineFlexible,
			wantProj: "flex-proj",
			wantSvc: map[string]string{
				"service": "service-flex",
				"version": "v1",
			},
			wantLabels: map[string]string{
				"appengine.service":  "service-flex",
				"appengine.version":  "v1",
				"appengine.instance": "instance-flex",
				"appengine.zone":     "europe-west1-b",
			},
		},
		{
			name: "gke",
			setup: func(t *testing.T) {
				withMetadataClient(t, &stubMetadataClient{
					onGCE: true,
					values: map[string]string{
						"instance/attributes/cluster-name":     "cluster-1",
						"instance/attributes/cluster-location": "us-east1",
					},
				})
				t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
				t.Setenv("GOOGLE_CLOUD_PROJECT", "gke-proj")
				t.Setenv("NAMESPACE_NAME", "payments")
				t.Setenv("POD_NAME", "pod-1")
				t.Setenv("CONTAINER_NAME", "container-1")
			},
			wantEnv:  RuntimeEnvKubernetes,
			wantProj: "gke-proj",
			wantLabels: map[string]string{
				"k8s.cluster.name":   "cluster-1",
				"k8s.location":       "us-east1",
				"k8s.namespace.name": "payments",
				"k8s.pod.name":       "pod-1",
				"k8s.container.name": "container-1",
			},
		},
		{
			name: "gce",
			setup: func(t *testing.T) {
				withMetadataClient(t, &stubMetadataClient{
					onGCE: true,
					values: map[string]string{
						"instance/id":        "789",
						"instance/zone":      "projects/p/zones/us-west1-a",
						"project/project-id": "gce-proj",
					},
				})
			},
			wantEnv:  RuntimeEnvComputeEngine,
			wantProj: "gce-proj",
			wantLabels: map[string]string{
				"gce.instance_id": "789",
				"gce.zone":        "us-west1-a",
			},
		},
		{
			name: "unknown",
			setup: func(t *testing.T) {
				withMetadataClient(t, &stubMetadataClient{onGCE: false})
			},
			wantEnv:  RuntimeEnvUnknown,
			wantProj: "",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resetRuntimeInfoCache()
			if tc.setup != nil {
				tc.setup(t)
			}
			info := detectRuntimeInfo()
			if info.Environment != tc.wantEnv {
				t.Fatalf("Environment = %v, want %v", info.Environment, tc.wantEnv)
			}
			if info.ProjectID != tc.wantProj {
				t.Fatalf("ProjectID = %q, want %q", info.ProjectID, tc.wantProj)
			}
			for k, v := range tc.wantLabels {
				if got := info.Labels[k]; got != v {
					t.Fatalf("label %q = %q, want %q", k, got, v)
				}
			}
			for k, v := range tc.wantSvc {
				if got := info.ServiceContext[k]; got != v {
					t.Fatalf("serviceContext[%q] = %q, want %q", k, got, v)
				}
			}
		})
	}
}

// TestRuntimeDefaults verifies the managed runtime heuristics that drive handler defaults.
func TestRuntimeDefaults(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T)
		wantEmit    bool
		wantAliases bool
	}{
		{
			name: "cloud_run_service_defaults",
			setup: func(t *testing.T) {
				t.Setenv("K_SERVICE", "svc")
				t.Setenv("K_REVISION", "rev")
				t.Setenv("K_CONFIGURATION", "cfg")
			},
			wantEmit:    false,
			wantAliases: true,
		},
		{
			name: "cloud_run_job_defaults",
			setup: func(t *testing.T) {
				t.Setenv("CLOUD_RUN_JOB", "job")
				t.Setenv("CLOUD_RUN_EXECUTION", "exec")
				t.Setenv("CLOUD_RUN_TASK_INDEX", "0")
				t.Setenv("CLOUD_RUN_TASK_ATTEMPT", "0")
			},
			wantEmit:    false,
			wantAliases: true,
		},
		{
			name: "cloud_functions_defaults",
			setup: func(t *testing.T) {
				t.Setenv("K_SERVICE", "svc")
				t.Setenv("FUNCTION_TARGET", "target")
				t.Setenv("FUNCTION_SIGNATURE_TYPE", "http")
			},
			wantEmit:    false,
			wantAliases: true,
		},
		{
			name: "gce_defaults",
			setup: func(t *testing.T) {
				withMetadataClient(t, &stubMetadataClient{
					onGCE: true,
					values: map[string]string{
						"project/project-id": "gce-proj",
					},
				})
			},
			wantEmit:    true,
			wantAliases: false,
		},
		{
			name: "unknown_defaults",
			setup: func(t *testing.T) {
				withMetadataClient(t, &stubMetadataClient{onGCE: false})
			},
			wantEmit:    true,
			wantAliases: false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			resetRuntimeInfoCache()
			if tc.setup != nil {
				tc.setup(t)
			}
			if got := defaultEmitTimeField(); got != tc.wantEmit {
				t.Fatalf("defaultEmitTimeField = %v, want %v", got, tc.wantEmit)
			}
			if got := defaultUseShortSeverityNames(); got != tc.wantAliases {
				t.Fatalf("defaultUseShortSeverityNames = %v, want %v", got, tc.wantAliases)
			}
		})
	}
}

// TestMetadataLookupRegionZone verifies region and zone parsing.
func TestMetadataLookupRegionZone(t *testing.T) {
	t.Parallel()

	lookup := newMetadataLookup(&stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"instance/region": "projects/1/regions/us-central1",
			"instance/zone":   "projects/1/zones/us-central1-b",
		},
	})

	if got := lookup.region(); got != "us-central1" {
		t.Fatalf("region() = %q, want %q", got, "us-central1")
	}
	if got := lookup.zone(); got != "us-central1-b" {
		t.Fatalf("zone() = %q, want %q", got, "us-central1-b")
	}
}

// TestMetadataLookupClusterDetails ensures cluster metadata helpers trim values.
func TestMetadataLookupClusterDetails(t *testing.T) {
	t.Parallel()

	lookup := newMetadataLookup(&stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"instance/attributes/cluster-name":     "gke-cluster",
			"instance/attributes/cluster-location": "us-east1-a",
		},
	})

	if got := lookup.clusterName(); got != "gke-cluster" {
		t.Fatalf("clusterName() = %q, want %q", got, "gke-cluster")
	}
	if got := lookup.clusterLocation(); got != "us-east1-a" {
		t.Fatalf("clusterLocation() = %q, want %q", got, "us-east1-a")
	}
}

// TestMetadataLookupRegionZoneFallbacks covers raw region/zone values and missing data.
func TestMetadataLookupRegionZoneFallbacks(t *testing.T) {
	t.Parallel()

	lookup := newMetadataLookup(&stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"instance/region": "us-west1",
			"instance/zone":   "us-west1-b",
		},
	})

	if got := lookup.region(); got != "us-west1" {
		t.Fatalf("region() = %q, want %q", got, "us-west1")
	}
	if got := lookup.zone(); got != "us-west1-b" {
		t.Fatalf("zone() = %q, want %q", got, "us-west1-b")
	}

	missing := newMetadataLookup(&stubMetadataClient{onGCE: true})
	if got := missing.region(); got != "" {
		t.Fatalf("region() with no metadata = %q, want empty", got)
	}
	if got := missing.zone(); got != "" {
		t.Fatalf("zone() with no metadata = %q, want empty", got)
	}
}

// TestMetadataLookupAvailabilityGuard exercises nil and cached availability paths.
func TestMetadataLookupAvailabilityGuard(t *testing.T) {
	t.Parallel()

	var nilLookup *metadataLookup
	if nilLookup.isAvailable() {
		t.Fatalf("nil lookup unexpectedly reported availability")
	}

	l := newMetadataLookup(nil)
	if l.isAvailable() {
		t.Fatalf("lookup without client unexpectedly reported availability")
	}

	counter := &countingMetadataClient{onGCE: true}
	l = newMetadataLookup(counter)
	if !l.isAvailable() {
		t.Fatalf("lookup with OnGCE=true reported unavailable")
	}
	if !l.isAvailable() {
		t.Fatalf("lookup should continue reporting cached availability")
	}
	if counter.calls != 1 {
		t.Fatalf("OnGCE() call count = %d, want 1", counter.calls)
	}

	counter = &countingMetadataClient{onGCE: false}
	l = newMetadataLookup(counter)
	if l.isAvailable() {
		t.Fatalf("lookup with OnGCE=false reported available")
	}
	if counter.calls != 1 {
		t.Fatalf("OnGCE() call count = %d, want 1", counter.calls)
	}
}

// TestDefaultMetadataClientHooks verifies that the override points are exercised.
func TestDefaultMetadataClientHooks(t *testing.T) {
	t.Parallel()

	origOnGCE := metadataOnGCEFunc
	origGet := metadataGetFunc
	t.Cleanup(func() {
		metadataOnGCEFunc = origOnGCE
		metadataGetFunc = origGet
	})

	var (
		onGCECalled  bool
		getCalled    bool
		capturedCtx  context.Context
		capturedPath string
	)

	metadataOnGCEFunc = func() bool {
		onGCECalled = true
		return true
	}
	metadataGetFunc = func(ctx context.Context, path string) (string, error) {
		getCalled = true
		capturedCtx = ctx
		capturedPath = path
		return "stub-value", nil
	}

	client := defaultMetadataClient{}
	if !client.OnGCE() {
		t.Fatalf("OnGCE() = false, want true")
	}
	if !onGCECalled {
		t.Fatalf("override hook for OnGCE was not invoked")
	}
	val, err := client.Get("project/project-id")
	if err != nil {
		t.Fatalf("Get() returned unexpected error: %v", err)
	}
	if !getCalled {
		t.Fatalf("override hook for Get was not invoked")
	}
	if capturedCtx == nil {
		t.Fatalf("Get() did not provide a context")
	}
	if capturedPath != "project/project-id" {
		t.Fatalf("Get() path = %q, want %q", capturedPath, "project/project-id")
	}
	if val != "stub-value" {
		t.Fatalf("Get() = %q, want %q", val, "stub-value")
	}
}
