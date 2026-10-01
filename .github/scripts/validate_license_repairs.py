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

"""Reproduce proposed header changes without executing candidate code.

Run this script and the formatter from trusted source before acquiring signing
or write credentials. Its output is not PR authorization; the publisher must
separately establish workflow, author, live head/base and branch authority.
"""

from __future__ import annotations

import argparse
import base64
import hashlib
import json
from pathlib import Path, PurePosixPath
import re
import subprocess
import tempfile


MAX_BYTES = 10 * 1024 * 1024
SHA = re.compile(r"[0-9a-f]{40}")
EXTENSIONS = {".go", ".js", ".ts", ".java", ".py", ".sh"}


def git(root: Path, *args: str) -> bytes:
    return subprocess.run(["git", "-C", str(root), *args], check=True,
                          capture_output=True, timeout=60).stdout


def unique_object(pairs: list[tuple[str, object]]) -> dict:
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Duplicate JSON key")
        result[key] = value
    return result


def decode(change: dict, prefix: str) -> bytes:
    encoded = change.get(prefix + "_base64")
    if not isinstance(encoded, str) or len(encoded) > MAX_BYTES:
        raise ValueError("Invalid or oversized replacement blob")
    value = base64.b64decode(encoded, validate=True)
    if hashlib.sha256(value).hexdigest() != change.get(prefix + "_sha256"):
        raise ValueError("Replacement blob hash mismatch")
    return value


def validate(args: argparse.Namespace) -> dict:
    if not SHA.fullmatch(args.head) or not SHA.fullmatch(args.base):
        raise ValueError("Expected immutable head and base SHAs")
    if args.receipt.stat().st_size > MAX_BYTES:
        raise ValueError("Oversized receipt")
    receipt = json.loads(args.receipt.read_text(encoding="utf-8"), object_pairs_hook=unique_object)
    expected = {"schema": 1, "proposed_only": True, "candidate_sha": args.head,
                "repository": args.repository, "run_id": args.run_id,
                "run_attempt": args.run_attempt, "formatter_exit_code": 0}
    if not isinstance(receipt, dict) or any(receipt.get(k) != v for k, v in expected.items()):
        raise ValueError("Receipt identity or formatter success mismatch")
    # The caller supplies these identities from trusted API/workflow evidence.
    if git(args.trusted_root, "rev-parse", "HEAD").decode().strip() != args.base:
        raise ValueError("Trusted policy checkout does not match authorized base")
    git(args.candidate, "merge-base", "--is-ancestor", args.base, args.head)
    for policy in (".licenserc.yaml", ".github/tools/go.mod", ".github/tools/go.sum"):
        if git(args.candidate, "show", args.head + ":" + policy) != git(args.trusted_root, "show", args.base + ":" + policy):
            raise ValueError("Candidate changes license or formatter policy")
    changes = receipt.get("changes")
    if not isinstance(changes, list) or len(changes) > 100:
        raise ValueError("Invalid replacement inventory")
    seen = set()
    verified = []
    total = 0
    with tempfile.TemporaryDirectory(prefix="license-reproduction-") as temporary:
        directory = Path(temporary)
        for change in changes:
            if not isinstance(change, dict):
                raise ValueError("Invalid replacement entry")
            name = change.get("path")
            if not isinstance(name, str) or len(name) > 1024:
                raise ValueError("Invalid replacement path")
            path = PurePosixPath(name)
            if (path.is_absolute() or str(path) != name or ".." in path.parts
                    or any(part.lower() == ".git" for part in path.parts)
                    or re.search(r"[\\:\x00-\x1f\x7f]", name)
                    or path.suffix not in EXTENSIONS or name in seen):
                raise ValueError("Unsafe or duplicate replacement path")
            seen.add(name)
            entry = git(args.candidate, "ls-tree", "-z", args.head, "--", name)
            if not re.fullmatch(rb"100(?:644|755) blob [0-9a-f]{40}\t" + re.escape(name.encode()) + b"\x00", entry):
                raise ValueError("Replacement must target a committed regular file")
            old, new = decode(change, "before"), decode(change, "after")
            total += len(old) + len(new)
            if total > MAX_BYTES or old == new:
                raise ValueError("Oversized or empty transformation")
            if git(args.candidate, "show", args.head + ":" + name) != old:
                raise ValueError("Proposal includes edits preceding the license fixer")
            target = directory.joinpath(*path.parts)
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(old)
            verified.append(change)
        if verified:
            subprocess.run([str(args.fixer.resolve()), "header", "fix", "-c",
                            str((args.trusted_root / ".licenserc.yaml").resolve())],
                           cwd=directory, check=True, timeout=120)
            for change in verified:
                target = directory.joinpath(*PurePosixPath(change["path"]).parts)
                if target.read_bytes() != decode(change, "after"):
                    raise ValueError("Proposal differs from trusted formatter reproduction")
    return {"schema": 1, "candidate_sha": args.head, "base_sha": args.base,
            "repository": args.repository, "run_id": args.run_id,
            "run_attempt": args.run_attempt, "reproduced_changes": verified}


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--receipt", type=Path, required=True)
    parser.add_argument("--candidate", type=Path, required=True)
    parser.add_argument("--trusted-root", type=Path, required=True)
    parser.add_argument("--fixer", type=Path, required=True)
    for name in ("head", "base", "repository", "run-id", "run-attempt"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    result = validate(args)
    with args.output.open("x", encoding="utf-8") as output:
        json.dump(result, output, indent=2)
        output.write("\n")


if __name__ == "__main__":
    main()
