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

"""Native Git release-range tests and controlled evidence transport tests."""

import copy
import json
import os
import re
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, patch

import release_policy as policy


class ReleaseRangeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.directory = Path(self.temp.name)
        self.git("init", "--quiet")
        self.git("config", "user.name", "Fixture")
        self.git("config", "user.email", "fixture@example.invalid")
        self.git("config", "commit.gpgsign", "false")
        self.write("version.go", 'var Version = "v1.0.0"\n')
        self.write("handler.go", "package fixture\n")
        self.commit()
        self.base = self.git("rev-parse", "HEAD")
        self.git("-c", "tag.gpgsign=false", "tag", "-a", "v1.0.0", "-m", "fixture")
        self.tag = self.git("rev-parse", "v1.0.0")
        self.releases = [{"tag_name": "v1.0.0", "draft": False, "prerelease": False,
                          "target_commitish": "not-the-release-commit"}]
        self.api = Mock(spec=policy.GitHub)
        self.api.pages.side_effect = lambda path: iter(self.releases)
        self.api.get.side_effect = lambda path, **kwargs: {
            "/git/ref/tags/v1.0.0": {"object": {"type": "tag", "sha": self.tag}},
            f"/git/tags/{self.tag}": {"object": {"type": "commit", "sha": self.base},
                                     "verification": {"verified": True, "reason": "valid"}},
        }[path]

    def git(self, *args):
        return subprocess.run(["git", *args], cwd=self.directory, check=True,
                              capture_output=True, text=True, encoding="utf-8").stdout.strip()

    def write(self, name, source):
        path = self.directory / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(source, encoding="utf-8")

    def commit(self):
        self.git("add", ".")
        self.git("commit", "--quiet", "-m", "fixture")

    def scope(self):
        with patch.object(policy, "git", side_effect=self.git):
            return policy.release_range(self.api, self.git("rev-parse", "HEAD"), "v1.0.1")

    def test_version_only_successor_covers_earlier_library_change(self):
        self.write("handler.go", "package fixture\nconst Added = true\n")
        self.commit()
        self.write("version.go", 'var Version = "v1.0.1"\n')
        self.commit()
        result = self.scope()
        self.assertEqual(result["release_base_sha"], self.base)
        self.assertEqual(json.loads(result["release_paths"]), ["handler.go", "version.go"])
        self.assertEqual(result["requires_cloud"], "true")

    def test_unpublished_and_draft_tags_are_not_the_boundary(self):
        self.write("handler.go", "package fixture\nconst Added = true\n")
        self.commit()
        self.git("-c", "tag.gpgsign=false", "tag", "v1.0.9")
        self.releases.append({"tag_name": "v1.0.9", "draft": True, "prerelease": False})
        self.assertEqual(self.scope()["release_base_sha"], self.base)

    def test_deleted_runtime_file_is_in_full_range(self):
        (self.directory / "handler.go").unlink()
        self.commit()
        self.assertIn("handler.go", json.loads(self.scope()["release_paths"]))
        self.assertEqual(self.scope()["requires_cloud"], "true")

    def test_example_only_delta_does_not_require_library_cloud(self):
        self.write(".examples/demo/main.go", "package main\n")
        self.commit()
        self.assertEqual(self.scope()["requires_cloud"], "false")

    def test_no_published_boundary_fails(self):
        self.releases.clear()
        with self.assertRaisesRegex(ValueError, "No verified published"):
            self.scope()


class CloudEvidenceTests(unittest.TestCase):
    def setUp(self):
        self.sha, self.head, self.tree = "a" * 40, "b" * 40, "c" * 40
        self.scope = {"candidate_sha": self.sha, "candidate_tree": self.tree}
        self.receipt = {"schema": 1, "root_sha": self.head, "profile": "root-parity",
                        "infrastructure_sha": "d" * 40, "dependency_mode": "floor",
                        "toolchain_mode": "repo", "result": "SUCCESS", "run_id": 10, "run_attempt": 1}
        self.check = {"id": 20, "name": "E2E Tests (GCP)", "app": {"id": 99},
                      "head_sha": self.head, "status": "completed", "conclusion": "success",
                      "details_url": "https://github.com/owner/repo/actions/runs/10"}
        self.run = {"path": ".github/workflows/manual-e2e-trigger.yml", "event": "workflow_dispatch",
                    "status": "completed", "conclusion": "success", "run_attempt": 1,
                    "html_url": self.check["details_url"]}
        self.pr = {"merged_at": "date", "merge_commit_sha": self.sha,
                   "base": {"ref": "main"}, "head": {"sha": self.head}}
        self.client = Mock(spec=policy.GitHub)

    def execute(self, *, tree=None, infrastructure_diff="", raw_receipt=None, extra_checks=()):
        self.client.pages.return_value = [self.pr]
        check = {**self.check, "output": {"text": raw_receipt if raw_receipt is not None else json.dumps(self.receipt)}}
        self.client.get.side_effect = lambda path: {
            f"/git/commits/{self.head}": {"tree": {"sha": tree or self.tree}},
            f"/commits/{self.head}/check-runs?per_page=100": {"check_runs": [check, *extra_checks]},
            "/actions/runs/10": self.run,
        }[path]
        with patch.object(policy, "git", return_value=infrastructure_diff):
            policy.require_cloud_evidence(self.client, self.scope, 99)

    def test_equal_tree_squash_reuses_success(self):
        self.execute()

    def test_changed_tree_or_infrastructure_cannot_reuse(self):
        for args in ({"tree": "f" * 40}, {"infrastructure_diff": ".e2e/cloudbuild/cloudbuild.yaml"}):
            with self.subTest(args=args), self.assertRaises(ValueError):
                self.execute(**args)

    def test_no_cloud_skipped_or_unstructured_check_cannot_substitute(self):
        for text in ("", "null", "not json", "{}"):
            with self.subTest(text=text), self.assertRaises(ValueError):
                self.execute(raw_receipt=text)
        for result in ("skipped", "failure", "cancelled", None):
            self.check["conclusion"] = result
            with self.subTest(result=result), self.assertRaises(ValueError):
                self.execute()

    def test_wrong_source_profile_mode_attempt_or_workflow_fails(self):
        original = copy.deepcopy(self.receipt)
        for key, value in (("root_sha", "f" * 40), ("profile", "combined-candidate"),
                           ("dependency_mode", "latest"), ("toolchain_mode", "latest"),
                           ("run_attempt", 2), ("result", "FAILURE")):
            self.receipt = {**original, key: value}
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.execute()
        self.receipt = original
        self.run["path"] = ".github/workflows/latest-go-canary.yml"
        with self.assertRaises(ValueError):
            self.execute()

    def test_newer_failed_check_blocks_older_success(self):
        failed = {**self.check, "id": 21, "conclusion": "failure"}
        with self.assertRaises(ValueError):
            self.execute(extra_checks=[failed])


class ReceiptProducerTests(unittest.TestCase):
    def test_actual_finalizers_emit_exact_subject_and_attempt(self):
        root = Path(__file__).resolve().parents[2]
        for filename, name in (
            ("validation_pipeline.yml", "Finalize stable E2E check after Cloud Build"),
            ("manual-e2e-trigger.yml", "Finalize E2E Check"),
        ):
            source = (root / ".github/workflows" / filename).read_text(encoding="utf-8")
            step = source.split("      - name: " + name + "\n", 1)[1]
            lines = step.split("          script: |\n", 1)[1].splitlines()
            body = []
            for line in lines:
                if line.strip() and not line.startswith("            "):
                    break
                body.append(line[12:] if line.strip() else "")
            script = re.sub(r"\$\{\{.*?\}\}", "123", "\n".join(body))
            harness = """
const context={repo:{owner:'owner',repo:'repo'},sha:'dddddddddddddddddddddddddddddddddddddddd',
  payload:{pull_request:{head:{sha:'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'}}},
  serverUrl:'https://github.com',runId:10};
const core={setOutput(){},setFailed:message=>{throw Error(message)}};
const github={rest:{checks:{update:async value=>console.log(JSON.stringify(value))}}};
"""
            result = subprocess.run(
                ["node", "--input-type=module", "-e", harness + "\nawait (async()=>{\n" + script + "\n})();"],
                env={**os.environ, "BUILD_STATUS": "SUCCESS", "CHECK_RUN_ID": "123",
                     "E2E_RUN_ID": "fixture", "POST_BUILD_VALIDATION_OK": "true",
                     "CHECK_RUN_NAME": "E2E Tests (GCP)", "GITHUB_RUN_ATTEMPT": "2",
                     "PR_NUMBER": "7", "TARGET_ROOT_SHA": "b" * 40},
                check=True, capture_output=True, text=True,
            )
            update = json.loads(result.stdout)
            receipt = json.loads(update["output"]["text"])
            with self.subTest(filename=filename):
                self.assertEqual(update["conclusion"], "success")
                self.assertEqual(receipt["root_sha"], "b" * 40)
                self.assertEqual(receipt["infrastructure_sha"], "d" * 40)
                self.assertEqual(receipt["run_attempt"], 2)
                self.assertEqual(receipt["run_id"], 10)
                self.assertEqual(receipt["profile"], "root-parity")

    def test_evidence_gate_precedes_signing_and_cannot_be_skipped(self):
        root = Path(__file__).resolve().parents[2]
        source = (root / ".github/workflows/auto-release.yml").read_text(encoding="utf-8")
        gate = source.split("      - name: Require Complete Release Evidence\n", 1)[1].split("      - name:", 1)[0]
        self.assertNotIn("if:", gate)
        self.assertNotIn("continue-on-error", gate)
        self.assertIn("release_policy.py release-gate", gate)
        self.assertLess(source.index("      - name: Require Complete Release Evidence"),
                        source.index("      - name: Generate Release App Token"))


if __name__ == "__main__":
    unittest.main()
