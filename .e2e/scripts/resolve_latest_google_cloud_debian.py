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

"""Resolve the newest Debian family Google Cloud and distroless both support."""

from __future__ import annotations

import json
import re
import shutil
import subprocess
import sys
from dataclasses import dataclass


FAMILY_PATTERN = re.compile(r"^debian-(\d+)$")
CODENAME_PATTERN = re.compile(r"debian-(\d+)-([a-z0-9-]+)$")
IMAGE_NAME_PATTERN = re.compile(r"^debian-(\d+)-([a-z0-9-]+)-v\d+")
GCLOUD_CANDIDATES = ("gcloud", "gcloud.cmd", "gcloud.exe")


@dataclass(frozen=True)
class DebianCandidate:
    family: str
    version: int
    codename: str


def run_gcloud(*args: str) -> subprocess.CompletedProcess[str]:
    gcloud_binary = next((shutil.which(candidate) for candidate in GCLOUD_CANDIDATES if shutil.which(candidate)), None)
    if gcloud_binary is None:
        raise RuntimeError("gcloud CLI was not found in PATH")

    return subprocess.run(
        [gcloud_binary, *args],
        text=True,
        capture_output=True,
        check=False,
    )


def available_families() -> list[str]:
    result = run_gcloud(
        "compute",
        "images",
        "list",
        "--project=debian-cloud",
        "--filter=family~'^debian-[0-9]+$'",
        "--format=value(family)",
    )
    if result.returncode != 0:
        raise RuntimeError(result.stderr.strip() or result.stdout.strip())

    families = {
        line.strip()
        for line in result.stdout.splitlines()
        if FAMILY_PATTERN.match(line.strip())
    }
    return sorted(
        families,
        key=lambda family: int(FAMILY_PATTERN.match(family).group(1)),  # type: ignore[union-attr]
        reverse=True,
    )


def describe_family(family: str) -> DebianCandidate:
    result = run_gcloud(
        "compute",
        "images",
        "describe-from-family",
        family,
        "--project=debian-cloud",
        "--format=json(name,licenses)",
    )
    if result.returncode != 0:
        raise RuntimeError(result.stderr.strip() or result.stdout.strip())

    data = json.loads(result.stdout)
    name = data.get("name", "")
    licenses = data.get("licenses", [])

    match = IMAGE_NAME_PATTERN.match(name) if isinstance(name, str) else None
    if match is None:
        for license_url in licenses:
            if not isinstance(license_url, str):
                continue
            tail = license_url.rstrip("/").split("/")[-1]
            match = CODENAME_PATTERN.match(tail)
            if match is not None:
                break

    if match is None:
        raise RuntimeError(f"could not derive Debian codename for family {family}")

    version = int(match.group(1))
    codename = match.group(2)
    return DebianCandidate(family=family, version=version, codename=codename)


def distroless_repo_exists(repository: str) -> bool:
    result = run_gcloud(
        "container",
        "images",
        "list-tags",
        repository,
        "--limit=1",
        "--format=value(digest)",
    )
    return result.returncode == 0 and bool(result.stdout.strip())


def main() -> int:
    try:
        for family in available_families():
            candidate = describe_family(family)
            distroless_tag = f"debian{candidate.version}"
            static_repo = f"gcr.io/distroless/static-{distroless_tag}"
            base_repo = f"gcr.io/distroless/base-{distroless_tag}"

            if distroless_repo_exists(static_repo) and distroless_repo_exists(base_repo):
                print(f"DEBIAN_FAMILY={candidate.family}")
                print(f"DEBIAN_VERSION={candidate.version}")
                print(f"DEBIAN_CODENAME={candidate.codename}")
                print(f"DISTROLESS_TAG={distroless_tag}")
                return 0
    except Exception as exc:
        print(f"failed to resolve latest Google Cloud Debian family: {exc}", file=sys.stderr)
        return 1

    print(
        "failed to find a Debian family with matching distroless static/base images",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
