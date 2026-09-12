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

"""Publish a verified README benchmark block through a signed, dedicated PR.

This command runs only in the isolated publisher job, after measurement. It never
runs Go, executes artifact code, force-pushes, or writes reviews/checks/comments.
The GitHub App token must be scoped to this repository's contents and PRs.
"""

from __future__ import annotations

import argparse
import base64
from contextlib import contextmanager
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
from typing import Iterator

import readme_benchmarks as reporting


BRANCH = "automation/readme-benchmarks"
BASE_BRANCH = "main"
TITLE = "docs: update README benchmark results"
SIGNER_NAME = "pjscruggs"
SIGNER_EMAIL = "PatrickJScruggs@gmail.com"
SIGNER_FINGERPRINT = "SHA256:xISwtftNFk0QOAUMwrPaNvh0UCsbVaxHw4ADIlBsgnY"
MAIN_REF = "refs/remotes/benchmark/main"
BENCHMARK_REF = "refs/remotes/benchmark/readme"
SHA_PATTERN = re.compile(r"[0-9a-f]{40}")


class Commands:
    """Run fixed argv commands without exposing secrets in diagnostics."""

    def __init__(self, repo: Path, repository: str):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ValueError("Invalid GitHub repository name")
        self.repo = repo.resolve()
        self.repository = repository
        self.remote = f"https://github.com/{repository}.git"
        self.env = dict(os.environ, LC_ALL="C", GIT_TERMINAL_PROMPT="0", GH_PROMPT_DISABLED="1")
        for name in ("BENCHMARK_SSH_PRIVATE_KEY_B64", "BENCHMARK_SSH_PUBLIC_KEY"):
            self.env.pop(name, None)
        self.git_config = [
            "-c", "core.hooksPath=" + os.devnull,
            "-c", "credential.helper=",
            "-c", "credential.helper=!gh auth git-credential",
        ]

    def run(self, args: list[str], *, check: bool = True) -> subprocess.CompletedProcess:
        result = subprocess.run(args, cwd=self.repo, env=self.env, capture_output=True,
                                timeout=120, check=False)
        if check and result.returncode:
            raise RuntimeError(f"{args[0]} command failed with exit status {result.returncode}")
        # Universal-newline translation would hide unintended edits to a CRLF
        # README outside the marker block. Git blobs must retain their bytes.
        return subprocess.CompletedProcess(args, result.returncode,
                                           result.stdout.decode("utf-8"), result.stderr.decode("utf-8"))

    def git(self, *args: str, check: bool = True) -> subprocess.CompletedProcess:
        return self.run(["git", *self.git_config, *args], check=check)

    def gh(self, *args: str) -> str:
        return self.run(["gh", *args]).stdout

    def revision(self, ref: str) -> str:
        value = self.git("rev-parse", "--verify", ref + "^{commit}").stdout.strip()
        if not SHA_PATTERN.fullmatch(value):
            raise ValueError("Git returned an invalid source revision")
        return value

    def readme(self, ref: str) -> str:
        entry = self.git("ls-tree", ref, "--", "README.md").stdout.strip()
        if not re.fullmatch(r"100(?:644|755) blob [0-9a-f]{40}\tREADME\.md", entry):
            raise ValueError("README.md must be a regular committed file")
        return self.git("show", ref + ":README.md").stdout

    def ancestor(self, old: str, new: str) -> bool:
        result = self.git("merge-base", "--is-ancestor", old, new, check=False)
        if result.returncode not in (0, 1):
            raise RuntimeError("Could not establish commit ancestry")
        return result.returncode == 0

    def fetch_main(self) -> str:
        self.git("fetch", "--no-tags", self.remote, f"refs/heads/{BASE_BRANCH}:{MAIN_REF}")
        return self.revision(MAIN_REF)


def paths(output: str) -> set[str]:
    return set(filter(None, output.split("\0")))


def outside_block(readme: str) -> str:
    """Validate markers and compare everything the automation does not own."""
    start, end = reporting.block_bounds(readme)
    return readme[:start] + readme[end:]


def require_readme_only(commands: Commands, base: str, head: str) -> None:
    changed = paths(commands.git("diff", "--name-only", "-z", base, head, "--").stdout)
    if changed - {"README.md"}:
        raise ValueError("Automation branch changes files other than README.md")
    if outside_block(commands.readme(base)) != outside_block(commands.readme(head)):
        raise ValueError("Automation branch changes README text outside its benchmark block")


def verify_commit(commands: Commands, revision: str) -> None:
    expected = [SIGNER_NAME, SIGNER_EMAIL, SIGNER_NAME, SIGNER_EMAIL]
    identity = commands.git("log", "-1", "--format=%an%x00%ae%x00%cn%x00%ce", revision).stdout.strip()
    if identity.split("\0") != expected:
        raise ValueError("Benchmark commit author or committer differs from pjscruggs")
    result = commands.git("verify-commit", revision)
    verification = result.stdout + result.stderr
    if SIGNER_FINGERPRINT not in verification or SIGNER_EMAIL not in verification:
        raise ValueError("Benchmark commit does not have the expected SSH signer")


@contextmanager
def signing(commands: Commands) -> Iterator[None]:
    """Install only the documented signer, then remove every secret file."""
    supplied_fingerprint = os.environ.get("BENCHMARK_SSH_FINGERPRINT", "")
    if supplied_fingerprint != SIGNER_FINGERPRINT:
        raise ValueError("BENCHMARK_SSH_FINGERPRINT must match the documented signing key")
    public = os.environ.get("BENCHMARK_SSH_PUBLIC_KEY", "").strip()
    if not re.fullmatch(r"ssh-ed25519 [A-Za-z0-9+/]+={0,2}(?: [^\r\n]*)?", public):
        raise ValueError("Expected one ED25519 public signing key")
    encoded = os.environ.get("BENCHMARK_SSH_PRIVATE_KEY_B64", "")
    if not encoded or len(encoded) > 16384:
        raise ValueError("Missing or invalid benchmark private signing key")
    try:
        private = base64.b64decode(encoded, validate=True)
    except ValueError as error:
        raise ValueError("Invalid base64 benchmark signing key") from error
    temporary = Path(os.environ["RUNNER_TEMP"]).resolve()
    directory = temporary / "benchmark-readme-signing"
    directory.mkdir(mode=0o700)
    original_config = list(commands.git_config)
    try:
        key = directory / "key"
        key.write_bytes(private)
        key.chmod(0o600)
        public_key = " ".join(public.split()[:2])
        (directory / "key.pub").write_text(public_key + "\n", encoding="utf-8")
        derived = commands.run(["ssh-keygen", "-y", "-f", str(key)]).stdout.strip()
        if derived.split()[:2] != public_key.split():
            raise ValueError("Private and public benchmark signing keys do not match")
        fingerprint = commands.run(["ssh-keygen", "-lf", str(directory / "key.pub")]).stdout.split()
        if len(fingerprint) < 2 or fingerprint[1] != SIGNER_FINGERPRINT:
            raise ValueError("Benchmark signing key fingerprint does not match")
        allowed = directory / "allowed_signers"
        allowed.write_text(f"{SIGNER_EMAIL} {public_key}\n", encoding="utf-8")
        commands.git_config.extend([
            "-c", "user.name=" + SIGNER_NAME,
            "-c", "user.email=" + SIGNER_EMAIL,
            "-c", "gpg.format=ssh",
            "-c", "user.signingkey=" + str(key),
            "-c", "gpg.ssh.allowedSignersFile=" + str(allowed),
            "-c", "commit.gpgsign=true",
        ])
        identity = f"{SIGNER_NAME} <{SIGNER_EMAIL}> "
        for variable in ("GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"):
            if not commands.git("var", variable).stdout.startswith(identity):
                raise ValueError("Environment overrides the required benchmark signing identity")
        yield
    finally:
        commands.git_config = original_config
        shutil.rmtree(directory)


def pull_requests(commands: Commands) -> list[dict]:
    fields = "number,state,title,body,headRefOid,baseRefName,headRefName,isCrossRepository,mergeCommit,autoMergeRequest"
    result = json.loads(commands.gh("pr", "list", "--repo", commands.repository,
                                    "--head", BRANCH, "--base", BASE_BRANCH,
                                    "--state", "all", "--limit", "100", "--json", fields))
    if not isinstance(result, list):
        raise ValueError("Unexpected pull request response")
    return result


def existing_pull(requests: list[dict], head: str, base: str, commands: Commands) -> dict | None:
    """Recognize only an open automation PR or an exactly matching merged PR."""
    active = [pr for pr in requests if pr["state"] == "OPEN"]
    if len(active) > 1:
        raise ValueError("Multiple open benchmark pull requests")
    candidates = active or [pr for pr in requests if pr["headRefOid"] == head and pr["state"] == "MERGED"]
    if not candidates:
        if any(pr["headRefOid"] == head and pr["state"] == "CLOSED" for pr in requests):
            raise ValueError("Benchmark pull request was intentionally closed without merging")
        # A previous push may have succeeded while PR creation failed. The caller
        # verifies the entire branch's signatures and exact marker-only diff
        # before this orphan can be reused or published.
        return None
    pr = candidates[0]
    if (pr["headRefOid"] != head or pr["baseRefName"] != BASE_BRANCH
            or pr["headRefName"] != BRANCH or pr["isCrossRepository"]
            or pr["title"] != TITLE or pr["body"]):
        raise ValueError("Existing benchmark pull request does not match the dedicated automation")
    if pr["state"] == "MERGED":
        merged = (pr.get("mergeCommit") or {}).get("oid", "")
        if not SHA_PATTERN.fullmatch(merged) or not commands.ancestor(merged, base):
            raise ValueError("Previous benchmark merge is not contained in current main")
        return None
    return pr


def remote_benchmark_head(commands: Commands) -> str | None:
    found = commands.git("ls-remote", "--heads", commands.remote, "refs/heads/" + BRANCH).stdout.strip()
    if not found:
        return None
    entries = found.splitlines()
    if len(entries) != 1 or len(entries[0].split()) != 2:
        raise ValueError("Unexpected benchmark branch response")
    sha, name = entries[0].split()
    if not SHA_PATTERN.fullmatch(sha) or name != "refs/heads/" + BRANCH:
        raise ValueError("Unexpected benchmark branch identity")
    commands.git("fetch", "--no-tags", commands.remote, "refs/heads/" + BRANCH + ":" + BENCHMARK_REF)
    if commands.revision(BENCHMARK_REF) != sha:
        raise ValueError("Benchmark branch moved during publication; retry later")
    return sha


def require_fresh_report(commands: Commands, report: dict, source: str, base: str) -> None:
    provenance = report["provenance"]
    if provenance["repository"] != commands.repository:
        raise ValueError("Benchmark report belongs to a different repository")
    if provenance["source_sha"] != source or not commands.ancestor(source, base):
        raise ValueError("Benchmark report is not from an ancestor of current main")
    expected = reporting.fingerprint_inputs(
        commands.repo, base, provenance["go_version"], provenance["runner"],
        provenance["image_os"], provenance["image_version"],
    )
    if provenance["input_fingerprint"] != expected:
        raise ValueError("Benchmark inputs changed after measurement; rerun benchmarks")


def prepare_commit(commands: Commands, report: dict, base: str, previous: str | None) -> str:
    """Append a signed result update, merging main when necessary without rebasing."""
    if previous and not commands.ancestor(previous, base):
        commands.git("checkout", "--detach", previous)
        if not commands.ancestor(base, previous):
            result = commands.git("merge", "--no-ff", "--no-commit", base, check=False)
            if result.returncode:
                conflicts = paths(commands.git("diff", "--name-only", "--diff-filter=U", "-z").stdout)
                if result.returncode != 1 or conflicts != {"README.md"}:
                    commands.git("merge", "--abort", check=False)
                    raise ValueError("Merging main conflicts outside the generated README block")
    else:
        commands.git("checkout", "--detach", base)
    generated = reporting.replace_report(commands.readme(base),
                                         reporting.render_report(report["benchmarks"], report["provenance"]))
    target = commands.repo / "README.md"
    target.write_text(generated, encoding="utf-8", newline="")
    commands.git("add", "--", "README.md")
    changed = paths(commands.git("diff", "--cached", "--name-only", "-z", base, "--").stdout)
    if changed - {"README.md"}:
        raise ValueError("Staged benchmark publication changes files other than README.md")
    # A merge may bring in unrelated existing main changes. Check the actual PR
    # delta, preserving even CRLF prose already present on the current base.
    commands.git("diff", "--cached", "--check", base, "--", "README.md")
    pending_merge = commands.git("rev-parse", "--verify", "MERGE_HEAD", check=False).returncode == 0
    pending_changes = commands.git("diff", "--cached", "--quiet", check=False)
    if pending_changes.returncode not in (0, 1):
        raise RuntimeError("Could not inspect staged benchmark update")
    if pending_merge or pending_changes.returncode == 1:
        commands.git("commit", "-S", "-m", TITLE)
    head = commands.revision("HEAD")
    require_readme_only(commands, base, head)
    if head != base:
        verify_commit(commands, head)
    if previous and not commands.ancestor(previous, head):
        raise ValueError("Benchmark update would require a force push")
    return head


def publish(commands: Commands, report: dict, source: str) -> str:
    reporting.validate_provenance(report["provenance"])
    reporting.render_report(report["benchmarks"], report["provenance"])
    if not SHA_PATTERN.fullmatch(source):
        raise ValueError("Expected an exact benchmark source SHA")
    if commands.git("status", "--porcelain").stdout.strip():
        raise ValueError("Publisher checkout must be clean")
    base = commands.fetch_main()
    require_fresh_report(commands, report, source, base)
    requests = pull_requests(commands)
    previous = remote_benchmark_head(commands)
    active = None
    if previous:
        active = existing_pull(requests, previous, base, commands)
        common = commands.git("merge-base", base, previous).stdout.strip()
        require_readme_only(commands, common, previous)
    elif any(pr["state"] == "OPEN" for pr in requests):
        raise ValueError("Open benchmark PR exists without its expected branch")
    with signing(commands):
        if previous:
            for revision in commands.git("rev-list", base + ".." + previous).stdout.splitlines():
                verify_commit(commands, revision)
        head = prepare_commit(commands, report, base, previous)
    if head == base:
        return "README benchmark results already match main"
    if commands.fetch_main() != base:
        raise ValueError("Main moved during publication; rerun before pushing")
    if active and active.get("autoMergeRequest"):
        commands.gh("pr", "merge", str(active["number"]), "--repo", commands.repository, "--disable-auto")
    commands.git("push", commands.remote, "HEAD:refs/heads/" + BRANCH)
    if not active:
        commands.gh("pr", "create", "--repo", commands.repository, "--base", BASE_BRANCH,
                    "--head", BRANCH, "--title", TITLE, "--body", "")
    current = [pr for pr in pull_requests(commands) if pr["state"] == "OPEN"]
    if len(current) != 1:
        raise ValueError("Expected exactly one open benchmark pull request after pushing")
    active = existing_pull(current, head, base, commands)
    if commands.fetch_main() != base or not commands.ancestor(base, head):
        raise ValueError("Main moved before auto-merge; benchmark PR remains pending")
    commands.gh("pr", "merge", str(active["number"]), "--repo", commands.repository,
                "--auto", "--squash", "--match-head-commit", head,
                "--subject", TITLE, "--body", "", "--author-email", SIGNER_EMAIL)
    return f"Benchmark PR #{active['number']} is ready for required checks and auto-merge"


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repo", type=Path, default=Path("."))
    parser.add_argument("--repository", default=os.environ.get("GITHUB_REPOSITORY", ""))
    parser.add_argument("--source-sha", required=True)
    parser.add_argument("--artifact-dir", required=True, type=Path)
    args = parser.parse_args()
    try:
        if os.environ.get("GITHUB_REF") != "refs/heads/main":
            raise ValueError("Benchmark publication is restricted to the main branch")
        if os.environ.get("GITHUB_EVENT_NAME") not in ("schedule", "workflow_dispatch"):
            raise ValueError("Benchmark publication requires a scheduled or manual workflow")
        if args.source_sha != os.environ.get("GITHUB_SHA"):
            raise ValueError("Benchmark source must match this workflow's source SHA")
        if os.environ.get("BENCHMARK_README_PUBLISH") != "true":
            raise ValueError("Benchmark publication has not been enabled")
        if not os.environ.get("GH_TOKEN"):
            raise ValueError("Missing repository-scoped benchmark GitHub App token")
        report = reporting.load_report(args.artifact_dir)
        expected_url = f"https://github.com/{args.repository}/actions/runs/{os.environ.get('GITHUB_RUN_ID', '')}"
        if report["provenance"]["workflow_url"] != expected_url:
            raise ValueError("Benchmark report is not from this workflow run")
        print(publish(Commands(args.repo, args.repository), report, args.source_sha))
    except (ValueError, RuntimeError, OSError, KeyError, subprocess.TimeoutExpired) as error:
        parser.exit(1, f"Benchmark publication stopped: {error}\n")


if __name__ == "__main__":
    main()
