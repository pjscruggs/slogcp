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

"""Retain the license formatter's exact changes separately from other fixes.

The output is untrusted proposed repair data, never merge or signing authority.
A trusted publisher must independently reproduce and authorize its changes.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import subprocess


def git(*args: str) -> bytes:
    return subprocess.run(["git", *args], check=True, capture_output=True).stdout


def snapshot() -> dict[str, bytes]:
    result = {}
    for raw in git("ls-files", "-z").split(b"\0"):
        if not raw:
            continue
        name = raw.decode("utf-8")
        path = Path(name)
        # Do not follow symlinks or include missing files/submodule directories.
        if path.is_symlink() or not path.is_file():
            continue
        result[name] = path.read_bytes()
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--fixer", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    output = args.output.resolve()
    root = Path(git("rev-parse", "--show-toplevel").decode().strip()).resolve()
    if output == root or root in output.parents:
        raise ValueError("Repair artifacts must be outside the candidate checkout")
    output.mkdir(parents=True, exist_ok=False)
    before = snapshot()
    head = git("rev-parse", "HEAD").decode().strip()
    # Explicit argv; the formatter is data, never interpolated into a shell.
    completed = subprocess.run([args.fixer, "header", "fix", "-c", ".licenserc.yaml"], check=False)
    after = snapshot()
    changes = []
    for name in sorted(before.keys() | after.keys()):
        old, new = before.get(name), after.get(name)
        if old == new:
            continue
        changes.append({
            "path": name,
            "before_sha256": hashlib.sha256(old).hexdigest() if old is not None else None,
            "after_sha256": hashlib.sha256(new).hexdigest() if new is not None else None,
            "before_base64": base64.b64encode(old).decode() if old is not None else None,
            "after_base64": base64.b64encode(new).decode() if new is not None else None,
        })
    receipt = {
        "schema": 1, "proposed_only": True, "candidate_sha": head,
        "repository": os.environ.get("GITHUB_REPOSITORY", ""),
        "run_id": os.environ.get("GITHUB_RUN_ID", ""),
        "run_attempt": os.environ.get("GITHUB_RUN_ATTEMPT", ""),
        "workflow": os.environ.get("GITHUB_WORKFLOW", ""),
        "formatter_exit_code": completed.returncode,
        "changes": changes,
    }
    (output / "license-repair.json").write_text(json.dumps(receipt, indent=2) + "\n", encoding="utf-8")
    print(f"Retained {len(changes)} proposed license repair(s); formatter exit {completed.returncode}.")
    return completed.returncode


if __name__ == "__main__":
    raise SystemExit(main())
