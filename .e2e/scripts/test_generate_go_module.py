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

from __future__ import annotations

import pathlib
import sys
import tempfile
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

from generate_go_module import discover_workspace_members, has_generated_module_metadata


class GenerateGoModuleTests(unittest.TestCase):
    def test_has_generated_module_metadata_only_uses_root_metadata_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            adapter_root = pathlib.Path(tmp) / "slogcp-grpc-adapter"
            nested_e2e = adapter_root / ".e2e"
            nested_e2e.mkdir(parents=True)
            (nested_e2e / "go.module.json").write_text("{}", encoding="utf-8")

            self.assertFalse(has_generated_module_metadata(adapter_root))
            self.assertTrue(has_generated_module_metadata(nested_e2e))

    def test_discover_workspace_members_ignores_nested_adapter_e2e_go_mod(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            module_dir = pathlib.Path(tmp) / "trace-target-app"
            adapter_dir = module_dir / "slogcp-grpc-adapter"
            nested_e2e = adapter_dir / ".e2e"
            nested_e2e.mkdir(parents=True)
            (adapter_dir / "go.mod").write_text(
                "module github.com/pjscruggs/slogcp-grpc-adapter\n",
                encoding="utf-8",
            )
            (nested_e2e / "go.mod").write_text(
                "module github.com/pjscruggs/slogcp-grpc-adapter-e2e\n",
                encoding="utf-8",
            )

            members = discover_workspace_members(
                module_dir=module_dir,
                pinned_modules=[
                    {
                        "module_path": "github.com/pjscruggs/slogcp-grpc-adapter",
                        "replace_path": "./slogcp-grpc-adapter",
                    }
                ],
            )

            self.assertEqual(members, [".", "./slogcp-grpc-adapter"])


if __name__ == "__main__":
    unittest.main()
