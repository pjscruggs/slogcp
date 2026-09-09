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
	"strings"
	"testing"
	"time"

	"github.com/pjscruggs/slogcp-e2e-internal/services/e2e-harness/controller"
)

type recordingDeployer struct {
	names    []string
	failGRPC bool
}

// GenerateServiceName provides stable fixture names.
func (d *recordingDeployer) GenerateServiceName(base string) string { return base }

// DeployService records setup and can fail a required downstream dependency.
func (d *recordingDeployer) DeployService(_ context.Context, cfg controller.ServiceConfig) (*controller.ServiceInstance, error) {
	d.names = append(d.names, cfg.Name)
	instance := &controller.ServiceInstance{Name: cfg.Name, URL: "https://" + cfg.Name + ".invalid"}
	if d.failGRPC && strings.Contains(cfg.Name, "grpc") {
		return instance, fmt.Errorf("required deployment failed")
	}
	return instance, nil
}

// TestScenarioDeploymentSelection keeps trace provisioning out of core-only scenarios.
func TestScenarioDeploymentSelection(t *testing.T) {
	for _, scenario := range buildScenarios("test-project") {
		t.Run(scenario.Name, func(t *testing.T) {
			manager := &recordingDeployer{failGRPC: true}
			deployment, err := deployScenarioServices(context.Background(), scenario, manager, imageConfig{}, "test-project", "", "")
			if shouldSkipTraceSuite(scenario) {
				if err != nil || len(manager.names) != 1 || manager.names[0] != "core-logging-target-app" || deployment.trace != nil {
					t.Fatalf("core-only deployment: names=%v err=%v", manager.names, err)
				}
			} else if err == nil || deployment.downGRPC == nil {
				t.Fatal("required gRPC failure must fail and retain partial cleanup ownership")
			}
		})
	}
}

// TestScenarioPlanPreservesAssertions makes the selected matrix explicit.
func TestScenarioPlanPreservesAssertions(t *testing.T) {
	want := []int{33, 4, 1, 1, 1, 1}
	scenarios := buildScenarios("test-project")
	if len(scenarios) != len(want) {
		t.Fatal("scenario matrix changed; review coverage")
	}
	for i, scenario := range scenarios {
		count, err := plannedScenarioTests(scenario)
		if err != nil || count != want[i] {
			t.Fatalf("%s: count=%d err=%v", scenario.Name, count, err)
		}
		manager := &recordingDeployer{}
		if _, err := deployScenarioServices(context.Background(), scenario, manager, imageConfig{}, "test-project", "", ""); err != nil {
			t.Fatal(err)
		}
		expected := 4
		if shouldSkipTraceSuite(scenario) {
			expected = 1
		}
		if len(manager.names) != expected {
			t.Fatalf("%s: deployments=%v", scenario.Name, manager.names)
		}
	}
}

// TestInvalidSelectionFailsBeforeDeployment rejects typo and empty selections.
func TestInvalidSelectionFailsBeforeDeployment(t *testing.T) {
	for _, scenario := range []scenarioDefinition{{CoreTests: []string{"unknown"}}, {CoreTests: []string{}, TraceTests: []string{}}} {
		manager := &recordingDeployer{}
		if _, err := deployScenarioServices(context.Background(), scenario, manager, imageConfig{}, "", "", ""); err == nil || len(manager.names) != 0 {
			t.Fatalf("invalid plan deployed services: %v", manager.names)
		}
	}
	if shouldSkipTraceSuite(scenarioDefinition{TraceTests: nil}) {
		t.Fatal("nil must select the complete trace suite")
	}
}

// TestScenarioCompletionRejectsMissingAssertions prevents partial-green runs.
func TestScenarioCompletionRejectsMissingAssertions(t *testing.T) {
	for _, stats := range []scenarioStats{{total: 4, passed: 3}, {total: 3, passed: 3}} {
		if validateScenarioCompletion(4, stats) == nil {
			t.Fatal("missing assertion accepted")
		}
	}
	if err := validateScenarioCompletion(4, scenarioStats{total: 4, passed: 4}); err != nil {
		t.Fatal(err)
	}
}

// TestHarnessTimeoutDefaultUsesEnvironmentSeconds verifies the harness timeout honors the environment override.
func TestHarnessTimeoutDefaultUsesEnvironmentSeconds(t *testing.T) {
	t.Setenv("E2E_TIMEOUT_SECONDS", "5400")

	got, err := harnessTimeoutDefault()
	if err != nil {
		t.Fatalf("harnessTimeoutDefault() error = %v", err)
	}

	if got != 90*time.Minute {
		t.Fatalf("harnessTimeoutDefault() = %v, want %v", got, 90*time.Minute)
	}
}

// TestHarnessTimeoutDefaultRejectsInvalidEnvironmentSeconds verifies invalid environment overrides are rejected.
func TestHarnessTimeoutDefaultRejectsInvalidEnvironmentSeconds(t *testing.T) {
	t.Setenv("E2E_TIMEOUT_SECONDS", "25m")

	if _, err := harnessTimeoutDefault(); err == nil {
		t.Fatal("harnessTimeoutDefault() error = nil, want error")
	}
}
