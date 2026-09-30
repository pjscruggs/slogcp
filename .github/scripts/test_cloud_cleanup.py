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

"""A failed cloud deletion must be confirmed absent before cleanup succeeds."""

from __future__ import annotations

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


CONFIG = Path(__file__).resolve().parents[2] / ".e2e/cloudbuild/cloudbuild.yaml"


def cleanup_function() -> str:
    source = CONFIG.read_text(encoding="utf-8")
    start = source.index("        delete_or_confirm_absent() {")
    end = source.index("        if [ -f /workspace/core_service_name.txt ]; then", start)
    return (source[start:end].replace("\r\n", "\n").replace("$$", "$")
            .replace("${_GCP_REGION}", "test-region")
            .replace("${PROJECT_ID}", "test-project"))


class CloudCleanupTests(unittest.TestCase):
    def run_case(self, deleted: bool, listed: str, list_ok: bool = True) -> int:
        with tempfile.TemporaryDirectory() as temporary:
            fake = Path(temporary) / "gcloud"
            fake.write_bytes(("#!/usr/bin/env bash\n"
                              "case \"$*\" in\n"
                              "  *delete*) exit \"$DELETE_RC\" ;;\n"
                              "  *list*) printf '%s\\n' \"$LISTED\"; exit \"$LIST_RC\" ;;\n"
                              "esac\nexit 2\n").encode())
            fake.chmod(0o755)
            script = ("#!/usr/bin/env bash\nset -euo pipefail\n" +
                      f"export DELETE_RC={'0' if deleted else '1'}\n"
                      f"export LISTED='{listed}'\n"
                      f"export LIST_RC={'0' if list_ok else '1'}\n" +
                      cleanup_function() +
                      "delete_or_confirm_absent service owned-service "
                      "gcloud run services delete owned-service --project test-project\n")
            scenario = Path(temporary) / "scenario.sh"
            scenario.write_bytes(script.encode())
            env = {**os.environ, "PATH": temporary + os.pathsep + os.environ["PATH"]}
            scenario_path = ("/mnt/" + scenario.drive[0].lower() +
                             scenario.as_posix()[2:] if os.name == "nt" else str(scenario))
            result = subprocess.run(["bash", scenario_path], env=env,
                                    capture_output=True, text=True)
            self.last_result = result
            return result.returncode

    def test_successful_delete(self):
        self.assertEqual(self.run_case(True, ""), 0, self.last_result.stderr)

    def test_failed_delete_with_absent_resource(self):
        self.assertEqual(self.run_case(False, "unrelated-service"), 0,
                         self.last_result.stderr)

    def test_failed_delete_with_remaining_resource(self):
        self.assertNotEqual(self.run_case(False, "owned-service"), 0)

    def test_failed_list_is_indeterminate(self):
        self.assertNotEqual(self.run_case(False, "", False), 0)


if __name__ == "__main__":
    unittest.main()
