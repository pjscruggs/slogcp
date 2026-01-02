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
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"cloud.google.com/go/compute/metadata"
)

// RuntimeInfo captures metadata about the current cloud environment.
type RuntimeInfo struct {
	ProjectID      string
	Labels         map[string]string
	ServiceContext map[string]string
	Environment    RuntimeEnvironment
}

// RuntimeEnvironment describes the runtime platform detected for slogcp.
type RuntimeEnvironment int

const (
	// RuntimeEnvUnknown indicates slogcp could not detect a known hosting environment.
	RuntimeEnvUnknown RuntimeEnvironment = iota
	// RuntimeEnvCloudRunService indicates execution on Cloud Run fully managed.
	RuntimeEnvCloudRunService
	// RuntimeEnvCloudRunJob indicates execution on Cloud Run jobs.
	RuntimeEnvCloudRunJob
	// RuntimeEnvCloudFunctions indicates execution on Cloud Functions.
	RuntimeEnvCloudFunctions
	// RuntimeEnvAppEngineStandard indicates execution on App Engine standard.
	RuntimeEnvAppEngineStandard
	// RuntimeEnvAppEngineFlexible indicates execution on App Engine flexible.
	RuntimeEnvAppEngineFlexible
	// RuntimeEnvKubernetes indicates execution on Kubernetes (including Google Kubernetes Engine).
	RuntimeEnvKubernetes
	// RuntimeEnvComputeEngine indicates execution on Compute Engine.
	RuntimeEnvComputeEngine
)

var (
	runtimeInfo              RuntimeInfo
	runtimeInfoOnce          sync.Once
	runtimeInfoMu            sync.Mutex
	runtimeInfoCacheDisabled atomic.Bool
	runtimeInfoPreset        atomic.Bool
)

var (
	metadataOnGCEWrapper          = metadata.OnGCE
	metadataGetWithContextWrapper = metadata.GetWithContext
)

var (
	metadataOnGCEFunc      atomic.Value // func() bool
	metadataGetFunc        atomic.Value // func(context.Context, string) (string, error)
	metadataClientFactory  atomic.Value // func() metadataClient
	metadataFactoryDefault = func() metadataClient { return defaultMetadataClient{} }
)

// init seeds the metadata hooks used for runtime detection.
func init() {
	setMetadataOnGCEFunc(func() bool {
		return metadataOnGCEWrapper()
	})
	setMetadataGetFunc(func(ctx context.Context, path string) (string, error) {
		return metadataGetWithContextWrapper(ctx, path)
	})
	setMetadataClientFactory(metadataFactoryDefault)
}

// setMetadataOnGCEFunc overrides the metadata availability probe.
func setMetadataOnGCEFunc(fn func() bool) {
	if fn == nil {
		fn = func() bool { return metadataOnGCEWrapper() }
	}
	metadataOnGCEFunc.Store(fn)
}

// getMetadataOnGCEFunc returns the current metadata availability probe.
func getMetadataOnGCEFunc() func() bool {
	if fn, ok := metadataOnGCEFunc.Load().(func() bool); ok && fn != nil {
		return fn
	}
	return func() bool { return metadataOnGCEWrapper() }
}

// setMetadataGetFunc overrides the metadata fetch hook.
func setMetadataGetFunc(fn func(context.Context, string) (string, error)) {
	if fn == nil {
		fn = func(ctx context.Context, path string) (string, error) {
			return metadataGetWithContextWrapper(ctx, path)
		}
	}
	metadataGetFunc.Store(fn)
}

// getMetadataGetFunc returns the current metadata fetch hook.
func getMetadataGetFunc() func(context.Context, string) (string, error) {
	if fn, ok := metadataGetFunc.Load().(func(context.Context, string) (string, error)); ok && fn != nil {
		return fn
	}
	return func(ctx context.Context, path string) (string, error) {
		return metadataGetWithContextWrapper(ctx, path)
	}
}

// setMetadataClientFactory overrides the metadata client factory.
func setMetadataClientFactory(fn func() metadataClient) {
	if fn == nil {
		fn = metadataFactoryDefault
	}
	metadataClientFactory.Store(fn)
}

// getMetadataClientFactory returns the current metadata client factory.
func getMetadataClientFactory() func() metadataClient {
	if fn, ok := metadataClientFactory.Load().(func() metadataClient); ok && fn != nil {
		return fn
	}
	return metadataFactoryDefault
}

var runtimeProjectIDEnvKeys = []string{
	"SLOGCP_TRACE_PROJECT_ID",
	"SLOGCP_PROJECT_ID",
	"SLOGCP_GCP_PROJECT",
	"GOOGLE_CLOUD_PROJECT",
	"GCLOUD_PROJECT",
	"GCP_PROJECT",
	"PROJECT_ID",
}

var serviceProjectIDEnvKeys = []string{
	"SLOGCP_GCP_PROJECT",
	"GOOGLE_CLOUD_PROJECT",
	"GCLOUD_PROJECT",
	"GCP_PROJECT",
}

// DetectRuntimeInfo inspects well-known environment variables to infer
// platform-specific labels and service context. Results are cached for reuse.
func DetectRuntimeInfo() RuntimeInfo {
	if runtimeInfoCacheDisabled.Load() {
		if runtimeInfoPreset.Load() {
			runtimeInfoMu.Lock()
			defer runtimeInfoMu.Unlock()
			return runtimeInfo
		}
		return detectRuntimeInfo()
	}

	runtimeInfoMu.Lock()
	defer runtimeInfoMu.Unlock()

	runtimeInfoOnce.Do(func() {
		runtimeInfo = detectRuntimeInfo()
	})
	return runtimeInfo
}

// detectRuntimeInfo inspects environment variables and metadata endpoints to infer runtime context.
func detectRuntimeInfo() RuntimeInfo {
	info := RuntimeInfo{}
	info.ProjectID = resolveProjectIDFromEnv("", runtimeProjectIDEnvKeys...)

	md := newMetadataLookup(getMetadataClientFactory()())

	if detectCloudFunction(&info, md) {
		ensureProjectID(&info, md)
		return info
	}
	if detectCloudRunService(&info, md) {
		ensureProjectID(&info, md)
		return info
	}
	if detectCloudRunJob(&info, md) {
		ensureProjectID(&info, md)
		return info
	}
	if detectAppEngine(&info, md) {
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
	if pid := md.projectID(); pid != "" {
		info.ProjectID = pid
	}
}

// detectCloudFunction populates metadata when running within Cloud Functions.
func detectCloudFunction(info *RuntimeInfo, md *metadataLookup) bool {
	service := trimmedEnv("K_SERVICE")
	target := trimmedEnv("FUNCTION_TARGET")
	signature := trimmedEnv("FUNCTION_SIGNATURE_TYPE")
	if service == "" || target == "" || signature == "" {
		return false
	}

	info.Environment = RuntimeEnvCloudFunctions

	revision := trimmedEnv("K_REVISION")
	region := firstNonEmpty(
		trimmedEnv("FUNCTION_REGION"),
		trimmedEnv("GOOGLE_CLOUD_REGION"),
		trimmedEnv("CLOUD_RUN_REGION"),
		md.region(),
	)

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

	info.ProjectID = resolveProjectIDFromEnv(info.ProjectID, serviceProjectIDEnvKeys...)
	if info.ProjectID == "" {
		info.ProjectID = md.projectID()
	}
	return true
}

// detectCloudRunService populates metadata when running within Cloud Run services.
func detectCloudRunService(info *RuntimeInfo, md *metadataLookup) bool {
	service := trimmedEnv("K_SERVICE")
	revision := trimmedEnv("K_REVISION")
	config := trimmedEnv("K_CONFIGURATION")
	if service == "" || revision == "" || config == "" {
		return false
	}

	info.Environment = RuntimeEnvCloudRunService

	region := firstNonEmpty(
		trimmedEnv("CLOUD_RUN_REGION"),
		trimmedEnv("GOOGLE_CLOUD_REGION"),
		md.region(),
	)

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

	info.ProjectID = resolveProjectIDFromEnv(info.ProjectID, serviceProjectIDEnvKeys...)
	if info.ProjectID == "" {
		info.ProjectID = md.projectID()
	}
	return true
}

// detectCloudRunJob populates metadata when running within Cloud Run jobs.
func detectCloudRunJob(info *RuntimeInfo, md *metadataLookup) bool {
	job := trimmedEnv("CLOUD_RUN_JOB")
	execution := trimmedEnv("CLOUD_RUN_EXECUTION")
	taskIndex := trimmedEnv("CLOUD_RUN_TASK_INDEX")
	taskAttempt := trimmedEnv("CLOUD_RUN_TASK_ATTEMPT")
	if job == "" || execution == "" || taskIndex == "" || taskAttempt == "" {
		return false
	}

	info.Environment = RuntimeEnvCloudRunJob

	region := firstNonEmpty(
		trimmedEnv("CLOUD_RUN_REGION"),
		trimmedEnv("GOOGLE_CLOUD_REGION"),
		md.region(),
	)

	info.ServiceContext = map[string]string{
		"service": job,
		"version": execution,
	}

	labels := map[string]string{
		"cloud_run.job":       job,
		"cloud_run.execution": execution,
	}
	labels["cloud_run.task_index"] = taskIndex
	labels["cloud_run.task_attempt"] = taskAttempt
	if region != "" {
		labels["cloud_run.region"] = region
	}
	info.Labels = labels

	info.ProjectID = resolveProjectIDFromEnv(info.ProjectID, serviceProjectIDEnvKeys...)
	if info.ProjectID == "" {
		info.ProjectID = md.projectID()
	}
	return true
}

// detectAppEngine populates metadata when running within App Engine.
func detectAppEngine(info *RuntimeInfo, md *metadataLookup) bool {
	service := trimmedEnv("GAE_SERVICE")
	version := trimmedEnv("GAE_VERSION")
	instance := trimmedEnv("GAE_INSTANCE")
	if service == "" || version == "" || instance == "" {
		return false
	}

	switch strings.ToLower(trimmedEnv("GAE_ENV")) {
	case "flex":
		info.Environment = RuntimeEnvAppEngineFlexible
	default:
		info.Environment = RuntimeEnvAppEngineStandard
	}

	info.ServiceContext = map[string]string{
		"service": service,
		"version": version,
	}
	labels := map[string]string{
		"appengine.service":  service,
		"appengine.version":  version,
		"appengine.instance": instance,
	}
	if zone := md.zone(); zone != "" {
		labels["appengine.zone"] = zone
	}
	info.Labels = labels

	info.ProjectID = resolveProjectIDFromEnv(info.ProjectID, serviceProjectIDEnvKeys...)
	info.ProjectID = firstValidProjectID(info.ProjectID, strings.TrimPrefix(trimmedEnv("GAE_APPLICATION"), "_"))
	if info.ProjectID == "" {
		info.ProjectID = md.projectID()
	}
	return true
}

// detectKubernetes populates metadata when running inside a Kubernetes cluster.
func detectKubernetes(info *RuntimeInfo, md *metadataLookup) bool {
	if trimmedEnv("KUBERNETES_SERVICE_HOST") == "" {
		return false
	}

	info.Environment = RuntimeEnvKubernetes

	clusterName := resolveClusterName(md)
	if clusterName == "" {
		return false
	}

	info.Labels = kubernetesLabels(md, clusterName)
	info.ProjectID = resolveKubernetesProject(info.ProjectID, md)
	return true
}

// resolveClusterName derives the Kubernetes cluster name from metadata or env.
func resolveClusterName(md *metadataLookup) string {
	if md == nil {
		return ""
	}
	if name := md.clusterName(); name != "" {
		return name
	}
	return trimmedEnv("CLUSTER_NAME")
}

// kubernetesLabels builds base labels describing the cluster, namespace, and pod.
func kubernetesLabels(md *metadataLookup, clusterName string) map[string]string {
	labels := map[string]string{
		"k8s.cluster.name": clusterName,
	}
	if location := clusterLocation(md); location != "" {
		labels["k8s.location"] = location
	}
	if namespace := kubernetesNamespace(); namespace != "" {
		labels["k8s.namespace.name"] = namespace
	}
	if pod := kubernetesPodName(); pod != "" {
		labels["k8s.pod.name"] = pod
	}
	if container := trimmedEnv("CONTAINER_NAME"); container != "" {
		labels["k8s.container.name"] = container
	}
	return labels
}

// clusterLocation returns the cluster location from metadata or environment.
func clusterLocation(md *metadataLookup) string {
	if md == nil {
		return trimmedEnv("CLUSTER_LOCATION")
	}
	return firstNonEmpty(trimmedEnv("CLUSTER_LOCATION"), md.clusterLocation())
}

// kubernetesNamespace resolves the namespace name from service account files or env.
func kubernetesNamespace() string {
	if namespace := readNamespace(); namespace != "" {
		return namespace
	}
	if ns := trimmedEnv("NAMESPACE_NAME"); ns != "" {
		return ns
	}
	return trimmedEnv("NAMESPACE")
}

// kubernetesPodName infers the pod name from env or hostname.
func kubernetesPodName() string {
	if pod := trimmedEnv("POD_NAME"); pod != "" {
		return pod
	}
	return trimmedEnv("HOSTNAME")
}

// resolveKubernetesProject determines the project ID for Kubernetes environments.
func resolveKubernetesProject(current string, md *metadataLookup) string {
	project := resolveProjectIDFromEnv(current, serviceProjectIDEnvKeys...)
	if project != "" {
		return project
	}
	if md == nil {
		return ""
	}
	return md.projectID()
}

// detectComputeEngine populates metadata when running on Google Compute Engine.
func detectComputeEngine(info *RuntimeInfo, md *metadataLookup) bool {
	instanceID := md.instanceID()
	if instanceID == "" {
		return false
	}

	info.Environment = RuntimeEnvComputeEngine

	labels := map[string]string{
		"gce.instance_id": instanceID,
	}
	if zone := md.zone(); zone != "" {
		labels["gce.zone"] = zone
	}
	info.Labels = labels

	if pid := md.projectID(); pid != "" {
		info.ProjectID = pid
	}
	return true
}

// trimmedEnv reads an environment variable and trims surrounding whitespace.
func trimmedEnv(key string) string {
	return strings.TrimSpace(os.Getenv(key))
}

// resolveProjectIDFromEnv returns the first valid project ID from the supplied env keys.
func resolveProjectIDFromEnv(current string, keys ...string) string {
	candidates := make([]string, 0, len(keys)+1)
	candidates = append(candidates, current)
	for _, key := range keys {
		candidates = append(candidates, trimmedEnv(key))
	}
	return firstValidProjectID(candidates...)
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

var projectIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// normalizeProjectID normalizes and validates a Cloud project identifier.
//
// It trims whitespace, strips an optional "projects/" prefix, truncates at the
// first '/', lowercases the result, and validates it against an expected GCP
// project ID format.
func normalizeProjectID(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}

	// Strip "projects/" case-insensitively.
	if strings.HasPrefix(strings.ToLower(s), "projects/") {
		s = s[len("projects/"):]
	}
	s = strings.TrimSpace(s)

	// If a full resource name was passed, keep only the first segment.
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}

	s = strings.ToLower(strings.TrimSpace(s))

	if !projectIDPattern.MatchString(s) {
		return "", false
	}
	return s, true
}

// firstValidProjectID returns the first valid normalized project ID from values.
func firstValidProjectID(values ...string) string {
	for _, v := range values {
		if pid, ok := normalizeProjectID(v); ok {
			return pid
		}
	}
	return ""
}

// kubernetesNamespacePath points at the mounted namespace file in Kubernetes.
// Tests may override this path to exercise readNamespace behaviour without needing
// to access the real serviceaccount mount.
var kubernetesNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// readNamespace reads the Kubernetes namespace from the serviceaccount secret.
func readNamespace() string {
	data, err := os.ReadFile(kubernetesNamespacePath)
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
	client    metadataClient
	cache     map[string]metadataCacheEntry
	once      sync.Once
	available bool
}

// metadataClient abstracts metadata interactions for easier testing.
type metadataClient interface {
	OnGCE() bool
	Get(path string) (string, error)
}

type defaultMetadataClient struct{}

// OnGCE reports whether the GCE metadata server is reachable.
func (defaultMetadataClient) OnGCE() bool {
	return getMetadataOnGCEFunc()()
}

// Get retrieves a metadata value for the provided path.
func (defaultMetadataClient) Get(path string) (string, error) {
	return getMetadataGetFunc()(context.Background(), path)
}

// newMetadataLookup constructs a metadata lookup with local caching.
func newMetadataLookup(client metadataClient) *metadataLookup {
	return &metadataLookup{
		client: client,
		cache:  make(map[string]metadataCacheEntry),
	}
}

// isAvailable caches whether the metadata service can be reached.
func (l *metadataLookup) isAvailable() bool {
	if l == nil {
		return false
	}
	l.once.Do(func() {
		if l.client == nil {
			return
		}
		l.available = l.client.OnGCE()
	})
	return l.available
}

// get retrieves and caches metadata values for the given path.
func (l *metadataLookup) get(path string) (string, bool) {
	if l == nil || !l.isAvailable() {
		return "", false
	}
	if entry, ok := l.cache[path]; ok && entry.populated {
		return entry.value, entry.ok
	}
	val, err := l.client.Get(path)
	if err != nil {
		if metadataLookupDisablesAvailability(err) {
			l.available = false
		}
		l.cache[path] = metadataCacheEntry{populated: true, ok: false}
		return "", false
	}
	val = strings.TrimSpace(val)
	ok := val != ""
	l.cache[path] = metadataCacheEntry{value: val, ok: ok, populated: true}
	return val, ok
}

// metadataLookupDisablesAvailability reports whether a metadata lookup error
// suggests the metadata service is unreachable so subsequent lookups should be
// skipped.
func metadataLookupDisablesAvailability(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var nde metadata.NotDefinedError
	if errors.As(err, &nde) {
		return false
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// projectID reads and normalizes the project ID from metadata.
func (l *metadataLookup) projectID() string {
	if val, ok := l.get("project/project-id"); ok {
		if pid, ok := normalizeProjectID(val); ok {
			return pid
		}
	}
	return ""
}

// region resolves the compute region from metadata, trimming the resource prefix.
func (l *metadataLookup) region() string {
	if val, ok := l.get("instance/region"); ok {
		if idx := strings.LastIndex(val, "/"); idx >= 0 && idx+1 < len(val) {
			return val[idx+1:]
		}
		return val
	}
	return ""
}

// zone resolves the compute zone from metadata, trimming the resource prefix.
func (l *metadataLookup) zone() string {
	if val, ok := l.get("instance/zone"); ok {
		if idx := strings.LastIndex(val, "/"); idx >= 0 && idx+1 < len(val) {
			return val[idx+1:]
		}
		return val
	}
	return ""
}

// instanceID returns the numeric instance identifier from metadata.
func (l *metadataLookup) instanceID() string {
	if val, ok := l.get("instance/id"); ok {
		return val
	}
	return ""
}

// clusterName returns the Kubernetes cluster name derived from metadata.
func (l *metadataLookup) clusterName() string {
	if val, ok := l.get("instance/attributes/cluster-name"); ok {
		return val
	}
	return ""
}

// clusterLocation returns the Kubernetes cluster location derived from metadata.
func (l *metadataLookup) clusterLocation() string {
	if val, ok := l.get("instance/attributes/cluster-location"); ok {
		return val
	}
	return ""
}
