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

"""Exercise benchmark provenance controls without creating cloud resources."""

from argparse import Namespace
from collections import Counter
import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

import manage
import run_matrix


class FreezeTests(unittest.TestCase):
    def test_dirty_source_is_rejected_before_archiving_or_building(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory) / "new-archive"
            with patch.object(manage, "execute", return_value=" M main.go") as execute:
                with self.assertRaisesRegex(RuntimeError, "Commit"):
                    manage.freeze(Namespace(archive=archive, name="baseline"))
            self.assertFalse(archive.exists())
            self.assertEqual(execute.call_count, 1)

    def test_archived_binary_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory)
            binary = archive / "baseline"
            binary.write_bytes(b"immutable baseline")
            with patch.object(manage, "execute", return_value="") as execute:
                with self.assertRaises(FileExistsError):
                    manage.freeze(Namespace(archive=archive, name="baseline"))
            self.assertEqual(binary.read_bytes(), b"immutable baseline")
            self.assertEqual(execute.call_count, 1)


class ImageTests(unittest.TestCase):
    def test_frozen_image_keeps_variant_names_and_uses_archived_runner(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory)
            frozen = archive / "frozen"
            frozen.mkdir()
            binary = frozen / "baseline"
            binary.write_bytes(b"frozen executable")
            (frozen / "run_matrix.py").write_bytes(b"frozen runner")
            (frozen / "Dockerfile").write_bytes(b"frozen Dockerfile")
            (frozen / "provenance.json").write_text(json.dumps({
                "binary_sha256": manage.sha256(binary), "harness_sha256": {"main.go": "hash"},
                "runner_sha256": manage.sha256(frozen / "run_matrix.py"),
                "dockerfile_sha256": manage.sha256(frozen / "Dockerfile"),
            }), encoding="utf-8")
            args = Namespace(
                archive=archive / "image", binary=[f"baseline={binary}"],
                project="example-project", region="example-region", bucket="example-bucket",
                image="example.invalid/image:baseline", build_sa="example-build-sa",
            )
            with patch.object(manage, "execute", return_value='{"id":"example-build"}') as execute:
                with contextlib.redirect_stdout(io.StringIO()):
                    manage.image(args)
            self.assertEqual((args.archive / "image-context/bin/baseline").read_bytes(), b"frozen executable")
            self.assertEqual((args.archive / "image-context/run_matrix.py").read_bytes(), b"frozen runner")
            provenance = json.loads((args.archive / "image-context/provenance.json").read_text())
            self.assertEqual(set(provenance), {"baseline"})
            execute.assert_called_once()

    def test_modified_binary_is_rejected_before_cloud_submission(self):
        with tempfile.TemporaryDirectory() as directory:
            archive = Path(directory)
            frozen = archive / "frozen"
            frozen.mkdir()
            binary = frozen / "baseline"
            binary.write_bytes(b"changed since freeze")
            (frozen / "provenance.json").write_text(json.dumps({"binary_sha256": "wrong"}), encoding="utf-8")
            args = Namespace(archive=archive, binary=[f"baseline={binary}"])
            with patch.object(manage, "execute") as execute:
                with self.assertRaisesRegex(ValueError, "hash mismatch"):
                    manage.image(args)
            execute.assert_not_called()

    def test_variant_cannot_escape_image_binary_directory(self):
        with tempfile.TemporaryDirectory() as directory:
            args = Namespace(archive=Path(directory), binary=["../outside=unused"])
            with patch.object(manage, "execute") as execute:
                with self.assertRaisesRegex(ValueError, "variant"):
                    manage.image(args)
            execute.assert_not_called()


class PlanTests(unittest.TestCase):
    def test_same_seed_reproduces_trial_order(self):
        first = run_matrix.plan(["baseline", "candidate"], 10, 270127, 2500, 100000)
        second = run_matrix.plan(["baseline", "candidate"], 10, 270127, 2500, 100000)
        self.assertEqual(first, second)
        self.assertNotEqual(first, run_matrix.plan(["baseline", "candidate"], 10, 270128, 2500, 100000))

    def test_paired_variants_are_adjacent_and_execute_identical_work(self):
        cases = run_matrix.plan(["baseline", "candidate"], 10, 270127, 2500, 100000)
        self.assertEqual(len(cases), 480)
        trial_ids = {case["trial_id"] for case in cases}
        self.assertEqual(len(trial_ids), len(cases))
        first_variant_counts = Counter()
        for offset in range(0, len(cases), 2):
            first, second = cases[offset:offset + 2]
            self.assertEqual({first["variant"], second["variant"]}, {"baseline", "candidate"})
            for key in ("payload", "concurrency", "sink", "mode", "count", "repeat"):
                self.assertEqual(first[key], second[key])
            first_variant_counts[first["variant"]] += 1
        self.assertGreater(first_variant_counts["baseline"], 0)
        self.assertGreater(first_variant_counts["candidate"], 0)

    def test_default_record_budget_counts_only_real_cloud_sinks(self):
        cases = run_matrix.plan(["baseline"], 10, 270127, 2500, 100000)
        real_logs = [case for case in cases if case["mode"] != "none" and case["sink"] == "stdout"]
        self.assertEqual(len(cases), 240)
        self.assertEqual(len(real_logs), 120)
        self.assertEqual(sum(case["count"] for case in real_logs), 300000)
        self.assertFalse(any(case["mode"] in ("none", "google-api", "slogcp-grpc") and case["sink"] == "discard" for case in cases))


class RunTests(unittest.TestCase):
    def args(self, directory, **changes):
        values = dict(
            archive=Path(directory), run_id="benchmark-example",
            project="example-project", region="example-region", bucket="example-bucket",
            image="example.invalid/image@sha256:" + "a" * 64,
            runtime_sa="runtime@example-project.iam.gserviceaccount.com",
            repeats=10, stdout_count=2500, discard_count=100000, warmup=200,
            include_slogcp_grpc=False,
            variant=["baseline", "candidate"],
        )
        values.update(changes)
        return Namespace(**values)

    def test_invalid_resource_name_is_rejected_before_cloud_calls(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.args(directory, run_id="invalid/resource")
            with patch.object(manage, "execute") as execute:
                with self.assertRaisesRegex(ValueError, "Run ID"):
                    manage.run(args)
            execute.assert_not_called()

    def test_mutable_image_or_invalid_limits_fail_before_cloud_calls(self):
        for changes in ({"image": "example.invalid/image:mutable"}, {"repeats": 21},
                        {"stdout_count": 10001}, {"variant": ["baseline", "baseline"]}):
            with self.subTest(changes=changes), tempfile.TemporaryDirectory() as directory:
                with patch.object(manage, "execute") as execute:
                    with self.assertRaises(ValueError):
                        manage.run(self.args(directory, **changes))
                execute.assert_not_called()
                self.assertFalse((Path(directory) / "job-env.json").exists())

    def test_run_scopes_both_cloud_calls_and_caps_resource_lifetime(self):
        with tempfile.TemporaryDirectory() as directory:
            args = self.args(directory)
            with patch.object(manage, "execute", return_value="{}") as execute:
                with contextlib.redirect_stdout(io.StringIO()):
                    manage.run(args)
            self.assertEqual(execute.call_count, 2)
            for call in execute.call_args_list:
                command = call.args[0]
                self.assertEqual(command[command.index("--project") + 1], "example-project")
                self.assertEqual(command[command.index("--region") + 1], "example-region")
            deployment = execute.call_args_list[0].args[0]
            for flag in ("--tasks=1", "--parallelism=1", "--max-retries=0", "--task-timeout=30m"):
                self.assertIn(flag, deployment)
            env = json.loads((Path(directory) / "job-env.json").read_text())
            self.assertEqual(json.loads(env["BENCH_VARIANTS"]), {
                "baseline": "/opt/bench/bin/baseline", "candidate": "/opt/bench/bin/candidate",
            })


if __name__ == "__main__":
    unittest.main()
