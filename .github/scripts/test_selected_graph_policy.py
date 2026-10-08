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
import copy
import hashlib
import json
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
    def _profile_evidence(self, scopes, inventory_hash, imports_by_scope=None):
        current, latest, floor = "go1.27.1", "go1.27.1", "go1.27.0"
        profiles = []
        imports_by_scope = imports_by_scope or {}
        for scope in sorted(scopes):
            for target in policy.PACKAGE_TARGETS:
                for compiler_axis in policy._compiler_axes(floor, current, latest, False):
                    imports = sorted(set(imports_by_scope.get(scope, []))) or ["example.test/root"]
                    identity = {"scope": scope, "manifest_sha256": "a" * 64,
                                "inventory_sha256": inventory_hash,
                                "source_sha256": "b" * 64,
                                "test_patterns": ["./..."], "tool_paths": [],
                                "imports": imports, **target, **compiler_axis}
                    digest = hashlib.sha256(json.dumps(
                        identity, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
                    profiles.append({**identity, "sha256": digest})
        return {"schema": 1, "scopes": sorted(scopes),
                "scope_go_floors": {scope: floor for scope in scopes},
                "scope_tool_roots": {scope: [] for scope in scopes},
                "tool_only_scopes": [], "current_compiler": current,
                "latest_stable_compiler": latest,
                "inventory_sha256": inventory_hash, "source_sha256": "b" * 64,
                "profiles": profiles}

    def _openpgp_candidate(self, imports_by_scope=None):
        finding = {"scope": ".", "module": policy.OPENPGP_MODULE,
                   "selected_path": policy.OPENPGP_MODULE, "version": "v0.57.0",
                   "advisory": policy.OPENPGP_ADVISORY,
                   "modified": policy.OPENPGP_MODIFIED}
        affected = [{"package": {"ecosystem": "Go", "name": policy.OPENPGP_MODULE,
                                   "purl": "pkg:golang/golang.org/x/crypto"},
                     "ranges": [{"type": "SEMVER", "events": [{"introduced": "0"}]}],
                     "ecosystem_specific": {"imports": [
                         {"path": item} for item in (
                             "golang.org/x/crypto/openpgp",
                             "golang.org/x/crypto/openpgp/packet",
                             "golang.org/x/crypto/openpgp/armor",
                             "golang.org/x/crypto/openpgp/clearsign",
                             "golang.org/x/crypto/openpgp/errors",
                             "golang.org/x/crypto/openpgp/elgamal",
                             "golang.org/x/crypto/openpgp/s2k")]},
                     "database_specific": {
                         "source": "https://vuln.go.dev/ID/GO-2026-5932.json"}}]
        raw_record = {
            "id": policy.OPENPGP_ADVISORY,
            "summary": "The golang.org/x/crypto/openpgp package is unmaintained, unsafe by design, and has known security issues",
            "details": "The golang.org/x/crypto/openpgp package is unsafe by design, has numerous known security issues, is not maintained, and should not be used.\n\nIf you are required to interoperate with OpenPGP systems and need a maintained package, consider github.com/ProtonMail/go-crypto/openpgp which is a maintained fork that aims to be a drop-in replacement for this package.",
            "modified": policy.OPENPGP_MODIFIED,
            "published": "2026-07-07T22:15:29Z",
            "related": ["CGA-485h-x96h-hqh7"],
            "database_specific": {"url": "https://pkg.go.dev/vuln/GO-2026-5932",
                                  "review_status": "REVIEWED"},
            "references": [{"type": "REPORT", "url": "https://go.dev/issue/44226"}],
            "affected": affected,
            "schema_version": "1.7.5",
        }
        raw_hash = hashlib.sha256(json.dumps(
            raw_record, sort_keys=True, separators=(",", ":")).encode()).hexdigest()
        candidate = {"schema": 2, "modules": ["."], "findings": [finding],
                     "selected": [{"scope": ".", "path": policy.OPENPGP_MODULE,
                                   "selected_path": policy.OPENPGP_MODULE,
                                   "selected_version": finding["version"]}],
                     "inventory_sha256": "c" * 64,
                     "advisory_fetched_at": policy.datetime.now(policy.timezone.utc).isoformat(),
                     "advisories": {policy.OPENPGP_ADVISORY: {
                         "modified": policy.OPENPGP_MODIFIED,
                         "sha256": raw_hash,
                         "aliases": [], "withdrawn": None, "affected": affected,
                         "raw_record": raw_record}}}
        candidate["package_profiles"] = self._profile_evidence(
            candidate["modules"], candidate["inventory_sha256"], imports_by_scope)
        candidate["applicability"] = policy._applicability(candidate)
        return candidate, finding

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

    def test_generated_local_replacement_uses_explicit_module_inventory(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            service = root / "service"
            library = service / "library"
            library.mkdir(parents=True)
            (service / "go.mod").write_text("module example.test/service\ngo 1.27\n")
            (library / "go.mod").write_text("module example.test/library\ngo 1.27\n")
            entries = [{"Path": "example.test/service", "Main": True},
                       {"Path": "example.test/library", "Version": "v1.0.0",
                        "Replace": {"Path": "./library"}}]
            with mock.patch.object(policy, "selected_modules", return_value=entries):
                selected, _ = policy.inventory(root, [service / "go.mod"],
                                               [service / "go.mod", library / "go.mod"])
            self.assertEqual(selected[0]["local_replacement"], "service/library")

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
            "id": "GO-1", "modified": "2026-07-10T05:44:31.101996029Z",
            "affected": [{"package": {"ecosystem": "Go", "name": "example.test/a"}}]}):
            records = policy.advisory_records(matches)
            self.assertEqual(records["GO-1"]["id"], "GO-1")
        root = Path.cwd()
        selected = [{"scope": ".", "path": "example.test/a",
                     "selected_path": "example.test/a", "selected_version": "v1.0.0"}]
        report = policy.report_for(root, [root / "go.mod"], selected, matches, records,
                                   policy.datetime.now(policy.timezone.utc).isoformat())
        self.assertEqual(report["findings"][0]["modified"],
                         report["advisories"]["GO-1"]["modified"])
        policy._applicability(report)

    def test_full_record_requires_affected_data(self) -> None:
        matches = {("example.test/a", "v1.0.0"): [{
            "id": "GO-1", "modified": "2026-09-30T00:00:00Z"}]}
        with mock.patch.object(policy, "request_json", return_value={
            "id": "GO-1", "modified": "2026-09-30T00:00:00Z"}):
            with self.assertRaisesRegex(policy.PolicyError, "affected package data"):
                policy.advisory_records(matches)

    def test_report_deduplicates_page_references_and_records_provenance(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            module = root / "go.mod"
            module.write_text("module example.test/root\ngo 1.27\n")
            selected = [{"scope": ".", "path": "example.test/a", "version": "v1.0.0",
                         "selected_path": "example.test/a", "selected_version": "v1.0.0"}]
            record = {"id": "GO-1", "modified": "2026-09-30T00:00:00Z",
                      "affected": [{"package": {"ecosystem": "Go", "name": "example.test/a"}}],
                      "aliases": ["GHSA-1"]}
            matches = {("example.test/a", "v1.0.0"): [
                {"id": "GO-1", "modified": record["modified"]},
                {"id": "GO-1", "modified": record["modified"]}]}
            with mock.patch.object(policy, "inventory", return_value=(selected, [])), \
                 mock.patch.object(policy, "query_osv", return_value=matches), \
                 mock.patch.object(policy, "advisory_records", return_value={"GO-1": record}), \
                 mock.patch.object(policy, "package_closure_evidence", return_value={}):
                report = policy.evaluate(root, [module])
            self.assertEqual(report["schema"], 2)
            self.assertEqual(len(report["findings"]), 1)
            self.assertEqual(report["advisories"]["GO-1"]["aliases"], ["GHSA-1"])
            self.assertEqual(len(report["advisories"]["GO-1"]["sha256"]), 64)
            self.assertIsNotNone(report["advisory_fetched_at"])

    def test_base_and_candidate_use_one_advisory_fetch(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            parent = Path(temporary)
            base, candidate = parent / "base", parent / "candidate"
            base.mkdir()
            candidate.mkdir()
            for root in (base, candidate):
                (root / "go.mod").write_text("module example.test/root\ngo 1.27\n")
            base_selected = [{"scope": ".", "path": "example.test/a", "version": "v1.0.0",
                              "selected_path": "example.test/a", "selected_version": "v1.0.0"}]
            candidate_selected = [{"scope": ".", "path": "example.test/a", "version": "v1.1.0",
                                   "selected_path": "example.test/a", "selected_version": "v1.1.0"}]
            base_query = {"package": {"ecosystem": "Go", "name": "example.test/a"},
                          "version": "v1.0.0"}
            candidate_query = {**base_query, "version": "v1.1.0"}
            matches = {("example.test/a", "v1.0.0"): [{
                "id": "GO-1", "modified": "2026-09-30T00:00:00Z"}],
                ("example.test/a", "v1.1.0"): [{
                    "id": "GO-1", "modified": "2026-09-30T00:00:00Z"}]}
            record = {"GO-1": {"id": "GO-1", "modified": "2026-09-30T00:00:00Z",
                               "affected": [{"package": {"ecosystem": "Go",
                                                         "name": "example.test/a"}}]}}
            with mock.patch.object(policy, "tracked_modules",
                                   side_effect=[[base / "go.mod"], [candidate / "go.mod"]]), \
                 mock.patch.object(policy, "inventory", side_effect=[
                     (base_selected, [base_query]), (candidate_selected, [candidate_query])]), \
                 mock.patch.object(policy, "query_osv", return_value=matches) as query, \
                 mock.patch.object(policy, "advisory_records", return_value=record), \
                 mock.patch.object(policy, "package_closure_evidence", return_value={}):
                report = policy.compare(base, candidate)
            query.assert_called_once()
            self.assertEqual(len(query.call_args.args[0]), 2)
            self.assertEqual(len(report["introduced"]), 1)
            self.assertEqual(report["introduced"][0]["version"], "v1.1.0")
            self.assertEqual(report["resolved"], [])
            self.assertEqual(report["persistent"], [])
            self.assertEqual(report["base"]["advisory_fetched_at"],
                             report["candidate"]["advisory_fetched_at"])

            with mock.patch.object(policy, "tracked_modules",
                                   side_effect=[[base / "go.mod"], [candidate / "go.mod"]]), \
                 mock.patch.object(policy, "inventory", side_effect=[
                     (base_selected, [base_query]), (base_selected, [base_query])]), \
                 mock.patch.object(policy, "query_osv", return_value=matches), \
                 mock.patch.object(policy, "advisory_records", return_value=record), \
                 mock.patch.object(policy, "package_closure_evidence", return_value={}):
                unchanged = policy.compare(base, candidate)
            self.assertEqual(unchanged["introduced"], [])
            self.assertEqual(unchanged["resolved"], [])
            self.assertEqual(len(unchanged["persistent"]), 1)

    def test_go_command_is_read_only_and_ignores_parent_workspace(self) -> None:
        with mock.patch.object(policy.subprocess, "run", return_value=subprocess.CompletedProcess(
            [], 0, '{"Path":"example.test/root","Main":true}', "")) as run:
            policy.selected_modules(Path("module"))
        self.assertEqual(run.call_args.kwargs["env"]["GOWORK"], "off")
        self.assertEqual(run.call_args.kwargs["env"]["GO111MODULE"], "on")
        self.assertEqual(run.call_args.kwargs["env"]["GOENV"], "off")
        self.assertEqual(run.call_args.kwargs["env"]["GOFLAGS"], "-mod=readonly")
        self.assertEqual(run.call_args.kwargs["env"]["GOTOOLCHAIN"], "local")

    def test_pinned_compiler_is_resolved_outside_candidate_and_verified(self):
        with tempfile.TemporaryDirectory() as temporary:
            goroot = Path(temporary) / "toolchain"
            bindir = goroot / "bin"
            bindir.mkdir(parents=True)
            executable = bindir / ("go.exe" if policy.os.name == "nt" else "go")
            executable.write_bytes(b"fixture executable path")
            responses = [
                subprocess.CompletedProcess([], 0, str(goroot), ""),
                subprocess.CompletedProcess([], 0, "go1.27.0\n", ""),
            ]
            with mock.patch.object(policy.subprocess, "run", side_effect=responses) as run:
                self.assertEqual(policy._resolve_go_binary("go1.27.0"), executable)
            self.assertEqual(run.call_args_list[0].kwargs["cwd"],
                             policy.tempfile.gettempdir())
            self.assertEqual(run.call_args_list[0].kwargs["env"]["GOTOOLCHAIN"], "go1.27.0")
            self.assertEqual(run.call_args_list[1].args[0],
                             [str(executable), "env", "GOVERSION"])
            self.assertEqual(run.call_args_list[1].kwargs["env"]["GOTOOLCHAIN"], "local")

    def test_package_closure_uses_supported_readonly_profiles_and_go_tool_roots(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            tools = root / ".github" / "tools"
            tools.mkdir(parents=True)
            manifest = tools / "go.mod"
            manifest.write_text("module example.test/tools\ngo 1.27\n")
            (tools / "go.sum").write_text("fixture checksum\n")
            runs = []

            def mocked_go(go_binary, module_dir, args, profile=None):
                runs.append((go_binary, module_dir, args, profile))
                if args == ["mod", "edit", "-json"]:
                    return json.dumps({"Go": "1.26.0",
                                       "Tool": [{"Path": "example.test/tool/cmd"}]})
                self.assertEqual(args[-1], "example.test/tool/cmd")
                self.assertNotIn("-test", args)
                return json.dumps({"ImportPath": "example.test/tool/cmd", "DepOnly": False})

            with mock.patch.object(policy, "_native_go_version", return_value="go1.27.1"), \
                 mock.patch.object(policy, "_latest_stable_go_version", return_value="go1.27.2"), \
                 mock.patch.object(policy, "_resolve_go_binary", return_value=Path("/toolchain/go")), \
                 mock.patch.object(policy, "_run_go", side_effect=mocked_go):
                evidence = policy.package_closure_evidence(root, [manifest], "c" * 64)
            self.assertEqual(len(evidence["profiles"]), 2 * len(policy.PACKAGE_TARGETS))
            self.assertEqual([entry["goarch"] for entry in evidence["profiles"]],
                             ["amd64", "amd64", "amd64", "amd64", "arm64", "arm64"])
            self.assertEqual({entry["compiler"] for entry in evidence["profiles"]},
                             {"go1.27.1", "go1.27.2"})
            self.assertEqual({tuple(entry["compiler_roles"]) for entry in evidence["profiles"]},
                             {("current",), ("latest_stable",)})
            self.assertEqual(evidence["scope_go_floors"], {".github/tools": "go1.26.0"})
            self.assertEqual(evidence["tool_only_scopes"], [".github/tools"])
            self.assertEqual(evidence["scope_tool_roots"],
                             {".github/tools": ["example.test/tool/cmd"]})
            self.assertTrue(all(entry["test_patterns"] == [] and
                                entry["tool_paths"] == ["example.test/tool/cmd"]
                                for entry in evidence["profiles"]))
            self.assertTrue(all(entry["imports"] == ["example.test/tool/cmd"]
                                for entry in evidence["profiles"]))
            self.assertEqual(len([run for run in runs if run[2][0] == "list"]), 6)

    def test_module_profiles_cover_declared_floor_current_and_distinct_latest(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            manifest = root / "go.mod"
            manifest.write_text("module example.test/root\ngo 1.27.0\n")
            (root / "main.go").write_text("package root\n")
            listed = []

            def mocked_go(go_binary, module_dir, args, profile=None):
                if args == ["mod", "edit", "-json"]:
                    return json.dumps({"Go": "1.27.0"})
                listed.append((go_binary, profile))
                return json.dumps({"ImportPath": "example.test/root", "DepOnly": False})

            with mock.patch.object(policy, "_native_go_version", return_value="go1.27.1"), \
                 mock.patch.object(policy, "_latest_stable_go_version", return_value="go1.27.2"), \
                 mock.patch.object(policy, "_resolve_go_binary", side_effect=lambda version: Path(version)), \
                 mock.patch.object(policy, "_run_go", side_effect=mocked_go):
                evidence = policy.package_closure_evidence(root, [manifest], "c" * 64)
            self.assertEqual(len(evidence["profiles"]), 3 * len(policy.PACKAGE_TARGETS))
            self.assertEqual({entry["compiler"] for entry in evidence["profiles"]},
                             {"go1.27.0", "go1.27.1", "go1.27.2"})
            self.assertEqual({tuple(entry["compiler_roles"]) for entry in evidence["profiles"]},
                             {("module_floor",), ("current",), ("latest_stable",)})
            self.assertTrue(all(entry["goamd64"] == "v1" for entry in
                                evidence["profiles"] if entry["goarch"] == "amd64"))
            self.assertTrue(all(entry["goarm64"] == "v8.0" for entry in
                                evidence["profiles"] if entry["goarch"] == "arm64"))

    def test_package_source_changes_during_inventory_fail_closed(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            manifest = root / "go.mod"
            manifest.write_text("module example.test/root\ngo 1.27\n")
            (root / "main.go").write_text("package root\n")
            calls = 0

            def mocked_go(go_binary, module_dir, args, profile=None):
                nonlocal calls
                calls += 1
                if args == ["mod", "edit", "-json"]:
                    return json.dumps({"Go": "1.27.0"})
                if args[0] == "list" and calls == 2:
                    (root / "main.go").write_text("package root\n// changed\n")
                return json.dumps({"ImportPath": "example.test/root", "DepOnly": False})

            with mock.patch.object(policy, "_native_go_version", return_value="go1.27.1"), \
                 mock.patch.object(policy, "_latest_stable_go_version", return_value="go1.27.1"), \
                 mock.patch.object(policy, "_resolve_go_binary", return_value=Path("/toolchain/go")), \
                 mock.patch.object(policy, "_run_go", side_effect=mocked_go):
                with self.assertRaisesRegex(policy.PolicyError, "changed during package inventory"):
                    policy.package_closure_evidence(root, [manifest], "c" * 64)

    def test_reviewed_openpgp_finding_is_admissible_only_with_complete_absence_proof(self):
        candidate, finding = self._openpgp_candidate()
        report = {"candidate": candidate, "introduced": [finding],
                  "persistent": [], "resolved": []}
        self.assertEqual(policy.blocking_findings(report), [])
        policy.require_clean_comparison(report)
        self.assertEqual(candidate["findings"], [finding])
        self.assertEqual(candidate["applicability"][0]["decision"],
                         "admissible_with_exception")

    def test_present_affected_import_blocks_and_raw_finding_remains_visible(self):
        candidate, finding = self._openpgp_candidate(
            {".": [policy.OPENPGP_IMPORTS[0]]})
        report = {"candidate": candidate, "introduced": [finding],
                  "persistent": [], "resolved": []}
        self.assertEqual(policy.blocking_findings(report), [finding])
        self.assertEqual(candidate["findings"], [finding])
        with self.assertRaisesRegex(policy.PolicyError, "blocking affected modules"):
            policy.require_clean_comparison(report)

    def test_missing_profile_coverage_and_tampered_profile_are_indeterminate(self):
        candidate, finding = self._openpgp_candidate()
        candidate["package_profiles"]["profiles"].pop()
        candidate["applicability"] = policy._applicability(candidate)
        report = {"candidate": candidate, "introduced": [finding], "persistent": []}
        self.assertEqual(policy.blocking_findings(report), [finding])
        candidate, finding = self._openpgp_candidate()
        candidate["package_profiles"]["profiles"][0]["imports"].append(
            policy.OPENPGP_IMPORTS[0])
        with self.assertRaisesRegex(policy.PolicyError, "applicability evidence"):
            policy.blocking_findings({"candidate": candidate, "introduced": [finding],
                                      "persistent": []})

    def test_changed_osv_ranges_and_other_advisories_never_receive_exception(self):
        candidate, finding = self._openpgp_candidate()
        candidate["advisories"][policy.OPENPGP_ADVISORY]["affected"][0]["ranges"] = []
        candidate["applicability"] = policy._applicability(candidate)
        report = {"candidate": candidate, "introduced": [finding], "persistent": []}
        self.assertEqual(policy.blocking_findings(report), [finding])
        candidate, finding = self._openpgp_candidate()
        finding["advisory"] = "GO-OTHER"
        candidate["findings"] = [finding]
        candidate["advisories"]["GO-OTHER"] = {"modified": finding["modified"]}
        candidate["applicability"] = policy._applicability(candidate)
        self.assertEqual(policy.blocking_findings(
            {"candidate": candidate, "introduced": [finding], "persistent": []}), [finding])

    def _generic_candidate(self, imports_by_scope=None):
        candidate, finding = self._openpgp_candidate(imports_by_scope)
        record = copy.deepcopy(candidate["advisories"][policy.OPENPGP_ADVISORY]["raw_record"])
        record["id"] = "GO-2026-9999"
        record["aliases"] = ["GHSA-example"]
        record["affected"][0]["ecosystem_specific"]["imports"] = [
            {"path": "golang.org/x/crypto/ssh", "symbols": ["SomeFunction"]}]
        finding["advisory"] = record["id"]
        candidate["advisories"] = {}
        self._retain_record(candidate, record)
        candidate["applicability"] = policy._applicability(candidate)
        return candidate, finding

    def _retain_record(self, candidate, record):
        content = json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
        candidate["advisories"][record["id"]] = {
            "raw_record": record, "sha256": hashlib.sha256(content).hexdigest(),
            "modified": record["modified"], "affected": record["affected"],
            "aliases": sorted(record.get("aliases", [])), "withdrawn": record.get("withdrawn")}

    def test_generic_package_absence_preserves_raw_finding_and_vex_justification(self):
        candidate, finding = self._generic_candidate()
        self.assertEqual(policy.blocking_findings(candidate), [])
        decision = candidate["applicability"][0]
        self.assertEqual(decision["status"], "not_affected")
        self.assertEqual(decision["justification"], "vulnerable_code_not_present")
        self.assertEqual(decision["affected_imports"], ["golang.org/x/crypto/ssh"])
        self.assertEqual(candidate["findings"], [finding])

    def test_generic_affected_package_blocks_without_call_graph_exemption(self):
        candidate, finding = self._generic_candidate({".": ["golang.org/x/crypto/ssh"]})
        self.assertEqual(policy.blocking_findings(candidate), [finding])

    def test_generic_incomplete_or_changed_advisory_blocks(self):
        for imports in (None, [], [{"path": "golang.org/x/crypto/..."}],
                        [{"path": "unrelated.test/package"}], [None]):
            with self.subTest(imports=imports):
                candidate, finding = self._generic_candidate()
                record = candidate["advisories"][finding["advisory"]]["raw_record"]
                record["affected"][0]["ecosystem_specific"]["imports"] = imports
                self._retain_record(candidate, record)
                candidate["applicability"] = policy._applicability(candidate)
                self.assertEqual(policy.blocking_findings(candidate), [finding])
        candidate, finding = self._generic_candidate()
        candidate["advisories"][finding["advisory"]]["raw_record"]["summary"] = "changed"
        candidate["applicability"] = policy._applicability(candidate)
        self.assertEqual(policy.blocking_findings(candidate), [finding])

    def test_alias_requires_verified_reciprocal_exact_version_go_finding(self):
        candidate, finding = self._generic_candidate()
        alias_finding = {**finding, "advisory": "GHSA-example"}
        alias_record = {"id": "GHSA-example", "modified": finding["modified"],
                        "aliases": [finding["advisory"]], "affected": []}
        self._retain_record(candidate, alias_record)
        candidate["findings"].append(alias_finding)
        candidate["applicability"] = policy._applicability(candidate)
        self.assertEqual(policy.blocking_findings(candidate), [])
        candidate["findings"] = [alias_finding]
        candidate["applicability"] = policy._applicability(candidate)
        self.assertEqual(policy.blocking_findings(candidate), [alias_finding])

    def test_generic_missing_profiles_and_stale_evidence_block(self):
        candidate, finding = self._generic_candidate()
        candidate["package_profiles"]["profiles"].pop()
        candidate["applicability"] = policy._applicability(candidate)
        self.assertEqual(policy.blocking_findings(candidate), [finding])
        candidate, _ = self._generic_candidate()
        candidate["advisory_fetched_at"] = "2000-01-01T00:00:00Z"
        with self.assertRaisesRegex(policy.PolicyError, "stale"):
            policy.blocking_findings(candidate)

    def test_generated_aggregate_requires_and_uses_generated_candidate_proof(self):
        tracked_candidate, finding = self._openpgp_candidate()
        generated_candidate, generated_finding = self._openpgp_candidate()
        generated_finding = {**generated_finding, "scope": "service"}
        generated_candidate["modules"] = ["service"]
        generated_candidate["findings"] = [generated_finding]
        generated_candidate["selected"][0]["scope"] = "service"
        generated_candidate["package_profiles"] = self._profile_evidence(
            ["service"], generated_candidate["inventory_sha256"])
        generated_candidate["applicability"] = policy._applicability(generated_candidate)
        tracked = {"candidate": tracked_candidate, "introduced": [finding],
                   "persistent": [], "resolved": []}
        generated = {"candidate": generated_candidate, "introduced": [generated_finding],
                     "persistent": [], "resolved": []}
        aggregate = {**tracked, "generated": generated,
                     "introduced": [finding, {**generated_finding,
                                              "scope": ".e2e/generated/service"}],
                     "persistent": []}
        self.assertEqual(policy.blocking_findings(aggregate), [])
        aggregate["introduced"] = [finding]
        with self.assertRaisesRegex(policy.PolicyError, "do not match"):
            policy.blocking_findings(aggregate)


if __name__ == "__main__":
    unittest.main()
