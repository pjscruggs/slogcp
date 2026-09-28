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

"""Structural regression tests for the checked-in Renovate policy.

These tests deliberately check the repository contract, not Renovate's complete
matcher implementation. The pinned validator and hosted artifact regeneration
remain required operational evidence.
"""

from __future__ import annotations

import json
from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[2]
CONFIG = json.loads((ROOT / "renovate.json").read_text(encoding="utf-8"))
RULES = CONFIG["packageRules"]


def rule(description: str) -> dict:
    matches = [item for item in RULES if item.get("description") == description]
    if len(matches) != 1:
        raise AssertionError(f"Expected one Renovate rule named {description!r}")
    return matches[0]


def structural_first_stable_match(
    *, package: str, current: str, new: str, update_type: str, path: str
) -> bool:
    """Model only the explicit conjunction asserted by the checked-in rule."""
    candidate = rule("Tidy optional modules when adopting their first stable release")
    return (
        package in candidate["matchPackageNames"]
        and re.match(r"^v?0\.", current) is not None
        and re.match(r"^v?1\.", new) is not None
        and update_type in candidate["matchUpdateTypes"]
        and path.startswith(".examples/")
        and path.endswith("/go.mod")
    )


class RenovatePolicyTests(unittest.TestCase):
    def test_benchmark_parent_replacement_is_discoverable_by_renovate(self) -> None:
        manifest = ROOT / ".benchmarks/go.mod"
        replacement = re.search(
            r"^replace github\.com/pjscruggs/slogcp/v2 => (\S+)$",
            manifest.read_text(encoding="utf-8"),
            re.MULTILINE,
        )
        self.assertIsNotNone(replacement)
        # Renovate's local-replacement discovery requires a slash after the dots.
        # Bare '..' is valid Go but omits this module from gomodTidyAll's graph.
        self.assertEqual(replacement[1], "../")
        self.assertEqual((manifest.parent / replacement[1]).resolve(), ROOT.resolve())

    def test_first_stable_exception_is_narrow_and_tidy_capable(self) -> None:
        candidate = rule(
            "Tidy optional modules when adopting their first stable release"
        )
        self.assertEqual(candidate["matchManagers"], ["gomod"])
        self.assertEqual(candidate["matchDatasources"], ["go"])
        self.assertEqual(candidate["matchFileNames"], [".examples/**/go.mod"])
        self.assertEqual(candidate["matchCurrentValue"], "/^v?0\\./")
        self.assertEqual(candidate["matchNewValue"], "/^v?1\\./")
        self.assertEqual(candidate["matchUpdateTypes"], ["major"])
        self.assertCountEqual(
            candidate["postUpdateOptions"],
            ["gomodTidyAll", "gomodUpdateImportPaths"],
        )
        self.assertTrue(candidate["automerge"])
        self.assertFalse(candidate["dependencyDashboardApproval"])

    def test_both_optional_example_modules_get_the_same_exception(self) -> None:
        for package, path in (
            (
                "github.com/pjscruggs/slogcp-grpc",
                ".examples/cloud-logging-grpc/go.mod",
            ),
            ("github.com/pjscruggs/slogcp-pubsub", ".examples/pubsub/go.mod"),
        ):
            with self.subTest(package=package):
                self.assertTrue(
                    structural_first_stable_match(
                        package=package,
                        current="v0.0.0-20260901000000-0123456789ab",
                        new="v1.0.0",
                        update_type="major",
                        path=path,
                    )
                )

    def test_true_major_and_unrelated_updates_do_not_get_the_exception(self) -> None:
        fixtures = (
            {
                "package": "github.com/pjscruggs/slogcp-grpc",
                "current": "v1.9.0",
                "new": "v2.0.0",
                "update_type": "major",
                "path": ".examples/grpc/go.mod",
            },
            {
                "package": "github.com/pjscruggs/slogcp/v2",
                "current": "v1.9.0",
                "new": "v2.0.0",
                "update_type": "major",
                "path": ".examples/grpc-adapter/go.mod",
            },
            {
                "package": "github.com/pjscruggs/slogcp-grpc",
                "current": "v0.9.0",
                "new": "v1.0.0",
                "update_type": "major",
                "path": "go.mod",
            },
            {
                "package": "github.com/pjscruggs/slogcp-grpc",
                "current": "v0.9.0",
                "new": "v1.0.0",
                "update_type": "minor",
                "path": ".examples/grpc/go.mod",
            },
        )
        for fixture in fixtures:
            with self.subTest(fixture=fixture):
                self.assertFalse(structural_first_stable_match(**fixture))

    def test_routine_major_review_does_not_claim_security_updates(self) -> None:
        candidate = rule(
            "Review routine example updates that require real major migrations"
        )
        self.assertIn(
            "$not(isVulnerabilityAlert = true or $exists(vulnerabilityFixVersion))",
            candidate["matchJsonata"],
        )
        self.assertFalse(candidate["automerge"])
        self.assertTrue(candidate["dependencyDashboardApproval"])
        security = rule(
            "Security floor updates must not wait for normal root Dependency Dashboard approval"
        )
        self.assertTrue(security["automerge"])
        self.assertFalse(security["dependencyDashboardApproval"])

    def test_exception_order_and_local_replace_exclusions_are_preserved(self) -> None:
        descriptions = [item.get("description") for item in RULES]
        review = descriptions.index(
            "Review routine example updates that require real major migrations"
        )
        stable = descriptions.index(
            "Tidy optional modules when adopting their first stable release"
        )
        local_example = descriptions.index(
            "Do not update the local unpublished slogcp requirement used by examples"
        )
        self.assertLess(review, stable)
        self.assertLess(stable, local_example)
        self.assertFalse(RULES[local_example]["enabled"])

    def test_benchmark_scopes_are_separate_and_not_yet_automergeable(self) -> None:
        module = rule("Validate external benchmark module dependencies separately")
        runtime = rule("Validate the benchmark Python container separately")
        self.assertEqual(module["matchFileNames"], [".benchmarks/go.mod"])
        self.assertEqual(runtime["matchFileNames"], [".benchmarks/Dockerfile"])
        self.assertNotEqual(module["groupName"], runtime["groupName"])
        for candidate in (module, runtime):
            self.assertEqual(candidate["additionalBranchPrefix"], "benchmarks-")
            self.assertIn("dependency-scope:benchmarks", candidate["addLabels"])
            self.assertFalse(candidate["automerge"])

    def test_only_locally_replaced_benchmark_self_requirement_is_ignored(self) -> None:
        candidate = rule("Preserve the locally replaced benchmark self-requirement")
        self.assertEqual(candidate["matchManagers"], ["gomod"])
        self.assertEqual(candidate["matchFileNames"], [".benchmarks/go.mod"])
        self.assertEqual(
            candidate["matchPackageNames"], ["github.com/pjscruggs/slogcp/v2"]
        )
        self.assertFalse(candidate["enabled"])
        descriptions = [item.get("description") for item in RULES]
        self.assertLess(
            descriptions.index("Validate external benchmark module dependencies separately"),
            descriptions.index("Preserve the locally replaced benchmark self-requirement"),
        )

    def test_benchmark_first_stable_exception_stays_in_benchmark_scope(self) -> None:
        candidate = rule("Tidy the benchmark gRPC module first stable update")
        self.assertEqual(candidate["matchFileNames"], [".benchmarks/go.mod"])
        self.assertEqual(
            candidate["matchPackageNames"],
            ["github.com/pjscruggs/slogcp-grpc"],
        )
        self.assertEqual(candidate["matchCurrentValue"], "/^v?0\\./")
        self.assertEqual(candidate["matchNewValue"], "/^v?1\\./")
        self.assertCountEqual(
            candidate["postUpdateOptions"],
            ["gomodTidyAll", "gomodUpdateImportPaths"],
        )
        self.assertEqual(candidate["additionalBranchPrefix"], "benchmarks-")
        self.assertFalse(candidate["automerge"])
        descriptions = [item.get("description") for item in RULES]
        self.assertLess(
            descriptions.index("Tidy the benchmark gRPC module first stable update"),
            descriptions.index("Preserve the locally replaced benchmark self-requirement"),
        )

    def test_affected_grpc_and_its_api_parent_are_held_in_examples(self) -> None:
        for description, package, allowed in (
            (
                "Exclude gRPC v1.84.0 affected by GO-2026-6443",
                "google.golang.org/grpc",
                "!/^v?1\\.84\\.0$/",
            ),
            (
                "Hold example API before its dependency on affected gRPC v1.84.0",
                "google.golang.org/api",
                "<0.299.0",
            ),
        ):
            with self.subTest(package=package):
                candidate = rule(description)
                self.assertEqual(candidate["matchManagers"], ["gomod"])
                self.assertEqual(candidate["matchDatasources"], ["go"])
                self.assertEqual(candidate["matchFileNames"], [".examples/**/go.mod"])
                self.assertEqual(candidate["matchPackageNames"], [package])
                self.assertEqual(candidate["allowedVersions"], allowed)
                self.assertNotIn("matchUpdateTypes", candidate)


if __name__ == "__main__":
    unittest.main()
