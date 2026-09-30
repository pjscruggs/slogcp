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

"""Focused checks for the independent consumer-floor probe."""

from __future__ import annotations

import importlib.util
from pathlib import Path
import tempfile
import unittest
from unittest import mock


SOURCE = Path(__file__).with_name("check_consumer_floor.py")
SPEC = importlib.util.spec_from_file_location("check_consumer_floor", SOURCE)
assert SPEC and SPEC.loader
probe = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(probe)


class ConsumerFloorTests(unittest.TestCase):
    def test_module_directive_preserves_full_import_path(self) -> None:
        self.assertEqual(probe.directive(
            "module github.com/pjscruggs/slogcp/v2\ngo 1.27.0\n", "module"),
            "github.com/pjscruggs/slogcp/v2")

    def test_consumer_uses_compatible_major_version(self) -> None:
        self.assertEqual(probe.consumer_version("example.com/module/v2"), "v2.0.0")
        self.assertEqual(probe.consumer_version("example.com/module"), "v0.0.0")

    def test_generated_consumer_has_only_candidate_requirement(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "go.mod").write_text(
                "module github.com/pjscruggs/slogcp/v2\n\ngo 1.27.0\n")
            seen = []

            def inspect(command: list[str], **kwargs: object) -> None:
                consumer = Path(kwargs["cwd"])
                manifest = (consumer / "go.mod").read_text()
                seen.append((command, manifest, kwargs["env"]))

            with mock.patch.object(probe.subprocess, "run", side_effect=inspect):
                probe.run(root)
            self.assertEqual(len(seen), 3)
            self.assertIn("require github.com/pjscruggs/slogcp/v2 v2.0.0", seen[0][1])
            self.assertIn(f"replace github.com/pjscruggs/slogcp/v2 => {root.as_posix()}",
                          seen[0][1])
            self.assertEqual(seen[0][2]["GOWORK"], "off")
            self.assertEqual(seen[0][2]["GOTOOLCHAIN"], "local")
            self.assertEqual(seen[1][2]["GOFLAGS"], "-mod=readonly")


if __name__ == "__main__":
    unittest.main()
