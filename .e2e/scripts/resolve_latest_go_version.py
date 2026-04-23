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

"""Resolve the latest stable Go release from go.dev."""

from __future__ import annotations

import json
import sys
import urllib.request
from typing import Iterable


GO_RELEASES_URL = "https://go.dev/dl/?mode=json&include=all"


def parse_version(version: str) -> tuple[int, ...] | None:
    parts: Iterable[str] = version.split(".")
    values = []
    for part in parts:
        if not part.isdigit():
            return None
        values.append(int(part))
    return tuple(values)


def main() -> int:
    try:
        with urllib.request.urlopen(GO_RELEASES_URL, timeout=10) as response:
            releases = json.load(response)
    except Exception as exc:
        print(f"failed to query {GO_RELEASES_URL}: {exc}", file=sys.stderr)
        return 1

    latest_version = None
    latest_numeric = None

    for release in releases:
        if not release.get("stable", False):
            continue

        version = release.get("version")
        if not isinstance(version, str) or not version.startswith("go"):
            continue

        numeric_part = version[2:]
        numeric = parse_version(numeric_part)
        if numeric is None:
            continue

        if latest_numeric is None or numeric > latest_numeric:
            latest_numeric = numeric
            latest_version = numeric_part

    if latest_version is None:
        print("no stable Go release discovered", file=sys.stderr)
        return 1

    print(latest_version)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
