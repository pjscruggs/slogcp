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

import unittest
from run_matrix import plan


class MatrixTest(unittest.TestCase):
    def test_variants_are_paired_and_cases_are_complete(self):
        cases = plan(["baseline", "candidate"], 10, 42, 2500, 100000)
        self.assertEqual(len(cases), 480)
        self.assertEqual(len({case["trial_id"] for case in cases}), len(cases))
        for index in range(0, len(cases), 2):
            a, b = cases[index:index + 2]
            self.assertEqual({a["variant"], b["variant"]}, {"baseline", "candidate"})
            for field in ("repeat", "payload", "concurrency", "sink", "mode", "count"):
                self.assertEqual(a[field], b[field])
            self.assertEqual(a["count"], 2500 if a["sink"] == "stdout" else 100000)

    def test_seed_reproduces_schedule(self):
        a = plan(["baseline", "candidate"], 2, 42, 1, 1)
        self.assertEqual(a, plan(["baseline", "candidate"], 2, 42, 1, 1))
        self.assertNotEqual(a, plan(["baseline", "candidate"], 2, 43, 1, 1))

    def test_optional_grpc_mode_runs_only_real_cloud_delivery(self):
        cases = plan(["baseline"], 10, 42, 2500, 100000, include_slogcp_grpc=True)
        self.assertEqual(len(cases), 280)
        grpc_cases = [case for case in cases if case["mode"] == "slogcp-grpc"]
        self.assertEqual(len(grpc_cases), 40)
        self.assertTrue(all(case["sink"] == "stdout" for case in grpc_cases))


if __name__ == "__main__":
    unittest.main()
