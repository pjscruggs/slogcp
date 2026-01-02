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

package slogcp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"cloud.google.com/go/compute/metadata"
)

// init disables runtime info caching during tests to avoid cross-test state.
func init() {
	runtimeInfoCacheDisabled.Store(true)
}

// resetRuntimeInfoCache clears cached runtime inspection state for isolated tests.
func resetRuntimeInfoCache() {
	runtimeInfoMu.Lock()
	runtimeInfoOnce = sync.Once{}
	runtimeInfo = RuntimeInfo{}
	runtimeInfoPreset.Store(false)
	runtimeInfoMu.Unlock()
	resetHandlerConfigCache()
}

// stubRuntimeInfo seeds cached runtime info for tests that need deterministic values.
func stubRuntimeInfo(info RuntimeInfo) {
	runtimeInfoMu.Lock()
	runtimeInfoOnce = sync.Once{}
	runtimeInfoOnce.Do(func() {
		runtimeInfo = info
	})
	runtimeInfoPreset.Store(true)
	runtimeInfoMu.Unlock()
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
	original := getMetadataClientFactory()
	setMetadataClientFactory(func() metadataClient { return client })
	t.Cleanup(func() { setMetadataClientFactory(original) })
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

type countingGetMetadataClient struct {
	*stubMetadataClient
	getCalls int
}

// Get increments the call counter and proxies the stub metadata client lookup.
func (c *countingGetMetadataClient) Get(path string) (string, error) {
	c.getCalls++
	return c.stubMetadataClient.Get(path)
}

type timeoutMetadataError struct{}

// Error implements [error] and reports the stub metadata failure.
func (timeoutMetadataError) Error() string { return "metadata timeout" }

// Timeout implements [net.Error] and reports a timeout condition.
func (timeoutMetadataError) Timeout() bool { return true }

// Temporary implements [net.Error] and reports the error as temporary.
func (timeoutMetadataError) Temporary() bool { return true }

type failingMetadataClient struct {
	onGCE    bool
	getCalls int
}

// OnGCE reports whether the stub metadata client is reachable.
func (c *failingMetadataClient) OnGCE() bool { return c.onGCE }

// Get returns a timeout-like error to simulate metadata transport failures.
func (c *failingMetadataClient) Get(string) (string, error) {
	c.getCalls++
	return "", timeoutMetadataError{}
}

// TestDetectKubernetesUsesMetadata verifies metadata-derived clusters populate labels.
func TestDetectKubernetesUsesMetadata(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("POD_NAME", "api-0")
	t.Setenv("CONTAINER_NAME", "edge")

	tmp := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(tmp, []byte("prod\n"), 0o600); err != nil {
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

// TestDetectKubernetesLegacyFallback exercises HOSTNAME and NAMESPACE fallbacks.
func TestDetectKubernetesLegacyFallback(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("CLUSTER_NAME", "legacy-cluster")
	t.Setenv("CLUSTER_LOCATION", "asia-south1")
	t.Setenv("NAMESPACE_NAME", "")
	t.Setenv("NAMESPACE", "legacy-ns")
	t.Setenv("POD_NAME", "")
	t.Setenv("HOSTNAME", "legacy-host")
	t.Setenv("CONTAINER_NAME", "legacy-container")
	t.Setenv("SLOGCP_GCP_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	tmpNamespace := filepath.Join(t.TempDir(), "namespace")
	prevPath := kubernetesNamespacePath
	kubernetesNamespacePath = tmpNamespace
	t.Cleanup(func() { kubernetesNamespacePath = prevPath })

	info := RuntimeInfo{}
	lookup := newMetadataLookup(&stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"project/project-id": "legacy-project",
		},
	})

	if !detectKubernetes(&info, lookup) {
		t.Fatalf("detectKubernetes returned false")
	}
	if got := info.Labels["k8s.namespace.name"]; got != "legacy-ns" {
		t.Fatalf("k8s.namespace.name = %q, want %q", got, "legacy-ns")
	}
	if got := info.Labels["k8s.pod.name"]; got != "legacy-host" {
		t.Fatalf("k8s.pod.name = %q, want %q", got, "legacy-host")
	}
	if got := info.ProjectID; got != "legacy-project" {
		t.Fatalf("ProjectID = %q, want %q", got, "legacy-project")
	}
}

// TestDetectKubernetesRequiresClusterName exercises the guard rails for missing cluster metadata.
func TestDetectKubernetesRequiresClusterName(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")

	info := RuntimeInfo{}
	lookup := newMetadataLookup(&stubMetadataClient{onGCE: true})

	if detectKubernetes(&info, lookup) {
		t.Fatalf("detectKubernetes should return false when cluster name is unavailable")
	}
}

// TestReadNamespaceUsesOverride ensures readNamespace honours the configurable path.
func TestReadNamespaceUsesOverride(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "namespace")
	if err := os.WriteFile(tmp, []byte("observability\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile(%q) = %v", tmp, err)
	}
	prev := kubernetesNamespacePath
	kubernetesNamespacePath = tmp
	t.Cleanup(func() { kubernetesNamespacePath = prev })

	if got := readNamespace(); got != "observability" {
		t.Fatalf("readNamespace() = %q, want %q", got, "observability")
	}
}

// TestDetectRuntimeInfoFallsBackToMetadata verifies metadata-derived project IDs populate defaults.
func TestDetectRuntimeInfoFallsBackToMetadata(t *testing.T) {
	resetRuntimeInfoCache()
	t.Cleanup(resetRuntimeInfoCache)

	withMetadataClient(t, &stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"project/project-id": "meta-project",
		},
	})

	info := DetectRuntimeInfo()
	if info.ProjectID != "meta-project" {
		t.Fatalf("ProjectID = %q, want %q", info.ProjectID, "meta-project")
	}
}

// TestDetectRuntimeInfoCloudRunService verifies Cloud Run environment variables populate runtime metadata.
func TestDetectRuntimeInfoCloudRunService(t *testing.T) {
	resetRuntimeInfoCache()
	t.Setenv("K_SERVICE", "svc")
	t.Setenv("K_REVISION", "rev")
	t.Setenv("K_CONFIGURATION", "cfg")
	t.Setenv("CLOUD_RUN_REGION", "us-central1")
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
	if got := info.Labels["cloud_run.region"]; got != "us-central1" {
		t.Fatalf("label cloud_run.region = %q, want %q", got, "us-central1")
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

// TestDetectAppEngineUsesMetadataProjectID ensures App Engine discovery falls back to metadata.
func TestDetectAppEngineUsesMetadataProjectID(t *testing.T) {
	t.Setenv("GAE_SERVICE", "default")
	t.Setenv("GAE_VERSION", "v2")
	t.Setenv("GAE_INSTANCE", "instance-2")
	t.Setenv("GAE_APPLICATION", "")
	t.Setenv("SLOGCP_GCP_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")

	info := RuntimeInfo{}
	lookup := newMetadataLookup(&stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"project/project-id": "meta-app",
		},
	})

	if !detectAppEngine(&info, lookup) {
		t.Fatalf("detectAppEngine returned false")
	}
	if got := info.ProjectID; got != "meta-app" {
		t.Fatalf("ProjectID = %q, want %q", got, "meta-app")
	}
	if info.Environment != RuntimeEnvAppEngineStandard {
		t.Fatalf("Environment = %v, want %v", info.Environment, RuntimeEnvAppEngineStandard)
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
				t.Setenv("GAE_APPLICATION", "proj-123")
			},
			wantEnv:  RuntimeEnvAppEngineStandard,
			wantProj: "proj-123",
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

// TestMetadataLookupCachesEntries verifies that metadata lookups reuse cached values.
func TestMetadataLookupCachesEntries(t *testing.T) {
	t.Parallel()

	client := &countingGetMetadataClient{
		stubMetadataClient: &stubMetadataClient{
			onGCE: true,
			values: map[string]string{
				"project/project-id": "cached-project",
			},
		},
	}

	lookup := newMetadataLookup(client)

	if got, ok := lookup.get("project/project-id"); !ok || got != "cached-project" {
		t.Fatalf("first lookup = %q, ok=%v; want cached-project, true", got, ok)
	}
	if client.getCalls != 1 {
		t.Fatalf("Get call count = %d, want 1", client.getCalls)
	}
	if got, ok := lookup.get("project/project-id"); !ok || got != "cached-project" {
		t.Fatalf("cached lookup = %q, ok=%v; want cached-project, true", got, ok)
	}
	if client.getCalls != 1 {
		t.Fatalf("cached lookup should not invoke metadata client; got %d calls", client.getCalls)
	}
}

// TestCachedConfigFromEnvHandlesClearedCache exercises the fallback path when the cache is cleared mid-race.
func TestCachedConfigFromEnvHandlesClearedCache(t *testing.T) {
	t.Parallel()

	resetHandlerConfigCache()
	origLoader := getLoadConfigFromEnv()
	origHook := getCachedConfigRaceHook()
	t.Cleanup(func() {
		setLoadConfigFromEnv(origLoader)
		setCachedConfigRaceHook(origHook)
		resetHandlerConfigCache()
	})

	start := make(chan struct{}, 2)
	release := make(chan struct{})
	setLoadConfigFromEnv(func(logger *slog.Logger) (handlerConfig, error) {
		start <- struct{}{}
		<-release
		return handlerConfig{Writer: io.Discard}, nil
	})
	setCachedConfigRaceHook(func() {
		resetHandlerConfigCache()
	})

	logger := slog.New(slog.DiscardHandler)

	var wg sync.WaitGroup
	results := make(chan error, 2)
	worker := func() {
		defer wg.Done()
		_, err := cachedConfigFromEnv(logger)
		results <- err
	}

	wg.Add(2)
	go worker()
	go worker()

	<-start
	<-start
	close(release)

	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("cachedConfigFromEnv returned %v", err)
		}
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

// TestClusterLocationNilLookupUsesEnv ensures clusterLocation falls back to environment variables when metadata is nil.
func TestClusterLocationNilLookupUsesEnv(t *testing.T) {
	t.Setenv("CLUSTER_LOCATION", "us-central1")
	if got := clusterLocation(nil); got != "us-central1" {
		t.Fatalf("clusterLocation(nil) = %q, want %q", got, "us-central1")
	}
}

// TestMetadataLookupDisablesAvailability exercises the helper that classifies metadata failures.
func TestMetadataLookupDisablesAvailability(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "context_canceled", err: context.Canceled, want: true},
		{name: "metadata_not_defined", err: metadata.NotDefinedError("instance/region"), want: false},
		{name: "generic_error", err: errors.New("boom"), want: false},
		{name: "net_error", err: timeoutMetadataError{}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := metadataLookupDisablesAvailability(tt.err); got != tt.want {
				t.Fatalf("metadataLookupDisablesAvailability(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestMetadataLookupDisablesAfterNetError ensures transient network errors disable further metadata lookups.
func TestMetadataLookupDisablesAfterNetError(t *testing.T) {
	t.Parallel()

	client := &failingMetadataClient{onGCE: true}
	lookup := newMetadataLookup(client)
	_ = lookup.region()
	if client.getCalls != 1 {
		t.Fatalf("Get() call count after region() = %d, want 1", client.getCalls)
	}

	_ = lookup.projectID()
	if client.getCalls != 1 {
		t.Fatalf("Get() call count after projectID() = %d, want 1", client.getCalls)
	}
}

// TestResolveClusterAndProjectFallbacks covers nil metadata lookups and current project preservation.
func TestResolveClusterAndProjectFallbacks(t *testing.T) {
	t.Parallel()

	if name := resolveClusterName(nil); name != "" {
		t.Fatalf("resolveClusterName(nil) = %q, want empty", name)
	}
	if proj := resolveKubernetesProject("", nil); proj != "" {
		t.Fatalf("resolveKubernetesProject(nil) = %q, want empty", proj)
	}

	lookup := newMetadataLookup(&stubMetadataClient{
		onGCE: true,
		values: map[string]string{
			"instance/attributes/cluster-name": "test-cluster",
			"project/project-id":               "meta-project",
		},
	})

	if name := resolveClusterName(lookup); name != "test-cluster" {
		t.Fatalf("resolveClusterName() = %q, want %q", name, "test-cluster")
	}
	if proj := resolveKubernetesProject("keep-current", lookup); proj != "keep-current" {
		t.Fatalf("resolveKubernetesProject(current) = %q, want keep-current", proj)
	}
}

// TestDefaultMetadataClientHooks verifies that the override points are exercised.
func TestDefaultMetadataClientHooks(t *testing.T) {
	origOnGCE := getMetadataOnGCEFunc()
	origGet := getMetadataGetFunc()
	t.Cleanup(func() {
		setMetadataOnGCEFunc(origOnGCE)
		setMetadataGetFunc(origGet)
	})

	var (
		onGCECalled  bool
		getCalled    bool
		capturedCtx  context.Context
		capturedPath string
	)

	setMetadataOnGCEFunc(func() bool {
		onGCECalled = true
		return true
	})
	setMetadataGetFunc(func(ctx context.Context, path string) (string, error) {
		getCalled = true
		capturedCtx = ctx
		capturedPath = path
		return "stub-value", nil
	})

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

// TestDefaultMetadataClientUsesWrappers ensures the default wrapper paths are executed.
func TestDefaultMetadataClientUsesWrappers(t *testing.T) {
	origOnGCE := metadataOnGCEWrapper
	origGet := metadataGetWithContextWrapper
	t.Cleanup(func() {
		metadataOnGCEWrapper = origOnGCE
		metadataGetWithContextWrapper = origGet
	})

	var (
		onGCECalled bool
		getCalled   bool
	)

	metadataOnGCEWrapper = func() bool {
		onGCECalled = true
		return true
	}
	metadataGetWithContextWrapper = func(ctx context.Context, path string) (string, error) {
		getCalled = true
		if ctx == nil {
			t.Fatalf("expected non-nil context")
		}
		if path != "project/project-id" {
			t.Fatalf("path = %q, want project/project-id", path)
		}
		return "wrapped-id", nil
	}

	client := defaultMetadataClient{}
	if !client.OnGCE() {
		t.Fatalf("OnGCE() = false, want true")
	}
	if !onGCECalled {
		t.Fatalf("wrapper for metadata.OnGCE was not invoked")
	}
	val, err := client.Get("project/project-id")
	if err != nil {
		t.Fatalf("Get() returned error: %v", err)
	}
	if val != "wrapped-id" {
		t.Fatalf("Get() = %q, want %q", val, "wrapped-id")
	}
	if !getCalled {
		t.Fatalf("wrapper for metadata.GetWithContext was not invoked")
	}
}

// TestMetadataHookFallbacks exercises nil and zero-value paths for metadata hooks.
func TestMetadataHookFallbacks(t *testing.T) {
	origOnGCE := getMetadataOnGCEFunc()
	origGet := getMetadataGetFunc()
	origFactory := getMetadataClientFactory()
	origOnGCEWrapper := metadataOnGCEWrapper
	origGetWrapper := metadataGetWithContextWrapper
	t.Cleanup(func() {
		setMetadataOnGCEFunc(origOnGCE)
		setMetadataGetFunc(origGet)
		setMetadataClientFactory(origFactory)
		metadataOnGCEWrapper = origOnGCEWrapper
		metadataGetWithContextWrapper = origGetWrapper
	})

	metadataOnGCEWrapper = func() bool { return true }
	metadataGetWithContextWrapper = func(ctx context.Context, path string) (string, error) {
		return "wrapped", nil
	}

	setMetadataOnGCEFunc(nil)
	if fn := getMetadataOnGCEFunc(); fn == nil {
		t.Fatalf("getMetadataOnGCEFunc returned nil after nil setter")
	} else {
		_ = fn()
	}

	metadataOnGCEFunc = atomic.Value{}
	_ = getMetadataOnGCEFunc()()

	setMetadataGetFunc(nil)
	if val, err := getMetadataGetFunc()(context.Background(), "project/project-id"); err != nil || val != "wrapped" {
		t.Fatalf("getMetadataGetFunc default = %q, %v; want wrapped, nil", val, err)
	}
	metadataGetFunc = atomic.Value{}
	if val, err := getMetadataGetFunc()(context.Background(), "project/project-id"); err != nil || val != "wrapped" {
		t.Fatalf("getMetadataGetFunc fallback = %q, %v; want wrapped, nil", val, err)
	}

	metadataClientFactory = atomic.Value{}
	if factory := getMetadataClientFactory(); factory == nil || factory() == nil {
		t.Fatalf("metadata client factory zero value fallback returned nil")
	}

	metadataClientFactory = atomic.Value{}
	setMetadataClientFactory(nil)
	if factory := getMetadataClientFactory(); factory == nil || factory() == nil {
		t.Fatalf("metadata client factory fallback returned nil")
	}
}

// TestNormalizeProjectID exercises validation and prefix handling for project IDs.
func TestNormalizeProjectID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		input  string
		want   string
		wantOK bool
	}{
		{name: "empty", input: "", want: "", wantOK: false},
		{name: "valid", input: "alpha-123", want: "alpha-123", wantOK: true},
		{name: "valid_min_length", input: "a23456", want: "a23456", wantOK: true},
		{name: "valid_max_length", input: "a12345678901234567890123456789", want: "a12345678901234567890123456789", wantOK: true},
		{name: "with_resource_prefix", input: "projects/my-project", want: "my-project", wantOK: true},
		{name: "with_uppercase_prefix_and_spaces", input: "  PROJECTS/service-001  ", want: "service-001", wantOK: true},
		{name: "normalizes_uppercase", input: "Alpha-123", want: "alpha-123", wantOK: true},
		{name: "reject_trailing_hyphen", input: "alpha-123-", want: "", wantOK: false},
		{name: "reject_short", input: "short", want: "", wantOK: false},
		{name: "reject_long", input: "abcdefghijklmnopqrstuvwxyz12345", want: "", wantOK: false},
		{name: "reject_leading_digit", input: "1project", want: "", wantOK: false},
		{name: "reject_slash", input: "my/project", want: "", wantOK: false},
		{name: "reject_underscore", input: "project_id", want: "", wantOK: false},
		{name: "reject_extra_path", input: "projects/proj/extra", want: "", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := normalizeProjectID(tt.input)
			if got != tt.want || ok != tt.wantOK {
				t.Fatalf("normalizeProjectID(%q) = (%q, %v), want (%q, %v)", tt.input, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
