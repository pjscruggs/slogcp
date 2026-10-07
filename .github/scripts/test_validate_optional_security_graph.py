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

"""Checks for the shared optional-module root repair gate."""

from __future__ import annotations

from pathlib import Path
from datetime import datetime, timezone
import tempfile
import types
import unittest
from unittest import mock

import validate_optional_security_graph as optional


def with_candidate(report: dict) -> dict:
    findings = [*report["introduced"], *report["persistent"]]
    candidate = {"findings": findings, "modules": sorted({item["scope"] for item in findings}) or ["."],
                 "selected": [{"scope": item["scope"], "path": item["module"],
                               "selected_path": item.get("selected_path"),
                               "selected_version": item.get("version")}
                              for item in findings],
                 "advisory_fetched_at": datetime.now(timezone.utc).isoformat(),
                 "advisories": {item["advisory"]: {"modified": item.get("modified")}
                                for item in findings}}
    candidate["applicability"] = optional.graph._applicability(candidate)
    return {**report, "candidate": candidate}


class OptionalSecurityGraphTests(unittest.TestCase):
    def test_non_releasing_candidate_does_not_query_advisories(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            candidate = types.SimpleNamespace(validate_event=lambda *_: "non_releasing")
            with mock.patch.object(optional, "load_candidate_policy", return_value=candidate), \
                 mock.patch.object(optional.graph, "compare_git") as compare, \
                 mock.patch.object(optional.Path, "cwd", return_value=root):
                result, report = optional.assess({}, "a" * 40, root)
            self.assertEqual((result, report), ("non_releasing", None))
            compare.assert_not_called()

    def test_security_patch_requires_floor_and_resolved_root_advisory(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            candidate = types.SimpleNamespace(
                validate_event=lambda *_: "security_patch",
                git=lambda *args: "b" * 40 if args[0] == "rev-parse" else "module fixture\n",
            )
            report = with_candidate({"persistent": [], "introduced": [], "resolved": [{"scope": ".", "module": "example.org/m", "advisory": "GO-1"}]})
            with mock.patch.object(optional, "load_candidate_policy", return_value=candidate), \
                 mock.patch.object(optional, "validate_root_floors") as floors, \
                 mock.patch.object(optional.graph, "compare_git", return_value=report) as compare, \
                 mock.patch.object(optional.Path, "cwd", return_value=root):
                result, actual = optional.assess({}, "a" * 40, root)
            self.assertEqual(result, "verified_security_repair")
            self.assertIs(actual, report)
            floors.assert_called_once()
            compare.assert_called_once_with(root, "a" * 40)

    def test_introduced_advisory_stops_security_patch(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            candidate = types.SimpleNamespace(
                validate_event=lambda *_: "security_patch",
                git=lambda *_: "b" * 40,
            )
            report = with_candidate({"persistent": [], "introduced": [{"scope": ".", "module": "example.org/m", "advisory": "GO-2"}],
                                     "resolved": [{"scope": ".", "module": "example.org/m", "advisory": "GO-1"}]})
            with mock.patch.object(optional, "load_candidate_policy", return_value=candidate), \
                 mock.patch.object(optional, "validate_root_floors"), \
                 mock.patch.object(optional.graph, "compare_git", return_value=report), \
                 mock.patch.object(optional.Path, "cwd", return_value=root):
                with self.assertRaisesRegex(ValueError, "GO-2"):
                    optional.assess({}, "a" * 40, root)


if __name__ == "__main__":
    unittest.main()
