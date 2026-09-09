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

"""Run the actual Cloud Build routing shell without making cloud requests."""

import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import textwrap
import unittest


ROOT = Path(__file__).resolve().parents[2]


class CloudRoutingTests(unittest.TestCase):
    def route(self, adapter, *, fail_generator=False, wrong_identity=False):
        source = (ROOT / ".e2e/cloudbuild/cloudbuild.yaml").read_text(encoding="utf-8")
        before = source.split("    id: 'Generate module files'", 1)[0]
        step = before.rsplit("\n  - name:", 1)[1]
        body = textwrap.dedent(step.split("      - |\n", 1)[1]).replace("$$", "$")
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            for name, value in {
                "go_version": "1.27.1",
                "slogcp_reference": "v1.2.7",
                "stream_tag_defaulted": "fixture",
                "e2e_run_id": "fixture",
            }.items():
                (directory / (name + ".txt")).write_text(value)
            sha = "a" * 40
            adapter_sha = "b" * 40 if adapter else ""
            (directory / "source-identities.json").write_text(
                json.dumps(
                    {
                        "root_commit": "wrong" if wrong_identity else sha,
                        "adapter_commit": adapter_sha,
                        "infrastructure_commit": "c" * 40,
                    }
                )
            )
            body = body.replace("/workspace", directory.as_posix())
            shell = (
                "C:/Program Files/Git/bin/bash.exe"
                if os.name == "nt"
                else shutil.which("bash")
            )
            script = 'python3() { "$TEST_PYTHON" "$@"; }\n'
            script += "gsutil() { return 0; }\n"
            script += 'bash() { printf "%s\\n" "$*" >> "$ROUTING_LOG"; '
            script += 'if [[ "$FAIL_GENERATOR" == true ]]; then return 17; fi; }\n'
            script += body
            log = directory / "routing.log"
            result = subprocess.run(
                [shell, "-c", script],
                cwd=directory,
                env={
                    **os.environ,
                    "TEST_PYTHON": sys.executable,
                    "ROUTING_LOG": log.as_posix(),
                    "FAIL_GENERATOR": str(fail_generator).lower(),
                    "_PR_SHA": sha,
                    "_SHORT_SHA": sha[:7],
                    "_ADAPTER_SHA": adapter_sha,
                    "_E2E_SOURCE_MODE": "local",
                    "_E2E_DEPENDENCY_MODE": "floor",
                    "_GCS_BUCKET_NAME": "fixture",
                },
                capture_output=True,
                text=True,
            )
            calls = log.read_text().splitlines() if log.exists() else []
            return result, calls

    def test_root_only_keeps_all_five_consumers_in_existing_mode(self):
        result, calls = self.route(False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0].count("--module-dir"), 5)
        self.assertNotIn("combined-candidate", calls[0])

    def test_adapter_separates_three_root_and_two_combined_consumers(self):
        result, calls = self.route(True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(len(calls), 2)
        self.assertEqual(calls[0].count("--module-dir"), 3)
        self.assertNotIn("/build-context/trace-grpc", calls[0])
        self.assertEqual(calls[1].count("--module-dir"), 2)
        self.assertIn("--graph-profile combined-candidate", calls[1])
        self.assertIn("--adapter-dir", calls[1])
        self.assertIn("combined-dependency-report.json", calls[1])

    def test_generator_failure_stops_the_cloud_step(self):
        result, calls = self.route(True, fail_generator=True)
        self.assertEqual(result.returncode, 17)
        self.assertEqual(len(calls), 1)

    def test_wrong_source_identity_stops_before_generation(self):
        result, calls = self.route(True, wrong_identity=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, [])
        self.assertIn("do not match requested commits", result.stderr)


if __name__ == "__main__":
    unittest.main()
