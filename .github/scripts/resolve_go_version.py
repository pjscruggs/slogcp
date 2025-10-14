#!/usr/bin/env python3
# Copyright 2025 Patrick J. Scruggs
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

"""Resolve the appropriate Go toolchain version for the workflow."""

from __future__ import annotations

import json
import os
import sys
import urllib.request
from typing import Iterable, Optional, Tuple


GO_RELEASES_URL = "https://go.dev/dl/?mode=json&include=all"


def parse_numeric_parts(version: str) -> Optional[Tuple[int, ...]]:
    parts: Iterable[str] = version.split(".")
    values = []
    for part in parts:
        if not part.isdigit():
            return None
        values.append(int(part))
    return tuple(values)


def select_version(
    go_version: str,
    releases: list[dict[str, object]],
) -> dict[str, object]:
    sections = go_version.split(".")
    if len(sections) < 2 or not all(part.isdigit() for part in sections[:2]):
        return {"status": "error", "version": go_version, "message": "unparseable go directive"}

    requested_major_minor = ".".join(sections[:2])
    prefix = f"go{requested_major_minor}"

    latest_patch: Optional[str] = None
    latest_patch_numeric: Optional[Tuple[int, ...]] = None
    stable_release: Optional[Tuple[Tuple[int, ...], str]] = None
    oldstable_release: Optional[Tuple[Tuple[int, ...], str]] = None
    stable_major_minor: Optional[Tuple[int, int]] = None

    for release in releases:
        version = release.get("version", "")
        if not isinstance(version, str) or not version.startswith("go"):
            continue

        numeric_part = version[2:]
        numeric_tuple = parse_numeric_parts(numeric_part)
        if numeric_tuple is None or len(numeric_tuple) < 2:
            continue

        current_major_minor = (numeric_tuple[0], numeric_tuple[1])
        is_stable = bool(release.get("stable", False))

        if is_stable:
            if stable_release is None:
                stable_release = (numeric_tuple, numeric_part)
                stable_major_minor = current_major_minor
            elif oldstable_release is None and current_major_minor != stable_major_minor:
                oldstable_release = (numeric_tuple, numeric_part)

        if is_stable and version.startswith(prefix):
            if latest_patch_numeric is None or numeric_tuple > latest_patch_numeric:
                latest_patch_numeric = numeric_tuple
                latest_patch = numeric_part

        if latest_patch is not None and stable_release is not None and oldstable_release is not None:
            break

    if latest_patch is not None:
        return {
            "status": "resolved",
            "requested": requested_major_minor,
            "version": latest_patch,
        }

    if stable_release is not None:
        payload: dict[str, object] = {
            "status": "fallback",
            "alias": "stable",
            "requested": requested_major_minor,
            "version": stable_release[1],
        }
        if oldstable_release is not None:
            payload["oldstable"] = oldstable_release[1]
        return payload

    return {"status": "error", "version": go_version, "message": "no stable releases discovered"}


def main() -> int:
    go_version = os.environ.get("GO_VERSION", "").strip()
    if not go_version:
        print("::warning::Go version cannot be determined; using workflow default.")
        return 1

    try:
        with urllib.request.urlopen(GO_RELEASES_URL, timeout=10) as response:
            releases = json.load(response)
    except Exception as exc:  # pragma: no cover - network access is non-deterministic.
        print(f"::warning::Failed to query Go releases: {exc}; using go {go_version}.")
        result = {"status": "error", "version": go_version}
    else:
        result = select_version(go_version, releases)

    status = result.get("status")
    version_value = str(result.get("version", go_version))

    if status == "resolved":
        requested = result.get("requested", go_version)
        print(f"Resolved go {requested} to go{version_value}.")
    elif status == "fallback":
        requested = result.get("requested", go_version)
        alias = result.get("alias", "stable")
        resolved = result.get("version")
        oldstable = result.get("oldstable")

        message = f"Go {requested} not released yet; falling back to {alias}"
        if isinstance(resolved, str):
            message += f" (go{resolved})"
        if isinstance(oldstable, str) and alias != "oldstable":
            message += f"; oldstable currently go{oldstable}"
        print(f"::warning::{message}.")
        version_value = str(alias)
    else:
        detail = result.get("message")
        if detail:
            print(f"::warning::{detail}; using go {go_version}.")
        version_value = go_version

    output_path = os.environ.get("GITHUB_OUTPUT")
    if not output_path:
        print("GITHUB_OUTPUT environment variable is not set.", file=sys.stderr)
        return 1

    with open(output_path, "a", encoding="utf-8") as handle:
        handle.write(f"version={version_value}\n")

    print(f"Using Go toolchain input: {version_value}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
