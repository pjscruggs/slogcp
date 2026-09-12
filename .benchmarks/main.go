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

// Command benchmarks measures one application workload with interchangeable logging clients.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"
	"time"

	"cloud.google.com/go/logging"
	"github.com/pjscruggs/slogcp"
	"google.golang.org/api/option"
	mrpb "google.golang.org/genproto/googleapis/api/monitoredres"
)

const (
	eventMessage  = "request completed"
	modeGoogleAPI = "google-api"
)

type config struct {
	Mode        string `json:"mode"`
	Payload     string `json:"payload"`
	Count       int    `json:"count"`
	Concurrency int    `json:"concurrency"`
	Warmup      int    `json:"warmup"`
	RunID       string `json:"run_id"`
	TrialID     string `json:"trial_id"`
	Sink        string `json:"sink"`
	Project     string `json:"project"`
	Location    string `json:"location"`
	ResultFile  string `json:"-"`
}

type resourceUsage struct {
	Supported   bool
	UserNS      int64
	SystemNS    int64
	MaxRSSBytes int64
}

type quantiles struct {
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	P99 int64 `json:"p99"`
	Max int64 `json:"max"`
}

type runtimeInfo struct {
	GoVersion     string            `json:"go_version"`
	GOOS          string            `json:"goos"`
	GOARCH        string            `json:"goarch"`
	GOMAXPROCS    int               `json:"gomaxprocs"`
	NumCPU        int               `json:"num_cpu"`
	BuildSettings map[string]string `json:"build_settings"`
	Dependencies  map[string]string `json:"dependencies"`
}

type result struct {
	SchemaVersion              int         `json:"schema_version"`
	Config                     config      `json:"config"`
	Runtime                    runtimeInfo `json:"runtime"`
	StartedAt                  time.Time   `json:"started_at"`
	FinishedAt                 time.Time   `json:"finished_at"`
	ProducerElapsedNS          int64       `json:"producer_elapsed_ns"`
	DrainElapsedNS             int64       `json:"drain_elapsed_ns"`
	CompletedElapsedNS         int64       `json:"completed_elapsed_ns"`
	ProducerRequestsPerSecond  float64     `json:"producer_requests_per_second"`
	CompletedRequestsPerSecond float64     `json:"completed_requests_per_second"`
	RequestLatencyNS           quantiles   `json:"request_latency_ns"`
	LogCallLatencyNS           quantiles   `json:"log_call_latency_ns"`
	CPUUserNS                  int64       `json:"cpu_user_ns"`
	CPUSystemNS                int64       `json:"cpu_system_ns"`
	OSMetricsAvailable         bool        `json:"os_metrics_available"`
	MaxRSSBytes                int64       `json:"max_rss_bytes"`
	AllocatedBytes             uint64      `json:"allocated_bytes"`
	Mallocs                    uint64      `json:"mallocs"`
	NumGC                      uint32      `json:"num_gc"`
	GCPauseNS                  uint64      `json:"gc_pause_ns"`
	OutputBytes                int64       `json:"output_bytes"`
	OutputWrites               int64       `json:"output_writes"`
	Errors                     []string    `json:"errors"`
	Checksum                   uint64      `json:"checksum"`
}

type event struct {
	runID, trialID, insertID string
	sequence                 int
	request                  map[string]any
	digest                   string
}

// errorList also receives asynchronous errors from the Google client's callback.
type errorList struct {
	mu     sync.Mutex
	values []string
}

func (e *errorList) add(err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.values = append(e.values, err.Error())
}

func (e *errorList) snapshot() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string{}, e.values...)
}

// countingWriter provides identical output serialization and write-error accounting.
// It intentionally does not implement Close: stdout is owned by the process.
type countingWriter struct {
	mu     sync.Mutex
	writer io.Writer
	bytes  int64
	writes int64
	errors *errorList
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.writer.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	w.bytes += int64(n)
	w.writes++
	w.errors.add(err)
	return n, err
}

func (w *countingWriter) stats() (int64, int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes, w.writes
}

type logSink struct {
	emit  func(context.Context, event)
	flush func() error
	close func() error
}

// newLogSink leaves async batching at the Google client's defaults. The stdout
// pair both synchronously encode and write; the API mode enqueues and then drains.
func newLogSink(ctx context.Context, cfg config, writer io.Writer, errs *errorList) (logSink, error) {
	noop := func() error { return nil }
	if cfg.Mode == "none" {
		return logSink{emit: func(context.Context, event) {}, flush: noop, close: noop}, nil
	}
	info := slogcp.DetectRuntimeInfo()
	labels := executionLabels()
	if cfg.Mode == "slogcp" {
		handler, err := slogcp.NewHandler(writer,
			slogcp.WithLevel(slog.LevelInfo),
			slogcp.WithSeverityAliases(false),
			slogcp.WithSourceLocationEnabled(false),
			slogcp.WithStackTraceEnabled(false),
			slogcp.WithTime(true),
			slogcp.WithTraceProjectID(cfg.Project),
		)
		if err != nil {
			return logSink{}, fmt.Errorf("create slogcp handler: %w", err)
		}
		logger := slog.New(handler)
		if len(labels) > 0 {
			logger = logger.With(slog.Any(slogcp.LabelsGroup, labels))
		}
		return logSink{
			emit: func(ctx context.Context, event event) {
				logger.LogAttrs(ctx, slog.LevelInfo, eventMessage,
					slog.String("run_id", event.runID),
					slog.String("trial_id", event.trialID),
					slog.Int("sequence", event.sequence),
					slog.String("logging.googleapis.com/insertId", event.insertID),
					slog.String("digest", event.digest),
					slog.Any("request", event.request),
				)
			},
			flush: noop,
			close: handler.Close,
		}, nil
	}
	var clientOptions []option.ClientOption
	if cfg.Mode == "google-stdout" {
		// RedirectAsJSON performs no ingestion RPCs and needs no credentials.
		clientOptions = append(clientOptions, option.WithoutAuthentication(), option.WithTelemetryDisabled())
	}
	client, err := logging.NewClient(ctx, cfg.Project, clientOptions...)
	if err != nil {
		return logSink{}, fmt.Errorf("create Google logging client: %w", err)
	}
	client.OnError = errs.add
	loggerOptions := []logging.LoggerOption{
		logging.CommonResource(monitoredResource(cfg)),
		logging.SourceLocationPopulation(logging.DoNotPopulateSourceLocation),
	}
	if cfg.Mode == "google-stdout" {
		loggerOptions = append(loggerOptions, logging.RedirectAsJSON(writer))
	} else {
		loggerOptions = append(loggerOptions, logging.ContextFunc(func() (context.Context, func()) {
			return context.WithTimeout(ctx, 2*time.Minute)
		}))
	}
	logger := client.Logger("logging-comparison-benchmark", loggerOptions...)
	return logSink{
		emit: func(_ context.Context, event event) {
			payload := map[string]any{
				"message":  eventMessage,
				"run_id":   event.runID,
				"trial_id": event.trialID,
				"sequence": event.sequence,
				"digest":   event.digest,
				"request":  event.request,
			}
			if len(info.ServiceContext) > 0 {
				payload["serviceContext"] = info.ServiceContext
			}
			logger.Log(logging.Entry{
				Timestamp: time.Now(), Severity: logging.Info,
				Payload: payload, InsertID: event.insertID, Labels: labels,
			})
		},
		flush: logger.Flush,
		close: client.Close,
	}, nil
}

func monitoredResource(cfg config) *mrpb.MonitoredResource {
	if job := os.Getenv("CLOUD_RUN_JOB"); job != "" {
		return &mrpb.MonitoredResource{Type: "cloud_run_job", Labels: map[string]string{
			"project_id": cfg.Project, "location": cfg.Location, "job_name": job,
		}}
	}
	return &mrpb.MonitoredResource{Type: "global", Labels: map[string]string{"project_id": cfg.Project}}
}

func executionLabels() map[string]string {
	labels := make(map[string]string)
	for name, key := range map[string]string{
		"CLOUD_RUN_EXECUTION":    "run.googleapis.com/execution_name",
		"CLOUD_RUN_TASK_INDEX":   "run.googleapis.com/task_index",
		"CLOUD_RUN_TASK_ATTEMPT": "run.googleapis.com/task_attempt",
	} {
		if value := os.Getenv(name); value != "" {
			labels[key] = value
		}
	}
	return labels
}

// application is the identical useful work and payload construction in every mode.
// The fixed hash work is deliberately modest so logging remains measurable.
func application(cfg config, trialID string, sequence int) (event, uint64) {
	var input [128]byte
	copy(input[:], "GET /v1/orders?include=items benchmark application")
	copy(input[100:], strconv.Itoa(sequence))
	digest := sha256.Sum256(input[:])
	for range 3 {
		digest = sha256.Sum256(digest[:])
	}
	request := map[string]any{
		"method": "GET", "route": "/v1/orders", "status": 200,
		"response_bytes": 2048 + sequence%256, "customer_id": sequence % 97,
	}
	if cfg.Payload == "nested" {
		request["headers"] = map[string]any{
			"accept": "application/json", "accept_language": "en-US",
			"user_agent": "benchmark-client/1.0", "tags": []string{"mobile", "returning", "priority"},
		}
		request["customer"] = map[string]any{
			"name": "Ada <Example> & 雪", "active": true,
			"address":     map[string]any{"city": "Chicago", "country": "US", "postal_code": "60601"},
			"preferences": map[string]any{"currency": "USD", "email": false, "discount": 0.125},
		}
		items := make([]any, 8)
		for index := range items {
			items[index] = map[string]any{
				"sku": "item-" + strconv.Itoa(index), "quantity": index + 1,
				"unit_price": 12.5 + float64(index),
				"metadata":   map[string]any{"warehouse": "central", "available": true, "category": "books"},
			}
		}
		request["items"] = items
	}
	return event{
		runID: cfg.RunID, trialID: trialID, sequence: sequence,
		insertID: cfg.RunID + "/" + trialID + "/" + strconv.Itoa(sequence),
		request:  request, digest: hex.EncodeToString(digest[:]),
	}, binary.LittleEndian.Uint64(digest[:8])
}

// runTrial measures all requests plus the final flush. Process setup, warmup,
// explicit pre-trial GC, latency storage, quantile sorting, and client teardown
// are excluded from the measured interval.
func runTrial(ctx context.Context, cfg config, output io.Writer) (result, error) {
	res := result{SchemaVersion: 1, Config: cfg, Runtime: buildRuntimeInfo(), Errors: []string{}}
	errs := new(errorList)
	writer := &countingWriter{writer: output, errors: errs}
	sink, err := newLogSink(ctx, cfg, writer, errs)
	if err != nil {
		return res, err
	}
	closed := false
	defer func() {
		if !closed {
			_ = sink.close()
		}
	}()
	for sequence := range cfg.Warmup {
		entry, _ := application(cfg, cfg.TrialID+"-warmup", sequence)
		sink.emit(ctx, entry)
	}
	errs.add(sink.flush())
	if values := errs.snapshot(); len(values) > 0 {
		res.Errors = values
		return res, fmt.Errorf("warmup logging failed: %v", values)
	}
	requestLatency := make([]int64, cfg.Count)
	logLatency := make([]int64, cfg.Count)
	checksums := make([]uint64, cfg.Concurrency)
	start := make(chan struct{})
	var ready, workers sync.WaitGroup
	ready.Add(cfg.Concurrency)
	workers.Add(cfg.Concurrency)
	for worker := range cfg.Concurrency {
		go func() {
			defer workers.Done()
			ready.Done()
			<-start
			for sequence := worker; sequence < cfg.Count; sequence += cfg.Concurrency {
				requestStart := time.Now()
				entry, checksum := application(cfg, cfg.TrialID, sequence)
				logStart := time.Now()
				sink.emit(ctx, entry)
				finished := time.Now()
				logLatency[sequence] = finished.Sub(logStart).Nanoseconds()
				requestLatency[sequence] = finished.Sub(requestStart).Nanoseconds()
				checksums[worker] ^= checksum
			}
		}()
	}
	ready.Wait()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	usageBefore, err := processUsage()
	if err != nil {
		close(start)
		workers.Wait()
		return res, err
	}
	bytesBefore, writesBefore := writer.stats()
	res.StartedAt = time.Now()
	close(start)
	workers.Wait()
	produced := time.Now()
	errs.add(sink.flush())
	res.FinishedAt = time.Now()
	usageAfter, usageErr := processUsage()
	runtime.ReadMemStats(&after)
	bytesAfter, writesAfter := writer.stats()
	errs.add(usageErr)
	errs.add(sink.close())
	closed = true
	res.ProducerElapsedNS = produced.Sub(res.StartedAt).Nanoseconds()
	res.DrainElapsedNS = res.FinishedAt.Sub(produced).Nanoseconds()
	res.CompletedElapsedNS = res.FinishedAt.Sub(res.StartedAt).Nanoseconds()
	if res.ProducerElapsedNS <= 0 {
		errs.add(errors.New("clock did not advance during production; increase count"))
	} else {
		res.ProducerRequestsPerSecond = float64(cfg.Count) / (float64(res.ProducerElapsedNS) / 1e9)
		res.CompletedRequestsPerSecond = float64(cfg.Count) / (float64(res.CompletedElapsedNS) / 1e9)
	}
	res.RequestLatencyNS = summarize(requestLatency)
	res.LogCallLatencyNS = summarize(logLatency)
	res.CPUUserNS = usageAfter.UserNS - usageBefore.UserNS
	res.CPUSystemNS = usageAfter.SystemNS - usageBefore.SystemNS
	res.OSMetricsAvailable = usageAfter.Supported
	res.MaxRSSBytes = usageAfter.MaxRSSBytes
	res.AllocatedBytes = after.TotalAlloc - before.TotalAlloc
	res.Mallocs = after.Mallocs - before.Mallocs
	res.NumGC = after.NumGC - before.NumGC
	res.GCPauseNS = after.PauseTotalNs - before.PauseTotalNs
	res.OutputBytes = bytesAfter - bytesBefore
	res.OutputWrites = writesAfter - writesBefore
	res.Checksum = combineChecksums(checksums)
	res.Errors = errs.snapshot()
	if len(res.Errors) > 0 {
		return res, fmt.Errorf("logging failed: %v", res.Errors)
	}
	return res, nil
}

func combineChecksums(checksums []uint64) uint64 {
	var combined uint64
	for _, checksum := range checksums {
		combined ^= checksum
	}
	return combined
}

func summarize(samples []int64) quantiles {
	if len(samples) == 0 {
		return quantiles{}
	}
	slices.Sort(samples)
	value := func(percentile float64) int64 {
		return samples[int(math.Ceil(percentile*float64(len(samples))))-1]
	}
	return quantiles{P50: value(.50), P95: value(.95), P99: value(.99), Max: samples[len(samples)-1]}
}

func buildRuntimeInfo() runtimeInfo {
	info := runtimeInfo{
		GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		GOMAXPROCS: runtime.GOMAXPROCS(0), NumCPU: runtime.NumCPU(),
		BuildSettings: make(map[string]string), Dependencies: make(map[string]string),
	}
	if build, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range build.Settings {
			info.BuildSettings[setting.Key] = setting.Value
		}
		for _, dependency := range build.Deps {
			version := dependency.Version
			if dependency.Replace != nil {
				version += " => " + dependency.Replace.Path + " " + dependency.Replace.Version
			}
			info.Dependencies[dependency.Path] = version
		}
	}
	return info
}

func parseConfig(args []string) (config, error) {
	var cfg config
	flags := flag.NewFlagSet("logging-benchmark", flag.ContinueOnError)
	flags.StringVar(&cfg.Mode, "mode", "slogcp", "none, slogcp, google-stdout, or google-api")
	flags.StringVar(&cfg.Payload, "payload", "small", "small or nested")
	flags.IntVar(&cfg.Count, "count", 10000, "measured application requests")
	flags.IntVar(&cfg.Concurrency, "concurrency", 1, "parallel request workers")
	flags.IntVar(&cfg.Warmup, "warmup", 250, "unmeasured requests using a separate trial ID")
	flags.StringVar(&cfg.RunID, "run-id", "", "unique experiment ID (required)")
	flags.StringVar(&cfg.TrialID, "trial-id", "", "unique trial ID within this experiment (required)")
	flags.StringVar(&cfg.ResultFile, "result-file", "", "result JSON file (required)")
	flags.StringVar(&cfg.Project, "project", os.Getenv("GOOGLE_CLOUD_PROJECT"), "explicit Google Cloud project")
	flags.StringVar(&cfg.Location, "location", os.Getenv("CLOUD_RUN_REGION"), "Cloud Run region")
	flags.StringVar(&cfg.Sink, "sink", "stdout", "stdout or discard (serialization diagnostic only)")
	if err := flags.Parse(args); err != nil {
		return cfg, fmt.Errorf("parse benchmark flags: %w", err)
	}
	if flags.NArg() != 0 {
		return cfg, errors.New("unexpected positional arguments")
	}
	err := errors.Join(cfg.validateWorkload(), cfg.validateOutput(), cfg.configureProject())
	return cfg, err
}

func (cfg config) validateWorkload() error {
	if !slices.Contains([]string{"none", "slogcp", "google-stdout", modeGoogleAPI}, cfg.Mode) {
		return errors.New("unsupported mode")
	}
	if cfg.Payload != "small" && cfg.Payload != "nested" {
		return errors.New("payload must be small or nested")
	}
	if cfg.Count < 1 || cfg.Concurrency < 1 || cfg.Concurrency > cfg.Count || cfg.Warmup < 1 {
		return errors.New("count and warmup must be positive; concurrency must be in [1,count]")
	}
	return nil
}

func (cfg config) validateOutput() error {
	if cfg.RunID == "" || cfg.TrialID == "" || cfg.ResultFile == "" {
		return errors.New("run-id, trial-id, and result-file are required")
	}
	if cfg.Sink != "stdout" && cfg.Sink != "discard" {
		return errors.New("sink must be stdout or discard")
	}
	if cfg.Mode == modeGoogleAPI && cfg.Sink == "discard" {
		return errors.New("google-api cannot use the discard sink")
	}
	return nil
}

func (cfg *config) configureProject() error {
	if cfg.Project == "" {
		if cfg.Mode == modeGoogleAPI {
			return errors.New("google-api requires an explicit project")
		}
		cfg.Project = "benchmark-local"
	}
	if cfg.Mode == modeGoogleAPI && os.Getenv("CLOUD_RUN_JOB") != "" && cfg.Location == "" {
		return errors.New("cloud Run API logging requires an explicit location")
	}
	return nil
}

func run(args []string) error {
	cfg, err := parseConfig(args)
	if err != nil {
		return err
	}
	output := io.Writer(os.Stdout)
	if cfg.Sink == "discard" {
		output = io.Discard
	}
	res, trialErr := runTrial(context.Background(), cfg, output)
	if trialErr != nil && len(res.Errors) == 0 {
		res.Errors = []string{trialErr.Error()}
	}
	encoded, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return errors.Join(trialErr, err)
	}
	if err := writeResult(cfg.ResultFile, append(encoded, '\n')); err != nil {
		return errors.Join(trialErr, err)
	}
	return trialErr
}

// writeResult scopes the file write to the directory explicitly selected by the caller.
func writeResult(path string, data []byte) error {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open result directory: %w", err)
	}
	writeErr := root.WriteFile(filepath.Base(path), data, 0o600)
	if err := errors.Join(writeErr, root.Close()); err != nil {
		return fmt.Errorf("write result file: %w", err)
	}
	return nil
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "logging benchmark:", err)
		os.Exit(1)
	}
}
