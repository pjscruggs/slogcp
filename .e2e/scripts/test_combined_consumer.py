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

"""Native offline module-selection tests for immutable combined consumers."""

import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

import generate_go_module as generator
import test_generate_go_module as fixtures


class CombinedConsumerTests(unittest.TestCase):
    def fixture(self, root):
        proxy = root / "proxy"
        for version in ("v1.0.0", "v1.1.0"):
            fixtures.GenerateGoModuleTests.write_proxy_module(
                proxy,
                "example.org/shared",
                version,
                go_mod="module example.org/shared\ngo 1.26.0\n",
                files={"shared.go": f'package shared\nconst Version = "{version}"\n'},
            )
        sources = []
        for name, module, version in (
            ("root", generator.SLOGCP_MODULE_PATH, "v1.0.0"),
            ("adapter", generator.ADAPTER_MODULE_PATH, "v1.1.0"),
        ):
            source = root / name
            source.mkdir()
            (source / "go.mod").write_bytes(
                f"module {module}\r\ngo 1.26.0\r\nrequire example.org/shared {version}\r\n".encode()
            )
            (source / "go.sum").write_bytes(b"")
            (source / "library.go").write_text(
                f'package {name}\nimport "example.org/shared"\nconst Version = shared.Version\n'
            )
            (source / "embedded.txt").write_bytes(b"retain all build inputs\r\n")
            sources.append(source)
        consumer = root / "consumer"
        consumer.mkdir()
        (consumer / "go.module.json").write_text(
            json.dumps(
                {
                    "module_path": generator.LOCAL_E2E_PREFIX + "fixture",
                    "seed_requirements": {"example.org/shared": "v1.0.0"},
                    "pinned_modules": [
                        {
                            "module_path": generator.SLOGCP_MODULE_PATH,
                            "version_source": "slogcp_reference",
                            "replace_path": "./slogcp",
                        },
                        {
                            "module_path": generator.ADAPTER_MODULE_PATH,
                            "version": "v1.0.0",
                            "replace_path": "./slogcp-grpc-adapter",
                        },
                    ],
                }
            )
        )
        (consumer / "consumer_test.go").write_text(
            'package fixture\nimport ("testing"; '
            f'root "{generator.SLOGCP_MODULE_PATH}"; '
            f'adapter "{generator.ADAPTER_MODULE_PATH}")\n'
            'func TestCombined(t *testing.T) { if root.Version != "v1.1.0" || '
            'adapter.Version != "v1.1.0" { t.Fatal("wrong native MVS selection") } }\n'
        )
        env = {
            **os.environ,
            "GOTOOLCHAIN": "local",
            "GOWORK": "off",
            "GOPROXY": proxy.as_uri(),
            "GOSUMDB": "off",
            "GONOSUMDB": "*",
            "GOPRIVATE": "",
            "GONOPROXY": "",
            "GOFLAGS": "",
            "GOMODCACHE": str(root / "modcache"),
            "CGO_ENABLED": "0",
        }
        version = (
            subprocess.check_output(
                generator.go_command("env", "GOVERSION"), env=env, text=True
            )
            .strip()
            .removeprefix("go")
        )
        return {
            "module_dir": consumer,
            "slogcp_dir": sources[0],
            "adapter_dir": sources[1],
            "go_version": version,
            "slogcp_reference": "v1.0.0",
            "env": env,
        }

    def test_native_mvs_preserves_both_library_floors_and_all_bytes(self):
        with tempfile.TemporaryDirectory() as temporary:
            args = self.fixture(Path(temporary))
            result = generator.generate_combined_consumer(**args)
            selected = {
                item["Path"]: item.get("Version") for item in result["module_graph"]
            }
            self.assertEqual(selected["example.org/shared"], "v1.1.0")
            for source in (args["slogcp_dir"], args["adapter_dir"]):
                self.assertIn(b"\r\n", (source / "go.mod").read_bytes())
                self.assertEqual((source / "go.sum").read_bytes(), b"")
            completed = generator.run_command(
                generator.go_command("test", "-mod=readonly", "./..."),
                cwd=args["module_dir"],
                env=args["env"],
            )
            self.assertEqual(
                completed.returncode, 0, completed.stdout + completed.stderr
            )
            repeated = generator.generate_combined_consumer(**args)
            self.assertEqual(
                result["source_fingerprints"], repeated["source_fingerprints"]
            )
            workspace_run = generator.run_command(
                generator.go_command("test", "./..."),
                cwd=args["module_dir"],
                env={**args["env"], "GOWORK": str(args["module_dir"] / "go.work")},
            )
            self.assertEqual(
                workspace_run.returncode, 0, workspace_run.stdout + workspace_run.stderr
            )

    def test_unused_adapter_is_not_reported_as_candidate_validation(self):
        with tempfile.TemporaryDirectory() as temporary:
            args = self.fixture(Path(temporary))
            (args["module_dir"] / "consumer_test.go").write_text(
                'package fixture\nimport _ "' + generator.SLOGCP_MODULE_PATH + '"\n'
            )
            with self.assertRaisesRegex(
                ValueError, "did not select the supplied candidate"
            ):
                generator.generate_combined_consumer(**args)

    def test_staged_source_mismatch_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            args = self.fixture(Path(temporary))
            generator.generate_combined_consumer(**args)
            (args["module_dir"] / "slogcp-grpc-adapter/embedded.txt").write_text(
                "wrong candidate"
            )
            with self.assertRaisesRegex(ValueError, "staged candidate differs"):
                generator.generate_combined_consumer(**args)

    def test_mutation_during_native_tidy_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            args = self.fixture(Path(temporary))
            tidy = generator.tidy_module

            def mutate(directory, env):
                result = tidy(directory, env)
                (args["module_dir"] / "slogcp-grpc-adapter/go.sum").write_text(
                    "changed"
                )
                return result

            with mock.patch.object(generator, "tidy_module", side_effect=mutate):
                with self.assertRaisesRegex(RuntimeError, "candidate source changed"):
                    generator.generate_combined_consumer(**args)

    def test_output_inside_candidate_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            args = self.fixture(Path(temporary))
            args["module_dir"] = args["slogcp_dir"] / "generated"
            args["module_dir"].mkdir()
            with self.assertRaisesRegex(ValueError, "separate trees"):
                generator.generate_combined_consumer(**args)

    def test_cli_requires_explicit_adapter_and_reports_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            args = self.fixture(Path(temporary))
            report_path = Path(temporary) / "report.json"
            completed = subprocess.run(
                [
                    sys.executable,
                    generator.__file__,
                    "--graph-profile",
                    "combined-candidate",
                    "--module-dir",
                    str(args["module_dir"]),
                    "--slogcp-dir",
                    str(args["slogcp_dir"]),
                    "--slogcp-reference",
                    args["slogcp_reference"],
                    "--go-version",
                    args["go_version"],
                    "--emit-dependency-report",
                    str(report_path),
                ],
                env=args["env"],
                capture_output=True,
                text=True,
            )
            self.assertNotEqual(completed.returncode, 0)
            self.assertIn("requires --adapter-dir", completed.stderr)
            self.assertEqual(json.loads(report_path.read_text())["status"], "failure")

    def test_wrong_compiler_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            args = self.fixture(Path(temporary))
            args["go_version"] = "0.0.0"
            with self.assertRaisesRegex(ValueError, "compiler does not match"):
                generator.generate_combined_consumer(**args)


if __name__ == "__main__":
    unittest.main()
