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

"""Security repairs must change a declared floor and remove a root advisory."""

from __future__ import annotations

import importlib.util
from pathlib import Path
import sys
import unittest


DIRECTORY = Path(__file__).parent
sys.path.insert(0, str(DIRECTORY))
SOURCE = DIRECTORY / "validate_security_graph.py"
SPEC = importlib.util.spec_from_file_location("validate_security_graph", SOURCE)
assert SPEC and SPEC.loader
policy = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(policy)


def manifest(requirement: str, extra: str = "") -> str:
    return ("module example.com/library/v2\n\ngo 1.27.0\n\n"
            f"require example.com/dependency {requirement}\n{extra}")


class SecurityGraphPolicyTests(unittest.TestCase):
    def test_stable_floor_increase_is_allowed(self) -> None:
        policy.validate_root_floors(manifest("v1.9.9"), manifest("v1.10.0"))

    def test_downgrade_same_version_or_major_jump_is_rejected(self) -> None:
        for version in ("v1.9.8", "v1.9.9", "v2.0.0"):
            with self.subTest(version=version), self.assertRaisesRegex(ValueError, "raise"):
                policy.validate_root_floors(manifest("v1.9.9"), manifest(version))

    def test_pseudo_version_and_new_root_override_need_manual_review(self) -> None:
        with self.assertRaisesRegex(ValueError, "stable Go module release"):
            policy.validate_root_floors(manifest("v1.9.9"),
                                        manifest("v1.9.10-0.20260930000000-abcdef012345"))
        with self.assertRaisesRegex(ValueError, "replace/exclude"):
            policy.validate_root_floors(manifest("v1.9.9"),
                                        manifest("v1.10.0", "replace example.com/dependency => ../fork\n"))

    def test_no_new_findings_and_real_root_resolution_are_required(self) -> None:
        repaired = {"scope": ".", "module": "example.com/dependency", "advisory": "GO-1"}
        policy.validate_graph_delta({"introduced": [], "resolved": [repaired]})
        with self.assertRaisesRegex(ValueError, "did not resolve"):
            policy.validate_graph_delta({"introduced": [], "resolved": [
                {**repaired, "scope": ".github/tools"}]})
        with self.assertRaisesRegex(ValueError, "introduced"):
            policy.validate_graph_delta({"introduced": [repaired], "resolved": [repaired]})


if __name__ == "__main__":
    unittest.main()
