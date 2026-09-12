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

"""Run bounded, local Go handler benchmarks and replace one README report block.

Only the Python standard library is required. No cloud resources are created.
``check`` exits 3 when the committed benchmark inputs were already reported;
other successful commands exit 0 and validation failures exit 1.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import platform
import re
import shlex
import signal
import statistics
import subprocess
import sys
import tempfile
import textwrap
from pathlib import Path


START = "<!-- BENCHMARKS:START -->"
END = "<!-- BENCHMARKS:END -->"
PROVENANCE = "<!-- BENCHMARKS:PROVENANCE "
PACKAGE = "github.com/pjscruggs/slogcp/v2"
RUNNER = "ubuntu-latest"
SAMPLE_COUNT = 10
CASES = (
    ("Typical", "Four common attributes"),
    ("NestedMixed", "Nested groups and mixed types"),
    ("TraceAbsent", "Trace context absent"),
    ("TracePresent", "Trace context present"),
    ("ErrorStackDisabled", "Error, stack capture disabled"),
    ("ErrorStackEnabled", "Error, stack capture enabled"),
)
BENCHMARK_NAMES = tuple("BenchmarkJSONHandlerCore/" + name for name, _ in CASES)
BENCHMARK_PATTERN = "^BenchmarkJSONHandlerCore$/^(" + "|".join(name for name, _ in CASES) + ")$"
COMMAND = (
    "go", "test", "-run=^$", "-bench=" + BENCHMARK_PATTERN, "-benchmem",
    "-benchtime=1s", "-count=10", "-cpu=1", "-timeout=10m", ".",
)
# Ignore personal Go configuration and make the runtime settings reproducible.
BENCHMARK_ENV = {
    "GOENV": "off", "GOFLAGS": "", "GOWORK": "off", "GOTOOLCHAIN": "local",
    "CGO_ENABLED": "0", "GOAMD64": "v1", "GOEXPERIMENT": "",
    "GOMAXPROCS": "1", "GOGC": "100", "GOMEMLIMIT": "off", "GODEBUG": "",
}
REQUIRED_INPUT_PATHS = frozenset((
    "go.mod", "go.sum", ".github/scripts/readme_benchmarks.py",
    ".github/scripts/publish_readme_benchmarks.py",
    ".github/workflows/benchmarks.yml",
))
BUILD_SUFFIXES = frozenset((
    ".go", ".s", ".S", ".h", ".c", ".cc", ".cpp", ".m", ".mm", ".f",
    ".F", ".for", ".f90", ".syso", ".swig", ".swigcxx",
))
SHA_RE = re.compile(r"[0-9a-f]{40}")
FINGERPRINT_RE = re.compile(r"[0-9a-f]{64}")
REPOSITORY_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*")
GO_VERSION_RE = re.compile(r"go version (go[1-9][0-9]*\.[0-9]+(?:\.[0-9]+)?(?:(?:beta|rc)[1-9][0-9]*)?) linux/amd64")
NUMBER_RE = re.compile(r"[0-9]+(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?")
CPU_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9 ._()/@:+,~-]{0,199}")
IMAGE_RE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{0,79}")
MAX_BLOCK_BYTES = 16_384
MAX_ARTIFACT_BYTES = 1_048_576


def canonical_json(value: object) -> str:
    return json.dumps(value, sort_keys=True, separators=(",", ":"), allow_nan=False)


def unique_object(pairs: list[tuple[str, object]]) -> dict:
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key: " + key)
        result[key] = value
    return result


def require_sha(value: str) -> str:
    if not isinstance(value, str) or SHA_RE.fullmatch(value) is None:
        raise ValueError("source SHA must be a full, lowercase 40-character commit SHA")
    return value


def block_bounds(readme: str) -> tuple[int, int]:
    if readme.count(START) != 1 or readme.count(END) != 1:
        raise ValueError("README must contain exactly one benchmark start and end marker")
    start, end = readme.index(START), readme.index(END)
    if start >= end:
        raise ValueError("README benchmark markers are reversed")
    for position, marker in ((start, START), (end, END)):
        if position and readme[position - 1] != "\n":
            raise ValueError("README benchmark markers must be on their own lines")
        tail = readme[position + len(marker):]
        if tail and not tail.startswith(("\n", "\r\n")):
            raise ValueError("README benchmark markers must be on their own lines")
    return start, end + len(END)


def validate_provenance(value: object) -> dict:
    required = {
        "schema", "source_sha", "source_url", "workflow_url", "repository",
        "input_fingerprint", "go_version", "goos", "goarch", "cpu", "runner",
        "command", "environment", "sample_count",
        "image_os", "image_version",
    }
    if not isinstance(value, dict) or set(value) != required:
        raise ValueError("benchmark provenance has missing or unexpected fields")
    if type(value["schema"]) is not int or value["schema"] != 1:
        raise ValueError("unsupported benchmark provenance schema")
    require_sha(value["source_sha"])
    repository = value["repository"]
    if not isinstance(repository, str) or REPOSITORY_RE.fullmatch(repository) is None:
        raise ValueError("invalid GitHub repository")
    base = "https://github.com/" + repository
    if value["source_url"] != base + "/commit/" + value["source_sha"]:
        raise ValueError("source URL must identify the exact source commit")
    if not isinstance(value["workflow_url"], str) or re.fullmatch(
        re.escape(base) + r"/actions/runs/[1-9][0-9]*", value["workflow_url"]
    ) is None:
        raise ValueError("workflow URL must identify a GitHub Actions run in the source repository")
    if not isinstance(value["input_fingerprint"], str) or FINGERPRINT_RE.fullmatch(value["input_fingerprint"]) is None:
        raise ValueError("invalid benchmark input fingerprint")
    if not isinstance(value["go_version"], str) or GO_VERSION_RE.fullmatch(value["go_version"]) is None:
        raise ValueError("benchmark Go version must identify a release toolchain on linux/amd64")
    if value["goos"] != "linux" or value["goarch"] != "amd64" or value["runner"] != RUNNER:
        raise ValueError("benchmark environment must be ubuntu-latest on linux/amd64")
    validate_image(value["image_os"], value["image_version"])
    if not isinstance(value["cpu"], str) or CPU_RE.fullmatch(value["cpu"]) is None:
        raise ValueError("invalid or missing CPU description")
    if value["command"] != list(COMMAND) or value["environment"] != BENCHMARK_ENV:
        raise ValueError("benchmark command or environment does not match the reporting policy")
    if type(value["sample_count"]) is not int or value["sample_count"] != SAMPLE_COUNT:
        raise ValueError("benchmark provenance must specify ten samples per case")
    return value


def validate_image(image_os: str, image_version: str) -> None:
    if any(not isinstance(value, str) or IMAGE_RE.fullmatch(value) is None for value in (image_os, image_version)):
        raise ValueError("actual runner ImageOS and ImageVersion metadata are required")


def previous_provenance(readme: str) -> dict | None:
    start, end = block_bounds(readme)
    block = readme[start:end]
    if PROVENANCE not in block:
        if PROVENANCE in readme:
            raise ValueError("benchmark provenance must be inside the report block")
        return None
    if block.count(PROVENANCE) != 1 or readme.count(PROVENANCE) != 1:
        raise ValueError("README must contain at most one benchmark provenance record")
    lines = [line for line in block.splitlines() if PROVENANCE in line]
    line = lines[0]
    if not line.startswith(PROVENANCE) or not line.endswith(" -->"):
        raise ValueError("malformed benchmark provenance marker")
    try:
        value = json.loads(line[len(PROVENANCE):-4], object_pairs_hook=unique_object)
    except (ValueError, TypeError) as exc:
        raise ValueError("malformed benchmark provenance JSON") from exc
    return validate_provenance(value)


def needs_report(readme: str, fingerprint: str, repository: str) -> bool:
    previous = previous_provenance(readme)
    if previous is None:
        return True
    if previous["repository"] != repository:
        # Rehearsal mirrors can start with a generated public README. Measure
        # afresh there; this is only a skip decision, never artifact acceptance.
        return True
    return previous["input_fingerprint"] != fingerprint


def parse_output(output: str) -> dict:
    """Reject incomplete runs, unexpected cases, and malformed Go benchmark data."""
    if len(output.encode("utf-8")) > MAX_ARTIFACT_BYTES:
        raise ValueError("benchmark output exceeds the size limit")
    metadata: dict[str, str] = {}
    samples: dict[str, list[dict[str, int | float]]] = {name: [] for name in BENCHMARK_NAMES}
    phase = "header"
    for line in output.splitlines():
        if not line.strip():
            continue
        if phase == "done":
            raise ValueError("unexpected output after the package result")
        header = re.fullmatch(r"(goos|goarch|pkg|cpu): (.+)", line)
        if header:
            key, value = header.groups()
            if phase != "header" or key in metadata:
                raise ValueError("duplicate or misplaced benchmark environment header")
            metadata[key] = value
            continue
        if line.startswith("Benchmark"):
            if phase not in ("header", "samples") or set(metadata) != {"goos", "goarch", "pkg", "cpu"}:
                raise ValueError("benchmark result has missing headers or follows PASS")
            phase = "samples"
            fields = line.split()
            if len(fields) != 8 or fields[3::2] != ["ns/op", "B/op", "allocs/op"]:
                raise ValueError("malformed benchmark metrics")
            name = fields[0].removesuffix("-1")
            if name not in samples:
                raise ValueError("unexpected benchmark case or CPU count: " + fields[0])
            if not fields[1].isascii() or not fields[1].isdigit() or int(fields[1]) <= 0:
                raise ValueError("benchmark iteration count must be positive")
            if NUMBER_RE.fullmatch(fields[2]) is None:
                raise ValueError("benchmark time must be finite and positive")
            ns = float(fields[2])
            if not math.isfinite(ns) or ns <= 0:
                raise ValueError("benchmark time must be finite and positive")
            if any(not item.isascii() or not item.isdigit() for item in (fields[4], fields[6])):
                raise ValueError("benchmark allocation metrics must be nonnegative integers")
            if any(len(item) > 19 or int(item) > 2**63 - 1 for item in (fields[1], fields[4], fields[6])):
                raise ValueError("benchmark integer metric is out of range")
            samples[name].append({
                "iterations": int(fields[1]), "ns_per_op": ns,
                "bytes_per_op": int(fields[4]), "allocs_per_op": int(fields[6]),
            })
            if len(samples[name]) > SAMPLE_COUNT:
                raise ValueError("too many benchmark samples for " + name)
            continue
        if line == "PASS":
            if phase != "samples":
                raise ValueError("missing benchmark samples or duplicate PASS")
            phase = "passed"
            continue
        if re.fullmatch(r"ok\s+" + re.escape(PACKAGE) + r"\s+[0-9]+(?:\.[0-9]+)?s", line):
            if phase != "passed":
                raise ValueError("package result must follow PASS")
            phase = "done"
            continue
        raise ValueError("unrecognized benchmark output: " + line[:160])
    if phase != "done":
        raise ValueError("benchmark output is incomplete: PASS and final package result are required")
    if metadata.get("goos") != "linux" or metadata.get("goarch") != "amd64" or metadata.get("pkg") != PACKAGE:
        raise ValueError("benchmark output must be for the root package on linux/amd64")
    if CPU_RE.fullmatch(metadata.get("cpu", "")) is None:
        raise ValueError("invalid or missing CPU description")
    for name, values in samples.items():
        if len(values) != SAMPLE_COUNT:
            raise ValueError(f"expected {SAMPLE_COUNT} samples for {name}, got {len(values)}")
    return {"environment": metadata, "samples": samples}


def wrap_prose(text: str) -> list[str]:
    # Keep multiword CPU models inside one inline-code span when wrapping prose.
    protected = re.sub(r"`[^`]+`", lambda match: match.group().replace(" ", "\u00a0"), text)
    lines = textwrap.wrap(protected, width=80, break_long_words=False, break_on_hyphens=False)
    if any(len(line) > 80 for line in lines):
        raise ValueError("benchmark prose contains an inline token longer than 80 columns")
    return [line.replace("\u00a0", " ") for line in lines]


def render_report(parsed: dict, provenance: dict) -> str:
    validate_provenance(provenance)
    # Recheck the association even when called independently of the CLI.
    if parsed["environment"] != {
        "goos": provenance["goos"], "goarch": provenance["goarch"],
        "cpu": provenance["cpu"], "pkg": PACKAGE,
    }:
        raise ValueError("benchmark output and provenance environments differ")
    if set(parsed["samples"]) != set(BENCHMARK_NAMES):
        raise ValueError("report must include every selected benchmark case")
    lines = [
        START,
        # The protocol stores a single-line machine record, not prose.
        "<!-- markdownlint-disable-next-line MD013 -->",
        PROVENANCE + canonical_json(provenance) + " -->",
        *wrap_prose(
            "These microbenchmarks measure the JSON handler with prebuilt records and an "
            "`io.Discard` writer. Timestamp emission and service context are enabled. "
            "Timing excludes record construction, output I/O, and Cloud Logging ingestion."
        ),
        "",
        *wrap_prose(
            "Each row uses all 10 one-second samples with `GOMAXPROCS=1`. The time "
            "and allocation columns show medians. The time range shows variation "
            "across samples rather than a confidence interval."
        ),
        "",
        "| Record | ns/op (median) | ns/op (min–max) | B/op (median) | allocs/op (median) |",
        "| --- | ---: | ---: | ---: | ---: |",
    ]
    for (name, label), full_name in zip(CASES, BENCHMARK_NAMES):
        values = parsed["samples"][full_name]
        if len(values) != SAMPLE_COUNT:
            raise ValueError("report must contain ten samples per case")
        times = [sample["ns_per_op"] for sample in values]
        byte_counts = [sample["bytes_per_op"] for sample in values]
        allocations = [sample["allocs_per_op"] for sample in values]
        for metrics, positive in ((times, True), (byte_counts, False), (allocations, False)):
            if any(type(value) not in (int, float) or not math.isfinite(value) or value < 0 or (positive and value == 0) for value in metrics):
                raise ValueError("report metrics must be finite and nonnegative, with positive times")
        lines.append(
            f"| {label} (`{name}`) | {statistics.median(times):,.1f} | "
            f"{min(times):,.1f}–{max(times):,.1f} | {statistics.median(byte_counts):,g} | "
            f"{statistics.median(allocations):,g} |"
        )
    go_version = GO_VERSION_RE.fullmatch(provenance["go_version"]).group(1)
    lines.extend([
        "",
        *wrap_prose(
            f"The [benchmark run][slogcp-readme-benchmark-run] used "
            f"commit [`{provenance['source_sha'][:12]}`][slogcp-readme-benchmark-source] "
            f"and `{go_version}`."
        ),
        *wrap_prose(
            f"The `{RUNNER}` runner used `linux/amd64` and image "
            f"`{provenance['image_os']}` version `{provenance['image_version']}`. "
            f"Its CPU was `{provenance['cpu']}`."
        ),
        *wrap_prose(
            "Hosted runner hardware and load vary between runs. These measurements "
            "describe handler costs. They do not predict application latency or "
            "establish performance regressions."
        ),
        "",
        "To repeat this measurement, run this command from the repository root.",
        "",
        "```sh",
        " ".join(key + "=" + shlex.quote(value) for key, value in BENCHMARK_ENV.items()) + " \\",
        "  " + shlex.join(COMMAND),
        "```",
        "",
        "[slogcp-readme-benchmark-source]: " + provenance["source_url"],
        "[slogcp-readme-benchmark-run]: " + provenance["workflow_url"],
        END,
    ])
    result = "\n".join(lines)
    if len(result.encode("utf-8")) > MAX_BLOCK_BYTES:
        raise ValueError("generated benchmark block exceeds the size limit")
    return result


def replace_report(readme: str, report: str) -> str:
    start, end = block_bounds(readme)
    report_start, report_end = block_bounds(report)
    if report_start != 0 or report_end != len(report):
        raise ValueError("replacement must contain only the benchmark marker block")
    if len(report.encode("utf-8")) > MAX_BLOCK_BYTES:
        raise ValueError("generated benchmark block exceeds the size limit")
    if previous_provenance(report) is None:
        raise ValueError("replacement benchmark report must include provenance")
    return readme[:start] + report + readme[end:]


def load_report(output_dir: Path) -> dict:
    """Validate the artifact against raw Go output before publication."""
    for filename in ("report.json", "benchmark.txt"):
        path = output_dir / filename
        if not path.is_file() or path.is_symlink() or path.stat().st_size > MAX_ARTIFACT_BYTES:
            raise ValueError("missing, nonregular, or oversized benchmark artifact: " + filename)
    report = json.loads(
        (output_dir / "report.json").read_text(encoding="utf-8"),
        object_pairs_hook=unique_object,
    )
    if not isinstance(report, dict) or set(report) != {"provenance", "benchmarks"}:
        raise ValueError("benchmark report artifact has missing or unexpected fields")
    validate_provenance(report["provenance"])
    parsed = parse_output((output_dir / "benchmark.txt").read_text(encoding="utf-8"))
    if canonical_json(report["benchmarks"]) != canonical_json(parsed):
        raise ValueError("benchmark report artifact differs from the raw Go output")
    render_report(parsed, report["provenance"])
    return report


def git(repo: Path, *args: str) -> str:
    return subprocess.run(
        ["git", "-C", str(repo), *args], check=True,
        capture_output=True, text=True, timeout=30,
    ).stdout.rstrip("\r\n")


def assert_clean_source(repo: Path, source_sha: str) -> None:
    require_sha(source_sha)
    if Path(git(repo, "rev-parse", "--show-toplevel")).resolve() != repo.resolve():
        raise ValueError("repo must name the Git repository root")
    if git(repo, "rev-parse", "HEAD") != source_sha:
        raise ValueError("source SHA must match the checked-out HEAD")
    if git(repo, "status", "--porcelain=v1", "--untracked-files=all"):
        raise ValueError("benchmarks require a clean source checkout, including untracked files")


def fingerprint_inputs(
    repo: Path, source_sha: str, go_version: str, runner: str,
    image_os: str, image_version: str,
) -> str:
    require_sha(source_sha)
    if GO_VERSION_RE.fullmatch(go_version) is None or runner != RUNNER:
        raise ValueError("unsupported benchmark toolchain or runner")
    validate_image(image_os, image_version)
    tree: dict[str, tuple[str, str, str]] = {}
    for item in git(repo, "ls-tree", "-r", "-z", source_sha).split("\0"):
        if not item:
            continue
        metadata, path = item.split("\t", 1)
        mode, kind, oid = metadata.split()
        tree[path] = (mode, kind, oid)
    independent_modules = tuple(path[:-len("go.mod")] for path in tree if path.endswith("/go.mod"))
    entries: dict[str, str] = {}
    for path, (mode, kind, oid) in tree.items():
        # Include local library dependencies such as slogcpasync, not just the
        # root package. Independently versioned modules and hidden infrastructure
        # are not compiled by this command. Docs-only changes must still skip.
        library_source = (
            Path(path).suffix in BUILD_SUFFIXES
            and not any(part.startswith(".") for part in path.split("/"))
            and not path.startswith(independent_modules)
        )
        if library_source or path in REQUIRED_INPUT_PATHS:
            if kind != "blob" or mode not in ("100644", "100755"):
                raise ValueError("benchmark input must be a regular committed file: " + path)
            entries[path] = mode + " " + oid
    if not REQUIRED_INPUT_PATHS.issubset(entries) or "json_handler_bench_test.go" not in entries:
        raise ValueError("committed benchmark source, reporter, workflow, or Go module files are missing")
    description = {
        "schema": 1, "files": entries, "go_version": go_version, "runner": runner,
        "image_os": image_os, "image_version": image_version,
        "goos": "linux", "goarch": "amd64", "command": COMMAND,
        "environment": BENCHMARK_ENV,
    }
    return hashlib.sha256(canonical_json(description).encode("utf-8")).hexdigest()


def benchmark_environment(repo: Path) -> tuple[dict[str, str], str]:
    if platform.system() != "Linux" or platform.machine().lower() not in ("x86_64", "amd64"):
        raise ValueError("README benchmarks must run on a Linux amd64 runner")
    environment = os.environ.copy()
    environment.update(BENCHMARK_ENV)
    validate_image(environment.get("ImageOS", ""), environment.get("ImageVersion", ""))
    go_version = subprocess.run(
        ["go", "version"], cwd=repo, env=environment, check=True,
        capture_output=True, text=True, timeout=60,
    ).stdout.strip()
    if GO_VERSION_RE.fullmatch(go_version) is None:
        raise ValueError("Go must be a release toolchain on linux/amd64")
    targets = subprocess.run(
        ["go", "env", "GOHOSTOS", "GOHOSTARCH", "GOOS", "GOARCH"],
        cwd=repo, env=environment, check=True, capture_output=True, text=True, timeout=60,
    ).stdout.splitlines()
    if targets != ["linux", "amd64", "linux", "amd64"]:
        raise ValueError("native linux/amd64 Go execution is required")
    return environment, go_version


def execute_benchmarks(repo: Path, environment: dict[str, str], output_dir: Path) -> str:
    # The Go command's own timeout is a second bound. Kill its process group on
    # interruption/timeout so a compiler or benchmark child cannot outlive it.
    with tempfile.TemporaryDirectory(prefix="slogcp-readme-benchmarks-") as temporary:
        environment = dict(environment, GOTMPDIR=temporary)
        process = subprocess.Popen(
            COMMAND, cwd=repo, env=environment, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, text=True, start_new_session=True,
        )
        try:
            stdout, stderr = process.communicate(timeout=720)
        except BaseException:
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass  # The child may have exited concurrently with the timeout.
            process.communicate()
            raise
        (output_dir / "benchmark.txt").write_text(stdout, encoding="utf-8")
        (output_dir / "benchmark.stderr.txt").write_text(stderr, encoding="utf-8")
        if process.returncode != 0:
            raise ValueError(f"Go benchmarks failed with exit code {process.returncode}; see benchmark artifacts")
        return stdout


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="operation", required=True)
    for operation in ("check", "run"):
        command_parser = commands.add_parser(operation)
        command_parser.add_argument("--repo", type=Path, default=Path("."))
        command_parser.add_argument("--readme", type=Path, default=Path("README.md"))
        command_parser.add_argument("--source-sha", required=True)
        command_parser.add_argument("--repository", required=True, help="GitHub owner/repository")
        command_parser.add_argument("--runner", choices=(RUNNER,), default=RUNNER)
        if operation == "run":
            command_parser.add_argument("--workflow-url", required=True)
            command_parser.add_argument("--output-dir", type=Path, required=True, help="artifact directory outside the source checkout")
    args = parser.parse_args(argv)
    repo = args.repo.resolve()
    readme = (repo / args.readme).resolve()
    if readme != repo / "README.md":
        raise ValueError("only the root README.md may be updated")
    if REPOSITORY_RE.fullmatch(args.repository) is None:
        raise ValueError("invalid GitHub repository")
    assert_clean_source(repo, args.source_sha)
    # Decode bytes directly to preserve all line endings outside the marker block.
    original = readme.read_bytes().decode("utf-8")
    block_bounds(original)
    environment, go_version = benchmark_environment(repo)
    image_os, image_version = environment.get("ImageOS", ""), environment.get("ImageVersion", "")
    validate_image(image_os, image_version)
    fingerprint = fingerprint_inputs(repo, args.source_sha, go_version, args.runner, image_os, image_version)
    needed = needs_report(original, fingerprint, args.repository)
    if args.operation == "check":
        print(canonical_json({"needed": needed, "input_fingerprint": fingerprint, "source_sha": args.source_sha}))
        return 0 if needed else 3
    if not needed:
        print(canonical_json({"updated": False, "reason": "benchmark inputs already reported"}))
        return 0
    provenance = {
        "schema": 1, "source_sha": args.source_sha, "repository": args.repository,
        "source_url": f"https://github.com/{args.repository}/commit/{args.source_sha}",
        "workflow_url": args.workflow_url, "input_fingerprint": fingerprint,
        "go_version": go_version, "goos": "linux", "goarch": "amd64",
        "cpu": "Pending", "runner": args.runner, "command": list(COMMAND),
        "image_os": image_os, "image_version": image_version,
        "environment": BENCHMARK_ENV, "sample_count": SAMPLE_COUNT,
    }
    validate_provenance(provenance)
    output_dir = args.output_dir.resolve()
    if output_dir == repo or repo in output_dir.parents:
        raise ValueError("benchmark artifacts must be stored outside the source checkout")
    output_dir.mkdir(parents=True, exist_ok=True)
    parsed = parse_output(execute_benchmarks(repo, environment, output_dir))
    provenance["cpu"] = parsed["environment"]["cpu"]
    report = render_report(parsed, provenance)
    updated = replace_report(original, report)
    (output_dir / "report.json").write_text(
        json.dumps({"provenance": provenance, "benchmarks": parsed}, indent=2, allow_nan=False) + "\n",
        encoding="utf-8",
    )
    assert_clean_source(repo, args.source_sha)
    if readme.read_bytes().decode("utf-8") != original:
        raise ValueError("README changed while benchmarks were running")
    if updated != original:
        readme.write_bytes(updated.encode("utf-8"))
    print(canonical_json({"updated": updated != original, "input_fingerprint": fingerprint, "source_sha": args.source_sha}))
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (ValueError, OSError, subprocess.SubprocessError) as exc:
        print(f"readme benchmarks: {exc}", file=sys.stderr)
        sys.exit(1)
