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

"""The generated audit must stage every declared cloud consumer."""

from __future__ import annotations

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock


SOURCE = Path(__file__).with_name("audit_generated_graphs.py")
SPEC = importlib.util.spec_from_file_location("audit_generated_graphs", SOURCE)
assert SPEC and SPEC.loader
audit = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(audit)


class GeneratedInventoryTests(unittest.TestCase):
    def test_manifest_inventory_rejects_an_unregistered_generator_input(self) -> None:
        expected = [f".e2e/services/{name}/go.module.json"
                    for name in (*audit.SERVICES, audit.TRACEPROTO)]
        payload = b"\0".join(path.encode() for path in expected) + b"\0"
        with mock.patch.object(audit.subprocess, "run", return_value=mock.Mock(
            stdout=payload)):
            self.assertEqual(sorted(audit.source_manifests(Path("."))), sorted(expected))
        with mock.patch.object(audit.subprocess, "run", return_value=mock.Mock(
            stdout=payload + b".e2e/services/new/go.module.json\0")):
            with self.assertRaisesRegex(audit.graph.PolicyError, "coverage changed"):
                audit.source_manifests(Path("."))

    def test_local_library_and_traceproto_sources_are_staged(self) -> None:
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary) / "root"
            stage = Path(temporary) / "stage"
            services = root / ".e2e/services"
            traceproto = services / audit.TRACEPROTO
            traceproto.mkdir(parents=True)
            (traceproto / "go.module.json").write_text("{}")
            (traceproto / "source.go").write_text("package traceproto\n")
            (root / "go.mod").write_text("module github.com/pjscruggs/slogcp/v2\n")
            for name in audit.SERVICES:
                directory = services / name
                directory.mkdir(parents=True)
                (directory / "go.module.json").write_text(json.dumps({
                    "pinned_modules": [
                        {"module_path": audit.TRACEPROTO_MODULE,
                         "replace_path": "./traceproto"},
                        {"module_path": audit.SLOGCP, "replace_path": "./slogcp"},
                    ]}))
            staged = audit.stage_sources(root, stage)
            self.assertEqual(len(staged), len(audit.SERVICES))
            for service in staged:
                self.assertTrue((service / "traceproto/source.go").is_file())
                self.assertTrue((service / "slogcp/go.mod").is_file())


if __name__ == "__main__":
    unittest.main()
