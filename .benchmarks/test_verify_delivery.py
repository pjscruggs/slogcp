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

"""Delivery verification tests: missing, duplicate and paginated cloud logs."""

import io
import json
from pathlib import Path
import tempfile
import unittest
import urllib.error

import verify_delivery as delivery


def record(trial, sequence, *, nested=False, run_id="run-1"):
    payload = {"run_id": run_id, "trial_id": trial, "sequence": sequence}
    return {"jsonPayload": {"message": payload} if nested else payload, "severity": "INFO"}


class DeliveryTests(unittest.TestCase):
    def setUp(self):
        self.manifest = {
            "slog": {"count": 2, "mode": "slogcp", "sink": "stdout"},
            "google": {"count": 2, "mode": "google-stdout", "sink": "stdout"},
            "off": {"count": 2, "mode": "none", "sink": "stdout"},
            "discard": {"count": 2, "mode": "slogcp", "sink": "discard"},
        }

    def test_verifies_both_payload_shapes_and_excludes_warmup(self):
        entries = [record("slog", 0), record("slog", 1)]
        entries += [record("google", 0, nested=True), record("google", 1.0, nested=True)]
        entries += [record("slog-warmup", 0), {"jsonPayload": {"message": "instrumentation"}}]
        result = delivery.assess_entries(entries, self.manifest, "run-1")
        self.assertTrue(result["passed"])
        self.assertEqual(result["expected_records"], 4)
        self.assertEqual(result["received_records"], 4)
        self.assertEqual(result["ignored_warmup_records"], 1)
        self.assertEqual(result["excluded_trials"], ["discard", "off"])

    def test_count_alone_cannot_hide_duplicate_and_missing_sequence(self):
        entries = [record("slog", 0), record("slog", 0)]
        result = delivery.assess_entries(entries, self.manifest, "run-1")
        self.assertFalse(result["passed"])
        self.assertEqual(result["trials"]["slog"]["missing"], [1])
        self.assertEqual(result["trials"]["slog"]["duplicates"], {"0": 2})

    def test_complete_sequences_with_wrong_or_missing_severity_fail(self):
        entries = [record("slog", 0), record("slog", 1)]
        entries += [record("google", 0, nested=True), record("google", 1, nested=True)]
        entries[0]["severity"] = "DEFAULT"
        del entries[2]["severity"]
        result = delivery.assess_entries(entries, self.manifest, "run-1")
        self.assertFalse(result["passed"])
        self.assertEqual(result["received_records"], result["expected_records"])
        self.assertEqual(result["invalid_severity_records"], {"slog": 1, "google": 1})
        self.assertEqual(result["trials"]["slog"]["missing"], [])
        self.assertFalse(result["trials"]["slog"]["complete"])
        self.assertFalse(result["trials"]["google"]["complete"])

    def test_rejects_invalid_and_out_of_range_and_unknown_records(self):
        entries = [record("slog", -1), record("slog", 2), record("google", True)]
        entries += [record("google", "1"), record("unknown", 0), record("off", 0)]
        result = delivery.assess_entries(entries, self.manifest, "run-1")
        self.assertFalse(result["passed"])
        self.assertEqual(result["trials"]["slog"]["out_of_range"], {"-1": 1, "2": 1})
        self.assertEqual(result["malformed_records"], {"google": 2})
        self.assertEqual(result["unknown_trials"], {"unknown": 1, "off": 1})

    def test_snapshots_do_not_accumulate_duplicates_between_polls(self):
        manifest = {"slog": self.manifest["slog"]}
        first = delivery.assess_entries([record("slog", 0)], manifest, "run-1")
        second = delivery.assess_entries([record("slog", 0), record("slog", 1)], manifest, "run-1")
        self.assertFalse(first["passed"])
        self.assertTrue(second["passed"])

    def test_filter_escapes_run_id_for_both_payload_paths(self):
        query = delivery.logging_filter('quoted"run')
        self.assertIn('jsonPayload.run_id="quoted\\"run"', query)
        self.assertIn('jsonPayload.message.run_id="quoted\\"run"', query)

    def test_manifest_accepts_suite_and_rejects_duplicate_trials(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "suite.json"
            path.write_text(json.dumps({"trials": [{"trial_id": "one", "count": 3}]}), encoding="utf-8")
            self.assertEqual(delivery.read_manifest(Path(directory))["one"]["count"], 3)
            path.write_text(json.dumps({"trials": [{"trial_id": "one", "count": 3}] * 2}), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "duplicate"):
                delivery.read_manifest(path)

    def test_manifest_reads_nested_application_result_configs(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "suite.json"
            path.write_text(json.dumps({"trials": [{
                "variant": "baseline", "repeat": 0,
                "config": {"trial_id": "one", "count": 3, "mode": "google-api", "sink": "stdout"},
            }]}), encoding="utf-8")
            expected = {"one": {"count": 3, "mode": "google-api", "sink": "stdout"}}
            self.assertEqual(delivery.read_manifest(path), expected)
            path.unlink()
            (Path(directory) / "one.json").write_text(json.dumps({
                "config": {"trial_id": "one", "count": 3, "mode": "google-api", "sink": "stdout"},
            }), encoding="utf-8")
            self.assertEqual(delivery.read_manifest(Path(directory)), expected)


class ReaderTests(unittest.TestCase):
    def setUp(self):
        self.now = 0.0
        self.requests = []
        self.responses = []

    def sleep(self, seconds):
        self.now += seconds

    def open(self, request, timeout):
        self.requests.append((self.now, json.loads(request.data), timeout))
        response = self.responses.pop(0)
        if isinstance(response, Exception):
            raise response
        return io.BytesIO(json.dumps(response).encode("utf-8"))

    def reader(self, deadline=60):
        return delivery.LoggingReader(
            "example-project", "secret-never-printed", deadline,
            opener=self.open, clock=lambda: self.now, sleep=self.sleep,
        )

    def test_paginates_empty_pages_and_limits_all_requests(self):
        self.responses = [
            {"entries": [record("one", 0)], "nextPageToken": "second"},
            {"nextPageToken": "third"},
            {"entries": [record("one", 1)]},
            {},
        ]
        reader = self.reader()
        self.assertEqual(len(list(reader.snapshot("run-1"))), 2)
        self.assertEqual(list(reader.snapshot("run-1")), [])
        self.assertEqual(reader.requests, 4)
        self.assertEqual(self.requests[1][1]["pageToken"], "second")
        self.assertEqual(self.requests[2][1]["pageToken"], "third")
        for earlier, later in zip(self.requests, self.requests[1:]):
            self.assertGreaterEqual(later[0] - earlier[0], 1.4 - 1e-9)

    def test_retries_transient_error_without_skipping_page(self):
        self.responses = [
            urllib.error.HTTPError(delivery.API_URL, 429, "rate limit", {}, None),
            {"entries": [record("one", 0)]},
        ]
        reader = self.reader()
        self.assertEqual(len(list(reader.snapshot("run-1"))), 1)
        self.assertEqual(self.requests[0][1], self.requests[1][1])
        self.assertGreaterEqual(self.requests[1][0], 5)

    def test_rejects_repeated_page_token(self):
        self.responses = [{"nextPageToken": "repeat"}] * 2
        with self.assertRaisesRegex(ValueError, "repeated"):
            list(self.reader().snapshot("run-1"))

    def test_deadline_during_pagination_cannot_report_complete(self):
        self.responses = [{"entries": [record("one", 0)], "nextPageToken": "more"}]
        with self.assertRaises(TimeoutError):
            list(self.reader(deadline=1).snapshot("run-1"))


if __name__ == "__main__":
    unittest.main()
