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

"""Compile the library as a fresh consumer at its declared Go floor.

The consumer has one requirement and one local replacement for the exact
candidate tree. Library replace/exclude directives do not propagate to it.
"""

from __future__ import annotations

import argparse
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


def directive(manifest: str, name: str) -> str:
    matches = re.findall(rf"(?m)^[ \t]*{name}[ \t]+(\S+)[ \t]*(?://.*)?$", manifest)
    if len(matches) != 1:
        raise ValueError(f"Expected one {name} directive")
    return matches[0]


def consumer_version(module_path: str) -> str:
    suffix = re.search(r"/v([2-9][0-9]*)$", module_path)
    return f"v{suffix.group(1)}.0.0" if suffix else "v0.0.0"


def run(root: Path) -> None:
    root = root.resolve()
    manifest = (root / "go.mod").read_text(encoding="utf-8")
    module = directive(manifest, "module")
    go_floor = directive(manifest, "go")
    if not re.fullmatch(r"1\.[0-9]+(?:\.[0-9]+)?", go_floor):
        raise ValueError("Unexpected Go compatibility directive")
    env = {**os.environ, "GOWORK": "off", "GOTOOLCHAIN": "local"}
    with tempfile.TemporaryDirectory(prefix="slogcp-consumer-floor-") as temporary:
        consumer = Path(temporary)
        (consumer / "go.mod").write_text(
            f"module example.com/consumer-floor\n\ngo {go_floor}\n\n"
            f"require {module} {consumer_version(module)}\n\n"
            f"replace {module} => {root.as_posix()}\n",
            encoding="utf-8",
        )
        (consumer / "consumer_test.go").write_text(
            f"package consumer\n\nimport _ \"{module}\"\n",
            encoding="utf-8",
        )
        subprocess.run(["go", "mod", "tidy", f"-go={go_floor}"],
                       cwd=consumer, env=env, check=True)
        after = (consumer / "go.mod").read_text(encoding="utf-8")
        if directive(after, "go") != go_floor:
            raise ValueError("Consumer Go floor changed during tidy")
        subprocess.run(["go", "list", "-m", "all"], cwd=consumer,
                       env={**env, "GOFLAGS": "-mod=readonly"}, check=True,
                       stdout=subprocess.DEVNULL)
        subprocess.run(["go", "test", "-mod=readonly", "./..."],
                       cwd=consumer, env=env, check=True)
    print(f"Consumer floor passed: {module} at Go {go_floor}")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    args = parser.parse_args()
    try:
        run(args.root)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Consumer floor failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
