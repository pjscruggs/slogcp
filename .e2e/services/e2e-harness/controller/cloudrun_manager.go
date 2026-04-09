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
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/googleapi"
	run "google.golang.org/api/run/v1"
)

// CloudRunManager manages lifecycle operations for Cloud Run services used in E2E tests.
type CloudRunManager struct {
	projectID      string
	region         string
	runID          string
	serviceAccount string
	invokerMembers []string
	servicesAPI    *run.ProjectsLocationsServicesService
}

// NewCloudRunManager constructs a CloudRunManager.
func NewCloudRunManager(ctx context.Context, projectID, region, runID, serviceAccount, callerServiceAccount string) (*CloudRunManager, error) {
	apiService, err := run.NewService(ctx)
	if err != nil {
		return nil, fmt.Errorf("creating Cloud Run API service: %w", err)
	}

	return &CloudRunManager{
		projectID:      projectID,
		region:         region,
		runID:          runID,
		serviceAccount: serviceAccount,
		invokerMembers: buildInvokerMembers(serviceAccount, callerServiceAccount),
		servicesAPI:    run.NewProjectsLocationsServicesService(apiService),
	}, nil
}

// ServiceConfig describes the deployment configuration for a Cloud Run service.
type ServiceConfig struct {
	// Name is the fully qualified service name (without the projects/locations prefix).
	Name string
	// Image is the container image URI to deploy.
	Image string
	// Env contains the environment variables that should be injected into the service.
	Env map[string]string
	// Labels are attached to the Cloud Run service metadata.
	Labels map[string]string
	// Port is the container port exposed. Defaults to 8080 if zero.
	Port int64
}

// ServiceInstance represents a deployed Cloud Run service.
type ServiceInstance struct {
	Name string
	URL  string

	deleteFn func(ctx context.Context) error
}

// Cleanup deletes the Cloud Run service.
func (i *ServiceInstance) Cleanup(ctx context.Context) error {
	if i == nil || i.deleteFn == nil {
		return nil
	}
	return i.deleteFn(ctx)
}

// GenerateServiceName returns a unique service name for the provided base identifier.
func (m *CloudRunManager) GenerateServiceName(base string) string {
	suffix := randomSuffix(6)
	parts := []string{strings.TrimSpace(base)}
	if m.runID != "" {
		parts = append(parts, m.runID)
	}
	parts = append(parts, suffix)
	return strings.ToLower(strings.Join(parts, "-"))
}

// DeployService creates a Cloud Run service according to the supplied configuration.
func (m *CloudRunManager) DeployService(ctx context.Context, cfg ServiceConfig) (*ServiceInstance, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("service name is required")
	}
	if cfg.Image == "" {
		return nil, fmt.Errorf("container image is required for service %s", cfg.Name)
	}

	containerPort := cfg.Port
	if containerPort == 0 {
		containerPort = 8080
	}

	container := &run.Container{
		Image: cfg.Image,
		Ports: []*run.ContainerPort{
			{ContainerPort: containerPort},
		},
		Env: buildEnvVars(cfg.Env),
	}

	service := &run.Service{
		ApiVersion: "serving.knative.dev/v1",
		Kind:       "Service",
		Metadata: &run.ObjectMeta{
			Name:   cfg.Name,
			Labels: cfg.Labels,
		},
		Spec: &run.ServiceSpec{
			Template: &run.RevisionTemplate{
				Metadata: &run.ObjectMeta{},
				Spec: &run.RevisionSpec{
					ServiceAccountName: m.serviceAccount,
					Containers:         []*run.Container{container},
				},
			},
		},
	}

	parent := m.location()
	if _, err := m.servicesAPI.Create(parent, service).Context(ctx).Do(); err != nil {
		return nil, fmt.Errorf("creating Cloud Run service %s: %w", cfg.Name, err)
	}

	resourceName := m.serviceResourceName(cfg.Name)
	if err := m.ensureInvokerPolicy(ctx, resourceName); err != nil {
		return nil, fmt.Errorf("updating IAM policy for %s: %w", cfg.Name, err)
	}

	service, err := m.waitForServiceReady(ctx, resourceName)
	if err != nil {
		return nil, fmt.Errorf("waiting for service %s readiness: %w", cfg.Name, err)
	}

	instance := &ServiceInstance{
		Name: cfg.Name,
		URL:  service.Status.Url,
	}
	instance.deleteFn = func(deleteCtx context.Context) error {
		return m.deleteService(deleteCtx, resourceName)
	}
	return instance, nil
}

// deleteService removes a single Cloud Run service and waits for deletion.
func (m *CloudRunManager) deleteService(ctx context.Context, resourceName string) error {
	log.Printf("Cleaning up Cloud Run service: %s", resourceName)
	_, err := m.servicesAPI.Delete(resourceName).Context(ctx).Do()
	if err != nil {
		var gErr *googleapi.Error
		if errors.As(err, &gErr) && gErr.Code == http.StatusNotFound {
			return nil
		}
		return fmt.Errorf("deleting service %s: %w", resourceName, err)
	}
	return m.waitForDeletion(ctx, resourceName)
}

// ensureInvokerPolicy grants invoke access only to the configured service accounts.
func (m *CloudRunManager) ensureInvokerPolicy(ctx context.Context, resource string) error {
	if len(m.invokerMembers) == 0 {
		return nil
	}

	policy, err := m.fetchIAMPolicy(ctx, resource)
	if err != nil {
		return err
	}

	const invokerRole = "roles/run.invoker"
	invokerBinding := ensurePolicyBinding(policy, invokerRole)
	appendMissingMembers(invokerBinding, m.invokerMembers)

	if err := m.applyIAMPolicy(ctx, resource, policy); err != nil {
		return err
	}
	return nil
}

// fetchIAMPolicy retrieves a service IAM policy, always returning a non-nil policy.
func (m *CloudRunManager) fetchIAMPolicy(ctx context.Context, resource string) (*run.Policy, error) {
	policy, err := m.servicesAPI.GetIamPolicy(resource).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("retrieving IAM policy: %w", err)
	}
	if policy == nil {
		return &run.Policy{}, nil
	}
	return policy, nil
}

// ensurePolicyBinding returns the first binding for role, creating one when absent.
func ensurePolicyBinding(policy *run.Policy, role string) *run.Binding {
	for _, binding := range policy.Bindings {
		if binding.Role == role {
			return binding
		}
	}

	binding := &run.Binding{Role: role}
	policy.Bindings = append(policy.Bindings, binding)
	return binding
}

// appendMissingMembers adds members to binding without duplicating existing entries.
func appendMissingMembers(binding *run.Binding, members []string) {
	for _, member := range members {
		if !contains(binding.Members, member) {
			binding.Members = append(binding.Members, member)
		}
	}
}

// applyIAMPolicy writes the IAM policy back to the Cloud Run service.
func (m *CloudRunManager) applyIAMPolicy(ctx context.Context, resource string, policy *run.Policy) error {
	_, err := m.servicesAPI.SetIamPolicy(resource, &run.SetIamPolicyRequest{Policy: policy}).Context(ctx).Do()
	if err != nil {
		return fmt.Errorf("setting IAM policy: %w", err)
	}
	return nil
}

// waitForServiceReady polls Cloud Run until the service reports Ready.
func (m *CloudRunManager) waitForServiceReady(ctx context.Context, resource string) (*run.Service, error) {
	const pollInterval = 2 * time.Second
	deadline := time.Now().Add(5 * time.Minute)

	for {
		service, err := m.servicesAPI.Get(resource).Context(ctx).Do()
		if err == nil {
			if isServiceReady(service) && service.Status != nil && service.Status.Url != "" {
				return service, nil
			}
		} else {
			if transient, code := isTransientServiceError(err); transient {
				log.Printf("Waiting for service %s: transient readiness error (code=%d): %v", resource, code, err)
			} else {
				return nil, fmt.Errorf("fetching service state: %w", err)
			}
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for service to become ready")
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for service %s canceled: %w", resource, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// isServiceReady reports whether the Cloud Run service is ready.
func isServiceReady(service *run.Service) bool {
	if service == nil || service.Status == nil {
		return false
	}
	for _, cond := range service.Status.Conditions {
		if cond.Type == "Ready" && strings.EqualFold(cond.Status, "True") {
			return true
		}
	}
	return false
}

// isTransientServiceError reports whether err is worth retrying during polling.
func isTransientServiceError(err error) (bool, int) {
	var gErr *googleapi.Error
	if errors.As(err, &gErr) {
		switch gErr.Code {
		case http.StatusNotFound,
			http.StatusTooManyRequests,
			http.StatusInternalServerError,
			http.StatusBadGateway,
			http.StatusServiceUnavailable,
			http.StatusGatewayTimeout:
			return true, gErr.Code
		}
		return false, gErr.Code
	}
	return false, 0
}

// location returns the Cloud Run parent location resource name.
func (m *CloudRunManager) location() string {
	return fmt.Sprintf("projects/%s/locations/%s", m.projectID, m.region)
}

// serviceResourceName returns the fully-qualified Cloud Run service name.
func (m *CloudRunManager) serviceResourceName(serviceName string) string {
	return fmt.Sprintf("%s/services/%s", m.location(), serviceName)
}

// buildEnvVars converts a string map to the stable Cloud Run env var format.
func buildEnvVars(env map[string]string) []*run.EnvVar {
	if len(env) == 0 {
		return nil
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := make([]*run.EnvVar, 0, len(env))
	for _, k := range keys {
		result = append(result, &run.EnvVar{Name: k, Value: env[k]})
	}
	return result
}

// contains reports whether candidate appears in values.
func contains(values []string, candidate string) bool {
	return slices.Contains(values, candidate)
}

// buildInvokerMembers converts service-account identities into IAM members.
func buildInvokerMembers(serviceAccounts ...string) []string {
	seen := make(map[string]struct{}, len(serviceAccounts))
	members := make([]string, 0, len(serviceAccounts))
	for _, raw := range serviceAccounts {
		member := normalizeInvokerMember(raw)
		if member == "" {
			continue
		}
		if _, ok := seen[member]; ok {
			continue
		}
		seen[member] = struct{}{}
		members = append(members, member)
	}
	sort.Strings(members)
	return members
}

// normalizeInvokerMember converts a caller identity into a canonical IAM member.
func normalizeInvokerMember(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, ":") {
		return trimmed
	}
	if strings.Contains(trimmed, "@") {
		return "serviceAccount:" + trimmed
	}
	return trimmed
}

// randomSuffix returns a random lowercase hexadecimal suffix of length characters.
func randomSuffix(length int) string {
	if length <= 0 {
		length = 6
	}
	buf := make([]byte, (length+1)/2)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return strings.ToLower(hex.EncodeToString(buf))[:length]
}

// CleanupRun removes any Cloud Run services created for this harness run.
// It targets services whose names include the run ID and that carry the
// "e2e-scenario" label that DeployService applies to harness-managed services.
func (m *CloudRunManager) CleanupRun(ctx context.Context) error {
	runID := strings.ToLower(m.runID)
	if runID == "" {
		return nil
	}

	var errs []string
	continueToken := ""
	for {
		resp, err := m.listServicesPage(ctx, continueToken)
		if err != nil {
			return fmt.Errorf("listing services for cleanup: %w", err)
		}
		for _, svc := range resp.Items {
			if !shouldCleanupService(svc, runID) {
				continue
			}
			resourceName := m.serviceResourceName(svc.Metadata.Name)
			if err := m.deleteService(ctx, resourceName); err != nil {
				errs = append(errs, fmt.Sprintf("%s: %v", svc.Metadata.Name, err))
			}
		}
		if resp.Metadata == nil || resp.Metadata.Continue == "" {
			break
		}
		continueToken = resp.Metadata.Continue
	}

	if len(errs) > 0 {
		return fmt.Errorf("cleanup run encountered errors: %s", strings.Join(errs, "; "))
	}
	return nil
}

// listServicesPage returns one page from Cloud Run services.list.
func (m *CloudRunManager) listServicesPage(ctx context.Context, continueToken string) (*run.ListServicesResponse, error) {
	call := m.servicesAPI.List(m.location()).Context(ctx)
	if continueToken != "" {
		call.Continue(continueToken)
	}
	resp, err := call.Do()
	if err != nil {
		return nil, fmt.Errorf("listing Cloud Run services: %w", err)
	}
	return resp, nil
}

// shouldCleanupService reports whether a service belongs to this harness run.
func shouldCleanupService(svc *run.Service, runID string) bool {
	if svc == nil || svc.Metadata == nil {
		return false
	}
	name := strings.ToLower(svc.Metadata.Name)
	if name == "" || !strings.Contains(name, runID) {
		return false
	}
	_, ok := svc.Metadata.Labels["e2e-scenario"]
	return ok
}

// waitForDeletion waits until a Cloud Run resource is no longer visible.
func (m *CloudRunManager) waitForDeletion(ctx context.Context, resource string) error {
	const pollInterval = 2 * time.Second
	deadline := time.Now().Add(2 * time.Minute)

	for {
		_, err := m.servicesAPI.Get(resource).Context(ctx).Do()
		if err != nil {
			var gErr *googleapi.Error
			if errors.As(err, &gErr) && gErr.Code == http.StatusNotFound {
				return nil
			}
			return fmt.Errorf("checking deletion status: %w", err)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for service deletion")
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for deletion of %s canceled: %w", resource, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
