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

import sweep_e2e_resources as sweep


BUILD = "a" * 8 + "-" + "b" * 4 + "-" + "c" * 4 + "-" + "d" * 4 + "-" + "e" * 12
RUN = "20260930T153909-25844"
SHORT = "r-20260930t153909-25844"


class SweepTests(unittest.TestCase):
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


if __name__ == "__main__":
    unittest.main()
