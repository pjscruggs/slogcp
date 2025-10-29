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
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// RuntimeInfo captures metadata about the current cloud environment.
type RuntimeInfo struct {
	ProjectID      string
	Labels         map[string]string
	ServiceContext map[string]string
}

var (
	runtimeInfo     RuntimeInfo
	runtimeInfoOnce sync.Once
)

// DetectRuntimeInfo inspects well-known environment variables to infer
// platform-specific labels and service context. Results are cached for reuse.
func DetectRuntimeInfo() RuntimeInfo {
	runtimeInfoOnce.Do(func() {
		runtimeInfo = detectRuntimeInfo()
	})
	return runtimeInfo
}

// detectRuntimeInfo inspects environment variables and metadata endpoints to infer runtime context.
func detectRuntimeInfo() RuntimeInfo {
	envProject := firstNonEmpty(
		strings.TrimSpace(os.Getenv("SLOGCP_TRACE_PROJECT_ID")),
		strings.TrimSpace(os.Getenv("SLOGCP_PROJECT_ID")),
		strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT")),
		strings.TrimSpace(os.Getenv("GCLOUD_PROJECT")),
		strings.TrimSpace(os.Getenv("GCP_PROJECT")),
		strings.TrimSpace(os.Getenv("PROJECT_ID")),
	)

	info := RuntimeInfo{}
	info.ProjectID = normalizeProjectID(envProject)

	md := newMetadataLookup()

	if detectCloudFunction(&info) {
		ensureProjectID(&info, md)
		return info
	}
	if detectCloudRunService(&info) {
		ensureProjectID(&info, md)
		return info
	}
	if detectCloudRunJob(&info) {
		ensureProjectID(&info, md)
		return info
	}
	if detectAppEngine(&info) {
		ensureProjectID(&info, md)
		return info
	}
	if detectKubernetes(&info, md) {
		ensureProjectID(&info, md)
		return info
	}
	if detectComputeEngine(&info, md) {
		ensureProjectID(&info, md)
		return info
	}

	ensureProjectID(&info, md)
	return info
}

// ensureProjectID populates the project ID using metadata if it has not been set.
func ensureProjectID(info *RuntimeInfo, md *metadataLookup) {
	if info.ProjectID != "" {
		return
	}
	if pid, ok := md.get("project/project-id"); ok && pid != "" {
		info.ProjectID = normalizeProjectID(pid)
	}
}

// detectCloudFunction populates metadata when running within Cloud Functions.
func detectCloudFunction(info *RuntimeInfo) bool {
	service := trimmedEnv("K_SERVICE")
	target := trimmedEnv("FUNCTION_TARGET")
	if service == "" || target == "" {
		return false
	}

	revision := trimmedEnv("K_REVISION")
	region := firstNonEmpty(trimmedEnv("FUNCTION_REGION"), trimmedEnv("GOOGLE_CLOUD_REGION"), trimmedEnv("CLOUD_RUN_REGION"))

	info.ServiceContext = map[string]string{
		"service": service,
	}
	if revision != "" {
		info.ServiceContext["version"] = revision
	}

	labels := map[string]string{
		"cloud_function.name":   service,
		"cloud_function.target": target,
	}
	if region != "" {
		labels["cloud_function.region"] = region
	}
	info.Labels = labels

	info.ProjectID = normalizeProjectID(firstNonEmpty(info.ProjectID, trimmedEnv("GOOGLE_CLOUD_PROJECT")))
	return true
}

// detectCloudRunService populates metadata when running within Cloud Run services.
func detectCloudRunService(info *RuntimeInfo) bool {
	service := trimmedEnv("K_SERVICE")
	revision := trimmedEnv("K_REVISION")
	if service == "" || revision == "" {
		return false
	}

	config := trimmedEnv("K_CONFIGURATION")
	region := firstNonEmpty(trimmedEnv("CLOUD_RUN_REGION"), trimmedEnv("GOOGLE_CLOUD_REGION"))

	info.ServiceContext = map[string]string{
		"service": service,
		"version": revision,
	}

	labels := map[string]string{
		"cloud_run.service":  service,
		"cloud_run.revision": revision,
	}
	if config != "" {
		labels["cloud_run.configuration"] = config
	}
	if region != "" {
		labels["cloud_run.region"] = region
	}
	info.Labels = labels

	info.ProjectID = normalizeProjectID(firstNonEmpty(info.ProjectID, trimmedEnv("GOOGLE_CLOUD_PROJECT")))
	return true
}

// detectCloudRunJob populates metadata when running within Cloud Run jobs.
func detectCloudRunJob(info *RuntimeInfo) bool {
	job := trimmedEnv("CLOUD_RUN_JOB")
	execution := trimmedEnv("CLOUD_RUN_EXECUTION")
	if job == "" || execution == "" {
		return false
	}

	region := firstNonEmpty(trimmedEnv("CLOUD_RUN_REGION"), trimmedEnv("GOOGLE_CLOUD_REGION"))

	info.ServiceContext = map[string]string{
		"service": job,
		"version": execution,
	}

	labels := map[string]string{
		"cloud_run.job":       job,
		"cloud_run.execution": execution,
	}
	if idx := trimmedEnv("CLOUD_RUN_TASK_INDEX"); idx != "" {
		labels["cloud_run.task_index"] = idx
	}
	if attempt := trimmedEnv("CLOUD_RUN_TASK_ATTEMPT"); attempt != "" {
		labels["cloud_run.task_attempt"] = attempt
	}
	if region != "" {
		labels["cloud_run.region"] = region
	}
	info.Labels = labels

	info.ProjectID = normalizeProjectID(firstNonEmpty(info.ProjectID, trimmedEnv("GOOGLE_CLOUD_PROJECT")))
	return true
}

// detectAppEngine populates metadata when running within App Engine.
func detectAppEngine(info *RuntimeInfo) bool {
	service := trimmedEnv("GAE_SERVICE")
	version := trimmedEnv("GAE_VERSION")
	if service == "" && version == "" {
		return false
	}

	info.ServiceContext = map[string]string{}
	labels := map[string]string{}
	if service != "" {
		info.ServiceContext["service"] = service
		labels["appengine.service"] = service
	}
	if version != "" {
		info.ServiceContext["version"] = version
		labels["appengine.version"] = version
	}
	if inst := trimmedEnv("GAE_INSTANCE"); inst != "" {
		labels["appengine.instance"] = inst
	}
	if len(labels) > 0 {
		info.Labels = labels
	}

	candidate := firstNonEmpty(info.ProjectID, trimmedEnv("GOOGLE_CLOUD_PROJECT"), strings.TrimPrefix(trimmedEnv("GAE_APPLICATION"), "_"))
	info.ProjectID = normalizeProjectID(candidate)
	return true
}

// detectKubernetes populates metadata when running inside a Kubernetes cluster.
func detectKubernetes(info *RuntimeInfo, md *metadataLookup) bool {
	if trimmedEnv("KUBERNETES_SERVICE_HOST") == "" {
		return false
	}

	labels := map[string]string{}
	if cluster, ok := md.get("instance/attributes/cluster-name"); ok && cluster != "" {
		labels["k8s.cluster.name"] = cluster
	}
	if location, ok := md.get("instance/attributes/cluster-location"); ok && location != "" {
		labels["k8s.location"] = location
	}

	if namespace := readNamespace(); namespace != "" {
		labels["k8s.namespace.name"] = namespace
	} else if ns := trimmedEnv("NAMESPACE_NAME"); ns != "" {
		labels["k8s.namespace.name"] = ns
	} else if ns := trimmedEnv("NAMESPACE"); ns != "" {
		labels["k8s.namespace.name"] = ns
	}

	if pod := trimmedEnv("POD_NAME"); pod != "" {
		labels["k8s.pod.name"] = pod
	} else if host := trimmedEnv("HOSTNAME"); host != "" {
		labels["k8s.pod.name"] = host
	}
	if container := trimmedEnv("CONTAINER_NAME"); container != "" {
		labels["k8s.container.name"] = container
	}
	if len(labels) > 0 {
		info.Labels = labels
	}

	info.ProjectID = normalizeProjectID(firstNonEmpty(info.ProjectID, trimmedEnv("GOOGLE_CLOUD_PROJECT")))
	if info.ProjectID == "" {
		if pid, ok := md.get("project/project-id"); ok && pid != "" {
			info.ProjectID = normalizeProjectID(pid)
		}
	}
	return true
}

// detectComputeEngine populates metadata when running on Google Compute Engine.
func detectComputeEngine(info *RuntimeInfo, md *metadataLookup) bool {
	instanceID, ok := md.get("instance/id")
	if !ok || instanceID == "" {
		return false
	}

	labels := map[string]string{}
	labels["gce.instance_id"] = instanceID
	if zone, ok := md.get("instance/zone"); ok && zone != "" {
		if idx := strings.LastIndex(zone, "/"); idx >= 0 && idx+1 < len(zone) {
			zone = zone[idx+1:]
		}
		labels["gce.zone"] = zone
	}
	info.Labels = labels

	if pid, ok := md.get("project/project-id"); ok && pid != "" {
		info.ProjectID = normalizeProjectID(pid)
	}
	return true
}

// trimmedEnv reads an environment variable and trims surrounding whitespace.
func trimmedEnv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

// firstNonEmpty returns the first non-empty string after trimming whitespace.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// normalizeProjectID strips common prefixes and leading underscores from project IDs.
func normalizeProjectID(id string) string {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "projects/")
	id = strings.TrimPrefix(id, "PROJECTS/")
	id = strings.TrimPrefix(id, "_")
	return id
}

// readNamespace reads the Kubernetes namespace from the serviceaccount secret.
func readNamespace() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

type metadataCacheEntry struct {
	value     string
	ok        bool
	populated bool
}

type metadataLookup struct {
	cache map[string]metadataCacheEntry
}

// newMetadataLookup constructs a metadata lookup with local caching.
func newMetadataLookup() *metadataLookup {
	return &metadataLookup{cache: make(map[string]metadataCacheEntry)}
}

// get retrieves and caches metadata values for the given path.
func (l *metadataLookup) get(path string) (string, bool) {
	if entry, ok := l.cache[path]; ok && entry.populated {
		return entry.value, entry.ok
	}
	val, ok := metadataFetch(path)
	l.cache[path] = metadataCacheEntry{value: val, ok: ok, populated: true}
	return val, ok
}

var metadataFetch = defaultMetadataFetch

// defaultMetadataFetch performs an HTTP request to the GCE metadata service.
func defaultMetadataFetch(path string) (string, bool) {
	host := trimmedEnv("GCE_METADATA_HOST")
	if host == "" {
		host = "metadata.google.internal"
	}
	url := fmt.Sprintf("http://%s/computeMetadata/v1/%s", host, path)

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("Metadata-Flavor", "Google")

	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			return "", false
		}
		return "", false
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(body)), true
}
