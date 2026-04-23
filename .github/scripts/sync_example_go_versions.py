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

"""Keep .examples Go module directives on the latest stable Go release."""

from __future__ import annotations

import argparse
import os
import re
import subprocess
import sys
import urllib.request
from pathlib import Path


GO_VERSION_URL = "https://go.dev/VERSION?m=text"
GO_DIRECTIVE_RE = re.compile(r"^\s*go\s+(\S+)\s*$")
LATEST_GO_RE = re.compile(r"^go(\d+\.\d+(?:\.\d+)?)\b")


def latest_go_version() -> str:
    """Return the latest stable Go version from go.dev without the go prefix."""
    with urllib.request.urlopen(GO_VERSION_URL, timeout=15) as response:
        payload = response.read().decode("utf-8", errors="replace")

    first_line = payload.splitlines()[0] if payload.splitlines() else ""
    match = LATEST_GO_RE.match(first_line.strip())
    if not match:
        raise ValueError(f"unexpected Go version response from {GO_VERSION_URL}: {payload!r}")
    return match.group(1)


def example_go_mods(repo_root: Path) -> list[Path]:
    """Return checked-in example go.mod files."""
    examples_root = repo_root / ".examples"
    if not examples_root.is_dir():
        return []
    return sorted(examples_root.rglob("go.mod"))


def go_directive_version(go_mod: Path) -> str | None:
    """Return the go directive version from a go.mod file."""
    for line in go_mod.read_text(encoding="utf-8").splitlines():
        match = GO_DIRECTIVE_RE.match(line)
        if match:
            return match.group(1)
    return None


def run_go_command(command: list[str], cwd: Path) -> None:
    """Run a go command in a module directory."""
    env = os.environ.copy()
    env.setdefault("GOTOOLCHAIN", "auto")
    completed = subprocess.run(command, cwd=cwd, env=env, text=True, check=False)
    if completed.returncode != 0:
        raise RuntimeError(f"{' '.join(command)} failed in {cwd}")


def sync_go_mod(go_mod: Path, version: str) -> None:
    """Update one example module to the requested Go version."""
    module_dir = go_mod.parent
    run_go_command(["go", "mod", "edit", f"-go={version}"], module_dir)
    run_go_command(["go", "mod", "tidy"], module_dir)


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--check", action="store_true", help="fail instead of updating stale files")
    parser.add_argument("--version", help="override the latest Go version; useful for tests")
    args = parser.parse_args()

    repo_root = Path(__file__).resolve().parents[2]
    try:
        target_version = args.version or latest_go_version()
    except Exception as exc:
        print(f"ERROR: failed to resolve latest Go version: {exc}", file=sys.stderr)
        return 1

    go_mods = example_go_mods(repo_root)
    if not go_mods:
        print("No .examples go.mod files found; skipping.")
        return 0

    stale: list[tuple[Path, str | None]] = []
    for go_mod in go_mods:
        current = go_directive_version(go_mod)
        if current != target_version:
            stale.append((go_mod, current))

    if args.check:
        if not stale:
            print(f"All .examples go.mod files use go {target_version}.")
            return 0

        for go_mod, current in stale:
            shown = current or "<missing>"
            print(f"{go_mod.relative_to(repo_root)} uses go {shown}; want go {target_version}", file=sys.stderr)
        return 1

    for go_mod, current in stale:
        shown = current or "<missing>"
        print(f"Updating {go_mod.relative_to(repo_root)} from go {shown} to go {target_version}")
        try:
            sync_go_mod(go_mod, target_version)
        except Exception as exc:
            print(f"ERROR: failed to update {go_mod}: {exc}", file=sys.stderr)
            return 1

    if not stale:
        print(f"All .examples go.mod files already use go {target_version}.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
