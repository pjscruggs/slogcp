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

"""Exercise source exports with real Git and the runner's actual shell."""

import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
RUNNER = ROOT / ".github/scripts/run_e2e_cloud_build.sh"
PREPARE = ROOT / ".e2e/cloudbuild/build-tools/prepare-build-context.sh"


def function(path, name):
    source = path.read_text(encoding="utf-8")
    start = source.index(name + "() {")
    return source[start : source.index("\n}", start) + 2]


class SourceExportTests(unittest.TestCase):
    def git(self, root, *args):
        return subprocess.check_output(
            ["git", "-C", str(root), *args], text=True
        ).strip()

    def fixture(self, directory):
        repo = directory / "repo"
        repo.mkdir()
        self.git(repo, "init", "--quiet")
        (repo / "go.mod").write_bytes(b"module example.org/library\r\ngo 1.26.0\r\n")
        (repo / "embedded.txt").write_bytes(b"embedded payload\x00\r\n")
        self.git(repo, "add", ".")
        self.git(
            repo,
            "-c",
            "user.name=Fixture",
            "-c",
            "user.email=fixture@example.org",
            "-c",
            "commit.gpgsign=false",
            "commit",
            "--quiet",
            "-m",
            "fixture",
        )
        sha = self.git(repo, "rev-parse", "HEAD")
        return repo, sha

    def shell(self, directory, script, **env):
        bash = (
            "C:/Program Files/Git/bin/bash.exe"
            if os.name == "nt"
            else shutil.which("bash")
        )
        self.assertTrue(bash)
        return subprocess.run(
            [bash, "-c", "set -euo pipefail\n" + script],
            cwd=directory,
            env={**os.environ, **env},
            capture_output=True,
            text=True,
        )

    def export(self, directory, sha, prefix=""):
        return self.shell(
            directory,
            prefix
            + function(RUNNER, "stage_go_module_checkout")
            + '\nstage_go_module_checkout repo exported "$REVISION"\n',
            REVISION=sha,
        )

    def test_exact_commit_includes_assets_but_not_worktree_changes(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            repo, sha = self.fixture(directory)
            expected = subprocess.check_output(
                ["git", "-C", str(repo), "show", f"{sha}:embedded.txt"]
            )
            (repo / "embedded.txt").write_bytes(b"uncommitted change")
            (repo / "untracked.txt").write_text("must not upload")
            result = self.export(directory, sha)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(
                (directory / "exported/embedded.txt").read_bytes(), expected
            )
            self.assertFalse((directory / "exported/untracked.txt").exists())
            self.assertFalse((directory / "exported/.git").exists())

    def test_symbolic_revision_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            self.fixture(directory)
            self.assertNotEqual(self.export(directory, "HEAD").returncode, 0)

    def test_existing_output_is_not_overwritten(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            _, sha = self.fixture(directory)
            (directory / "exported").mkdir()
            marker = directory / "exported/keep.txt"
            marker.write_text("keep")
            self.assertNotEqual(self.export(directory, sha).returncode, 0)
            self.assertEqual(marker.read_text(), "keep")

    def test_archive_error_is_not_hidden_by_tar(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            _, sha = self.fixture(directory)
            result = self.export(
                directory,
                sha,
                'git() { if [[ "$*" == *"archive"* ]]; then return 42; fi; command git "$@"; }\n',
            )
            self.assertNotEqual(result.returncode, 0)

    def test_docker_context_preserves_non_go_assets(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            repo, _ = self.fixture(directory)
            result = self.shell(
                directory,
                function(PREPARE, "copy_slogcp_workspace")
                + "\ncopy_slogcp_workspace repo context\n",
            )
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(
                (directory / "context/slogcp/embedded.txt").read_bytes(),
                (repo / "embedded.txt").read_bytes(),
            )
            self.assertFalse((directory / "context/slogcp/.git").exists())

    def test_adapter_selection_is_explicit(self):
        source = RUNNER.read_text(encoding="utf-8")
        self.assertNotIn('adapter_checkout="../slogcp-grpc-adapter"', source)
        self.assertIn(
            '"$E2E_ADAPTER_SHA"', function(RUNNER, "stage_local_build_source")
        )
        self.assertIn('"$PR_SHA"', function(RUNNER, "stage_local_build_source"))

    def test_full_staging_exports_committed_infrastructure_and_services(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            repo, _ = self.fixture(directory)
            services = repo / ".e2e/services"
            services.mkdir(parents=True)
            (services / "service.txt").write_text("committed service")
            (repo / ".e2e/config.txt").write_text("committed infrastructure")
            self.git(repo, "add", ".")
            self.git(
                repo,
                "-c",
                "user.name=Fixture",
                "-c",
                "user.email=fixture@example.org",
                "-c",
                "commit.gpgsign=false",
                "commit",
                "--quiet",
                "-m",
                "infrastructure",
            )
            sha = self.git(repo, "rev-parse", "HEAD")
            (services / "service.txt").write_text("uncommitted service")
            (repo / ".e2e/config.txt").write_text("uncommitted infrastructure")
            script = (
                function(RUNNER, "stage_go_module_checkout")
                + "\n"
                + function(RUNNER, "stage_local_build_source")
            )
            result = self.shell(
                repo,
                script + "\nstage_local_build_source ../staged .e2e\n",
                PR_SHA=sha,
                E2E_ADAPTER_SHA="",
                E2E_ADAPTER_CHECKOUT="",
            )
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            self.assertEqual(
                (directory / "staged/services/service.txt").read_text(),
                "committed service",
            )
            self.assertEqual(
                (directory / "staged/config.txt").read_text(),
                "committed infrastructure",
            )
            self.assertIn(
                sha, (directory / "staged/source-identities.json").read_text()
            )


if __name__ == "__main__":
    unittest.main()
