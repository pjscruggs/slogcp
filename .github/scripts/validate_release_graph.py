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

"""Refresh selected-graph evidence before publishing a mainline release."""

from __future__ import annotations

import argparse
import json
from pathlib import Path
import subprocess
import sys

import audit_generated_graphs
import selected_graph_policy


def validate(root: Path, base: str) -> dict:
    tracked = selected_graph_policy.compare_git(root, base)
    generated = audit_generated_graphs.compare_generated_git(root, base)
    introduced = [*tracked["introduced"], *(
        {**item, "scope": ".e2e/generated/" + item["scope"]}
        for item in generated["introduced"]
    )]
    persistent = [*tracked["persistent"], *(
        {**item, "scope": ".e2e/generated/" + item["scope"]}
        for item in generated["persistent"]
    )]
    report = {"schema": 2, "base": base,
              "candidate": subprocess.run(
                  ["git", "-C", str(root), "rev-parse", "HEAD"],
                  check=True, capture_output=True, text=True).stdout.strip(),
              "tracked": tracked, "generated": generated,
              "introduced": introduced, "persistent": persistent}
    return report


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--base", required=True,
                        help="Exact first-parent commit before the release")
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    try:
        report = validate(args.root.resolve(), args.base)
        if args.report:
            args.report.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    except (ValueError, selected_graph_policy.PolicyError, OSError,
            subprocess.CalledProcessError, json.JSONDecodeError) as error:
        print(f"Release graph policy indeterminate or unsafe: {error}", file=sys.stderr)
        return 1
    try:
        selected_graph_policy.require_clean_comparison(report)
    except (ValueError, selected_graph_policy.PolicyError) as error:
        print(f"Release selected graph is unsafe: {error}", file=sys.stderr)
        return 1
    print("Release selected graph: clean; "
          f"tracked fetched {report['tracked']['advisory_fetched_at']}; "
          f"generated fetched {report['generated']['advisory_fetched_at']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
