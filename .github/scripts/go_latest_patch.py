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
"""Resolve the most recent Go patch release for a major/minor input."""

from __future__ import annotations

import json
import sys
import urllib.request
from typing import Iterable, Optional, Tuple


GO_RELEASES_URL = "https://go.dev/dl/?mode=json&include=all"


class _VersionError(Exception):
    """Raised when the provided Go version string is invalid."""


def _parse_numeric_parts(version: str) -> Optional[Tuple[int, ...]]:
    """Return the numeric tuple for a dotted version string.

    Non-numeric sections invalidate the version and cause ``None`` to be
    returned, which signals that the version should be ignored.
    """

    parts: Iterable[str] = version.split(".")
    values = []
    for part in parts:
        if not part.isdigit():
            return None
        values.append(int(part))
    return tuple(values)


def _latest_patch_for_major_minor(go_version: str) -> str:
    """Return the most recent patch version for the provided Go version.

    ``go_version`` must be in the format ``MAJOR.MINOR`` or ``MAJOR.MINOR.PATCH``.
    The returned value omits the ``go`` prefix, matching the format expected by
    ``actions/setup-go``.
    """

    sections = go_version.split(".")
    if len(sections) < 2 or not all(part.isdigit() for part in sections[:2]):
        raise _VersionError("Go version must start with 'MAJOR.MINOR'")

    major_minor = ".".join(sections[:2])
    prefix = f"go{major_minor}"

    try:
        with urllib.request.urlopen(GO_RELEASES_URL, timeout=10) as response:
            releases = json.load(response)
    except Exception as exc:  # pragma: no cover - network failures are non-deterministic.
        raise _VersionError(str(exc)) from exc

    latest_numeric: Optional[Tuple[int, ...]] = None
    latest_version = go_version

    for release in releases:
        version = release.get("version", "")
        if not version.startswith(prefix):
            continue

        numeric_part = version[len("go") :]
        numeric_tuple = _parse_numeric_parts(numeric_part)
        if numeric_tuple is None:
            continue

        if latest_numeric is None or numeric_tuple > latest_numeric:
            latest_numeric = numeric_tuple
            latest_version = numeric_part

    return latest_version


def main() -> int:
    if len(sys.argv) != 2:
        print("Usage: go_latest_patch.py <go version>", file=sys.stderr)
        return 2

    go_version = sys.argv[1].strip()
    if not go_version:
        print("Go version cannot be empty", file=sys.stderr)
        return 2

    try:
        print(_latest_patch_for_major_minor(go_version))
    except _VersionError:
        print(go_version)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
