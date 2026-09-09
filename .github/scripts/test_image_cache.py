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

"""Exercise the image-cache hashing shell with real archive/hash tools."""

import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
CACHE = ROOT / ".e2e/cloudbuild/build-tools/evaluate-image-cache.sh"
BUILD = ROOT / ".e2e/cloudbuild/cloudbuild.yaml"


class ImageCacheTests(unittest.TestCase):
    def hash_context(self, directory, *, fail_tar=False, toolchain="repo"):
        source = CACHE.read_text(encoding="utf-8")
        start = source.index('HASH="$(')
        end = source.index("HASH_MANIFEST_PATH=", start)
        bash = shutil.which("bash")
        if os.name == "nt":
            bash = "C:/Program Files/Git/bin/bash.exe"
        self.assertTrue(bash, "Bash is required to test the cloud cache script")
        script = "set -euo pipefail\n"
        if fail_tar:
            script += "tar() { printf 'archive read failed\\n' >&2; return 2; }\n"
        script += source[start:end] + '\nprintf "%s\\n" "$HASH"\n'
        return subprocess.run(
            [bash, "-c", script],
            cwd=directory,
            env={
                **os.environ,
                "HASH_SOURCE_PATH": "context",
                "SERVICE_ID": "fixture",
                "GO_VERSION": "1.27.1",
                "DEBIAN_CODENAME": "trixie",
                "DISTROLESS_TAG": "nonroot",
                "DEPENDENCY_MODE": "floor",
                "TOOLCHAIN_MODE": toolchain,
            },
            capture_output=True,
            text=True,
            check=False,
        )

    def test_archive_failure_does_not_produce_a_cache_key(self):
        with tempfile.TemporaryDirectory() as temporary:
            (Path(temporary) / "context").mkdir()
            result = self.hash_context(temporary, fail_tar=True)
            self.assertNotEqual(result.returncode, 0, result.stdout)
            self.assertEqual(result.stdout, "")
            self.assertIn("archive read failed", result.stderr)

    def test_generated_inputs_and_policy_affect_deterministic_key(self):
        with tempfile.TemporaryDirectory() as temporary:
            context = Path(temporary) / "context"
            context.mkdir()
            manifest = context / "go.mod"
            manifest.write_text("module example.org/harness\ngo 1.26.0\n")
            first = self.hash_context(temporary)
            self.assertEqual(first.returncode, 0, first.stderr)
            self.assertRegex(first.stdout.strip(), r"^[0-9a-f]{64}$")
            os.utime(manifest, (100000, 100000))
            self.assertEqual(first.stdout, self.hash_context(temporary).stdout)
            manifest.write_text("module example.org/harness\ngo 1.27.0\n")
            changed = self.hash_context(temporary)
            self.assertEqual(changed.returncode, 0, changed.stderr)
            self.assertNotEqual(first.stdout, changed.stdout)
            latest = self.hash_context(temporary, toolchain="latest")
            self.assertEqual(latest.returncode, 0, latest.stderr)
            self.assertNotEqual(changed.stdout, latest.stdout)

    def test_every_image_hashes_its_finalized_build_context(self):
        calls = re.findall(
            r"evaluate-image-cache\.sh\s+\\\s*\n"
            r"\s*(\S+)\s+\\\s*\n"
            r"\s*(\S+)\s+\\\s*\n"
            r"\s*(\S+)\s+\\\s*\n",
            BUILD.read_text(encoding="utf-8"),
        )
        self.assertEqual(len(calls), 5)
        for service, hash_source, build_context in calls:
            with self.subTest(service=service):
                self.assertEqual(hash_source, build_context)
                self.assertTrue(build_context.startswith("/workspace/build-context/"))

    def test_existing_manifest_cannot_bypass_docker_build(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            (root / "context").mkdir()
            digest = self.hash_context(temporary).stdout.strip()
            values = {
                "stream_tag_defaulted": "fixture",
                "app_version": "fixture-version",
                "go_version": "1.27.1",
                "debian_codename": "trixie",
                "distroless_tag": "nonroot",
                "dependency_mode": "floor",
                "toolchain_mode": "repo",
            }
            for name, value in values.items():
                (root / (name + ".txt")).write_text(value)
            script = 'gsutil() { if [[ "$1" == cp ]]; then printf "%s" "$FIXTURE_JSON" > "$3"; fi; }\n'
            script += "gcloud() { return 0; }\ndocker() { return 0; }\n"
            script += CACHE.read_text(encoding="utf-8").replace(
                "/workspace", root.as_posix()
            )
            bash = (
                "C:/Program Files/Git/bin/bash.exe"
                if os.name == "nt"
                else shutil.which("bash")
            )
            result = subprocess.run(
                [
                    bash,
                    "-c",
                    script,
                    "fixture",
                    "fixture",
                    "context",
                    "context",
                    "Dockerfile",
                    "image",
                ],
                cwd=root,
                env={
                    **os.environ,
                    "_ARTIFACT_REGISTRY_REPO": "example.invalid/repo",
                    "_GCS_BUCKET_NAME": "fixture",
                    "FIXTURE_JSON": '{"hash":"' + digest + '","versionTag":"old"}',
                },
                capture_output=True,
                text=True,
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            state = (root / "cache/fixture.env").read_text()
            self.assertIn("SHOULD_BUILD=true", state)
            self.assertIn("NEEDS_RETAG=false", state)
            self.assertIn("CACHE_FROM_TAG=example.invalid/repo/image:fixture", state)

    def test_image_tags_are_unique_per_cloud_build(self):
        line = next(
            line.strip().replace("$$", "$")
            for line in BUILD.read_text(encoding="utf-8").splitlines()
            if line.strip().startswith('APP_VERSION="')
        )
        bash = (
            "C:/Program Files/Git/bin/bash.exe"
            if os.name == "nt"
            else shutil.which("bash")
        )
        tags = []
        for build_id in (
            "11111111-1111-1111-1111-111111111111",
            "22222222-2222-2222-2222-222222222222",
        ):
            tags.append(
                subprocess.check_output(
                    [bash, "-c", line + '\nprintf "%s" "$APP_VERSION"'],
                    env={
                        **os.environ,
                        "STREAM_TAG_DEFAULTED": "pr-81-floor",
                        "_PR_SHA": "a" * 40,
                        "BUILD_ID": build_id,
                    },
                    text=True,
                )
            )
        self.assertNotEqual(*tags)
        self.assertTrue(all(len(tag) <= 128 for tag in tags))


if __name__ == "__main__":
    unittest.main()
