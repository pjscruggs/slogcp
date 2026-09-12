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

"""Run bounded, randomized application trials on one Cloud Run Job instance."""

import hashlib
import json
import os
from pathlib import Path
import random
import subprocess
import sys
import time
import urllib.parse
import urllib.request


def digest(path):
    return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def upload(path, bucket, prefix):
    """Create an immutable result object using the job's attached identity."""
    request = urllib.request.Request(
        "http://metadata.google.internal/computeMetadata/v1/instance/"
        "service-accounts/default/token", headers={"Metadata-Flavor": "Google"})
    with urllib.request.urlopen(request, timeout=15) as response:
        token = json.load(response)["access_token"]
    query = urllib.parse.urlencode({"uploadType": "media", "ifGenerationMatch": "0",
                                    "name": prefix + "/" + path.name})
    request = urllib.request.Request(
        "https://storage.googleapis.com/upload/storage/v1/b/"
        + urllib.parse.quote(bucket, safe="") + "/o?" + query,
        data=path.read_bytes(), method="POST",
        headers={"Authorization": "Bearer " + token, "Content-Type": "application/json"})
    with urllib.request.urlopen(request, timeout=60) as response:
        response.read()


def read_optional(path):
    try:
        return Path(path).read_text()
    except OSError:
        return None


def plan(variants, repeats, seed, stdout_count, discard_count, include_slogcp_grpc=False):
    """Keep variants adjacent while balancing/randomizing their order by block."""
    rng = random.Random(seed)
    blocks = []
    stdout_modes = ("none", "slogcp", "google-stdout", "google-api")
    if include_slogcp_grpc:
        stdout_modes += ("slogcp-grpc",)
    for repeat in range(repeats):
        cases = [(payload, concurrency, sink, mode)
                 for payload in ("small", "nested")
                 for concurrency in (1, 16)
                 for sink, modes in (("stdout", stdout_modes),
                                     ("discard", ("slogcp", "google-stdout")))
                 for mode in modes]
        rng.shuffle(cases)
        for payload, concurrency, sink, mode in cases:
            order = list(variants)
            rng.shuffle(order)
            for variant in order:
                trial = f"r{repeat:02d}-{payload}-c{concurrency}-{sink}-{mode}-{variant}"
                blocks.append(dict(trial_id=trial, repeat=repeat, variant=variant,
                                   payload=payload, concurrency=concurrency,
                                   sink=sink, mode=mode,
                                   count=stdout_count if sink == "stdout" else discard_count))
    return blocks


def main():
    run_id = os.environ["BENCH_RUN_ID"]
    bucket = os.environ["BENCH_RESULTS_BUCKET"]
    project = os.environ["GOOGLE_CLOUD_PROJECT"]
    region = os.environ["BENCH_REGION"]
    variants = json.loads(os.environ.get("BENCH_VARIANTS", '{"baseline":"/opt/bench/bin/baseline"}'))
    repeats = int(os.environ.get("BENCH_REPEATS", "10"))
    stdout_count = int(os.environ.get("BENCH_STDOUT_COUNT", "2500"))
    discard_count = int(os.environ.get("BENCH_DISCARD_COUNT", "100000"))
    warmup = int(os.environ.get("BENCH_WARMUP", "200"))
    seed = int(os.environ.get("BENCH_SEED", "270127"))
    if not (1 <= repeats <= 20 and 16 <= stdout_count <= 10000
            and 16 <= discard_count <= 500000 and 1 <= warmup <= 1000):
        raise ValueError("Benchmark limits exceeded")
    if not variants or len(variants) > 3:
        raise ValueError("Expected one to three variants")
    output = Path("/tmp/benchmark-results")
    output.mkdir(exist_ok=True)
    prefix = "benchmarks/" + run_id
    suite = dict(schema_version=1, run_id=run_id, started_at=time.time(),
                 seed=seed, repeats=repeats, warmup=warmup, trials=[],
                 variants={name: dict(path=path, sha256=digest(path))
                           for name, path in variants.items()},
                 host={"cpuinfo": read_optional("/proc/cpuinfo"),
                       "cpu_max": read_optional("/sys/fs/cgroup/cpu.max"),
                       "memory_max": read_optional("/sys/fs/cgroup/memory.max"),
                       "execution": os.environ.get("CLOUD_RUN_EXECUTION"),
                       "task_attempt": os.environ.get("CLOUD_RUN_TASK_ATTEMPT")},
                 provenance=json.loads(Path("/opt/bench/provenance.json").read_text()))
    include_slogcp_grpc = os.environ.get("BENCH_INCLUDE_SLOGCP_GRPC", "false") == "true"
    planned = plan(variants, repeats, seed, stdout_count, discard_count, include_slogcp_grpc)
    for index, case in enumerate(planned):
        result_path = output / (case["trial_id"] + ".json")
        args = [variants[case["variant"]], "-project", project, "-location", region,
                "-run-id", run_id, "-warmup", str(warmup), "-result-file", str(result_path)]
        for flag in ("payload", "concurrency", "sink", "mode", "count"):
            args.extend(["-" + flag, str(case[flag])])
        args.extend(["-trial-id", case["trial_id"]])
        print(f"Trial {index + 1}/{len(planned)}: {case['trial_id']}", file=sys.stderr, flush=True)
        completed = subprocess.run(args, timeout=180, check=False)
        if not result_path.exists():
            raise RuntimeError("Trial produced no result: " + case["trial_id"])
        result = json.loads(result_path.read_text())
        result.update(variant=case["variant"], repeat=case["repeat"], order=index,
                      process_exit_code=completed.returncode)
        result_path.write_text(json.dumps(result, indent=2) + "\n")
        upload(result_path, bucket, prefix)
        suite["trials"].append(result)
        if completed.returncode:
            raise RuntimeError("Trial failed: " + case["trial_id"])
        time.sleep(float(os.environ.get("BENCH_COOLDOWN_SECONDS", "1")))
    suite["finished_at"] = time.time()
    suite["complete"] = True
    suite_path = output / "suite.json"
    suite_path.write_text(json.dumps(suite, indent=2) + "\n")
    upload(suite_path, bucket, prefix)
    print(f"Completed {len(planned)} trials; result archive uploaded.", file=sys.stderr, flush=True)


if __name__ == "__main__":
    main()
