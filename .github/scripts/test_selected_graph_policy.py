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

"""Regression cases for complete selected-graph security decisions."""

from __future__ import annotations

import importlib.util
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock


SOURCE = Path(__file__).with_name("selected_graph_policy.py")
SPEC = importlib.util.spec_from_file_location("selected_graph_policy", SOURCE)
assert SPEC and SPEC.loader
policy = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(policy)


class SelectedGraphPolicyTests(unittest.TestCase):
    def test_selected_but_unimported_transitive_module_is_queried(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "go.mod").write_text("module example.test/root\ngo 1.27\n")
            entries = [
                {"Path": "example.test/root", "Main": True},
                {"Path": "example.test/parent", "Version": "v1.1.0"},
                {"Path": "example.test/unimported", "Version": "v2.0.0", "Indirect": True},
            ]
            with mock.patch.object(policy, "tracked_modules", return_value=[root / "go.mod"]), \
                 mock.patch.object(policy, "selected_modules", return_value=entries):
                selected, queries = policy.inventory(root, [root / "go.mod"])
            self.assertEqual(len(selected), 2)
            self.assertIn({"package": {"ecosystem": "Go", "name": "example.test/unimported"},
                           "version": "v2.0.0"}, queries)

    def test_local_replacement_must_have_an_inventoried_module(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "go.mod").write_text("module example.test/root\ngo 1.27\n")
            entries = [{"Path": "example.test/root", "Main": True},
                       {"Path": "example.test/other", "Version": "v1.0.0",
                        "Replace": {"Path": "./missing"}}]
            with mock.patch.object(policy, "tracked_modules", return_value=[root / "go.mod"]), \
                 mock.patch.object(policy, "selected_modules", return_value=entries):
                with self.assertRaisesRegex(policy.PolicyError, "Uninventoried"):
                    policy.inventory(root, [root / "go.mod"])

    def test_paginated_batch_is_complete_for_each_query(self) -> None:
        queries = [{"package": {"ecosystem": "Go", "name": "example.test/a"},
                    "version": "v1.0.0"},
                   {"package": {"ecosystem": "Go", "name": "example.test/b"},
                    "version": "v2.0.0"}]
        responses = [
            {"results": [{"vulns": [{"id": "GO-1", "modified": "today"}],
                          "next_page_token": "more"}, {"vulns": []}]},
            {"results": [{"vulns": [{"id": "GO-2", "modified": "today"}]}]},
        ]
        with mock.patch.object(policy, "request_json", side_effect=responses) as request:
            result = policy.query_osv(queries)
        self.assertEqual([entry["id"] for entry in result[("example.test/a", "v1.0.0")]],
                         ["GO-1", "GO-2"])
        self.assertEqual(result[("example.test/b", "v2.0.0")], [])
        self.assertEqual(request.call_args_list[1].args[1]["queries"][0]["page_token"], "more")

    def test_incomplete_or_repeated_pages_fail_closed(self) -> None:
        query = {"package": {"ecosystem": "Go", "name": "example.test/a"},
                 "version": "v1.0.0"}
        with mock.patch.object(policy, "request_json", return_value={"results": []}):
            with self.assertRaisesRegex(policy.PolicyError, "incomplete"):
                policy.query_osv([query])
        response = {"results": [{"vulns": [], "next_page_token": "same"}]}
        with mock.patch.object(policy, "request_json", return_value=response):
            with self.assertRaisesRegex(policy.PolicyError, "repeated"):
                policy.query_osv([query])

    def test_advisory_change_during_scan_is_indeterminate(self) -> None:
        matches = {("example.test/a", "v1.0.0"): [{"id": "GO-1", "modified": "2026-09-30T00:00:00Z"}]}
        with mock.patch.object(policy, "request_json", return_value={
            "id": "GO-1", "modified": "2026-09-30T00:00:01Z"}):
            with self.assertRaisesRegex(policy.PolicyError, "changed"):
                policy.advisory_records(matches)

    def test_osv_timestamp_precision_does_not_look_like_an_update(self) -> None:
        matches = {("example.test/a", "v1.0.0"): [{
            "id": "GO-1", "modified": "2026-07-10T05:44:31.101996Z"}]}
        with mock.patch.object(policy, "request_json", return_value={
            "id": "GO-1", "modified": "2026-07-10T05:44:31.101996029Z"}):
            self.assertEqual(policy.advisory_records(matches)["GO-1"]["id"], "GO-1")

    def test_go_command_is_read_only_and_ignores_parent_workspace(self) -> None:
        with mock.patch.object(policy.subprocess, "run", return_value=subprocess.CompletedProcess(
            [], 0, '{"Path":"example.test/root","Main":true}', "")) as run:
            policy.selected_modules(Path("module"))
        self.assertEqual(run.call_args.kwargs["env"]["GOWORK"], "off")
        self.assertEqual(run.call_args.kwargs["env"]["GOFLAGS"], "-mod=readonly")
        self.assertEqual(run.call_args.kwargs["env"]["GOTOOLCHAIN"], "local")


if __name__ == "__main__":
    unittest.main()
