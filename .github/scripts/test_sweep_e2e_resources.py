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

"""The cloud sweeper acts only on a terminal build's exact owned resources."""

from __future__ import annotations

import unittest
from unittest import mock
import importlib.util
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import textwrap

SOURCE = (Path(__file__).resolve().parents[2] / ".e2e/cloudbuild/build-tools" /
          "sweep_e2e_resources.py")
SPEC = importlib.util.spec_from_file_location("sweep_e2e_resources", SOURCE)
assert SPEC and SPEC.loader
sweep = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(sweep)


BUILD = "a" * 8 + "-" + "b" * 4 + "-" + "c" * 4 + "-" + "d" * 4 + "-" + "e" * 12
RUN = "20260930T153909-25844"
SHORT = "r-20260930t153909-25844"


class SweepTests(unittest.TestCase):
    def test_cleanup_uses_sdk_interpreter_without_python_on_path(self):
        config = SOURCE.parents[1] / "cleanup.yaml"
        script = textwrap.dedent(config.read_text(encoding="utf-8").split(
            "      - |\n", 1)[1]).replace("$$", "$")
        values = {"PROJECT_ID": "cleanup-project", "_GCP_REGION": "test-region",
                  "_CALLER_SERVICE_ACCOUNT": "test-caller", "_ORIGINAL_BUILD_ID": BUILD,
                  "_E2E_RUN_ID": RUN, "_TRACE_PUBSUB_TOPIC": "test-topic",
                  "_TRACE_PUBSUB_SUBSCRIPTION": "test-subscription"}
        script = re.sub(r"\$\{([A-Z0-9_]+)\}", lambda match: values[match[1]], script)
        bash = (str(Path(os.environ["ProgramFiles"]) / "Git/bin/bash.exe")
                if os.name == "nt" else shutil.which("bash"))
        if not bash or not Path(bash).is_file():
            self.skipTest("Bash is required to exercise the Cloud Build entry point")
        with tempfile.TemporaryDirectory() as directory:
            interpreter = Path(directory) / "SDK Python with spaces"
            output = Path(directory) / "arguments"
            interpreter.write_text(
                '#!/bin/sh\nprintf "%s\\n" "$@" > "$CLEANUP_ARGUMENTS"\n',
                encoding="utf-8", newline="\n")
            interpreter.chmod(0o755)
            environment = {**os.environ, "SDK_PYTHON": interpreter.as_posix(),
                           "CLEANUP_ARGUMENTS": output.as_posix()}
            # Shadow any host Python so the original image assumption fails.
            bootstrap = '''
                python3() { return 127; }
                gcloud() {
                    [[ "$*" == 'info --project cleanup-project --format=value(basic.python_location)' ]] || return 1
                    printf '%s\\n' "$SDK_PYTHON"
                }
            '''
            result = subprocess.run([bash, "-euo", "pipefail", "-c",
                                     textwrap.dedent(bootstrap) + script],
                                    env=environment, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(output.read_text().splitlines(), [
                "cloudbuild/build-tools/sweep_e2e_resources.py",
                "--project", "cleanup-project", "--region", "test-region",
                "--build-id", BUILD, "--run-id", RUN, "--topic-base", "test-topic",
                "--subscription-base", "test-subscription"])

    def test_waiting_runner_sweeps_before_reporting_success(self):
        runner = Path(__file__).with_name("run_e2e_cloud_build.sh").read_text(
            encoding="utf-8")
        self.assertLess(runner.index('if [[ "$E2E_NO_WAIT" == "true" ]]'),
                        runner.index('cleanup_substitutions='))
        self.assertLess(runner.index('cleanup_substitutions='),
                        runner.rindex("emit_outputs"))
        self.assertIn('"$SOURCE_PATH/cloudbuild/cleanup.yaml"', runner)
        self.assertIn('BUILD_STATUS="FAILURE"', runner)

    def test_inventory_selects_only_exact_run_resources(self):
        services = [
            {"metadata": {"name": f"core-log-app-{SHORT}"}},
            {"metadata": {"name": f"scenario-{SHORT}-abc123",
                          "labels": {"e2e-scenario": "trace"}}},
            {"metadata": {"name": f"unrelated-{SHORT}-abc123"}},
            {"metadata": {"name": "core-log-app-other-run"}},
        ]
        jobs = [{"metadata": {"name": f"e2e-hjob-{SHORT}-1790783406"}},
                {"metadata": {"name": "e2e-hjob-other-1790783406"}}]
        topics = [{"name": f"projects/project/topics/topic-{SHORT}"}]
        subscriptions = [{"name": f"projects/project/subscriptions/sub-{SHORT}"}]
        with mock.patch.object(sweep, "gcloud_json", side_effect=[
                services, jobs, topics, subscriptions]):
            found = sweep.owned_resources("project", "us-central1", RUN,
                                          "topic", "sub")
        self.assertEqual(len(found["service"]), 2)
        self.assertEqual(len(found["job"]), 1)
        self.assertEqual(found["topic"], {f"topic-{SHORT}"})
        self.assertEqual(found["subscription"], {f"sub-{SHORT}"})

    def test_active_or_wrong_build_cannot_trigger_deletion(self):
        for status, run_id in (("WORKING", RUN), ("SUCCESS", "another-run")):
            with self.subTest(status=status, run_id=run_id), \
                 mock.patch.object(sweep, "gcloud_json", return_value={
                     "id": BUILD, "status": status,
                     "substitutions": {"_E2E_RUN_ID": run_id}}), \
                 mock.patch.object(sweep, "cloud_delete") as delete:
                with self.assertRaises(sweep.SweepError):
                    sweep.sweep("project", "us-central1", BUILD, RUN,
                                "topic", "sub")
                delete.assert_not_called()

    def test_terminal_build_requires_empty_followup_inventory(self):
        found = {"service": {"core-log-app-" + SHORT}, "job": set(),
                 "subscription": set(), "topic": set()}
        empty = {kind: set() for kind in found}
        with mock.patch.object(sweep, "gcloud_json", return_value={
                 "id": BUILD, "status": "FAILURE",
                 "substitutions": {"_E2E_RUN_ID": RUN}}), \
             mock.patch.object(sweep, "owned_resources", side_effect=[found, empty]), \
             mock.patch.object(sweep, "cloud_delete") as delete:
            removed = sweep.sweep("project", "us-central1", BUILD, RUN,
                                  "topic", "sub")
        self.assertEqual(removed["service"], 1)
        delete.assert_called_once()

    def test_unconfirmed_deletion_fails_closed(self):
        found = {"service": {"core-log-app-" + SHORT}, "job": set(),
                 "subscription": set(), "topic": set()}
        with mock.patch.object(sweep, "gcloud_json", return_value={
                 "id": BUILD, "status": "FAILURE",
                 "substitutions": {"_E2E_RUN_ID": RUN}}), \
             mock.patch.object(sweep, "owned_resources", return_value=found), \
             mock.patch.object(sweep, "cloud_delete"), \
             mock.patch.object(sweep.time, "sleep"):
            with self.assertRaisesRegex(sweep.SweepError, "remain"):
                sweep.sweep("project", "us-central1", BUILD, RUN,
                            "topic", "sub")

    def test_job_termination_refreshes_scenario_resources(self):
        job = "e2e-hjob-" + SHORT + "-1790783406"
        early = "core-log-app-" + SHORT
        late = "scenario-" + SHORT + "-abc123"
        found = {"service": {early}, "job": {job},
                 "subscription": set(), "topic": set()}
        refreshed = {**found, "service": {early, late}, "job": set()}
        empty = {kind: set() for kind in found}
        with mock.patch.object(sweep, "gcloud_json", return_value={
                 "id": BUILD, "status": "CANCELLED",
                 "substitutions": {"_E2E_RUN_ID": RUN}}), \
             mock.patch.object(sweep, "owned_resources", side_effect=[
                 found, refreshed, empty]), \
             mock.patch.object(sweep, "cloud_delete") as delete:
            removed = sweep.sweep("project", "us-central1", BUILD, RUN,
                                  "topic", "sub")
        self.assertEqual(removed["job"], 1)
        self.assertEqual(removed["service"], 2)
        self.assertEqual(delete.call_args_list, [
            mock.call("run", "jobs", "delete", job,
                      "--project=project", "--region=us-central1"),
            *[mock.call("run", "services", "delete", name,
                        "--platform=managed", "--project=project",
                        "--region=us-central1") for name in (early, late)],
        ])

    def test_followup_resources_are_retried_and_counted_once(self):
        early = "core-log-app-" + SHORT
        late = "scenario-" + SHORT + "-abc123"
        found = {"service": {early}, "job": set(),
                 "subscription": set(), "topic": set()}
        remaining = {**found, "service": {early, late}}
        empty = {kind: set() for kind in found}
        with mock.patch.object(sweep, "gcloud_json", return_value={
                 "id": BUILD, "status": "FAILURE",
                 "substitutions": {"_E2E_RUN_ID": RUN}}), \
             mock.patch.object(sweep, "owned_resources", side_effect=[
                 found, remaining, empty]), \
             mock.patch.object(sweep, "cloud_delete") as delete, \
             mock.patch.object(sweep.time, "sleep") as sleep:
            removed = sweep.sweep("project", "us-central1", BUILD, RUN,
                                  "topic", "sub")
        self.assertEqual(removed["service"], 2)
        self.assertEqual(delete.call_count, 3)
        sleep.assert_called_once_with(3)


if __name__ == "__main__":
    unittest.main()
