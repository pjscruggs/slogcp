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

import base64
from contextlib import nullcontext
import os
from pathlib import Path
import re
import subprocess
import tempfile
import unittest
from unittest.mock import Mock, call, patch

import publish_readme_benchmarks as publisher
import readme_benchmarks as reporting


SOURCE = "a" * 40
BASE = "b" * 40
HEAD = "c" * 40
PREVIOUS = "d" * 40
FINGERPRINT = "e" * 64
REPOSITORY = "pjscruggs/slogcp"
README = "Introduction\r\n\r\n" + reporting.START + "\r\nPending\r\n" + reporting.END + "\r\n\r\nUsage\r\n"


def completed(stdout="", returncode=0, stderr=""):
    return subprocess.CompletedProcess([], returncode, stdout, stderr)


def report_fixture():
    cpu = "AMD EPYC 7763 64-Core Processor"
    lines = ["goos: linux", "goarch: amd64", "pkg: " + reporting.PACKAGE, "cpu: " + cpu]
    for name in reporting.BENCHMARK_NAMES:
        lines.extend(f"{name}\t1000\t100.0 ns/op\t16 B/op\t1 allocs/op" for _ in range(10))
    parsed = reporting.parse_output("\n".join(lines + ["PASS", "ok  " + reporting.PACKAGE + " 60.1s", ""]))
    return {
        "benchmarks": parsed,
        "provenance": {
            "schema": 1, "source_sha": SOURCE, "repository": REPOSITORY,
            "source_url": f"https://github.com/{REPOSITORY}/commit/{SOURCE}",
            "workflow_url": f"https://github.com/{REPOSITORY}/actions/runs/123",
            "input_fingerprint": FINGERPRINT, "go_version": "go version go1.27.1 linux/amd64",
            "goos": "linux", "goarch": "amd64", "cpu": cpu, "runner": "ubuntu-latest",
            "image_os": "ubuntu24", "image_version": "20260907.1.0",
            "command": list(reporting.COMMAND), "environment": dict(reporting.BENCHMARK_ENV),
            "sample_count": 10,
        },
    }


def pr_fixture(state="OPEN", head=PREVIOUS):
    return dict(number=17, state=state, title=publisher.TITLE, body="", headRefOid=head,
                baseRefName="main", headRefName=publisher.BRANCH, isCrossRepository=False,
                mergeCommit={"oid": SOURCE} if state == "MERGED" else None,
                autoMergeRequest={"enabledAt": "2026-09-12T00:00:00Z"} if state == "OPEN" else None)


def commands_fixture(repo=Path(".")):
    commands = Mock(spec=publisher.Commands)
    commands.repo = repo
    commands.repository = REPOSITORY
    commands.remote = f"https://github.com/{REPOSITORY}.git"
    commands.git.return_value = completed()
    commands.ancestor.return_value = True
    commands.revision.return_value = HEAD
    commands.fetch_main.return_value = BASE
    commands.readme.return_value = README
    return commands


class CommandTests(unittest.TestCase):
    def test_git_blob_newlines_are_preserved_and_secret_environment_is_removed(self):
        with patch.dict(os.environ, {"BENCHMARK_SSH_PRIVATE_KEY_B64": "secret", "BENCHMARK_SSH_PUBLIC_KEY": "public"}):
            commands = publisher.Commands(Path("."), REPOSITORY)
        result = subprocess.CompletedProcess([], 0, b"Intro\r\nTail\r\n", b"")
        with patch.object(publisher.subprocess, "run", return_value=result):
            self.assertEqual(commands.run(["git", "show"]).stdout, "Intro\r\nTail\r\n")
        self.assertNotIn("BENCHMARK_SSH_PRIVATE_KEY_B64", commands.env)
        self.assertNotIn("BENCHMARK_SSH_PUBLIC_KEY", commands.env)
        self.assertIn("core.hooksPath=" + os.devnull, commands.git_config)

    def test_remote_repository_is_validated(self):
        for repository in ("--evil", "org/repo/extra", "org/repo;bad", "https://evil.example/repo"):
            with self.subTest(repository=repository), self.assertRaises(ValueError):
                publisher.Commands(Path("."), repository)

    def test_readme_symlink_is_rejected(self):
        commands = publisher.Commands(Path("."), REPOSITORY)
        with patch.object(commands, "git", return_value=completed(f"120000 blob {HEAD}\tREADME.md\n")):
            with self.assertRaisesRegex(ValueError, "regular"):
                commands.readme(BASE)

    def test_bad_command_diagnostics_do_not_include_secret_stdout_or_stderr(self):
        commands = publisher.Commands(Path("."), REPOSITORY)
        result = subprocess.CompletedProcess([], 1, b"private", b"secret")
        with patch.object(publisher.subprocess, "run", return_value=result):
            with self.assertRaisesRegex(RuntimeError, "exit status 1") as raised:
                commands.run(["ssh-keygen", "-y"])
        self.assertNotIn("private", str(raised.exception))
        self.assertNotIn("secret", str(raised.exception))


class ScopeTests(unittest.TestCase):
    def test_only_marker_block_can_change(self):
        commands = commands_fixture()
        commands.git.return_value = completed("README.md\0")
        for replacement, allowed in (
            (README.replace("Pending", "Results"), True),
            (README.replace("Usage", "Unexpected prose"), False),
            (README.replace("\r\n", "\n"), False),
        ):
            commands.readme.side_effect = [README, replacement]
            with self.subTest(allowed=allowed):
                if allowed:
                    publisher.require_readme_only(commands, BASE, HEAD)
                else:
                    with self.assertRaisesRegex(ValueError, "outside"):
                        publisher.require_readme_only(commands, BASE, HEAD)

    def test_additional_paths_fail_before_readme_is_read(self):
        commands = commands_fixture()
        commands.git.return_value = completed("README.md\0version.go\0")
        with self.assertRaisesRegex(ValueError, "other than"):
            publisher.require_readme_only(commands, BASE, HEAD)
        commands.readme.assert_not_called()

    def test_freshness_uses_current_tree_and_measured_toolchain(self):
        commands = commands_fixture()
        report = report_fixture()
        with patch.object(reporting, "fingerprint_inputs", return_value=FINGERPRINT) as fingerprint:
            publisher.require_fresh_report(commands, report, SOURCE, BASE)
        fingerprint.assert_called_once_with(
            commands.repo, BASE, "go version go1.27.1 linux/amd64", "ubuntu-latest",
            "ubuntu24", "20260907.1.0",
        )

    def test_stale_inputs_wrong_repository_and_unrelated_source_fail(self):
        commands = commands_fixture()
        with patch.object(reporting, "fingerprint_inputs", return_value="0" * 64):
            with self.assertRaisesRegex(ValueError, "inputs changed"):
                publisher.require_fresh_report(commands, report_fixture(), SOURCE, BASE)
        wrong = report_fixture()
        wrong["provenance"]["repository"] = "other/repo"
        with self.assertRaisesRegex(ValueError, "different repository"):
            publisher.require_fresh_report(commands, wrong, SOURCE, BASE)
        commands.ancestor.return_value = False
        with self.assertRaisesRegex(ValueError, "ancestor"):
            publisher.require_fresh_report(commands, report_fixture(), SOURCE, BASE)


class PullLifecycleTests(unittest.TestCase):
    def test_recognizes_open_and_squash_merged_heads(self):
        commands = commands_fixture()
        current = pr_fixture()
        self.assertEqual(publisher.existing_pull([current], PREVIOUS, BASE, commands), current)
        self.assertIsNone(publisher.existing_pull([pr_fixture("MERGED")], PREVIOUS, BASE, commands))
        commands.ancestor.assert_called_with(SOURCE, BASE)

    def test_orphan_recovery_does_not_reopen_intentionally_closed_pr(self):
        commands = commands_fixture()
        self.assertIsNone(publisher.existing_pull([], PREVIOUS, BASE, commands))
        with self.assertRaisesRegex(ValueError, "intentionally closed"):
            publisher.existing_pull([pr_fixture("CLOSED")], PREVIOUS, BASE, commands)

    def test_mismatched_pr_identity_or_uncontained_previous_merge_fails(self):
        commands = commands_fixture()
        for field, value in (("headRefOid", HEAD), ("isCrossRepository", True),
                             ("body", "unexpected public text"), ("baseRefName", "release")):
            current = pr_fixture()
            current[field] = value
            with self.subTest(field=field), self.assertRaises(ValueError):
                publisher.existing_pull([current], PREVIOUS, BASE, commands)
        commands.ancestor.return_value = False
        with self.assertRaisesRegex(ValueError, "not contained"):
            publisher.existing_pull([pr_fixture("MERGED")], PREVIOUS, BASE, commands)


class WorkflowConfigurationTests(unittest.TestCase):
    def test_measurement_and_publisher_follow_latest_ubuntu(self):
        workflow = (Path(__file__).resolve().parents[1] / "workflows/benchmarks.yml").read_text(encoding="utf-8")
        self.assertEqual(re.findall(r"^\s+runs-on:\s*(\S+)", workflow, re.MULTILINE),
                         ["ubuntu-latest", "ubuntu-latest"])
        self.assertEqual(re.findall(r"--runner\s+(\S+)", workflow),
                         ["ubuntu-latest", "ubuntu-latest"])


class SigningTests(unittest.TestCase):
    def signing_environment(self, directory):
        return dict(RUNNER_TEMP=directory,
                    BENCHMARK_SSH_FINGERPRINT=publisher.SIGNER_FINGERPRINT,
                    BENCHMARK_SSH_PUBLIC_KEY="ssh-ed25519 ZmFrZQ==",
                    BENCHMARK_SSH_PRIVATE_KEY_B64=base64.b64encode(b"fake test key").decode())

    def test_key_cleanup_on_success_and_body_failure(self):
        for failing in (False, True):
            with self.subTest(failing=failing), tempfile.TemporaryDirectory() as directory:
                commands = publisher.Commands(Path(directory), REPOSITORY)
                original = list(commands.git_config)
                identity = f"{publisher.SIGNER_NAME} <{publisher.SIGNER_EMAIL}> 1 +0000\n"
                with (patch.dict(os.environ, self.signing_environment(directory)),
                      patch.object(commands, "run", side_effect=[completed("ssh-ed25519 ZmFrZQ=="),
                           completed("256 " + publisher.SIGNER_FINGERPRINT + " test (ED25519)")]),
                      patch.object(commands, "git", return_value=completed(identity))):
                    try:
                        with publisher.signing(commands):
                            self.assertTrue((Path(directory) / "benchmark-readme-signing/key").exists())
                            if failing:
                                raise RuntimeError("body failed")
                    except RuntimeError:
                        self.assertTrue(failing)
                self.assertFalse((Path(directory) / "benchmark-readme-signing").exists())
                self.assertEqual(commands.git_config, original)

    def test_key_mismatch_cleans_files_and_cannot_enter_body(self):
        with tempfile.TemporaryDirectory() as directory:
            commands = publisher.Commands(Path(directory), REPOSITORY)
            with (patch.dict(os.environ, self.signing_environment(directory)),
                  patch.object(commands, "run", return_value=completed("ssh-ed25519 b3RoZXI=")),
                  self.assertRaisesRegex(ValueError, "do not match")):
                with publisher.signing(commands):
                    self.fail("mismatched key allowed")
            self.assertFalse((Path(directory) / "benchmark-readme-signing").exists())

    def test_signer_configuration_is_not_an_identity_escape_hatch(self):
        with patch.dict(os.environ, {"BENCHMARK_SSH_FINGERPRINT": "SHA256:unexpected"}):
            with self.assertRaisesRegex(ValueError, "documented"):
                with publisher.signing(commands_fixture()):
                    self.fail("unexpected key allowed")

    def test_commit_identity_and_expected_fingerprint_are_verified(self):
        commands = commands_fixture()
        identity = "\0".join((publisher.SIGNER_NAME, publisher.SIGNER_EMAIL) * 2)
        signature = f'Good "git" signature for {publisher.SIGNER_EMAIL} with ED25519 key {publisher.SIGNER_FINGERPRINT}'
        commands.git.side_effect = [completed(identity), completed(stderr=signature)]
        publisher.verify_commit(commands, HEAD)
        commands.git.side_effect = [completed(identity.replace("pjscruggs", "someone", 1))]
        with self.assertRaisesRegex(ValueError, "author or committer"):
            publisher.verify_commit(commands, HEAD)
        commands.git.side_effect = [completed(identity), completed(stderr="Good signature from another key")]
        with self.assertRaisesRegex(ValueError, "expected SSH"):
            publisher.verify_commit(commands, HEAD)


class PrepareTests(unittest.TestCase):
    def prepare(self, directory, *, conflicts="README.md\0", staged="README.md\0"):
        commands = commands_fixture(Path(directory))
        commands.ancestor.side_effect = lambda old, new: old == PREVIOUS and new == HEAD

        def git(*args, **kwargs):
            if args[:3] == ("merge", "--no-ff", "--no-commit"):
                return completed(returncode=1)
            if args[:3] == ("diff", "--name-only", "--diff-filter=U"):
                return completed(conflicts)
            if args[:3] == ("diff", "--cached", "--name-only"):
                return completed(staged)
            if args == ("diff", "--cached", "--quiet"):
                return completed(returncode=1)
            return completed()

        commands.git.side_effect = git
        return commands

    def test_signed_merge_preserves_current_main_prose_without_force_push(self):
        with tempfile.TemporaryDirectory() as directory:
            commands = self.prepare(directory)
            with patch.object(publisher, "require_readme_only"), patch.object(publisher, "verify_commit") as verify:
                self.assertEqual(publisher.prepare_commit(commands, report_fixture(), BASE, PREVIOUS), HEAD)
            actual = (Path(directory) / "README.md").read_bytes().decode()
            self.assertEqual(publisher.outside_block(actual), publisher.outside_block(README))
            commands.git.assert_any_call("merge", "--no-ff", "--no-commit", BASE, check=False)
            commands.git.assert_any_call("commit", "-S", "-m", publisher.TITLE)
            verify.assert_called_once_with(commands, HEAD)

    def test_conflicts_outside_readme_abort_without_commit(self):
        with tempfile.TemporaryDirectory() as directory:
            commands = self.prepare(directory, conflicts="README.md\0handler.go\0")
            with self.assertRaisesRegex(ValueError, "conflicts outside"):
                publisher.prepare_commit(commands, report_fixture(), BASE, PREVIOUS)
            commands.git.assert_any_call("merge", "--abort", check=False)
            self.assertFalse(any(item.args[:1] == ("commit",) for item in commands.git.call_args_list))

    def test_unexpected_staged_file_cannot_be_committed(self):
        with tempfile.TemporaryDirectory() as directory:
            commands = self.prepare(directory, staged="README.md\0version.go\0")
            with self.assertRaisesRegex(ValueError, "other than README"):
                publisher.prepare_commit(commands, report_fixture(), BASE, PREVIOUS)
            self.assertFalse(any(item.args[:1] == ("commit",) for item in commands.git.call_args_list))


class PublicationTests(unittest.TestCase):
    def execute(self, commands, *, previous=None, initial_requests=None):
        with (patch.object(reporting, "fingerprint_inputs", return_value=FINGERPRINT),
              patch.object(publisher, "pull_requests", side_effect=[initial_requests or [], [pr_fixture(head=HEAD)]]),
              patch.object(publisher, "remote_benchmark_head", return_value=previous),
              patch.object(publisher, "require_readme_only"),
              patch.object(publisher, "verify_commit") as verify,
              patch.object(publisher, "signing", return_value=nullcontext()),
              patch.object(publisher, "prepare_commit", return_value=HEAD)):
            result = publisher.publish(commands, report_fixture(), SOURCE)
            return result, verify.call_args_list

    def test_first_publication_creates_empty_body_pr_and_guarded_auto_merge(self):
        commands = commands_fixture()
        self.execute(commands)
        commands.git.assert_any_call("push", commands.remote, "HEAD:refs/heads/" + publisher.BRANCH)
        commands.gh.assert_any_call("pr", "create", "--repo", REPOSITORY, "--base", "main", "--head",
                                    publisher.BRANCH, "--title", publisher.TITLE, "--body", "")
        merged = commands.gh.call_args.args
        self.assertIn("--auto", merged)
        self.assertIn("--squash", merged)
        self.assertIn("--match-head-commit", merged)
        self.assertEqual(merged[merged.index("--match-head-commit") + 1], HEAD)
        all_args = [argument for item in commands.git.call_args_list + commands.gh.call_args_list for argument in item.args]
        for forbidden in ("--force", "--force-with-lease", "--admin", "comment", "review"):
            self.assertNotIn(forbidden, all_args)

    def test_main_advance_before_push_prevents_every_external_mutation(self):
        commands = commands_fixture()
        commands.fetch_main.side_effect = [BASE, SOURCE]
        with self.assertRaisesRegex(ValueError, "before pushing"):
            self.execute(commands)
        commands.gh.assert_not_called()
        self.assertFalse(any(item.args[:1] == ("push",) for item in commands.git.call_args_list))

    def test_main_advance_after_push_leaves_pr_without_auto_merge(self):
        commands = commands_fixture()
        commands.fetch_main.side_effect = [BASE, BASE, SOURCE]
        with self.assertRaisesRegex(ValueError, "remains pending"):
            self.execute(commands)
        self.assertFalse(any("--auto" in item.args for item in commands.gh.call_args_list))

    def test_existing_auto_merge_is_disabled_before_branch_is_updated(self):
        commands = commands_fixture()
        ordered = Mock()
        ordered.attach_mock(commands.gh, "gh")
        ordered.attach_mock(commands.git, "git")
        self.execute(commands, previous=PREVIOUS, initial_requests=[pr_fixture()])
        calls = ordered.mock_calls
        disable = calls.index(call.gh("pr", "merge", "17", "--repo", REPOSITORY, "--disable-auto"))
        push = calls.index(call.git("push", commands.remote, "HEAD:refs/heads/" + publisher.BRANCH))
        self.assertLess(disable, push)
        self.assertFalse(any(item.args[:2] == ("pr", "create") for item in commands.gh.call_args_list))

    def test_orphan_branch_recovery_verifies_every_unmerged_commit(self):
        commands = commands_fixture()

        def git(*args, **kwargs):
            if args[0] == "rev-list":
                return completed(PREVIOUS + "\n")
            return completed()

        commands.git.side_effect = git
        _, verified = self.execute(commands, previous=PREVIOUS)
        self.assertEqual(verified, [call(commands, PREVIOUS)])
        self.assertTrue(any(item.args[:2] == ("pr", "create") for item in commands.gh.call_args_list))

    def test_recovery_when_auto_merge_was_never_enabled_skips_disable_mutation(self):
        commands = commands_fixture()
        current = pr_fixture()
        current["autoMergeRequest"] = None
        self.execute(commands, previous=PREVIOUS, initial_requests=[current])
        self.assertFalse(any("--disable-auto" in item.args for item in commands.gh.call_args_list))
        self.assertTrue(any("--auto" in item.args for item in commands.gh.call_args_list))

    def test_stale_report_is_rejected_before_any_signing_or_mutation(self):
        commands = commands_fixture()
        with (patch.object(reporting, "fingerprint_inputs", return_value="0" * 64),
              patch.object(publisher, "signing") as signer,
              self.assertRaisesRegex(ValueError, "inputs changed")):
            publisher.publish(commands, report_fixture(), SOURCE)
        signer.assert_not_called()
        commands.gh.assert_not_called()


class FixtureCommands(publisher.Commands):
    """Exercise actual Git/index behavior only in a disposable test repository.

    Signing is tested independently above. These test-only commits never enter
    the project repository or a remote; remove -S and disable inherited signing
    so tests do not access the developer's private signing key.
    """

    def __init__(self, repo):
        super().__init__(repo, REPOSITORY)
        self.git_config.extend([
            "-c", "user.name=Benchmark fixture", "-c", "user.email=fixture@example.invalid",
            "-c", "commit.gpgsign=false", "-c", "core.autocrlf=false",
            "-c", "core.safecrlf=false",
        ])
        for name in ("GH_TOKEN", "GITHUB_TOKEN", "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL",
                     "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"):
            self.env.pop(name, None)

    def git(self, *args, check=True):
        if args and args[0] == "commit":
            args = tuple(argument for argument in args if argument != "-S")
        return super().git(*args, check=check)


class RealGitLifecycleTests(unittest.TestCase):
    """Verify merge/index/ancestry guarantees that argv mocks cannot establish."""

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="slogcp-publish-git-test-")
        self.addCleanup(self.temporary.cleanup)
        self.repo = Path(self.temporary.name)
        self.commands = FixtureCommands(self.repo)
        self.commands.git("init", "--initial-branch=main")
        self.base = self.commit_files({"README.md": README, "handler.go": "package fixture\n// original\n"})

    def commit_files(self, files):
        for filename, content in files.items():
            (self.repo / filename).write_bytes(content.encode())
        self.commands.git("add", "--", *files)
        self.commands.git("commit", "-m", "test: fixture input")
        return self.commands.revision("HEAD")

    def report(self, source, run=123):
        report = report_fixture()
        report["provenance"].update(source_sha=source,
                                    source_url=f"https://github.com/{REPOSITORY}/commit/{source}",
                                    workflow_url=f"https://github.com/{REPOSITORY}/actions/runs/{run}")
        return report

    def prepare(self, base, previous=None, run=123):
        with patch.object(publisher, "verify_commit") as verify:
            head = publisher.prepare_commit(self.commands, self.report(base, run), base, previous)
        verify.assert_called_once_with(self.commands, head)
        return head

    def assert_result_only(self, base, head):
        publisher.require_readme_only(self.commands, base, head)
        self.assertEqual(publisher.paths(self.commands.git("diff", "--name-only", "-z", base, head).stdout),
                         {"README.md"})
        self.assertTrue(self.commands.ancestor(base, head))
        self.assertFalse(self.commands.git("status", "--porcelain").stdout.strip())
        self.assertEqual(publisher.outside_block(self.commands.readme(base)),
                         publisher.outside_block(self.commands.readme(head)))

    def test_first_report_is_a_readme_only_child_and_preserves_crlf(self):
        head = self.prepare(self.base)
        self.assertEqual(self.commands.git("rev-parse", "HEAD^").stdout.strip(), self.base)
        self.assert_result_only(self.base, head)
        self.assertTrue((self.repo / "README.md").read_bytes().startswith(b"Introduction\r\n\r\n"))

    def test_existing_branch_merges_new_main_code_and_retains_new_main_prose(self):
        previous = self.prepare(self.base)
        self.commands.git("checkout", "main")
        base = self.commit_files({"handler.go": "package fixture\n// new main code\n",
                                  "README.md": README.replace("Usage", "New usage text")})
        head = self.prepare(base, previous, run=124)
        self.assert_result_only(base, head)
        self.assertTrue(self.commands.ancestor(previous, head))
        self.assertEqual(self.commands.git("show", head + ":handler.go").stdout,
                         "package fixture\n// new main code\n")
        self.assertIn("New usage text", self.commands.readme(head))

    def test_readme_merge_conflict_uses_latest_main_outside_block(self):
        previous = self.prepare(self.base)
        self.commands.git("checkout", "main")
        main_readme = README.replace("Pending", "Main replaced the benchmark block").replace("Usage", "Current usage")
        base = self.commit_files({"README.md": main_readme})
        head = self.prepare(base, previous, run=124)
        self.assert_result_only(base, head)
        self.assertEqual(self.commands.git("rev-list", "--parents", "-n", "1", head).stdout.split(),
                         [head, previous, base])
        self.assertIn("Current usage", self.commands.readme(head))
        self.assertNotIn("<<<<<<<", self.commands.readme(head))

    def test_retained_squash_merged_branch_can_advance_without_force(self):
        previous = self.prepare(self.base)
        self.commands.git("checkout", "main")
        self.commands.git("merge", "--squash", previous)
        self.commands.git("commit", "-m", "docs: fixture squash")
        base = self.commands.revision("HEAD")
        self.assertFalse(self.commands.ancestor(previous, base))
        head = self.prepare(base, previous, run=124)
        self.assert_result_only(base, head)
        self.assertTrue(self.commands.ancestor(previous, head))

    def test_non_readme_branch_change_is_rejected_before_new_commit(self):
        previous = self.prepare(self.base)
        modified = self.commit_files({"handler.go": "package fixture\n// unexpected branch edit\n"})
        with self.assertRaisesRegex(ValueError, "other than README"):
            self.prepare(self.base, modified, run=124)
        self.assertEqual(self.commands.revision("HEAD"), modified)
        self.assertTrue(self.commands.ancestor(previous, modified))

    def test_non_readme_merge_conflict_is_aborted(self):
        self.prepare(self.base)
        previous = self.commit_files({"handler.go": "package fixture\n// branch conflict\n"})
        self.commands.git("checkout", "main")
        base = self.commit_files({"handler.go": "package fixture\n// main conflict\n"})
        with self.assertRaisesRegex(ValueError, "conflicts outside"):
            self.prepare(base, previous, run=124)
        self.assertEqual(self.commands.revision("HEAD"), previous)
        self.assertNotEqual(self.commands.git("rev-parse", "--verify", "MERGE_HEAD", check=False).returncode, 0)


if __name__ == "__main__":
    unittest.main()
