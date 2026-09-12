#!/usr/bin/env python3
# Copyright 2025-2026 Patrick J. Scruggs
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Summarize archived application trials and paired candidate measurements."""

import argparse
from collections import defaultdict
import json
import math
from pathlib import Path
import random
import statistics


BOOTSTRAP_SAMPLES = 10000
BOOTSTRAP_SEED = 270127
CASE_FIELDS = ("payload", "concurrency", "sink", "mode")
METRICS = {
    "cpu_ns_per_request": ("CPU ns/request", "lower"),
    "mallocs_per_request": ("allocs/request", "lower"),
    "allocated_bytes_per_request": ("allocated B/request", "lower"),
    "completed_requests_per_second": ("completed requests/s", "higher"),
    "producer_requests_per_second": ("producer requests/s", "higher"),
    "request_p95_ns": ("application p95 ns", "lower"),
    "drain_elapsed_ns": ("drain ns/trial", "lower"),
}


def finite_number(value, name, *, positive=False):
    """Reject invalid measurements instead of silently selecting successful data."""
    if (isinstance(value, bool) or not isinstance(value, (int, float))
            or not math.isfinite(value) or value < 0 or (positive and value == 0)):
        raise ValueError(f"{name} must be a finite {'positive' if positive else 'nonnegative'} number")
    return value


def trial_metrics(trial):
    """Normalize totals using completed requests from a successful trial."""
    count = finite_number(trial["config"]["count"], "config.count", positive=True)
    elapsed = finite_number(trial["completed_elapsed_ns"], "completed_elapsed_ns", positive=True)
    producer = finite_number(trial["producer_elapsed_ns"], "producer_elapsed_ns", positive=True)
    return {
        "cpu_ns_per_request": (finite_number(trial["cpu_user_ns"], "cpu_user_ns")
                               + finite_number(trial["cpu_system_ns"], "cpu_system_ns")) / count,
        "mallocs_per_request": finite_number(trial["mallocs"], "mallocs") / count,
        "allocated_bytes_per_request": finite_number(trial["allocated_bytes"], "allocated_bytes") / count,
        "completed_requests_per_second": count * 1e9 / elapsed,
        "producer_requests_per_second": count * 1e9 / producer,
        "request_p95_ns": finite_number(trial["request_latency_ns"]["p95"], "request p95"),
        "drain_elapsed_ns": finite_number(trial["drain_elapsed_ns"], "drain_elapsed_ns"),
    }


def case_key(trial):
    return tuple(trial["config"][field] for field in CASE_FIELDS)


def describe_case(key):
    return dict(zip(CASE_FIELDS, key))


def validate_suite(suite):
    """Require complete, error-free trials with unambiguous pairing keys."""
    if suite.get("schema_version") != 1 or suite.get("complete") is not True:
        raise ValueError("Expected a complete suite with schema_version=1")
    repeats = suite.get("repeats")
    if isinstance(repeats, bool) or not isinstance(repeats, int) or repeats < 1:
        raise ValueError("suite.repeats must be a positive integer")
    if not suite.get("trials"):
        raise ValueError("Suite contains no trials")
    indexed = {}
    groups = defaultdict(set)
    for trial in suite["trials"]:
        if trial.get("process_exit_code") != 0 or trial.get("errors") not in (0, []):
            raise ValueError("Suite contains a failed trial")
        if trial.get("os_metrics_available") is False:
            raise ValueError("Trial lacks operating-system CPU measurements")
        config = trial["config"]
        if config["payload"] not in ("small", "nested"):
            raise ValueError("Unknown application payload")
        if config["mode"] not in ("none", "slogcp", "slogcp-grpc", "google-stdout", "google-api"):
            raise ValueError("Unknown logger mode")
        if config["sink"] not in ("stdout", "discard"):
            raise ValueError("Unknown output sink")
        if not isinstance(config["count"], int) or isinstance(config["count"], bool):
            raise ValueError("config.count must be an integer")
        finite_number(config["concurrency"], "concurrency", positive=True)
        finite_number(config["warmup"], "warmup")
        repeat = trial["repeat"]
        if isinstance(repeat, bool) or not isinstance(repeat, int) or not 0 <= repeat < repeats:
            raise ValueError("Invalid repeat index")
        variant = trial["variant"]
        if variant not in suite["variants"]:
            raise ValueError("Trial references an undeclared variant")
        key = (variant, case_key(trial), repeat)
        if key in indexed:
            raise ValueError("Duplicate trial pairing key")
        trial_metrics(trial)
        indexed[key] = trial
        groups[(variant, case_key(trial))].add(repeat)
    for repeats_seen in groups.values():
        if repeats_seen != set(range(repeats)):
            raise ValueError("Case has missing repetitions")
    return indexed


def percentile(values, fraction):
    """Linearly interpolate a quantile of an already sorted sample."""
    position = (len(values) - 1) * fraction
    lower = int(position)
    upper = min(lower + 1, len(values) - 1)
    return values[lower] + (values[upper] - values[lower]) * (position - lower)


def paired_ratio(reference, measured, *, samples=BOOTSTRAP_SAMPLES, seed=BOOTSTRAP_SEED):
    """Bootstrap the ratio of medians by resampling intact measurement pairs."""
    if not reference or len(reference) != len(measured):
        raise ValueError("A ratio requires nonempty, equal-length paired samples")
    for value in reference + measured:
        finite_number(value, "paired measurement")
    base = statistics.median(reference)
    candidate = statistics.median(measured)
    result = dict(n=len(reference), reference_median=base, measured_median=candidate,
                  ratio=None, change_percent=None, ci95=None)
    if base == 0:
        result["undefined_reason"] = "Reference median is zero"
        return result
    result.update(ratio=candidate / base, change_percent=100 * (candidate / base - 1))
    rng = random.Random(seed)
    ratios = []
    for _ in range(samples):
        indexes = rng.choices(range(len(reference)), k=len(reference))
        denominator = statistics.median([reference[index] for index in indexes])
        if denominator == 0:
            result["undefined_reason"] = "A bootstrap reference median is zero"
            return result
        ratios.append(statistics.median([measured[index] for index in indexes]) / denominator)
    ratios.sort()
    result["ci95"] = [percentile(ratios, 0.025), percentile(ratios, 0.975)]
    return result


def compare_trials(reference, measured):
    """Match by repeat, checking that workload sizes and warmups agree."""
    ref = {trial["repeat"]: trial for trial in reference}
    other = {trial["repeat"]: trial for trial in measured}
    if len(ref) != len(reference) or len(other) != len(measured) or ref.keys() != other.keys():
        raise ValueError("Comparison has duplicate or unmatched repetitions")
    left, right = [], []
    for repeat in sorted(ref):
        for field in ("payload", "concurrency", "sink", "count", "warmup"):
            if ref[repeat]["config"][field] != other[repeat]["config"][field]:
                raise ValueError(f"Paired workload differs: {field}")
        for field in ("go_version", "goos", "goarch", "gomaxprocs"):
            if ref[repeat].get("runtime", {}).get(field) != other[repeat].get("runtime", {}).get(field):
                raise ValueError(f"Paired runtime differs: {field}")
        if ref[repeat].get("checksum") != other[repeat].get("checksum"):
            raise ValueError("Paired completed application work differs: checksum")
        left.append(trial_metrics(ref[repeat]))
        right.append(trial_metrics(other[repeat]))
    return {metric: paired_ratio([row[metric] for row in left], [row[metric] for row in right])
            for metric in METRICS}


def safe_environment(suite):
    """Expose measurement conditions without copying infrastructure identifiers."""
    host = suite.get("host", {})
    models = sorted({line.partition(":")[2].strip()
                     for line in (host.get("cpuinfo") or "").splitlines()
                     if line.partition(":")[0].strip() == "model name"})
    runtime = [trial.get("runtime", {}) for trial in suite["trials"]]
    return dict(cpu_models=models, cpu_max=host.get("cpu_max"), memory_max=host.get("memory_max"),
                go_versions=sorted({item["go_version"] for item in runtime if "go_version" in item}),
                platforms=sorted({f"{item.get('goos', '?')}/{item.get('goarch', '?')}" for item in runtime}),
                gomaxprocs=sorted({item["gomaxprocs"] for item in runtime if "gomaxprocs" in item}),
                variants={name: {"sha256": value.get("sha256")}
                          for name, value in suite["variants"].items()})


def summarize_suite(suite, name):
    indexed = validate_suite(suite)
    groups = defaultdict(list)
    for (variant, key, _), trial in indexed.items():
        groups[(variant, key)].append(trial)
    rows, comparisons, api_comparisons, grpc_comparisons = [], [], [], []
    for (variant, key), trials in sorted(groups.items()):
        values = [trial_metrics(trial) for trial in trials]
        rows.append(dict(variant=variant, **describe_case(key), n=len(trials),
                         medians={metric: statistics.median([value[metric] for value in values])
                                  for metric in METRICS}))
        if key[-1] == "slogcp":
            for mode, output in (("google-stdout", comparisons), ("google-api", api_comparisons)):
                control = groups.get((variant, key[:-1] + (mode,)))
                if control:
                    output.append(dict(variant=variant, **describe_case(key),
                                       reference_mode=mode, measured_mode="slogcp",
                                       metrics=compare_trials(control, trials)))
        if key[-1] == "slogcp-grpc":
            control = groups.get((variant, key[:-1] + ("google-api",)))
            if control:
                grpc_comparisons.append(dict(variant=variant, **describe_case(key),
                    reference_mode="google-api", measured_mode="slogcp-grpc",
                    metrics=compare_trials(control, trials)))
    return dict(name=name, repeats=suite["repeats"], environment=safe_environment(suite),
                groups=rows, slogcp_over_google_stdout=comparisons,
                slogcp_over_google_api=api_comparisons,
                slogcp_grpc_over_google_api=grpc_comparisons)


def build_report(baseline, paired=None):
    """Keep the archived baseline separate from the interleaved causal comparison."""
    if set(baseline.get("variants", {})) != {"baseline"}:
        raise ValueError("Archived baseline suite must contain only the baseline variant")
    report = dict(schema_version=1, bootstrap=dict(samples=BOOTSTRAP_SAMPLES, seed=BOOTSTRAP_SEED,
                  confidence=0.95, method="paired percentile bootstrap of ratio of medians"),
                  metric_definitions={key: dict(label=value[0], favorable=value[1])
                                      for key, value in METRICS.items()},
                  suites=[summarize_suite(baseline, "archived_baseline")], candidate_over_baseline=[])
    if paired is not None:
        variants = set(paired.get("variants", {}))
        if "baseline" not in variants or len(variants) < 2:
            raise ValueError("Paired suite must contain baseline and at least one candidate variant")
        indexed = validate_suite(paired)
        groups = defaultdict(list)
        for (variant, key, _), trial in indexed.items():
            groups[(variant, key)].append(trial)
        reference_cases = {key for variant, key in groups if variant == "baseline"}
        for candidate in sorted(variants - {"baseline"}):
            measured_cases = {key for variant, key in groups if variant == candidate}
            if reference_cases != measured_cases:
                raise ValueError("Candidate and baseline cases do not match")
            for key in sorted(reference_cases):
                report["candidate_over_baseline"].append(dict(variant=candidate, **describe_case(key),
                    metrics=compare_trials(groups[("baseline", key)], groups[(candidate, key)])))
        report["suites"].append(summarize_suite(paired, "paired"))
    return report


def number(value):
    return f"{value:,.3f}"


def ratio_cell(result):
    if result["ratio"] is None:
        return "undefined (zero reference)"
    interval = result["ci95"]
    ci = f" [{interval[0]:.3f}, {interval[1]:.3f}]" if interval else " [CI undefined]"
    return f"{result['ratio']:.3f}{ci}"


def markdown_report(report):
    """Render a portable report containing no job, bucket, project, or URL values."""
    lines = ["# Google Cloud application logging benchmarks", "",
             "Each row summarizes repeated executions of the same application function. "
             "Request latency is measured inside the process; it is not HTTP network latency. "
             "CPU and allocations are divided by the completed request count. Completed throughput "
             "includes draining the logger; producer throughput excludes the final drain. "
             "Application p95 is the median of trial p95 values, "
             "not a pooled percentile.", "",
             "`slogcp` and `google-stdout` share the stdout transport and are the direct library "
             "comparison. `google-api` uses the Google client's default asynchronous gRPC transport "
             "and is shown separately "
             "as a deployment-mode comparison. `none` is the application control. `discard` rows "
             "are encoding diagnostics; they do not measure Cloud Logging transport or ingestion.", ""]
    for suite in report["suites"]:
        environment = suite["environment"]
        lines += [f"## {suite['name'].replace('_', ' ')}", "",
                  f"Repetitions per case: **{suite['repeats']}**. Go: "
                  f"{', '.join(environment['go_versions']) or 'unrecorded'}; platform: "
                  f"{', '.join(environment['platforms'])}; GOMAXPROCS: {environment['gomaxprocs']}.",
                  f"CPU model: {', '.join(environment['cpu_models']) or 'unrecorded'}; "
                  f"CPU quota: {(environment['cpu_max'] or 'unrecorded').strip()}; "
                  f"memory limit: {(environment['memory_max'] or 'unrecorded').strip()}.", "",
                  "| Variant | Payload | Workers | Sink | Logger | n | CPU ns/request | "
                  "allocs/request | allocated B/request | completed requests/s | producer requests/s | app p95 us | drain ms/trial |",
                  "|---|---|---:|---|---|---:|---:|---:|---:|---:|---:|---:|---:|"]
        for row in suite["groups"]:
            value = row["medians"]
            cells = [row['variant'], row['payload'], str(row['concurrency']), row['sink'], row['mode'], str(row['n'])]
            cells += [number(value[name]) for name in list(METRICS)[:5]]
            cells += [number(value['request_p95_ns'] / 1000), number(value['drain_elapsed_ns'] / 1e6)]
            lines.append("| " + " | ".join(cells) + " |")
        lines += ["", "### slogcp / google-stdout ratios", "",
                  "These compare different logger modes within each repetition; their processes "
                  "are randomized across a repetition and are not necessarily adjacent. Ratio < 1 "
                  "means less CPU, allocation, latency, or drain time; throughput improves above 1. "
                  "Brackets contain 95% bootstrap intervals.", ""]
        lines.extend(comparison_table(suite["slogcp_over_google_stdout"], variant=True))
        lines.append("")
        lines += ["### slogcp / Google default gRPC ratios", "",
                  "These include representation, buffering, and transport differences. Producer "
                  "throughput includes application work and enqueueing while API delivery can "
                  "continue in the background. Completed throughput includes the final API flush. "
                  "Stdout return and API acknowledgement are different completion boundaries; "
                  "the bounded trials do not measure steady-state API saturation. Ratios use the "
                  "same per-repetition pairing and 95% bootstrap method as the stdout comparison.", ""]
        lines.extend(comparison_table(suite["slogcp_over_google_api"], variant=True))
        lines.append("")
        if suite["slogcp_grpc_over_google_api"]:
            lines += ["### slogcp gRPC / Google default gRPC ratios", "",
                      "Both modes use the official client's default buffered API delivery. "
                      "The slogcp mode also enriches and converts each entry. Completed throughput "
                      "includes the final client flush for both modes. Compare CPU, allocation, "
                      "and completed throughput for the measured workload before selecting a transport.", ""]
            lines.extend(comparison_table(suite["slogcp_grpc_over_google_api"], variant=True))
            lines.append("")
    if report["candidate_over_baseline"]:
        lines += ["## Candidate / baseline ratios from interleaved pairs", "",
                  "Each pair shares its case and repetition within the second suite. The archived "
                  "baseline establishes the pre-change record; it is not substituted for the "
                  "interleaved baseline when calculating these effects. Google and no-logger rows "
                  "are controls for changes unrelated to slogcp encoding.", ""]
        lines.extend(comparison_table(report["candidate_over_baseline"], variant=True))
        lines.append("")
    lines += ["## Limits", "",
              "Intervals use 10,000 paired bootstrap resamples with fixed seed 270127, resampling "
              "whole repeat pairs and taking the ratio of their medians. Repetitions remain the "
              "sample size: 10 repetitions do not become 10,000 independent observations. "
              "Short trials on one Cloud Run Job instance measure this hardware, quota, workload, "
              "and runtime configuration; they do not establish fleet-wide performance. Interval "
              "precision is limited at small n, and no multiple-comparison correction is applied.", "",
              "Trial success and drain completion do not themselves prove every record reached "
              "Cloud Logging. Use the separately archived ingestion verification before making a "
              "delivery claim. Stdout writes and API flushes have different delivery boundaries. "
              "Subprocess controls and repeated runs help expose drift but do not remove all "
              "collector buffering, CPU scheduling, or network variability. Undefined ratios "
              "arise when the reference median is zero; no epsilon is added.", ""]
    return "\n".join(lines)


def comparison_table(rows, *, variant=False):
    labels = (["Variant"] if variant else []) + ["Payload", "Workers", "Sink", "Logger", "n"]
    labels += [value[0] for value in METRICS.values()]
    lines = ["| " + " | ".join(labels) + " |", "|" + "---|" * len(labels)]
    for row in rows:
        cells = ([row["variant"]] if variant else []) + [row["payload"], str(row["concurrency"]),
                row["sink"], row["mode"], str(row["metrics"]["cpu_ns_per_request"]["n"])]
        cells += [ratio_cell(row["metrics"][metric]) for metric in METRICS]
        lines.append("| " + " | ".join(cells) + " |")
    return lines


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("baseline", type=Path, help="Completed, archived baseline suite.json")
    parser.add_argument("--paired", type=Path, help="Completed suite.json with baseline and one or more candidate variants")
    parser.add_argument("--output-dir", type=Path, required=True, help="Local directory for summary.json and summary.md")
    args = parser.parse_args()
    try:
        baseline = json.loads(args.baseline.read_text(encoding="utf-8"))
        paired = json.loads(args.paired.read_text(encoding="utf-8")) if args.paired else None
        report = build_report(baseline, paired)
    except (OSError, ValueError, KeyError, TypeError) as error:
        parser.error(str(error))
    args.output_dir.mkdir(parents=True, exist_ok=True)
    (args.output_dir / "summary.json").write_text(json.dumps(report, indent=2, allow_nan=False) + "\n", encoding="utf-8")
    (args.output_dir / "summary.md").write_text(markdown_report(report), encoding="utf-8")
    print("Wrote summary.json and summary.md")


if __name__ == "__main__":
    main()
