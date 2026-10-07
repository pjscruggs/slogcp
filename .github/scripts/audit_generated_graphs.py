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

"""Generate every declared cloud consumer and audit its selected Go modules."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile

import selected_graph_policy as graph


SERVICES = (
    "e2e-harness",
    "target-apps/core-logging-target-app",
    "target-apps/trace-downstream-http",
    "target-apps/trace-downstream-grpc",
    "target-apps/trace-target-app",
)
TRACEPROTO = "traceproto"
SLOGCP = "github.com/pjscruggs/slogcp/v2"
TRACEPROTO_MODULE = "github.com/pjscruggs/slogcp-e2e-internal/services/traceproto"


def source_manifests(root: Path) -> list[str]:
    result = subprocess.run(["git", "-C", str(root), "ls-files", "-z"],
                            check=True, capture_output=True)
    found = sorted(item.decode() for item in result.stdout.split(b"\0")
                   if item.endswith(b"go.module.json") and
                   item.startswith(b".e2e/services/"))
    expected = sorted(f".e2e/services/{name}/go.module.json"
                      for name in (*SERVICES, TRACEPROTO))
    if found != expected:
        raise graph.PolicyError(f"Generated module manifest coverage changed: {found}")
    return found


def stage_sources(root: Path, stage: Path) -> list[Path]:
    source = root / ".e2e/services"
    target = stage / "services"
    shutil.copytree(source, target)
    destinations = []
    for name in SERVICES:
        service = target / name
        metadata = json.loads((service / "go.module.json").read_text(encoding="utf-8"))
        for item in metadata.get("pinned_modules", []):
            replacement = item.get("replace_path")
            if not replacement:
                continue
            destination = (service / replacement).resolve()
            if not destination.is_relative_to(service.resolve()):
                raise graph.PolicyError(f"Generated replacement escapes service: {name}")
            if item.get("module_path") == TRACEPROTO_MODULE:
                shutil.copytree(target / TRACEPROTO, destination)
            elif item.get("module_path") == SLOGCP:
                shutil.copytree(root, destination, ignore=shutil.ignore_patterns(
                    ".git", ".github", ".e2e", ".examples", ".benchmarks", "scratch"))
        destinations.append(service)
    return destinations


def prepare_generated(root: Path, stage: Path) -> tuple[list[Path], dict]:
    manifests = source_manifests(root)
    services = stage_sources(root, stage)
    version_source = (root / "version.go").read_text(encoding="utf-8")
    version = re.findall(r'(?m)^(?:var|const) Version = "(v\d+\.\d+\.\d+)"$',
                         version_source)
    if len(version) != 1:
        raise graph.PolicyError("Expected one canonical root Version for E2E generation")
    runtime = subprocess.run(["go", "env", "GOVERSION"], check=True,
                             capture_output=True, text=True).stdout.strip()
    if not re.fullmatch(r"go1\.\d+\.\d+", runtime):
        raise graph.PolicyError(f"Unexpected Go runtime: {runtime}")
    generator_report = stage / "generator-report.json"
    command = [sys.executable, str(root / ".e2e/scripts/generate_go_module.py")]
    for service in services:
        command.extend(["--module-dir", str(service)])
    command.extend(["--go-version", runtime.removeprefix("go"),
                    "--slogcp-dir", str(root), "--slogcp-reference", version[0],
                    "--dependency-mode", "floor",
                    "--slogcp-shared-parity-scope", "package",
                    "--emit-dependency-report", str(generator_report)])
    completed = subprocess.run(command, cwd=root, capture_output=True, text=True)
    if completed.returncode:
        raise graph.PolicyError(f"Generated E2E modules failed: {completed.stderr[-2000:]}")
    generation = json.loads(generator_report.read_text(encoding="utf-8"))
    if generation.get("status") != "success":
        raise graph.PolicyError("Generated E2E dependency report is incomplete")
    module_files = sorted((stage / "services").rglob("go.mod"))
    if not module_files or not all((service / "go.mod") in module_files for service in services):
        raise graph.PolicyError("Generated E2E module inventory is incomplete")
    metadata = {"generated_inputs": manifests, "go_runtime": runtime,
                "root_version": version[0], "generated_module_count": len(module_files)}
    return module_files, metadata


def generate_and_audit(root: Path, stage: Path) -> dict:
    module_files, metadata = prepare_generated(root, stage)
    report = graph.evaluate(stage, module_files, module_files)
    report.update(metadata)
    return report


def compare_generated(base_root: Path, candidate_root: Path,
                      base_stage: Path, candidate_stage: Path) -> dict:
    base_files, base_metadata = prepare_generated(base_root, base_stage)
    candidate_files, candidate_metadata = prepare_generated(candidate_root, candidate_stage)
    report = graph.compare_explicit(base_stage, base_files, candidate_stage,
                                    candidate_files, base_files, candidate_files)
    report["base_generated"] = base_metadata
    report["candidate_generated"] = candidate_metadata
    return report


def compare_generated_git(root: Path, base: str) -> dict:
    if not re.fullmatch(r"[a-f0-9]{40}", base):
        raise graph.PolicyError("Generated comparison requires an exact base commit SHA")
    with tempfile.TemporaryDirectory(prefix="generated-graph-compare-") as temporary:
        parent = Path(temporary)
        base_root = parent / "base-source"
        subprocess.run(["git", "-C", str(root), "worktree", "add", "--detach",
                        str(base_root), base], check=True, capture_output=True, text=True)
        try:
            return compare_generated(base_root, root, parent / "base-stage",
                                     parent / "candidate-stage")
        finally:
            subprocess.run(["git", "-C", str(root), "worktree", "remove",
                            "--force", str(base_root)], check=True,
                           capture_output=True, text=True)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--base", help="Compare generated consumers with this base commit")
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    try:
        root = args.root.resolve()
        if args.base:
            report = compare_generated_git(root, args.base)
        else:
            with tempfile.TemporaryDirectory(prefix="generated-graph-audit-") as temporary:
                report = generate_and_audit(root, Path(temporary))
    except (graph.PolicyError, OSError, subprocess.CalledProcessError,
            ValueError, json.JSONDecodeError) as error:
        print(f"Generated graph audit indeterminate: {error}", file=sys.stderr)
        return 2
    if args.report:
        args.report.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    candidate = report["candidate"] if args.base else report
    count = (report["candidate_generated"]["generated_module_count"] if args.base
             else report["generated_module_count"])
    print(f"Generated graph audit: {candidate['result']}; "
          f"{count} modules; {len(candidate['findings'])} findings")
    if args.base:
        print(f"  Introduced: {len(report['introduced'])}; "
              f"resolved: {len(report['resolved'])}; "
              f"persistent: {len(report['persistent'])}")
    try:
        return 1 if graph.blocking_findings(report) else 0
    except graph.PolicyError as error:
        print(f"Generated package policy indeterminate: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
