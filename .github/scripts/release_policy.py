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

"""Validate immutable release subjects and resume their publication."""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path


VERSION = re.compile(
    r'^(?:var|const) Version = "(v(0|1)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*))"$',
    re.MULTILINE,
)


def version_of(source: str) -> str:
    matches = list(VERSION.finditer(source))
    if len(matches) != 1:
        raise ValueError("Expected one canonical v0/v1 Version declaration")
    return matches[0][1]


def git(*args: str) -> str:
    return subprocess.run(
        ["git", *args], check=True, capture_output=True, text=True
    ).stdout.strip()


def plan(event: str, ref: str, sha: str, requested: str = "") -> dict[str, str]:
    if event not in {"push", "workflow_dispatch"} or ref != "refs/heads/main":
        raise ValueError("Releases must use a main-branch push or manual run")
    if not re.fullmatch(r"[0-9a-f]{40}", sha) or git("rev-parse", "HEAD") != sha:
        raise ValueError("Checkout does not match the immutable workflow SHA")
    current = version_of(git("show", f"{sha}:version.go"))
    parent = git("rev-parse", f"{sha}^")
    previous = version_of(git("show", f"{parent}:version.go"))
    if requested and requested != current:
        raise ValueError("Requested release does not match the source Version")
    if current == previous:
        if event == "workflow_dispatch":
            raise ValueError(
                "This commit has no Version transition; rerun the original release workflow"
            )
        return {"should_release": "false"}
    if tuple(map(int, current[1:].split("."))) <= tuple(
        map(int, previous[1:].split("."))
    ):
        raise ValueError("A release must advance the previous mainline Version")
    return {"should_release": "true", "version": current}


class ApiError(RuntimeError):
    def __init__(self, status: int):
        super().__init__(f"GitHub API request failed (HTTP {status})")
        self.status = status


class GitHub:
    def __init__(self, repository: str, token: str):
        if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repository):
            raise ValueError("Invalid repository")
        self.base = f"https://api.github.com/repos/{repository}"
        self.token = token

    def request(self, path: str, body: dict | None = None) -> dict:
        request = urllib.request.Request(
            self.base + path,
            data=json.dumps(body).encode() if body is not None else None,
            headers={
                "Authorization": f"Bearer {self.token}",
                "Accept": "application/vnd.github+json",
                "X-GitHub-Api-Version": "2022-11-28",
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                result = json.load(response)
        except urllib.error.HTTPError as error:
            raise ApiError(error.code) from error
        except (urllib.error.URLError, TimeoutError) as error:
            raise ApiError(503) from error
        if not isinstance(result, dict):
            raise ValueError("GitHub returned a non-object response")
        return result

    def get(self, path: str, *, allow_missing: bool = False) -> dict | None:
        for attempt in range(3):
            try:
                return self.request(path)
            except ApiError as error:
                if error.status == 404 and allow_missing:
                    return None
                if error.status not in {429, 500, 502, 503, 504} or attempt == 2:
                    raise
                time.sleep(2**attempt)
        raise AssertionError("Unreachable")


def verify_tag(
    client: GitHub, version: str, sha: str, *, allow_missing: bool = False
) -> bool:
    ref = client.get(f"/git/ref/tags/{version}", allow_missing=allow_missing)
    if ref is None:
        return False
    if ref.get("object", {}).get("type") != "tag":
        raise ValueError("Release tag must be annotated")
    tag = client.get(f"/git/tags/{ref['object']['sha']}")
    target = (tag or {}).get("object", {})
    if target.get("type") != "commit" or target.get("sha") != sha:
        raise ValueError("Release tag does not target the immutable release commit")
    verification = tag.get("verification", {})
    if (
        verification.get("verified") is not True
        or verification.get("reason") != "valid"
    ):
        raise ValueError("GitHub has not verified the release tag signature")
    return True


def publish(client: GitHub, version: str, sha: str) -> None:
    verify_tag(client, version, sha)
    for attempt in range(3):
        existing = client.get(f"/releases/tags/{version}", allow_missing=True)
        if existing is not None:
            if (
                existing.get("tag_name") != version
                or existing.get("draft") is not False
            ):
                raise ValueError(
                    "Existing release is not the expected published release"
                )
            return
        try:
            client.request(
                "/releases",
                {
                    "tag_name": version,
                    "target_commitish": sha,
                    "name": version,
                    "generate_release_notes": True,
                    "draft": False,
                    "prerelease": False,
                    "make_latest": "legacy",
                },
            )
        except ApiError as error:
            if error.status not in {422, 429, 500, 502, 503, 504}:
                raise
            if attempt == 2:
                raise
            time.sleep(2**attempt)
            continue
        return


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "operation", choices=["plan", "tag-state", "verify-tag", "publish"]
    )
    args = parser.parse_args()
    if args.operation == "plan":
        outputs = plan(
            os.environ["GITHUB_EVENT_NAME"],
            os.environ["GITHUB_REF"],
            os.environ["GITHUB_SHA"],
            os.environ.get("INPUT_VERSION", ""),
        )
    else:
        client = GitHub(os.environ["GITHUB_REPOSITORY"], os.environ["GH_TOKEN"])
        version, sha = os.environ["VERSION"], os.environ["GITHUB_SHA"]
        if not re.fullmatch(r"v[01]\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", version):
            raise ValueError("Invalid release version")
        if args.operation == "publish":
            publish(client, version, sha)
            outputs = {}
        else:
            exists = verify_tag(
                client, version, sha, allow_missing=args.operation == "tag-state"
            )
            outputs = {"tag_exists": str(exists).lower()}
    if os.environ.get("GITHUB_OUTPUT"):
        with Path(os.environ["GITHUB_OUTPUT"]).open("a", encoding="utf-8") as output:
            for key, value in outputs.items():
                output.write(f"{key}={value}\n")


if __name__ == "__main__":
    main()
