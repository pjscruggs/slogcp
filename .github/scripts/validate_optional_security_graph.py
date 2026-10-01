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

"""Verify optional-module Renovate root repairs with the shared graph policy."""

from __future__ import annotations

import argparse
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys

import selected_graph_policy as graph
from validate_security_graph import validate_graph_delta, validate_root_floors


def load_candidate_policy(root: Path):
    source = root / ".github/scripts/validate_renovate_pr.py"
    if not source.is_file():
        raise ValueError("Candidate repository has no Renovate scope policy")
    spec = importlib.util.spec_from_file_location("optional_candidate_policy", source)
    if spec is None or spec.loader is None:
        raise ValueError("Cannot load candidate Renovate scope policy")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def assess(event: dict, base: str, root: Path) -> tuple[str, dict | None]:
    root = root.resolve()
    if Path.cwd().resolve() != root:
        raise ValueError("Run optional security validation from the candidate checkout")
    candidate = load_candidate_policy(root)
    kind = candidate.validate_event(event, base)
    if kind != "security_patch":
        return kind, None
    head = candidate.git("rev-parse", "HEAD").strip()
    validate_root_floors(candidate.git("show", f"{base}:go.mod"),
                         candidate.git("show", f"{head}:go.mod"))
    report = graph.compare_git(root, base)
    validate_graph_delta(report)
    return "verified_security_repair", report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository", type=Path, default=Path.cwd())
    parser.add_argument("--base", required=True)
    parser.add_argument("--event", type=Path,
                        default=os.environ.get("GITHUB_EVENT_PATH"))
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    if not args.event:
        parser.error("--event or GITHUB_EVENT_PATH is required")
    try:
        event = json.loads(args.event.read_text(encoding="utf-8"))
        result, report = assess(event, args.base, args.repository)
        if report and args.report:
            args.report.write_text(json.dumps(report, indent=2) + "\n",
                                   encoding="utf-8")
    except (ValueError, graph.PolicyError, OSError, subprocess.CalledProcessError,
            json.JSONDecodeError) as error:
        print(f"Optional security graph validation failed: {error}", file=sys.stderr)
        return 1
    print(f"Optional security graph validation: {result} (base={args.base})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
