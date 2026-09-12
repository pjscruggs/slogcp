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

from __future__ import annotations

import copy
import io
import json
import os
import re
import shutil
import subprocess
import textwrap
import unittest
import urllib.error
from pathlib import Path
from unittest.mock import Mock, patch

import release_policy as policy


SHA = "a" * 40
PARENT = "b" * 40


class ReleaseIntentTests(unittest.TestCase):
    def check(self, previous="v1.2.3", current="v1.2.4", event="push", requested=""):
        values = {
            ("rev-parse", "HEAD"): SHA,
            ("rev-parse", SHA + "^"): PARENT,
            ("show", SHA + ":version.go"): f'var Version = "{current}"',
            ("show", PARENT + ":version.go"): f'var Version = "{previous}"',
        }
        with patch.object(policy, "git", side_effect=lambda *args: values[args]):
            return policy.plan(event, "refs/heads/main", SHA, requested)

    def test_semantic_patch_and_human_minor_transition(self):
        self.assertEqual(self.check(), {"should_release": "true", "version": "v1.2.4"})
        self.assertEqual(self.check(current="v1.3.0")["version"], "v1.3.0")

    def test_same_version_does_not_release(self):
        self.assertEqual(self.check(current="v1.2.3"), {"should_release": "false"})

    def test_no_release_from_later_unrelated_main_commit(self):
        with self.assertRaisesRegex(ValueError, "original release workflow"):
            self.check(current="v1.2.3", event="workflow_dispatch")

    def test_dispatch_matches_exact_transition(self):
        self.assertEqual(
            self.check(event="workflow_dispatch", requested="v1.2.4")["version"],
            "v1.2.4",
        )
        with self.assertRaisesRegex(ValueError, "does not match"):
            self.check(event="workflow_dispatch", requested="v1.2.5")

    def test_rejects_decrease_wrong_major_and_noncanonical_versions(self):
        for current in ("v1.2.2", "v1.02.4", "v2.0.0", "v1.2.4-rc1"):
            with self.subTest(current=current), self.assertRaises(ValueError):
                self.check(current=current)

    def test_rejects_duplicate_declarations(self):
        with self.assertRaises(ValueError):
            policy.version_of('var Version = "v1.2.3"\nconst Version = "v1.2.3"')

    def test_rejects_wrong_source_and_event(self):
        with (
            patch.object(policy, "git", return_value=PARENT),
            self.assertRaises(ValueError),
        ):
            policy.plan("push", "refs/heads/main", SHA)
        for event, ref in (
            ("workflow_dispatch", "refs/heads/feature"),
            ("pull_request", "refs/heads/main"),
        ):
            with self.subTest(event=event, ref=ref), self.assertRaises(ValueError):
                policy.plan(event, ref, SHA)


class GitHubReadTests(unittest.TestCase):
    def setUp(self):
        self.client = policy.GitHub("owner/repo", "test-token")

    def test_only_404_means_missing(self):
        with patch.object(self.client, "request", side_effect=policy.ApiError(404)):
            self.assertIsNone(self.client.get("/tag", allow_missing=True))
            with self.assertRaises(policy.ApiError):
                self.client.get("/tag")
        for status in (401, 403, 422):
            with (
                self.subTest(status=status),
                patch.object(
                    self.client, "request", side_effect=policy.ApiError(status)
                ),
                self.assertRaises(policy.ApiError),
            ):
                self.client.get("/tag", allow_missing=True)

    def test_transient_read_retries_are_bounded(self):
        with (
            patch.object(
                self.client, "request", side_effect=[policy.ApiError(503), {"ok": True}]
            ) as request,
            patch.object(policy.time, "sleep"),
        ):
            self.assertEqual(self.client.get("/tag"), {"ok": True})
            self.assertEqual(request.call_count, 2)
        with (
            patch.object(
                self.client, "request", side_effect=policy.ApiError(503)
            ) as request,
            patch.object(policy.time, "sleep"),
            self.assertRaises(policy.ApiError),
        ):
            self.client.get("/tag")
        self.assertEqual(request.call_count, 3)

    def test_real_request_serializes_json_and_maps_http_failure(self):
        response = io.BytesIO(b'{"id": 1}')
        with patch.object(
            policy.urllib.request, "urlopen", return_value=response
        ) as transport:
            self.assertEqual(
                self.client.request("/releases", {"draft": False}), {"id": 1}
            )
        request = transport.call_args.args[0]
        self.assertEqual(
            request.full_url, "https://api.github.com/repos/owner/repo/releases"
        )
        self.assertEqual(request.get_method(), "POST")
        self.assertEqual(json.loads(request.data), {"draft": False})
        error = urllib.error.HTTPError(request.full_url, 403, "Forbidden", {}, None)
        with (
            patch.object(policy.urllib.request, "urlopen", side_effect=error),
            self.assertRaises(policy.ApiError) as caught,
        ):
            self.client.request("/releases")
        self.assertEqual(caught.exception.status, 403)


class PublicationTests(unittest.TestCase):
    def setUp(self):
        self.ref = {"object": {"type": "tag", "sha": "c" * 40}}
        self.tag = {
            "object": {"type": "commit", "sha": SHA},
            "verification": {"verified": True, "reason": "valid"},
        }
        self.release = {"tag_name": "v1.2.4", "draft": False}
        self.client = Mock(spec=policy.GitHub)

    def test_existing_valid_tag_and_release_are_noop(self):
        self.client.get.side_effect = [self.ref, self.tag, self.release]
        policy.publish(self.client, "v1.2.4", SHA)
        self.client.request.assert_not_called()

    def test_valid_tag_with_missing_release_is_completed(self):
        self.client.get.side_effect = [self.ref, self.tag, None]
        policy.publish(self.client, "v1.2.4", SHA)
        body = self.client.request.call_args.args[1]
        self.assertEqual(body["tag_name"], "v1.2.4")
        self.assertEqual(body["target_commitish"], SHA)
        self.assertFalse(body["draft"])
        self.assertFalse(body["prerelease"])
        self.assertEqual(self.client.request.call_count, 1)

    def test_timeout_after_creation_reconciles_without_duplicate_create(self):
        self.client.get.side_effect = [self.ref, self.tag, None, self.release]
        self.client.request.side_effect = policy.ApiError(503)
        with patch.object(policy.time, "sleep"):
            policy.publish(self.client, "v1.2.4", SHA)
        self.assertEqual(self.client.request.call_count, 1)

    def test_failed_creation_has_bounded_retry_and_no_new_version(self):
        self.client.get.side_effect = [self.ref, self.tag, None, None, None]
        self.client.request.side_effect = policy.ApiError(503)
        with patch.object(policy.time, "sleep"), self.assertRaises(policy.ApiError):
            policy.publish(self.client, "v1.2.4", SHA)
        self.assertEqual(self.client.request.call_count, 3)
        self.assertEqual(
            {call.args[1]["tag_name"] for call in self.client.request.call_args_list},
            {"v1.2.4"},
        )

    def test_rejects_bad_tag_target_type_and_signature_before_publication(self):
        mutations = [
            ("object", "sha", PARENT),
            ("object", "type", "tag"),
            ("verification", "verified", False),
            ("verification", "reason", "unsigned"),
        ]
        for field, key, value in mutations:
            with self.subTest(field=field, key=key):
                tag = copy.deepcopy(self.tag)
                tag[field][key] = value
                self.client.get.side_effect = [self.ref, tag]
                with self.assertRaises(ValueError):
                    policy.publish(self.client, "v1.2.4", SHA)
                self.client.request.assert_not_called()
        self.client.get.side_effect = [{"object": {"type": "commit", "sha": SHA}}]
        with self.assertRaisesRegex(ValueError, "annotated"):
            policy.publish(self.client, "v1.2.4", SHA)

    def test_absent_tag_state_is_distinct_from_invalid_tag(self):
        self.client.get.return_value = None
        self.assertFalse(
            policy.verify_tag(self.client, "v1.2.4", SHA, allow_missing=True)
        )
        self.client.get.assert_called_once_with(
            "/git/ref/tags/v1.2.4", allow_missing=True
        )

    def test_draft_or_mismatched_release_is_not_success(self):
        for release in (
            {"tag_name": "v1.2.5", "draft": False},
            {"tag_name": "v1.2.4", "draft": True},
        ):
            with self.subTest(release=release):
                self.client.get.side_effect = [self.ref, self.tag, release]
                with self.assertRaises(ValueError):
                    policy.publish(self.client, "v1.2.4", SHA)
                self.client.request.assert_not_called()


class ReleaseWorkflowTests(unittest.TestCase):
    def test_readme_benchmark_results_cannot_trigger_auto_release(self):
        workflow = (
            Path(__file__).resolve().parents[1] / "workflows/auto-release.yml"
        ).read_text(encoding="utf-8")
        push = re.search(r"(?ms)^  push:\n(.*?)(?=^  \S|\Z)", workflow)
        self.assertIsNotNone(push)
        paths = re.search(r"(?m)^    paths: \[([^\]]+)\]$", push[1])
        self.assertIsNotNone(paths, "Auto release must keep an explicit path allowlist")
        allowed = {path.strip() for path in paths[1].split(",")}
        self.assertEqual(allowed, {"version.go"})
        self.assertFalse(allowed.intersection({"README.md"}))
        # Even a dispatch cannot release a results-only commit after a version bump.
        intent = ReleaseIntentTests()
        self.assertEqual(intent.check(current="v1.2.3"), {"should_release": "false"})
        with self.assertRaisesRegex(ValueError, "original release workflow"):
            intent.check(current="v1.2.3", event="workflow_dispatch")

    def test_actual_publisher_validation_guard_fails_closed(self):
        workflow = (
            Path(__file__).resolve().parents[1] / "workflows/auto-release.yml"
        ).read_text(encoding="utf-8")
        step = workflow.split(
            "      - name: Require Successful Release Validation\n", 1
        )[1].split("\n      - name:", 1)[0]
        script = textwrap.dedent(step.split("        run: |\n", 1)[1])
        git_bash = Path("C:/Program Files/Git/bin/bash.exe")
        bash = str(git_bash) if git_bash.exists() else shutil.which("bash")
        if not bash:
            self.skipTest("bash is required to execute the publisher validation guard")
        for result in ("success", "failure", "skipped", "cancelled", "", "neutral"):
            for passed in ("true", "false", ""):
                with self.subTest(result=result, passed=passed):
                    completed = subprocess.run(
                        [bash, "--noprofile", "--norc", "-c", script],
                        env={
                            "PATH": os.environ.get("PATH", ""),
                            "SystemRoot": os.environ.get("SystemRoot", ""),
                            "VALIDATION_RESULT": result,
                            "VALIDATION_PASSED": passed,
                        },
                        capture_output=True,
                        text=True,
                        check=False,
                    )
                    self.assertEqual(
                        completed.returncode == 0,
                        result == "success" and passed == "true",
                    )


if __name__ == "__main__":
    unittest.main()
