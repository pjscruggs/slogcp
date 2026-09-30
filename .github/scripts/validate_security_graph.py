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

"""Require a Renovate root security patch to repair the selected Go graph.

This is a repair and no-new-finding guard. Existing selected-graph findings
still need resolution or narrow reviewed exceptions before a clean-graph
merge policy can be required.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys

import selected_graph_policy as graph
import audit_generated_graphs as generated_graph
import validate_renovate_pr as candidate_policy


RELEASE = re.compile(r"^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(?:\+incompatible)?$")
ROOT_OVERRIDE = re.compile(r"(?m)^\s*(?:replace|exclude)\b")


def release_version(value: str) -> tuple[int, int, int]:
    match = RELEASE.fullmatch(value)
    if not match:
        raise ValueError(f"Automatic root repair needs a stable Go module release: {value}")
    return tuple(int(part) for part in match.groups())


def validate_root_floors(base: str, head: str) -> None:
    if ROOT_OVERRIDE.search(head):
        raise ValueError("Automatic root repair must not rely on replace/exclude")
    before = candidate_policy.requirements(base)
    after = candidate_policy.requirements(head)
    changed = [path for path in sorted(set(before) | set(after))
               if before.get(path) != after.get(path)]
    if not changed:
        raise ValueError("A root security patch must raise a declared dependency floor")
    increased = False
    for path in changed:
        if path not in after:
            raise ValueError(f"Automatic root repair must not remove a requirement: {path}")
        new = release_version(after[path])
        if path in before:
            old = release_version(before[path])
            if new[0] != old[0] or new <= old:
                raise ValueError(f"Automatic root repair must raise {path} within its major version")
        increased = True
    if not increased:
        raise ValueError("Root dependency floors did not increase")


def validate_graph_delta(report: dict) -> None:
    if report["introduced"]:
        details = ", ".join(f"{item['scope']} {item['module']} {item['advisory']}"
                            for item in report["introduced"])
        raise ValueError(f"Selected graph introduced affected modules: {details}")
    repaired = [item for item in report["resolved"] if item["scope"] == "."]
    if not repaired:
        raise ValueError("Root selected graph did not resolve an applicable advisory")


def assess(event: dict, base: str, root: Path) -> tuple[str, dict | None]:
    kind = candidate_policy.validate_event(event, base)
    if kind != "security_patch":
        return kind, None
    head = candidate_policy.git("rev-parse", "HEAD").strip()
    validate_root_floors(candidate_policy.git("show", f"{base}:go.mod"),
                         candidate_policy.git("show", f"{head}:go.mod"))
    report = graph.compare_git(root, base)
    generated = generated_graph.compare_generated_git(root, base)
    report["generated"] = generated
    report["introduced"].extend({**item, "scope": ".e2e/generated/" + item["scope"]}
                                  for item in generated["introduced"])
    return "security_repair_candidate", report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", required=True)
    parser.add_argument("--event", default=os.environ.get("GITHUB_EVENT_PATH"))
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    if not args.event:
        parser.error("--event or GITHUB_EVENT_PATH is required")
    try:
        event = json.loads(Path(args.event).read_text(encoding="utf-8"))
        result, report = assess(event, args.base, args.root.resolve())
        if report and args.report:
            args.report.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
        if report:
            validate_graph_delta(report)
            result = "verified_security_repair"
    except (ValueError, graph.PolicyError, OSError, subprocess.CalledProcessError,
            json.JSONDecodeError) as error:
        print(f"Security graph validation failed: {error}", file=sys.stderr)
        return 1
    print(f"Security graph validation: {result} (base={args.base})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
