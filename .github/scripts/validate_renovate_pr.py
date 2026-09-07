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

"""Validate Renovate's module scope and release marker against the current base."""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path, PurePosixPath
import re
import subprocess


VERSION = re.compile(
    r'^(?:var|const) Version = "v(?P<version>(?:0|[1-9]\d*)\.'
    r'(?:0|[1-9]\d*)\.(?:0|[1-9]\d*))"$',
    re.MULTILINE,
)
RENOVATE_AUTHORS = {"renovate[bot]", "app/renovate"}
SECURITY = re.compile(r"security|vulnerab|cve|ghsa", re.IGNORECASE)


def one_directive(source: str, directive: str) -> str:
    values = re.findall(
        rf"(?m)^[ \t]*{re.escape(directive)}[ \t]+(\S+)[ \t]*(?://.*)?$", source
    )
    if len(values) != 1:
        raise ValueError(f"Expected exactly one {directive} directive")
    return values[0]


def requirements(source: str) -> dict[str, str]:
    result: dict[str, str] = {}
    in_require = False
    for original in source.splitlines():
        line = original.split("//", 1)[0].strip()
        if line == "require (":
            in_require = True
            continue
        if in_require and line == ")":
            in_require = False
            continue
        if not line:
            continue
        if line.startswith("require "):
            line = line.removeprefix("require ").strip()
        elif not in_require:
            continue
        fields = line.split()
        if len(fields) != 2 or fields[0] in result:
            raise ValueError(f"Unsupported or duplicate require entry: {original}")
        result[fields[0]] = fields[1]
    if in_require:
        raise ValueError("Unclosed require block")
    return result


def version(source: str) -> tuple[int, int, int]:
    matches = list(VERSION.finditer(source))
    if len(matches) != 1:
        raise ValueError("Expected one canonical public Version declaration")
    return tuple(int(part) for part in matches[0].group("version").split("."))


def inspect_candidate(
    base_module: str,
    head_module: str,
    base_version: str,
    head_version: str,
    changed_paths: list[str],
    security_hint: bool,
    examples_dir: str = ".examples",
) -> str:
    if one_directive(base_module, "module") != one_directive(head_module, "module"):
        raise ValueError("Renovate must not change the root module identity")
    if one_directive(base_module, "go") != one_directive(head_module, "go"):
        raise ValueError("Renovate must preserve the root Go compatibility directive")

    before, after = requirements(base_module), requirements(head_module)
    changed_requirements = {
        name for name, selected in after.items() if before.get(name) != selected
    }
    root_requirements_changed = before != after
    previous_version, candidate_version = version(base_version), version(head_version)

    if not root_requirements_changed:
        if head_version != base_version:
            raise ValueError(
                "An update without a root dependency repair must not change version.go"
            )
        return "non_releasing"

    if not security_hint or not changed_requirements:
        raise ValueError(
            "Root dependency changes require a security repair, not routine minimum-version updates"
        )

    def allowed_path(path: str) -> bool:
        if path in {"go.mod", "go.sum", "version.go"}:
            return True
        parsed = PurePosixPath(path)
        return (
            path.startswith(examples_dir.rstrip("/") + "/")
            and len(parsed.parts) >= 3
            and parsed.name in {"go.mod", "go.sum"}
        )

    unexpected = [path for path in changed_paths if not allowed_path(path)]
    if unexpected:
        raise ValueError(f"Root security repair contains unrelated files: {unexpected}")
    expected_version = (*previous_version[:2], previous_version[2] + 1)
    if candidate_version != expected_version:
        raise ValueError(
            f"Root security repair must prepare next patch {expected_version}, got {candidate_version}"
        )

    def without_version_value(source: str) -> str:
        return VERSION.sub(
            lambda match: match.group(0).replace(match.group("version"), "VERSION"),
            source,
        )

    if without_version_value(base_version) != without_version_value(head_version):
        raise ValueError(
            "A security release may change only the Version value in version.go"
        )
    return "security_patch"


def git(*args: str) -> str:
    return subprocess.run(
        ["git", *args], check=True, capture_output=True, text=True, encoding="utf-8"
    ).stdout


def validate_event(event: dict, base: str, examples_dir: str = ".examples") -> str:
    pr = event.get("pull_request")
    if (
        not isinstance(pr, dict)
        or pr.get("user", {}).get("login") not in RENOVATE_AUTHORS
    ):
        return "not_renovate"
    repository = str(event.get("repository", {}).get("full_name", "")).lower()
    base_repository = str(
        pr.get("base", {}).get("repo", {}).get("full_name", "")
    ).lower()
    head_repository = str(
        (pr.get("head", {}).get("repo") or {}).get("full_name", "")
    ).lower()
    if not repository or repository != base_repository or repository != head_repository:
        raise ValueError("Renovate validation requires a same-repository pull request")
    if pr.get("base", {}).get("ref") != "main":
        raise ValueError("Renovate validation expects a main-targeting pull request")
    head = pr.get("head", {}).get("sha", "")
    if not re.fullmatch(r"[a-f0-9]{40}", base) or not re.fullmatch(
        r"[a-f0-9]{40}", head
    ):
        raise ValueError("Expected exact current-base and candidate commit SHAs")
    if git("rev-parse", "HEAD").strip() != head:
        raise ValueError("The checkout is not the event's exact candidate")
    git("merge-base", "--is-ancestor", base, head)
    changed_paths = (
        git("diff", "--name-only", "--no-renames", "-z", base, head)
        .rstrip("\0")
        .split("\0")
    )
    labels = [label.get("name", "") for label in pr.get("labels", [])]
    hint = any(
        SECURITY.search(value)
        for value in [pr.get("title", ""), pr["head"].get("ref", ""), *labels]
    )
    return inspect_candidate(
        git("show", f"{base}:go.mod"),
        git("show", f"{head}:go.mod"),
        git("show", f"{base}:version.go"),
        git("show", f"{head}:version.go"),
        changed_paths,
        hint,
        examples_dir,
    )


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base", required=True)
    parser.add_argument("--event", default=os.environ.get("GITHUB_EVENT_PATH"))
    parser.add_argument("--examples-dir", default=".examples")
    args = parser.parse_args()
    if not args.event:
        parser.error("--event or GITHUB_EVENT_PATH is required")
    try:
        event = json.loads(Path(args.event).read_text(encoding="utf-8"))
        result = validate_event(event, args.base, args.examples_dir)
    except (ValueError, OSError, subprocess.CalledProcessError) as error:
        print(f"Renovate candidate validation failed: {error}")
        return 1
    print(f"Renovate candidate validation: {result} (base={args.base})")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
