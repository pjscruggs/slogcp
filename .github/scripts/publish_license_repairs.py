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

"""Authorize, independently reproduce, and optionally sign header-only repairs.

Execute only from trusted main. Candidate files and artifact JSON are data;
candidate code, hooks, filters and tools are never executed. Never merge or
write public comments. Publishing is explicitly disabled unless configured.
"""

from __future__ import annotations

import argparse
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import zipfile

import publish_readme_benchmarks as signing_policy
import validate_license_repairs as repairs


def authority(commands: signing_policy.Commands, run_id: str) -> tuple[dict, dict] | None:
    if not run_id.isdecimal():
        raise ValueError("Invalid workflow run ID")
    run = json.loads(commands.gh("api", f"repos/{commands.repository}/actions/runs/{run_id}"))
    if (run["event"] != "pull_request" or run["status"] != "completed"
            or str(run["id"]) != run_id or not repairs.SHA.fullmatch(run["head_sha"])
            or run["path"] != ".github/workflows/validation_pipeline.yml"
            or run["repository"]["full_name"] != commands.repository):
        return None
    associated = run.get("pull_requests", [])
    # GitHub can return an empty run association even for a PR event. Resolve
    # through the commit endpoint, then require exact live head and branch.
    if not associated:
        candidates = json.loads(commands.gh("api", "--paginate", "--slurp",
                                            f"repos/{commands.repository}/commits/{run['head_sha']}/pulls"))
        associated = [pr for page in candidates for pr in page
                      if pr["state"] == "open" and pr["head"]["sha"] == run["head_sha"]
                      and pr["head"]["ref"] == run["head_branch"]]
    if len(associated) != 1:
        return None
    number = associated[0]["number"]
    if not isinstance(number, int) or number < 1:
        raise ValueError("Invalid associated pull request")
    pr = json.loads(commands.gh("api", f"repos/{commands.repository}/pulls/{number}"))
    if (pr["state"] != "open" or pr["user"]["login"] != "pjscruggs"
            or pr["user"]["type"] != "User" or not pr["head"].get("repo")
            or pr["head"]["repo"]["full_name"] != commands.repository
            or pr["base"]["ref"] != "main" or pr["head"]["ref"] == "main"
            or pr["head"]["sha"] != run["head_sha"]):
        return None
    branch = pr["head"]["ref"]
    if branch.startswith(("renovate/", "dependabot/", "automation/")):
        return None
    commands.git("check-ref-format", "refs/heads/" + branch)
    base = json.loads(commands.gh("api", f"repos/{commands.repository}/branches/main"))["commit"]["sha"]
    if pr["base"]["sha"] != base or commands.revision("HEAD") != base:
        return None
    return run, pr


def proposal(commands: signing_policy.Commands, run: dict) -> bytes | None:
    prefix = f"license-repair-{run['id']}-{run['run_attempt']}-requirements"
    pages = json.loads(commands.gh("api", "--paginate", "--slurp",
                                  f"repos/{commands.repository}/actions/runs/{run['id']}/artifacts"))
    artifacts = [a for page in pages for a in page["artifacts"] if a["name"] == prefix]
    if not artifacts:
        return None
    if len(artifacts) != 1 or artifacts[0]["expired"] or artifacts[0]["size_in_bytes"] > repairs.MAX_BYTES:
        raise ValueError("Ambiguous, expired or oversized license artifact")
    artifact_id = artifacts[0]["id"]
    downloaded = subprocess.run(["gh", "api", f"repos/{commands.repository}/actions/artifacts/{artifact_id}/zip"],
                                env=commands.env, capture_output=True, check=True, timeout=60).stdout
    if len(downloaded) > repairs.MAX_BYTES:
        raise ValueError("Oversized artifact archive")
    with zipfile.ZipFile(io.BytesIO(downloaded)) as archive:
        entries = archive.infolist()
        if len(entries) != 1 or entries[0].filename != "license-repair.json" or entries[0].file_size > repairs.MAX_BYTES:
            raise ValueError("Unexpected license artifact contents")
        return archive.read(entries[0])


def prepare(commands: signing_policy.Commands, args: argparse.Namespace) -> dict | None:
    authorized = authority(commands, args.run_id)
    if authorized is None:
        return None
    run, pr = authorized
    raw = proposal(commands, run)
    if raw is None:
        print("Authorized current PR has no matching license receipt.")
        return None
    head, base = pr["head"]["sha"], pr["base"]["sha"]
    commands.git("fetch", "--no-tags", commands.remote, head)
    with tempfile.TemporaryDirectory(prefix="license-receipt-") as temporary:
        receipt = Path(temporary) / "receipt.json"
        receipt.write_bytes(raw)
        result = repairs.validate(argparse.Namespace(
            receipt=receipt, candidate=commands.repo, trusted_root=commands.repo,
            fixer=args.fixer, head=head, base=base, repository=commands.repository,
            run_id=str(run["id"]), run_attempt=str(run["run_attempt"])))
    result.update(pr_number=pr["number"], branch=pr["head"]["ref"])
    return result


def publish(commands: signing_policy.Commands, args: argparse.Namespace, plan: dict) -> str:
    if os.environ.get("LICENSE_REPAIR_PUBLISH") != "true":
        raise ValueError("License repair publishing is disabled")
    # Reuse the existing expected pjscruggs signer verification/cleanup, never
    # the independently named release-bot identity or its signing key.
    mapping = {"BENCHMARK_SSH_PRIVATE_KEY_B64": "LICENSE_REPAIR_SSH_PRIVATE_KEY_B64",
               "BENCHMARK_SSH_PUBLIC_KEY": "LICENSE_REPAIR_SSH_PUBLIC_KEY",
               "BENCHMARK_SSH_FINGERPRINT": "LICENSE_REPAIR_SSH_FINGERPRINT"}
    original = {name: os.environ.get(name) for name in mapping}
    try:
        for target, source in mapping.items():
            os.environ[target] = os.environ.get(source, "")
        with tempfile.TemporaryDirectory(prefix="license-index-") as temporary:
            commands.env["GIT_INDEX_FILE"] = str(Path(temporary) / "index")
            commands.git("read-tree", plan["candidate_sha"])
            with signing_policy.signing(commands):
                for change in plan["reproduced_changes"]:
                    name = change["path"]
                    blob = subprocess.run(["git", *commands.git_config, "hash-object", "-w", "--stdin"],
                                          cwd=commands.repo, env=commands.env, input=repairs.decode(change, "after"),
                                          capture_output=True, check=True, timeout=60).stdout.decode().strip()
                    mode = commands.git("ls-tree", plan["candidate_sha"], "--", name).stdout.split()[0]
                    commands.git("update-index", "--add", "--cacheinfo", mode, blob, name)
                tree = commands.git("write-tree").stdout.strip()
                commit = commands.git("commit-tree", "-S", tree, "-p", plan["candidate_sha"],
                                      "-m", "fix: normalize license headers").stdout.strip()
                signing_policy.verify_commit(commands, commit)
                if (commands.git("log", "-1", "--format=%P", commit).stdout.strip() != plan["candidate_sha"]
                        or commands.git("log", "-1", "--format=%T", commit).stdout.strip() != tree):
                    raise ValueError("Signed repair does not match planned parent and tree")
                changed_paths = signing_policy.paths(commands.git(
                    "diff", "--name-only", "-z", plan["candidate_sha"], commit).stdout)
                if changed_paths != {change["path"] for change in plan["reproduced_changes"]}:
                    raise ValueError("Signed repair changes paths outside the reproduced proposal")
                # Do not accept partial or changed evidence between stages.
                refreshed = authority(commands, args.run_id)
                if (refreshed is None or refreshed[1]["head"]["sha"] != plan["candidate_sha"]
                        or refreshed[1]["base"]["sha"] != plan["base_sha"]
                        or str(refreshed[0]["run_attempt"]) != plan["run_attempt"]
                        or refreshed[1]["head"]["ref"] != plan["branch"]):
                    raise ValueError("License repair authority was superseded")
                commands.git("push", commands.remote, f"{commit}:refs/heads/{plan['branch']}")
                print("Published one signed header repair; new-head validation is required.")
                return commit
    finally:
        commands.env.pop("GIT_INDEX_FILE", None)
        for name, value in original.items():
            if value is None:
                os.environ.pop(name, None)
            else:
                os.environ[name] = value


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--fixer", type=Path, required=True)
    parser.add_argument("--report", type=Path)
    parser.add_argument("--publish", action="store_true")
    args = parser.parse_args()
    commands = signing_policy.Commands(Path.cwd(), args.repository)
    signing_values = {}
    for name in ("LICENSE_REPAIR_SSH_PRIVATE_KEY_B64", "LICENSE_REPAIR_SSH_PUBLIC_KEY",
                 "LICENSE_REPAIR_SSH_FINGERPRINT"):
        commands.env.pop(name, None)
        # The trusted formatter also runs before signing secrets are restored.
        signing_values[name] = os.environ.pop(name, "")
    plan = prepare(commands, args)
    needed = bool(plan and plan["reproduced_changes"])
    report = {"schema": 1, "run_id": args.run_id, "repository": args.repository,
              "decision": "ineligible" if plan is None else "verified_repairs" if needed else "clean"}
    if plan is not None:
        report.update(candidate_sha=plan["candidate_sha"], base_sha=plan["base_sha"],
                      run_attempt=plan["run_attempt"], pr_number=plan["pr_number"],
                      paths=[change["path"] for change in plan["reproduced_changes"]])
    if output := os.environ.get("GITHUB_OUTPUT"):
        with open(output, "a", encoding="utf-8") as stream:
            stream.write(f"needed={str(needed).lower()}\n")
    if plan is None:
        print("No eligible current header repair.")
    elif not needed:
        print("Authorized current candidate receipt verified: zero header repairs.")
    elif args.publish:
        try:
            os.environ.update(signing_values)
            report["published_commit"] = publish(commands, args, plan)
            report["decision"] = "published"
        finally:
            for name in signing_values:
                os.environ.pop(name, None)
    else:
        print(f"Independently reproduced {len(plan['reproduced_changes'])} header repair(s).")
    if args.report:
        with args.report.open("x", encoding="utf-8") as stream:
            json.dump(report, stream, indent=2)
            stream.write("\n")


if __name__ == "__main__":
    main()
