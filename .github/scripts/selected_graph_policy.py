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

"""Check the selected modules of every tracked Go module against OSV.

The query is for exact selected versions, so OSV owns Go advisory range
evaluation. A checksum or an unselected go.mod graph edge is not a selection.
"""

from __future__ import annotations

import argparse
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time
import urllib.error
import urllib.parse
import urllib.request


API = "https://api.osv.dev/v1"
MAX_BATCH = 100
MAX_PAGES = 20


class PolicyError(Exception):
    """An incomplete inventory or advisory answer cannot authorize a merge."""


def tracked_modules(root: Path) -> list[Path]:
    result = subprocess.run(
        ["git", "-C", str(root), "ls-files", "-z"],
        check=True, capture_output=True,
    )
    modules = sorted(
        root / os.fsdecode(item)
        for item in result.stdout.split(b"\0")
        if item and Path(os.fsdecode(item)).name == "go.mod"
    )
    if not modules or not all(path.is_file() for path in modules):
        raise PolicyError("Tracked Go module inventory is empty or incomplete")
    return modules


def json_stream(source: str) -> list[dict]:
    decoder = json.JSONDecoder()
    offset = 0
    values = []
    while offset < len(source):
        while offset < len(source) and source[offset].isspace():
            offset += 1
        if offset == len(source):
            break
        value, offset = decoder.raw_decode(source, offset)
        if not isinstance(value, dict):
            raise PolicyError("Go returned a non-object module entry")
        values.append(value)
    if not values:
        raise PolicyError("Go returned an empty selected module list")
    return values


def selected_modules(module_dir: Path) -> list[dict]:
    env = {**os.environ, "GOWORK": "off", "GOFLAGS": "-mod=readonly", "GOTOOLCHAIN": "local"}
    result = subprocess.run(
        ["go", "-C", str(module_dir), "list", "-m", "-json", "all"],
        capture_output=True, text=True, encoding="utf-8", env=env,
    )
    if result.returncode:
        raise PolicyError(f"Cannot resolve {module_dir}: {result.stderr.strip()}")
    values = json_stream(result.stdout)
    if sum(entry.get("Main") is True for entry in values) != 1:
        raise PolicyError(f"Expected one main module in {module_dir}")
    return values


def inventory(root: Path, module_files: list[Path]) -> tuple[list[dict], list[dict]]:
    known = {path.parent.resolve() for path in tracked_modules(root)}
    selected: list[dict] = []
    queries: set[tuple[str, str]] = set()
    for manifest in module_files:
        scope = manifest.parent.resolve().relative_to(root.resolve()).as_posix() or "."
        for entry in selected_modules(manifest.parent):
            if entry.get("Error") or not isinstance(entry.get("Path"), str):
                raise PolicyError(f"Malformed selected module in {scope}")
            if entry.get("Main"):
                continue
            replacement = entry.get("Replace")
            actual = replacement if replacement is not None else entry
            path, version = actual.get("Path"), actual.get("Version")
            if not isinstance(path, str) or not path:
                raise PolicyError(f"Missing selected module path in {scope}")
            if replacement is not None and not version:
                local = (manifest.parent / path).resolve()
                if local not in known:
                    raise PolicyError(f"Uninventoried local replacement in {scope}: {path}")
                selected.append({"scope": scope, "path": entry["Path"],
                                 "version": entry.get("Version"),
                                 "local_replacement": local.relative_to(root.resolve()).as_posix()})
                continue
            if not isinstance(version, str) or not version.startswith("v"):
                raise PolicyError(f"Unversioned selected module in {scope}: {path}")
            selected.append({"scope": scope, "path": entry["Path"],
                             "version": entry.get("Version"),
                             "selected_path": path, "selected_version": version})
            queries.add((path, version))
    if not selected:
        raise PolicyError("No dependency modules were selected")
    return sorted(selected, key=lambda row: (row["scope"], row["path"])), [
        {"package": {"ecosystem": "Go", "name": path}, "version": version}
        for path, version in sorted(queries)
    ]


def request_json(path: str, payload: dict | None = None) -> dict:
    data = json.dumps(payload).encode() if payload is not None else None
    request = urllib.request.Request(
        API + path, data=data,
        headers={"Accept": "application/json", "Content-Type": "application/json"},
    )
    for attempt in range(3):
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                answer = json.load(response)
            if not isinstance(answer, dict):
                raise PolicyError("OSV returned a non-object response")
            return answer
        except (urllib.error.HTTPError, urllib.error.URLError, TimeoutError) as error:
            status = getattr(error, "code", None)
            if attempt == 2 or status not in {None, 429, 500, 502, 503, 504}:
                raise PolicyError(f"OSV request failed: {error}") from error
            time.sleep(2**attempt)
    raise AssertionError("unreachable")


def query_osv(queries: list[dict]) -> dict[tuple[str, str], list[dict]]:
    found: dict[tuple[str, str], list[dict]] = {}
    for start in range(0, len(queries), MAX_BATCH):
        pending = queries[start:start + MAX_BATCH]
        seen_tokens: set[tuple[str, str, str]] = set()
        for _ in range(MAX_PAGES):
            answer = request_json("/querybatch", {"queries": pending})
            results = answer.get("results")
            if not isinstance(results, list) or len(results) != len(pending):
                raise PolicyError("OSV batch response is incomplete")
            next_page = []
            for query, result in zip(pending, results, strict=True):
                if not isinstance(result, dict) or not isinstance(result.get("vulns", []), list):
                    raise PolicyError("OSV batch result is malformed")
                key = (query["package"]["name"], query["version"])
                records = result.get("vulns", [])
                if any(not isinstance(record, dict) or not record.get("id") or
                       not record.get("modified") for record in records):
                    raise PolicyError("OSV returned an incomplete advisory reference")
                found.setdefault(key, []).extend(records)
                token = result.get("next_page_token")
                if token:
                    if not isinstance(token, str) or (key[0], key[1], token) in seen_tokens:
                        raise PolicyError("OSV pagination is malformed or repeated")
                    seen_tokens.add((key[0], key[1], token))
                    next_page.append({**query, "page_token": token})
            if not next_page:
                break
            pending = next_page
        else:
            raise PolicyError("OSV pagination exceeded its bound")
    return found


def advisory_records(matches: dict[tuple[str, str], list[dict]]) -> dict[str, dict]:
    def same_instant(left: str, right: str) -> bool:
        try:
            return datetime.fromisoformat(left.replace("Z", "+00:00")) == datetime.fromisoformat(
                right.replace("Z", "+00:00"))
        except ValueError as error:
            raise PolicyError("OSV advisory modification timestamp is malformed") from error

    expected: dict[str, str] = {}
    for records in matches.values():
        for item in records:
            previous = expected.setdefault(item["id"], item["modified"])
            if not same_instant(previous, item["modified"]):
                raise PolicyError(f"Advisory changed during scan: {item['id']}")
    details = {}
    for advisory_id, modified in sorted(expected.items()):
        record = request_json("/vulns/" + urllib.parse.quote(advisory_id, safe=""))
        if record.get("id") != advisory_id or not isinstance(record.get("modified"), str) or \
                not same_instant(record["modified"], modified):
            raise PolicyError(f"Advisory changed during scan: {advisory_id}")
        if not isinstance(record.get("affected"), list) or not record["affected"]:
            raise PolicyError(f"Advisory has no affected package data: {advisory_id}")
        details[advisory_id] = record
    return details


def evaluate(root: Path, module_files: list[Path]) -> dict:
    selected, queries = inventory(root, module_files)
    matches = query_osv(queries)
    details = advisory_records(matches)
    findings = []
    for row in selected:
        if "selected_path" not in row:
            continue
        key = (row["selected_path"], row["selected_version"])
        for item in matches[key]:
            record = details[item["id"]]
            if record.get("withdrawn"):
                continue
            findings.append({"scope": row["scope"], "module": row["path"],
                             "selected_path": key[0], "version": key[1],
                             "advisory": item["id"], "modified": item["modified"]})
    findings = list({(item["scope"], item["module"], item["selected_path"],
                      item["version"], item["advisory"]): item
                     for item in findings}.values())
    findings.sort(key=lambda item: (item["scope"], item["module"], item["advisory"]))
    canonical = json.dumps(selected, sort_keys=True, separators=(",", ":")).encode()
    advisory_provenance = {}
    for advisory_id, record in sorted(details.items()):
        content = json.dumps(record, sort_keys=True, separators=(",", ":")).encode()
        advisory_provenance[advisory_id] = {
            "modified": record["modified"],
            "sha256": hashlib.sha256(content).hexdigest(),
            "aliases": sorted(record.get("aliases", [])),
            "withdrawn": record.get("withdrawn"),
        }
    return {"schema": 2, "result": "affected" if findings else "clean",
            "advisory_fetched_at": datetime.now(timezone.utc).isoformat(),
            "advisories": advisory_provenance,
            "inventory_sha256": hashlib.sha256(canonical).hexdigest(),
            "modules": [path.parent.resolve().relative_to(root.resolve()).as_posix() or "."
                        for path in module_files],
            "selected": selected, "findings": findings}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path.cwd())
    parser.add_argument("--module", action="append", type=Path)
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    try:
        root = args.root.resolve()
        modules = [root / item / "go.mod" for item in args.module] if args.module else tracked_modules(root)
        if not all(path.is_file() for path in modules):
            raise PolicyError("A requested Go module has no go.mod")
        report = evaluate(root, modules)
    except (PolicyError, OSError, subprocess.CalledProcessError, json.JSONDecodeError) as error:
        print(f"Selected graph policy indeterminate: {error}", file=sys.stderr)
        return 2
    if args.report:
        args.report.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(f"Selected graph policy: {report['result']}; "
          f"{len(report['modules'])} modules; {len(report['findings'])} findings; "
          f"inventory {report['inventory_sha256']}")
    for item in report["findings"]:
        print(f"  {item['scope']}: {item['selected_path']}@{item['version']} "
              f"{item['advisory']}")
    return 1 if report["findings"] else 0


if __name__ == "__main__":
    raise SystemExit(main())
