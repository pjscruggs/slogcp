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

"""Freeze binaries, build images, and launch bounded Cloud Run benchmarks."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess

HERE = Path(__file__).resolve().parent
REPO = HERE.parent


def execute(args, cwd=None, env=None):
    executable = shutil.which(str(args[0])) or str(args[0])
    return subprocess.run([executable, *map(str, args[1:])], cwd=cwd, env=env,
                          check=True, capture_output=True, text=True).stdout.strip()


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2) + "\n", encoding="utf-8")


def sha256(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def freeze(args):
    """Require a committed source snapshot before producing a baseline."""
    if not re.fullmatch(r"[a-z][a-z0-9-]*", args.name):
        raise ValueError("Invalid variant name")
    if execute(["git", "status", "--porcelain"], REPO):
        raise RuntimeError("Commit the benchmark and source changes before freezing")
    archive = args.archive.resolve()
    archive.mkdir(parents=True, exist_ok=True)
    binary = archive / args.name
    if any(archive.iterdir()):
        raise FileExistsError("Refusing to overwrite a nonempty archive")
    revision = execute(["git", "rev-parse", "HEAD"], REPO)
    env = dict(os.environ, GOOS="linux", GOARCH="amd64", CGO_ENABLED="0",
               GOTOOLCHAIN="go1.27.1", GOEXPERIMENT="")
    execute(["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false",
             "-o", binary, "."], HERE, env)
    execute(["git", "archive", "--format=tar.gz", "-o", archive / "source.tar.gz", "HEAD"], REPO)
    dependencies = execute(["go", "list", "-mod=readonly", "-m", "all"], HERE, env)
    (archive / "dependencies.txt").write_text(dependencies + "\n", encoding="utf-8")
    provenance = dict(name=args.name, revision=revision,
                      tree=execute(["git", "rev-parse", "HEAD^{tree}"], REPO),
                      compiler=execute(["go", "version"], HERE, env),
                      binary_sha256=sha256(binary), source_sha256=sha256(archive / "source.tar.gz"),
                      binary_build_info=execute(["go", "version", "-m", binary], HERE, env),
                      build_flags=["-mod=readonly", "-trimpath", "-buildvcs=false"],
                      environment={key: env[key] for key in ("GOOS", "GOARCH", "CGO_ENABLED", "GOTOOLCHAIN", "GOEXPERIMENT")},
                      dependencies=dependencies,
                      harness_sha256={path.name: sha256(path) for path in HERE.glob("*.go")},
                      runner_sha256=sha256(HERE / "run_matrix.py"),
                      dockerfile_sha256=sha256(HERE / "Dockerfile"))
    for name in ("run_matrix.py", "Dockerfile"):
        shutil.copy2(HERE / name, archive / name)
    write_json(archive / "provenance.json", provenance)
    print(f"Frozen {args.name} at {revision}; SHA-256 {provenance['binary_sha256']}")


def image(args):
    output = args.archive.resolve()
    context = output / "image-context"
    context.mkdir(parents=True, exist_ok=False)
    (context / "bin").mkdir()
    provenance = {}
    harness = None
    frozen_runner = None
    for spec in args.binary:
        name, source = spec.split("=", 1)
        if not re.fullmatch(r"[a-z][a-z0-9-]*", name):
            raise ValueError("Invalid variant name")
        source = Path(source).resolve()
        data = json.loads((source.parent / "provenance.json").read_text())
        if sha256(source) != data["binary_sha256"]:
            raise ValueError("Archived binary hash mismatch")
        current_harness = (data["harness_sha256"], data["runner_sha256"], data["dockerfile_sha256"])
        if harness is not None and harness != current_harness:
            raise ValueError("Variants must use the identical frozen benchmark harness")
        harness = current_harness
        frozen_runner = source.parent
        for harness_name, key in (("run_matrix.py", "runner_sha256"), ("Dockerfile", "dockerfile_sha256")):
            if sha256(source.parent / harness_name) != data[key]:
                raise ValueError("Archived harness hash mismatch")
        shutil.copy2(source, context / "bin" / name)
        provenance[name] = data
    write_json(context / "provenance.json", provenance)
    for name in ("Dockerfile", "run_matrix.py"):
        shutil.copy2(frozen_runner / name, context / name)
    build = execute(["gcloud", "builds", "submit", context, "--project", args.project,
                     "--region", args.region, "--config", HERE / "cloudbuild.yaml",
                     "--service-account", "projects/" + args.project + "/serviceAccounts/" + args.build_sa,
                     "--gcs-source-staging-dir", "gs://" + args.bucket + "/benchmarks/sources",
                     "--substitutions", "_IMAGE=" + args.image, "--async", "--quiet", "--format=json"])
    data = json.loads(build)
    write_json(output / "build.json", data)
    print("Submitted image build " + data["id"])


def run(args):
    if not re.search(r"@sha256:[0-9a-f]{64}$", args.image):
        raise ValueError("Deploy the immutable image digest returned by Cloud Build")
    if not (1 <= args.repeats <= 20 and 16 <= args.stdout_count <= 10000
            and 16 <= args.discard_count <= 500000 and 1 <= args.warmup <= 1000):
        raise ValueError("Benchmark limits exceeded")
    if not 1 <= len(args.variant) <= 3 or len(set(args.variant)) != len(args.variant):
        raise ValueError("Expected one to three distinct variants")
    if any(not re.fullmatch(r"[a-z][a-z0-9-]*", name) for name in args.variant):
        raise ValueError("Invalid variant name")
    output = args.archive.resolve()
    output.mkdir(parents=True, exist_ok=True)
    if not re.fullmatch(r"[a-z][a-z0-9-]{0,50}", args.run_id):
        raise ValueError("Run ID must be a short lowercase Cloud Run name")
    variables = dict(BENCH_RUN_ID=args.run_id, BENCH_RESULTS_BUCKET=args.bucket,
                     GOOGLE_CLOUD_PROJECT=args.project, BENCH_REGION=args.region,
                     BENCH_REPEATS=str(args.repeats), BENCH_STDOUT_COUNT=str(args.stdout_count),
                     BENCH_DISCARD_COUNT=str(args.discard_count), BENCH_WARMUP=str(args.warmup),
                     BENCH_VARIANTS=json.dumps({name: "/opt/bench/bin/" + name for name in args.variant}),
                     GOMAXPROCS="2")
    env_path = output / "job-env.json"
    if env_path.exists():
        raise FileExistsError("Refusing to overwrite an existing run")
    write_json(env_path, variables)
    job = execute(["gcloud", "run", "jobs", "deploy", args.run_id, "--project", args.project,
                   "--region", args.region, "--image", args.image,
                   "--service-account", args.runtime_sa, "--cpu=2", "--memory=1Gi",
                   "--tasks=1", "--parallelism=1", "--max-retries=0", "--task-timeout=30m",
                   "--env-vars-file", env_path, "--quiet", "--format=json"])
    write_json(output / "job.json", json.loads(job))
    execution = execute(["gcloud", "run", "jobs", "execute", args.run_id,
                         "--project", args.project, "--region", args.region,
                         "--async", "--quiet", "--format=json"])
    write_json(output / "execution.json", json.loads(execution))
    print("Benchmark job is underway: " + args.run_id)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    frozen = commands.add_parser("freeze")
    frozen.add_argument("--name", required=True)
    frozen.add_argument("--archive", required=True, type=Path)
    frozen.set_defaults(func=freeze)
    build = commands.add_parser("image")
    build.add_argument("--binary", action="append", required=True, help="name=/path/to/frozen/binary")
    build.add_argument("--build-sa", required=True)
    build.set_defaults(func=image)
    launch = commands.add_parser("run")
    launch.add_argument("--runtime-sa", required=True)
    launch.add_argument("--run-id", required=True)
    launch.add_argument("--variant", action="append", required=True)
    launch.add_argument("--repeats", type=int, default=10)
    launch.add_argument("--stdout-count", type=int, default=2500)
    launch.add_argument("--discard-count", type=int, default=100000)
    launch.add_argument("--warmup", type=int, default=200)
    launch.set_defaults(func=run)
    for command in (build, launch):
        command.add_argument("--archive", required=True, type=Path)
        command.add_argument("--project", required=True)
        command.add_argument("--region", required=True)
        command.add_argument("--bucket", required=True)
        command.add_argument("--image", required=True)
    args = parser.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
