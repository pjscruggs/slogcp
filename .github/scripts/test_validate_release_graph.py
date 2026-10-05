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

"""Release graph preflight uses both selected inventories and fails closed."""

from __future__ import annotations

import contextlib
import io
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

import validate_release_graph as policy


class ReleaseGraphTests(unittest.TestCase):
    def invoke(self, tracked: dict, generated: dict) -> tuple[int, dict]:
        with tempfile.TemporaryDirectory() as temporary:
            report = Path(temporary) / "report.json"
            with mock.patch.object(sys, "argv", ["validate_release_graph.py",
                                                "--base", "a" * 40,
                                                "--report", str(report)]), \
                 mock.patch.object(policy.selected_graph_policy, "compare_git",
                                   return_value=tracked), \
                 mock.patch.object(policy.audit_generated_graphs,
                                   "compare_generated_git", return_value=generated), \
                 mock.patch.object(policy.subprocess, "run") as run, \
                 contextlib.redirect_stdout(io.StringIO()), \
                 contextlib.redirect_stderr(io.StringIO()):
                run.return_value.stdout = "b" * 40 + "\n"
                outcome = policy.main()
            return outcome, json.loads(report.read_text(encoding="utf-8"))

    def test_rejects_existing_findings_without_new_selections(self):
        tracked = {"advisory_fetched_at": "2026-09-30T00:00:00Z",
                   "introduced": [], "persistent": [{"scope": ".", "module": "example.org/x", "advisory": "OLD"}]}
        generated = {"advisory_fetched_at": "2026-09-30T00:00:00Z",
                     "introduced": [], "persistent": [{"scope": ".", "module": "example.org/x", "advisory": "OLD"}]}
        outcome, report = self.invoke(tracked, generated)
        self.assertEqual(outcome, 1)
        self.assertEqual(report["introduced"], [])

    def test_rejects_generated_introduction_and_retains_evidence(self):
        tracked = {"advisory_fetched_at": "2026-09-30T00:00:00Z",
                   "introduced": [], "persistent": []}
        generated = {"advisory_fetched_at": "2026-09-30T00:00:00Z",
                     "introduced": [{"scope": "services/x", "module": "example.org/x",
                                     "advisory": "GO-EXAMPLE"}], "persistent": []}
        outcome, report = self.invoke(tracked, generated)
        self.assertEqual(outcome, 1)
        self.assertEqual(report["introduced"][0]["scope"],
                         ".e2e/generated/services/x")


if __name__ == "__main__":
    unittest.main()
