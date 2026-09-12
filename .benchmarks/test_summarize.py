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

"""Exercise paired statistics, malformed input rejection, and report boundaries."""

import copy
import importlib.util
from pathlib import Path
import unittest


SPEC = importlib.util.spec_from_file_location("summarize", Path(__file__).with_name("summarize.py"))
summary = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(summary)


def trial(repeat, *, variant="baseline", mode="slogcp", factor=1):
    scale = 100 + repeat * 10
    return dict(variant=variant, repeat=repeat, process_exit_code=0, errors=0,
                config=dict(mode=mode, payload="small", concurrency=1, sink="stdout", count=100, warmup=10),
                completed_elapsed_ns=scale * factor * 1000, drain_elapsed_ns=scale * factor,
                cpu_user_ns=scale * factor * 100, cpu_system_ns=0,
                mallocs=scale * factor, allocated_bytes=scale * factor * 10,
                request_latency_ns={"p95": scale * factor},
                runtime=dict(go_version="go1.27.1", goos="linux", goarch="amd64", gomaxprocs=2))


def suite(*, paired=False):
    variants = {"baseline": dict(sha256="a" * 64, path="/private/build/baseline")}
    if paired:
        variants["candidate"] = dict(sha256="b" * 64, path="/private/build/candidate")
    trials = [trial(repeat, variant=variant, mode=mode,
                    factor=0.5 if paired and variant == "candidate" and mode == "slogcp" else 1)
              for variant in variants for mode in ("slogcp", "google-stdout") for repeat in range(3)]
    return dict(schema_version=1, complete=True, repeats=3, variants=variants, trials=trials,
                host=dict(execution="private-generated-job", cpuinfo="model name\t: Test CPU\n",
                          cpu_max="200000 100000\n", memory_max="1073741824\n"),
                run_id="private-run", provenance={"bucket": "private-bucket"})


class SummarizeTests(unittest.TestCase):
    def test_pairing_follows_repeat_not_input_order(self):
        reference = [trial(0), trial(1), trial(2)]
        measured = [trial(2, factor=0.5), trial(0, factor=0.5), trial(1, factor=0.5)]
        actual = summary.compare_trials(reference, measured)
        self.assertEqual(actual["cpu_ns_per_request"]["ratio"], 0.5)
        self.assertEqual(actual["cpu_ns_per_request"]["ci95"], [0.5, 0.5])
        self.assertEqual(actual["completed_requests_per_second"]["ratio"], 2)

    def test_bootstrap_is_paired_and_deterministic(self):
        first = summary.paired_ratio([1, 10, 100, 1000], [2, 20, 200, 2000])
        self.assertEqual(first["ci95"], [2, 2])
        varying = summary.paired_ratio([1, 2, 4, 8], [1, 4, 5, 12])
        self.assertEqual(varying, summary.paired_ratio([1, 2, 4, 8], [1, 4, 5, 12]))
        self.assertLess(varying["ci95"][0], varying["ci95"][1])

    def test_zero_reference_is_explicit(self):
        self.assertIsNone(summary.paired_ratio([0, 0], [1, 2])["ratio"])
        unstable = summary.paired_ratio([0, 0, 1, 1], [1, 1, 1, 1])
        self.assertEqual(unstable["ratio"], 2)
        self.assertIsNone(unstable["ci95"])

    def test_failed_incomplete_duplicate_and_missing_trials_are_rejected(self):
        for mutation in (lambda value: value.update(complete=False),
                         lambda value: value["trials"][0].update(process_exit_code=1),
                         lambda value: value["trials"][0].update(errors=1),
                         lambda value: value["trials"].append(copy.deepcopy(value["trials"][0])),
                         lambda value: value["trials"].pop()):
            invalid = suite()
            mutation(invalid)
            with self.assertRaises(ValueError):
                summary.validate_suite(invalid)

    def test_pairing_rejects_different_workloads_and_missing_candidate_cases(self):
        left, right = [trial(0)], [trial(0)]
        right[0]["config"]["count"] += 1
        with self.assertRaisesRegex(ValueError, "count"):
            summary.compare_trials(left, right)
        right = [trial(0)]
        right[0]["runtime"]["go_version"] = "go1.26.7"
        with self.assertRaisesRegex(ValueError, "go_version"):
            summary.compare_trials(left, right)
        right = [trial(0)]
        right[0]["checksum"] = 1
        with self.assertRaisesRegex(ValueError, "checksum"):
            summary.compare_trials(left, right)
        paired = suite(paired=True)
        paired["trials"] = [item for item in paired["trials"]
                            if not (item["variant"] == "candidate" and item["config"]["mode"] == "slogcp")]
        with self.assertRaisesRegex(ValueError, "cases do not match"):
            summary.build_report(suite(), paired)

    def test_normalization_uses_completed_duration(self):
        value = trial(0)
        value["completed_requests_per_second"] = 999
        self.assertEqual(summary.trial_metrics(value)["completed_requests_per_second"], 1000000)
        value["allocated_bytes"] = float("nan")
        with self.assertRaises(ValueError):
            summary.trial_metrics(value)

    def test_report_preserves_controls_and_does_not_copy_private_provenance(self):
        report = summary.build_report(suite(), suite(paired=True))
        comparisons = {row["mode"]: row for row in report["candidate_over_baseline"]}
        self.assertEqual(comparisons["slogcp"]["metrics"]["cpu_ns_per_request"]["ratio"], 0.5)
        self.assertEqual(comparisons["google-stdout"]["metrics"]["cpu_ns_per_request"]["ratio"], 1)
        text = summary.markdown_report(report)
        self.assertIn("not HTTP network latency", text)
        self.assertIn("encoding diagnostics", text)
        self.assertIn("not a pooled percentile", text)
        self.assertNotIn("private-", repr(report))
        self.assertNotIn("/private/", repr(report))
        self.assertIn("Test CPU", text)

    def test_multiple_named_candidates_are_compared_separately(self):
        paired = suite(paired=True)
        paired["variants"]["nativev2"] = paired["variants"].pop("candidate")
        paired["variants"]["unsorted"] = dict(sha256="c" * 64)
        for item in paired["trials"]:
            if item["variant"] == "candidate":
                item["variant"] = "nativev2"
        paired["trials"].extend([trial(repeat, variant="unsorted", mode=mode, factor=0.25)
                                 for mode in ("slogcp", "google-stdout") for repeat in range(3)])
        report = summary.build_report(suite(), paired)
        comparisons = {(row["variant"], row["mode"]): row for row in report["candidate_over_baseline"]}
        self.assertEqual(comparisons[("nativev2", "slogcp")]["metrics"]["cpu_ns_per_request"]["ratio"], 0.5)
        self.assertEqual(comparisons[("unsorted", "slogcp")]["metrics"]["cpu_ns_per_request"]["ratio"], 0.25)
        self.assertIn("nativev2", summary.markdown_report(report))


if __name__ == "__main__":
    unittest.main()
