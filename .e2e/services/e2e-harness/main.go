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
	"flag"
	"fmt"
	"log"
	"maps"
	"os"
	"regexp"
	"strings"
	"time"

	"cloud.google.com/go/pubsub/v2"
	// boolPtr returns a pointer to v.

	//go:fix inline
	pubsubpb "cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"github.com/pjscruggs/slogcp-e2e-internal/services/e2e-harness/client"
	"github.com/pjscruggs/slogcp-e2e-internal/services/e2e-harness/controller"
	"github.com/pjscruggs/slogcp-e2e-internal/services/e2e-harness/tests"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type imageConfig struct {
	coreLogging    string
	traceTarget    string
	downstreamHTTP string
	downstreamGRPC string
}

const (
	separatorLine                  = "------------------------------------------------------------------------"
	defaultTracePubSubTopic        = "slogcp-trace-pubsub"
	defaultTracePubSubSubscription = "slogcp-trace-pubsub-sub"
	maxPubSubResourceIDLength      = 255
)

type scenarioDefinition struct {
	Name                string
	CoreEnv             map[string]string
	DownstreamHTTPEnv   map[string]string
	DownstreamGRPCEnv   map[string]string
	TraceEnv            map[string]string
	CoreExpectedVersion string
	CoreExpectedLogID   string
	TraceExpectedProjID string
	// CoreTests controls which core logging cases execute. Nil runs the full
	// suite, while an empty slice skips the suite entirely.
	CoreTests []string
	// TraceTests mirrors CoreTests semantics for the trace suite.
	TraceTests                  []string
	TraceDisableHTTPPropagation *bool
	TraceDisableGRPCPropagation *bool
	TraceDefaultSampled         *bool
}

type scenarioDeployment struct {
	definition scenarioDefinition
	core       *controller.ServiceInstance
	trace      *controller.ServiceInstance
	downHTTP   *controller.ServiceInstance
	downGRPC   *controller.ServiceInstance

	traceDownstreamHTTP string
	traceDownstreamGRPC string
}

type harnessConfig struct {
	projectID            string
	region               string
	serviceAccount       string
	callerServiceAccount string
	pubsubTopic          string
	pubsubSub            string
	runID                string
	timeout              time.Duration
	images               imageConfig
}

// main runs the E2E harness process.
func main() {
	os.Exit(run())
}

// run parses configuration, provisions shared clients, and executes scenarios.
func run() int {
	cfg, err := parseHarnessConfig()
	if err != nil {
		log.Printf("Invalid harness configuration: %v", err)
		return 1
	}
	cfg.pubsubTopic = resolvePubSubResourceID(cfg.pubsubTopic, cfg.runID)
	cfg.pubsubSub = resolvePubSubResourceID(cfg.pubsubSub, cfg.runID)

	logHarnessConfig(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	if err := ensurePubSubResources(ctx, cfg.projectID, cfg.pubsubTopic, cfg.pubsubSub); err != nil {
		log.Printf("Failed to ensure Pub/Sub resources: %v", err)
		return 1
	}
	defer cleanupPubSubResources(cfg.projectID, cfg.pubsubTopic, cfg.pubsubSub)

	loggingClient, traceClient, manager, err := initializeHarnessDependencies(ctx, cfg)
	if err != nil {
		log.Printf("Failed to initialize harness dependencies: %v", err)
		return 1
	}
	defer closeLoggingClient(loggingClient)
	defer closeTraceClient(traceClient)

	exitCode, allFailures, totalTests, totalPassed, totalFailed := runAllScenarios(
		ctx,
		cfg,
		manager,
		loggingClient,
		traceClient,
	)

	logOverallSummary(totalTests, totalPassed, totalFailed, exitCode, allFailures)
	cleanupManager(manager)

	if exitCode == 0 {
		log.Println("\nAll scenarios passed!")
	}

	return exitCode
}

// parseHarnessConfig parses and validates command-line configuration.
func parseHarnessConfig() (harnessConfig, error) {
	const defaultRegion = "us-central1"

	var (
		projectID            = flag.String("project-id", os.Getenv("GOOGLE_CLOUD_PROJECT"), "GCP project ID")
		region               = flag.String("region", os.Getenv("E2E_GCP_REGION"), "Cloud Run region for test deployments")
		coreImage            = flag.String("core-image", os.Getenv("CORE_TARGET_IMAGE"), "Container image for the core logging target app")
		traceImage           = flag.String("trace-image", os.Getenv("TRACE_TARGET_IMAGE"), "Container image for the trace target app")
		httpImage            = flag.String("trace-http-image", os.Getenv("TRACE_HTTP_IMAGE"), "Container image for the downstream HTTP app")
		grpcImage            = flag.String("trace-grpc-image", os.Getenv("TRACE_GRPC_IMAGE"), "Container image for the downstream gRPC app")
		serviceAccount       = flag.String("service-account", os.Getenv("E2E_SERVICE_ACCOUNT"), "Service account email used by Cloud Run services")
		callerServiceAccount = flag.String("caller-service-account", os.Getenv("E2E_CALLER_SERVICE_ACCOUNT"), "Service account email used by the harness job when invoking private Cloud Run services")
		pubsubTopic          = flag.String("trace-pubsub-topic", os.Getenv("TRACE_PUBSUB_TOPIC"), "Pub/Sub topic ID for trace pubsub tests")
		pubsubSub            = flag.String("trace-pubsub-subscription", os.Getenv("TRACE_PUBSUB_SUBSCRIPTION"), "Pub/Sub subscription ID for trace pubsub tests")
		// timeout bounds the full harness run, including service provisioning,
		// health checks, and sequential scenario execution. The default value
		// is sized for multi-service runs to complete reliably in CI.
		timeout = flag.Duration("timeout", 25*time.Minute, "Overall test timeout")
		runID   = flag.String("run-id", os.Getenv("E2E_RUN_ID"), "Identifier used to ensure isolated service names")
	)

	flag.Parse()

	cfg := harnessConfig{
		projectID:            strings.TrimSpace(*projectID),
		region:               strings.TrimSpace(*region),
		serviceAccount:       strings.TrimSpace(*serviceAccount),
		callerServiceAccount: strings.TrimSpace(*callerServiceAccount),
		pubsubTopic:          strings.TrimSpace(*pubsubTopic),
		pubsubSub:            strings.TrimSpace(*pubsubSub),
		runID:                strings.TrimSpace(*runID),
		timeout:              *timeout,
		images: imageConfig{
			coreLogging:    strings.TrimSpace(*coreImage),
			traceTarget:    strings.TrimSpace(*traceImage),
			downstreamHTTP: strings.TrimSpace(*httpImage),
			downstreamGRPC: strings.TrimSpace(*grpcImage),
		},
	}

	if err := normalizeHarnessConfig(&cfg, defaultRegion); err != nil {
		return harnessConfig{}, err
	}

	return cfg, nil
}

// normalizeHarnessConfig applies defaults and validates required values.
func normalizeHarnessConfig(cfg *harnessConfig, defaultRegion string) error {
	if cfg.projectID == "" {
		return fmt.Errorf("project ID is required (use -project-id flag or GOOGLE_CLOUD_PROJECT env var)")
	}
	if cfg.region == "" {
		log.Printf("Region not provided, defaulting to %s", defaultRegion)
		cfg.region = defaultRegion
	}
	if missingRequiredImages(cfg.images) {
		return fmt.Errorf("all service images (core, trace, downstream HTTP, downstream gRPC) must be provided")
	}
	if cfg.serviceAccount == "" {
		return fmt.Errorf("service account email is required (use -service-account flag or E2E_SERVICE_ACCOUNT env var)")
	}
	if cfg.pubsubTopic == "" {
		log.Printf("Pub/Sub topic not provided, defaulting to %s", defaultTracePubSubTopic)
		cfg.pubsubTopic = defaultTracePubSubTopic
	}
	if cfg.pubsubSub == "" {
		log.Printf("Pub/Sub subscription not provided, defaulting to %s", defaultTracePubSubSubscription)
		cfg.pubsubSub = defaultTracePubSubSubscription
	}
	if cfg.runID == "" {
		cfg.runID = fmt.Sprintf("local-%d", time.Now().Unix())
		log.Printf("Run ID not provided, defaulting to %s", cfg.runID)
	}

	return nil
}

var invalidPubSubIDChars = regexp.MustCompile(`[^a-z0-9-]+`)

// resolvePubSubResourceID returns a sanitized per-run Pub/Sub resource ID.
func resolvePubSubResourceID(base, runID string) string {
	base = strings.TrimSpace(base)
	runID = strings.TrimSpace(runID)

	if runID != "" && !strings.Contains(strings.ToLower(base), strings.ToLower(runID)) {
		base = base + "-" + runID
	}

	base = strings.ToLower(base)
	base = invalidPubSubIDChars.ReplaceAllString(base, "-")
	base = strings.Trim(base, "-")
	base = strings.Trim(base, "._~+%")
	if base == "" {
		base = "e2e-pubsub"
	}
	if base[0] < 'a' || base[0] > 'z' {
		base = "e2e-" + base
	}
	if len(base) > maxPubSubResourceIDLength {
		base = strings.Trim(base[:maxPubSubResourceIDLength], "-")
	}
	if base == "" {
		base = "e2e-pubsub"
	}
	return base
}

// missingRequiredImages reports whether any required target image is missing.
func missingRequiredImages(images imageConfig) bool {
	return images.coreLogging == "" ||
		images.traceTarget == "" ||
		images.downstreamHTTP == "" ||
		images.downstreamGRPC == ""
}

// logHarnessConfig logs the active harness configuration values.
func logHarnessConfig(cfg harnessConfig) {
	log.Printf("Starting E2E test harness")
	log.Printf("Project ID: %s", cfg.projectID)
	log.Printf("Region: %s", cfg.region)
	log.Printf("Run ID: %s", cfg.runID)
	log.Printf("Timeout: %v", cfg.timeout)
	log.Printf("Service account: %s", cfg.serviceAccount)
	if cfg.callerServiceAccount != "" {
		log.Printf("Caller service account: %s", cfg.callerServiceAccount)
	}
	log.Printf("Pub/Sub topic: %s", cfg.pubsubTopic)
	log.Printf("Pub/Sub subscription: %s", cfg.pubsubSub)
}

// initializeHarnessDependencies creates shared clients and the Cloud Run manager.
func initializeHarnessDependencies(ctx context.Context, cfg harnessConfig) (*client.LoggingClient, *client.TraceClient, *controller.CloudRunManager, error) {
	loggingClient, err := client.NewLoggingClient(ctx, cfg.projectID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating logging client: %w", err)
	}

	traceClient, err := client.NewTraceClient(ctx, cfg.projectID)
	if err != nil {
		closeLoggingClient(loggingClient)
		return nil, nil, nil, fmt.Errorf("creating trace client: %w", err)
	}

	manager, err := controller.NewCloudRunManager(ctx, cfg.projectID, cfg.region, cfg.runID, cfg.serviceAccount, cfg.callerServiceAccount)
	if err != nil {
		closeTraceClient(traceClient)
		closeLoggingClient(loggingClient)
		return nil, nil, nil, fmt.Errorf("initializing Cloud Run manager: %w", err)
	}

	return loggingClient, traceClient, manager, nil
}

// closeLoggingClient closes the logging client while logging close warnings.
func closeLoggingClient(loggingClient *client.LoggingClient) {
	if loggingClient == nil {
		return
	}
	defer func() {
		if closeErr := loggingClient.Close(); closeErr != nil {
			log.Printf("Warning: failed to close logging client: %v", closeErr)
		}
	}()
}

// closeTraceClient closes the trace client while logging close warnings.
func closeTraceClient(traceClient *client.TraceClient) {
	if traceClient == nil {
		return
	}
	defer func() {
		if closeErr := traceClient.Close(); closeErr != nil {
			log.Printf("Warning: failed to close trace client: %v", closeErr)
		}
	}()
}

// runAllScenarios executes all configured scenarios and aggregates totals.
func runAllScenarios(ctx context.Context, cfg harnessConfig, manager *controller.CloudRunManager, loggingClient *client.LoggingClient, traceClient *client.TraceClient) (int, []string, int, int, int) {
	scenarios := buildScenarios(cfg.projectID)

	var (
		totalTests  int
		totalPassed int
		totalFailed int
		allFailures []string
		exitCode    int
	)

	for _, scenario := range scenarios {
		log.Println()
		log.Println(separatorLine)
		log.Printf("Running scenario: %s", scenario.Name)
		log.Println(separatorLine)

		scenarioStats, err := runScenario(ctx, scenario, manager, cfg.images, cfg.projectID, cfg.pubsubTopic, cfg.pubsubSub, loggingClient, traceClient)
		if err != nil {
			log.Printf("Scenario %s encountered a fatal error: %v", scenario.Name, err)
			allFailures = append(allFailures, fmt.Sprintf("[%s] scenario setup: %v", scenario.Name, err))
			exitCode = 1
			break
		}

		totalTests += scenarioStats.total
		totalPassed += scenarioStats.passed
		totalFailed += scenarioStats.failed
		allFailures = append(allFailures, scenarioStats.failures...)

		if scenarioStats.failed > 0 {
			exitCode = 1
		}
	}

	return exitCode, allFailures, totalTests, totalPassed, totalFailed
}

// logOverallSummary logs aggregate run results and failures.
func logOverallSummary(totalTests, totalPassed, totalFailed, exitCode int, allFailures []string) {
	// Each scenario executes in the single project/region configured for the harness.
	// The harness cannot provision resources in alternate projects or regions, so
	// scenario-specific expectations must assume the shared deployment context.
	// The baseline scenario keeps the full core and trace suites so cross-cutting
	// checks such as severity validation execute exactly once per harness run.
	log.Println()
	log.Println(separatorLine)
	log.Printf("Overall Test Summary:")
	log.Printf("  Total:  %d", totalTests)
	log.Printf("  Passed: %d", totalPassed)
	log.Printf("  Failed: %d", totalFailed)

	if exitCode != 0 {
		printFailures(allFailures)
	}
}

// cleanupManager performs final cleanup with a bounded timeout.
func cleanupManager(manager *controller.CloudRunManager) {
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 3*time.Minute)
	if err := manager.CleanupRun(cleanupCtx); err != nil {
		log.Printf("Warning: final harness cleanup encountered errors: %v", err)
	}
	cleanupCancel()
}

// buildScenarios returns the scenario matrix for a harness run.
func buildScenarios(projectID string) []scenarioDefinition {
	return []scenarioDefinition{
		{
			Name:                "baseline",
			CoreEnv:             nil,
			DownstreamHTTPEnv:   nil,
			DownstreamGRPCEnv:   nil,
			TraceEnv:            nil,
			CoreExpectedVersion: "dev",
			CoreExpectedLogID:   "run.googleapis.com/stdout",
			TraceExpectedProjID: projectID,
			CoreTests:           nil,
			TraceTests:          nil,
		},
		{
			Name: "missing-adc",
			CoreEnv: map[string]string{
				"GOOGLE_APPLICATION_CREDENTIALS": "/nonexistent/adc.json",
			},
			DownstreamHTTPEnv:   nil,
			DownstreamGRPCEnv:   nil,
			TraceEnv:            nil,
			CoreExpectedVersion: "dev",
			CoreExpectedLogID:   "run.googleapis.com/stdout",
			TraceExpectedProjID: projectID,
			CoreTests: []string{
				"TestScenarioStartupMetadata",
				"TestStructuredPayload",
				"TestBatchLogging",
				"TestLogName",
			},
			TraceTests: []string{},
		},
		{
			Name: "custom-app-version",
			CoreEnv: map[string]string{
				"APP_VERSION": "custom-app-version",
			},
			DownstreamHTTPEnv:   nil,
			DownstreamGRPCEnv:   nil,
			TraceEnv:            nil,
			CoreExpectedVersion: "custom-app-version",
			CoreExpectedLogID:   "run.googleapis.com/stdout",
			TraceExpectedProjID: projectID,
			CoreTests: []string{
				"TestScenarioStartupMetadata",
			},
			TraceTests: []string{},
		},
		{
			Name:              "trace-disable-http-propagation",
			CoreEnv:           nil,
			DownstreamHTTPEnv: nil,
			DownstreamGRPCEnv: nil,
			TraceEnv: map[string]string{
				"TRACE_DISABLE_HTTP_TRACE_PROPAGATION": "true",
			},
			CoreExpectedVersion: "dev",
			CoreExpectedLogID:   "run.googleapis.com/stdout",
			TraceExpectedProjID: projectID,
			CoreTests:           []string{},
			TraceTests: []string{
				"TraceStartupConfiguration",
			},
			TraceDisableHTTPPropagation: new(true),
		},
		{
			Name:              "trace-disable-grpc-propagation",
			CoreEnv:           nil,
			DownstreamHTTPEnv: nil,
			DownstreamGRPCEnv: nil,
			TraceEnv: map[string]string{
				"TRACE_DISABLE_GRPC_TRACE_PROPAGATION": "true",
			},
			CoreExpectedVersion: "dev",
			CoreExpectedLogID:   "run.googleapis.com/stdout",
			TraceExpectedProjID: projectID,
			CoreTests:           []string{},
			TraceTests: []string{
				"TraceStartupConfiguration",
			},
			TraceDisableGRPCPropagation: new(true),
		},
		{
			Name:              "trace-default-unsampled",
			CoreEnv:           nil,
			DownstreamHTTPEnv: nil,
			DownstreamGRPCEnv: nil,
			TraceEnv: map[string]string{
				"TRACE_DEFAULT_SAMPLED": "false",
			},
			CoreExpectedVersion: "dev",
			CoreExpectedLogID:   "run.googleapis.com/stdout",
			TraceExpectedProjID: projectID,
			CoreTests:           []string{},
			TraceTests: []string{
				"TraceStartupConfiguration",
			},
			TraceDefaultSampled: new(false),
		},
	}
}

type scenarioStats struct {
	total    int
	passed   int
	failed   int
	failures []string
}

// runScenario deploys one scenario's services and executes its selected tests.
func runScenario(ctx context.Context, definition scenarioDefinition, manager *controller.CloudRunManager, images imageConfig, projectID, pubsubTopic, pubsubSubscription string, loggingClient *client.LoggingClient, traceClient *client.TraceClient) (scenarioStats, error) {
	scenarioCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	deployment, err := deployScenarioServices(scenarioCtx, definition, manager, images, projectID, pubsubTopic, pubsubSubscription)
	if err != nil {
		deployment.cleanup(context.Background())
		return scenarioStats{}, err
	}
	defer deployment.cleanup(context.Background())

	coreClient, traceTargetClient, err := initializeScenarioTargetClients(scenarioCtx, deployment)
	if err != nil {
		return scenarioStats{}, err
	}

	stats, err := runCoreSuiteForScenario(scenarioCtx, definition, deployment, coreClient, loggingClient, projectID)
	if err != nil {
		return scenarioStats{}, err
	}

	if stats.failed > 0 {
		return stats, nil
	}

	if shouldSkipTraceSuite(definition) {
		log.Printf("Skipping trace suite for scenario %s (no tests selected)", definition.Name)
		return stats, nil
	}

	traceStats, err := runTraceSuiteForScenario(scenarioCtx, definition, deployment, traceTargetClient, loggingClient, traceClient, projectID)
	if err != nil {
		return stats, err
	}
	mergeScenarioStats(&stats, traceStats)

	return stats, nil
}

// initializeScenarioTargetClients creates per-scenario target clients and
// validates service readiness via health checks.
func initializeScenarioTargetClients(ctx context.Context, deployment *scenarioDeployment) (*client.TargetClient, *client.TargetClient, error) {
	coreClient, err := client.NewTargetClient(ctx, deployment.core.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("creating core target client: %w", err)
	}

	traceTargetClient, err := client.NewTargetClient(ctx, deployment.trace.URL)
	if err != nil {
		return nil, nil, fmt.Errorf("creating trace target client: %w", err)
	}

	log.Printf("Performing health check on core target app (%s)...", deployment.core.Name)
	if err := coreClient.HealthCheck(ctx); err != nil {
		return nil, nil, fmt.Errorf("core target health check failed: %w", err)
	}
	log.Printf("Core target app healthy (%s)", deployment.core.URL)

	log.Printf("Performing health check on trace target app (%s)...", deployment.trace.Name)
	if err := traceTargetClient.HealthCheck(ctx); err != nil {
		return nil, nil, fmt.Errorf("trace target health check failed: %w", err)
	}
	log.Printf("Trace target app healthy (%s)", deployment.trace.URL)

	return coreClient, traceTargetClient, nil
}

// runCoreSuiteForScenario executes the selected core logging tests.
func runCoreSuiteForScenario(ctx context.Context, definition scenarioDefinition, deployment *scenarioDeployment, coreClient *client.TargetClient, loggingClient *client.LoggingClient, projectID string) (scenarioStats, error) {
	coreSuite := tests.NewCoreLoggingTestSuite(coreClient, loggingClient, projectID, tests.CoreScenarioConfig{
		ScenarioName:       definition.Name,
		ServiceName:        deployment.core.Name,
		ExpectedAppVersion: definition.CoreExpectedVersion,
		ExpectedLogID:      definition.CoreExpectedLogID,
	})
	coreTests, err := selectTests(coreSuite.GetTests(), definition.CoreTests)
	if err != nil {
		return scenarioStats{}, err
	}

	stats := scenarioStats{total: len(coreTests)}
	passed, failed, failures := runTestSuite(ctx, definition.Name, "Core Logging", coreTests)
	stats.passed += passed
	stats.failed += failed
	stats.failures = append(stats.failures, failures...)

	return stats, nil
}

// shouldSkipTraceSuite reports whether trace tests are explicitly disabled.
func shouldSkipTraceSuite(definition scenarioDefinition) bool {
	return definition.TraceTests != nil && len(definition.TraceTests) == 0
}

// runTraceSuiteForScenario executes the selected trace tests.
func runTraceSuiteForScenario(ctx context.Context, definition scenarioDefinition, deployment *scenarioDeployment, traceTargetClient *client.TargetClient, loggingClient *client.LoggingClient, traceClient *client.TraceClient, projectID string) (scenarioStats, error) {
	traceSuite := tests.NewTraceTestSuite(traceTargetClient, loggingClient, traceClient, projectID, tests.TraceScenarioConfig{
		ScenarioName:                definition.Name,
		ServiceName:                 deployment.trace.Name,
		ExpectedDownstreamHTTP:      deployment.traceDownstreamHTTP,
		ExpectedDownstreamGRPC:      deployment.traceDownstreamGRPC,
		ExpectedTraceProjectID:      definition.TraceExpectedProjID,
		DisableHTTPTracePropagation: definition.TraceDisableHTTPPropagation,
		DisableGRPCTracePropagation: definition.TraceDisableGRPCPropagation,
		DefaultTraceSampled:         definition.TraceDefaultSampled,
	})
	traceTests, err := selectTests(traceSuite.GetTests(), definition.TraceTests)
	if err != nil {
		return scenarioStats{}, err
	}

	stats := scenarioStats{total: len(traceTests)}
	passed, failed, failures := runTestSuite(ctx, definition.Name, "Trace", traceTests)
	stats.passed += passed
	stats.failed += failed
	stats.failures = append(stats.failures, failures...)

	return stats, nil
}

// mergeScenarioStats adds src totals into dst.
func mergeScenarioStats(dst *scenarioStats, src scenarioStats) {
	if dst == nil {
		return
	}
	dst.total += src.total
	dst.passed += src.passed
	dst.failed += src.failed
	dst.failures = append(dst.failures, src.failures...)
}

// selectTests filters all to the requested test names while preserving order.
func selectTests(all []tests.TestCase, selected []string) ([]tests.TestCase, error) {
	if selected == nil {
		return all, nil
	}

	if len(selected) == 0 {
		return []tests.TestCase{}, nil
	}

	lookup := make(map[string]tests.TestCase, len(all))
	for _, tc := range all {
		lookup[tc.Name] = tc
	}

	filtered := make([]tests.TestCase, 0, len(selected))
	for _, name := range selected {
		tc, ok := lookup[name]
		if !ok {
			return nil, fmt.Errorf("unknown test case %q", name)
		}
		filtered = append(filtered, tc)
	}

	return filtered, nil
}

// deployScenarioServices provisions the Cloud Run services needed for a scenario.
func deployScenarioServices(ctx context.Context, definition scenarioDefinition, manager *controller.CloudRunManager, images imageConfig, projectID, pubsubTopic, pubsubSubscription string) (*scenarioDeployment, error) {
	deployment := &scenarioDeployment{definition: definition}

	downHTTPName := manager.GenerateServiceName("trace-downstream-http")
	downHTTPEnv := mergeEnvs(map[string]string{
		"GOOGLE_CLOUD_PROJECT":        projectID,
		"SLOGCP_LOGICAL_SERVICE":      "trace-downstream-http",
		"SLOGCP_RUNTIME_SERVICE_NAME": downHTTPName,
		"SLOGCP_BUILD_ID":             definition.Name,
		"TRACE_PUBSUB_TOPIC":          pubsubTopic,
		"TRACE_PUBSUB_SUBSCRIPTION":   pubsubSubscription,
	}, definition.DownstreamHTTPEnv)

	downHTTPService, err := manager.DeployService(ctx, controller.ServiceConfig{
		Name:   downHTTPName,
		Image:  images.downstreamHTTP,
		Env:    downHTTPEnv,
		Labels: map[string]string{"e2e-scenario": definition.Name},
	})
	if err != nil {
		return deployment, fmt.Errorf("deploying downstream HTTP service %s: %w", downHTTPName, err)
	}
	deployment.downHTTP = downHTTPService
	if downHTTPService.URL == "" {
		return deployment, fmt.Errorf("downstream HTTP service %s has empty URL", downHTTPName)
	}

	downGRPCName := manager.GenerateServiceName("trace-downstream-grpc")
	downGRPCEnv := mergeEnvs(map[string]string{
		"GOOGLE_CLOUD_PROJECT":        projectID,
		"SLOGCP_LOGICAL_SERVICE":      "trace-downstream-grpc",
		"SLOGCP_RUNTIME_SERVICE_NAME": downGRPCName,
		"SLOGCP_BUILD_ID":             definition.Name,
	}, definition.DownstreamGRPCEnv)

	downGRPCService, err := manager.DeployService(ctx, controller.ServiceConfig{
		Name:   downGRPCName,
		Image:  images.downstreamGRPC,
		Env:    downGRPCEnv,
		Labels: map[string]string{"e2e-scenario": definition.Name},
	})
	if err != nil {
		return deployment, fmt.Errorf("deploying downstream gRPC service %s: %w", downGRPCName, err)
	}
	deployment.downGRPC = downGRPCService
	if downGRPCService.URL == "" {
		return deployment, fmt.Errorf("downstream gRPC service %s has empty URL", downGRPCName)
	}

	deployment.traceDownstreamHTTP = downHTTPService.URL
	deployment.traceDownstreamGRPC = deriveGRPCTarget(downGRPCService.URL)

	traceName := manager.GenerateServiceName("trace-target-app")
	traceEnv := mergeEnvs(map[string]string{
		"GOOGLE_CLOUD_PROJECT":        projectID,
		"DOWNSTREAM_HTTP_URL":         deployment.traceDownstreamHTTP,
		"DOWNSTREAM_GRPC_TARGET":      deployment.traceDownstreamGRPC,
		"SLOGCP_LOGICAL_SERVICE":      "trace-target-app",
		"SLOGCP_RUNTIME_SERVICE_NAME": traceName,
		"SLOGCP_BUILD_ID":             definition.Name,
		"TRACE_PUBSUB_TOPIC":          pubsubTopic,
		"TRACE_PUBSUB_SUBSCRIPTION":   pubsubSubscription,
	}, definition.TraceEnv)

	traceService, err := manager.DeployService(ctx, controller.ServiceConfig{
		Name:   traceName,
		Image:  images.traceTarget,
		Env:    traceEnv,
		Labels: map[string]string{"e2e-scenario": definition.Name},
	})
	if err != nil {
		return deployment, fmt.Errorf("deploying trace target service %s: %w", traceName, err)
	}
	deployment.trace = traceService
	if traceService.URL == "" {
		return deployment, fmt.Errorf("trace target service %s has empty URL", traceName)
	}

	coreName := manager.GenerateServiceName("core-logging-target-app")
	coreEnv := mergeEnvs(map[string]string{
		"GOOGLE_CLOUD_PROJECT":        projectID,
		"SLOGCP_LOGICAL_SERVICE":      "core-logging-target-app",
		"SLOGCP_RUNTIME_SERVICE_NAME": coreName,
		"SLOGCP_BUILD_ID":             definition.Name,
	}, definition.CoreEnv)

	coreService, err := manager.DeployService(ctx, controller.ServiceConfig{
		Name:   coreName,
		Image:  images.coreLogging,
		Env:    coreEnv,
		Labels: map[string]string{"e2e-scenario": definition.Name},
	})
	if err != nil {
		return deployment, fmt.Errorf("deploying core target service %s: %w", coreName, err)
	}
	deployment.core = coreService
	if coreService.URL == "" {
		return deployment, fmt.Errorf("core service %s has empty URL", coreName)
	}

	return deployment, nil
}

// cleanup tears down any services deployed for the scenario.
func (d *scenarioDeployment) cleanup(ctx context.Context) {
	services := []*controller.ServiceInstance{d.core, d.trace, d.downHTTP, d.downGRPC}
	for _, svc := range services {
		if svc == nil {
			continue
		}
		if err := svc.Cleanup(ctx); err != nil {
			log.Printf("Warning: failed to cleanup service %s: %v", svc.Name, err)
		}
	}
}

// deriveGRPCTarget converts a Cloud Run HTTPS URL into a gRPC authority target.
func deriveGRPCTarget(serviceURL string) string {
	trimmed := strings.TrimPrefix(serviceURL, "https://")
	trimmed = strings.TrimSuffix(trimmed, "/")
	if trimmed == "" {
		return ""
	}
	return trimmed + ":443"
}

// mergeEnvs overlays overrides on top of base and returns the merged environment.
func mergeEnvs(base map[string]string, overrides map[string]string) map[string]string {
	result := make(map[string]string, len(base)+len(overrides))
	maps.Copy(result, base)
	maps.Copy(result, overrides)
	return result
}

// ensurePubSubResources ensures the shared Pub/Sub topic and subscription exist.
func ensurePubSubResources(ctx context.Context, projectID, topicID, subscriptionID string) error {
	pubSubClient, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		return fmt.Errorf("creating pubsub client: %w", err)
	}
	defer func() {
		if closeErr := pubSubClient.Close(); closeErr != nil {
			log.Printf("Warning: failed to close Pub/Sub client: %v", closeErr)
		}
	}()

	topicName := fmt.Sprintf("projects/%s/topics/%s", projectID, topicID)
	if err := ensureTopicExists(ctx, pubSubClient, topicName); err != nil {
		return err
	}

	subscriptionName := fmt.Sprintf("projects/%s/subscriptions/%s", projectID, subscriptionID)
	subscription, err := ensureSubscriptionExists(ctx, pubSubClient, subscriptionName, topicName)
	if err != nil {
		return err
	}
	if subscription == nil {
		return nil
	}

	if strings.TrimSpace(subscription.GetTopic()) != topicName {
		return fmt.Errorf(
			"pubsub subscription %s is attached to topic %s, expected %s",
			subscriptionName,
			subscription.GetTopic(),
			topicName,
		)
	}

	return nil
}

// cleanupPubSubResources deletes per-run Pub/Sub resources using a fresh context.
func cleanupPubSubResources(projectID, topicID, subscriptionID string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pubSubClient, err := pubsub.NewClient(cleanupCtx, projectID)
	if err != nil {
		log.Printf("Warning: failed to create Pub/Sub client for cleanup: %v", err)
		return
	}
	defer func() {
		if closeErr := pubSubClient.Close(); closeErr != nil {
			log.Printf("Warning: failed to close Pub/Sub cleanup client: %v", closeErr)
		}
	}()

	if err := deleteSubscriptionIfExists(cleanupCtx, pubSubClient, projectID, subscriptionID); err != nil {
		log.Printf("Warning: failed to delete Pub/Sub subscription %s: %v", subscriptionID, err)
	}
	if err := deleteTopicIfExists(cleanupCtx, pubSubClient, projectID, topicID); err != nil {
		log.Printf("Warning: failed to delete Pub/Sub topic %s: %v", topicID, err)
	}
}

// ensureTopicExists creates the topic when missing.
func ensureTopicExists(ctx context.Context, pubSubClient *pubsub.Client, topicName string) error {
	if _, err := pubSubClient.TopicAdminClient.GetTopic(ctx, &pubsubpb.GetTopicRequest{Topic: topicName}); err != nil {
		if status.Code(err) != codes.NotFound {
			return fmt.Errorf("getting pubsub topic %s: %w", topicName, err)
		}

		log.Printf("Creating Pub/Sub topic: %s", topicName)
		if _, err := pubSubClient.TopicAdminClient.CreateTopic(ctx, &pubsubpb.Topic{Name: topicName}); err != nil && status.Code(err) != codes.AlreadyExists {
			return fmt.Errorf("creating pubsub topic %s: %w", topicName, err)
		}
	}
	return nil
}

// ensureSubscriptionExists creates the subscription when missing and returns its metadata.
func ensureSubscriptionExists(ctx context.Context, pubSubClient *pubsub.Client, subscriptionName, topicName string) (*pubsubpb.Subscription, error) {
	subscription, err := pubSubClient.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{
		Subscription: subscriptionName,
	})
	if err == nil {
		return subscription, nil
	}

	if status.Code(err) != codes.NotFound {
		return nil, fmt.Errorf("getting pubsub subscription %s: %w", subscriptionName, err)
	}

	log.Printf("Creating Pub/Sub subscription: %s", subscriptionName)
	created, createErr := pubSubClient.SubscriptionAdminClient.CreateSubscription(ctx, &pubsubpb.Subscription{
		Name:  subscriptionName,
		Topic: topicName,
	})
	if createErr == nil || status.Code(createErr) == codes.AlreadyExists {
		if created != nil {
			return created, nil
		}
		return &pubsubpb.Subscription{Topic: topicName}, nil
	}
	return nil, fmt.Errorf("creating pubsub subscription %s: %w", subscriptionName, createErr)
}

// deleteSubscriptionIfExists removes the subscription when present.
func deleteSubscriptionIfExists(ctx context.Context, pubSubClient *pubsub.Client, projectID, subscriptionID string) error {
	if strings.TrimSpace(subscriptionID) == "" {
		return nil
	}
	subscriptionName := fmt.Sprintf("projects/%s/subscriptions/%s", projectID, subscriptionID)
	err := pubSubClient.SubscriptionAdminClient.DeleteSubscription(ctx, &pubsubpb.DeleteSubscriptionRequest{
		Subscription: subscriptionName,
	})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting subscription %s: %w", subscriptionName, err)
	}
	return nil
}

// deleteTopicIfExists removes the topic when present.
func deleteTopicIfExists(ctx context.Context, pubSubClient *pubsub.Client, projectID, topicID string) error {
	if strings.TrimSpace(topicID) == "" {
		return nil
	}
	topicName := fmt.Sprintf("projects/%s/topics/%s", projectID, topicID)
	err := pubSubClient.TopicAdminClient.DeleteTopic(ctx, &pubsubpb.DeleteTopicRequest{
		Topic: topicName,
	})
	if status.Code(err) == codes.NotFound {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting topic %s: %w", topicName, err)
	}
	return nil
}

// runTestSuite runs a set of test cases within a scenario and returns statistics.
func runTestSuite(ctx context.Context, scenarioName, suiteName string, testCases []tests.TestCase) (passed, failed int, failures []string) {
	log.Printf("\nRunning %d %s test cases for scenario %s...\n", len(testCases), suiteName, scenarioName)

	for _, tc := range testCases {
		qualifiedName := fmt.Sprintf("[%s] %s", scenarioName, tc.Name)
		log.Println(separatorLine)
		log.Printf("Running: %s", qualifiedName)
		log.Printf("Description: %s", tc.Description)

		startTime := time.Now()
		err := tc.Execute(ctx)
		duration := time.Since(startTime)

		if err != nil {
			failed++
			failures = append(failures, fmt.Sprintf("%s: %v", qualifiedName, err))
			log.Printf("FAILED: %s (took %v)", qualifiedName, duration)
			log.Printf("   Error: %v", err)
		} else {
			passed++
			log.Printf("PASSED: %s (took %v)", qualifiedName, duration)
		}
		log.Println()
	}

	return passed, failed, failures
}

// printFailures logs a final flat list of failing scenario/test outcomes.
func printFailures(failures []string) {
	if len(failures) == 0 {
		return
	}
	log.Println("\nFailed tests:")
	for _, failure := range failures {
		log.Printf("  - %s", failure)
	}
}
