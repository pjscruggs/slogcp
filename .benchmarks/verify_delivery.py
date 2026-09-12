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

"""Verify benchmark records actually arrived in Cloud Logging.

All cloud identifiers come from CLI arguments. Raw API evidence is written only
to the requested local output directory, never to public workflow output.
"""

from __future__ import annotations

import argparse
from collections import Counter
from datetime import datetime, timedelta, timezone
import gzip
import json
from pathlib import Path
import shutil
import subprocess
import sys
import time
from typing import Any, Callable, Iterable
import urllib.error
import urllib.parse
import urllib.request


API_URL = "https://logging.googleapis.com/v2/entries:list"
RESPONSE_FIELDS = "entries(jsonPayload,timestamp,receiveTimestamp,insertId,severity),nextPageToken"
MIN_REQUEST_INTERVAL = 1.4
MAX_EXPECTED_COUNT = 10_000_000


def parse_timestamp(value: Any) -> datetime:
    """Normalize suite Unix timestamps and application RFC 3339 timestamps."""
    if isinstance(value, (int, float)) and not isinstance(value, bool):
        return datetime.fromtimestamp(value, timezone.utc)
    if not isinstance(value, str):
        raise ValueError("timestamp must be RFC 3339 text or Unix seconds")
    parsed = datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamp must include a timezone")
    return parsed.astimezone(timezone.utc)


def read_time_range(path: Path, start_override: str | None = None, end_override: str | None = None) -> tuple[str, str]:
    """Bound indexed log scans to this experiment, allowing five-minute skew."""
    if path.is_dir():
        suite = path / "suite.json"
        paths = [suite] if suite.exists() else sorted(path.glob("*.json"))
    else:
        paths = [path]
    starts = []
    finishes = []
    for source in paths:
        data = json.loads(source.read_text(encoding="utf-8-sig"))
        documents = data if isinstance(data, list) else [data]
        if isinstance(data, dict):
            documents += data.get("trials", data.get("results", []))
        for item in documents:
            if not isinstance(item, dict):
                continue
            if "started_at" in item:
                starts.append(parse_timestamp(item["started_at"]))
            if "finished_at" in item:
                finishes.append(parse_timestamp(item["finished_at"]))
    if not starts and not start_override:
        raise ValueError("results lack start timestamps; supply --start-time")
    start = parse_timestamp(start_override) if start_override else min(starts)
    finish = parse_timestamp(end_override) if end_override else max(finishes, default=datetime.now(timezone.utc))
    if finish < start:
        raise ValueError("end timestamp precedes start timestamp")
    padding = timedelta(minutes=5)
    return tuple(value.isoformat(timespec="microseconds").replace("+00:00", "Z") for value in (start - padding, finish + padding))


def read_manifest(path: Path) -> dict[str, dict[str, Any]]:
    """Accept a trial list, a suite's trials/results list, or an ID/count map."""
    if path.is_dir():
        suite = path / "suite.json"
        if suite.exists():
            return read_manifest(suite)
        documents = []
        for file in sorted(path.glob("*.json")):
            value = json.loads(file.read_text(encoding="utf-8-sig"))
            if isinstance(value, dict) and ("trial_id" in value or isinstance(value.get("config"), dict)):
                documents.append(value)
        data: Any = documents
    else:
        data = json.loads(path.read_text(encoding="utf-8-sig"))
    if isinstance(data, dict):
        if "trials" in data:
            data = data["trials"]
        elif "results" in data:
            data = data["results"]
        elif "trial_id" in data:
            data = [data]
        else:
            data = [{"trial_id": key, "count": value} for key, value in data.items()]
    if not isinstance(data, list) or not data:
        raise ValueError("manifest must contain at least one trial")
    expected: dict[str, dict[str, Any]] = {}
    for item in data:
        if not isinstance(item, dict):
            raise ValueError("every manifest trial must be an object")
        item = item.get("config", item)
        if not isinstance(item, dict):
            raise ValueError("every manifest trial config must be an object")
        trial_id = item.get("trial_id")
        if not isinstance(trial_id, str) or not trial_id:
            raise ValueError("every manifest trial needs a nonempty trial_id")
        if trial_id in expected:
            raise ValueError(f"duplicate manifest trial_id: {trial_id}")
        count = item.get("count")
        if isinstance(count, bool) or not isinstance(count, int) or not 0 <= count <= MAX_EXPECTED_COUNT:
            raise ValueError(f"invalid count for trial {trial_id}")
        mode = item.get("mode", "slogcp")
        sink = item.get("sink", "stdout")
        expected[trial_id] = {"count": count, "mode": mode, "sink": sink}
    return expected


def logged_trials(manifest: dict[str, dict[str, Any]]) -> dict[str, dict[str, Any]]:
    return {
        trial: config for trial, config in manifest.items()
        if config["mode"] != "none" and config["sink"] != "discard"
    }


def logging_filter(run_id: str, time_range: tuple[str, str] | None = None) -> str:
    encoded = json.dumps(run_id)
    query = (
        'resource.type="cloud_run_job" AND '
        f"(jsonPayload.run_id={encoded} OR jsonPayload.message.run_id={encoded})"
    )
    if time_range:
        query += f" AND timestamp>={json.dumps(time_range[0])} AND timestamp<={json.dumps(time_range[1])}"
    return query


def marker(entry: dict[str, Any], run_id: str) -> dict[str, Any] | None:
    """Google RedirectAsJSON puts Entry.Payload inside the message object."""
    payload = entry.get("jsonPayload")
    if not isinstance(payload, dict):
        return None
    if payload.get("run_id") == run_id:
        return payload
    message = payload.get("message")
    if isinstance(message, dict) and message.get("run_id") == run_id:
        return message
    return None


def assess_entries(
    entries: Iterable[dict[str, Any]],
    manifest: dict[str, dict[str, Any]],
    run_id: str,
) -> dict[str, Any]:
    expected = logged_trials(manifest)
    counts: dict[str, Counter[int]] = {trial: Counter() for trial in expected}
    malformed: Counter[str] = Counter()
    invalid_severity: Counter[str] = Counter()
    unknown: Counter[str] = Counter()
    ignored_warmup = 0
    ignored_unrelated = 0
    total = 0
    for entry in entries:
        total += 1
        value = marker(entry, run_id)
        if value is None:
            ignored_unrelated += 1
            continue
        trial_id = value.get("trial_id")
        if not isinstance(trial_id, str):
            malformed["missing_or_invalid_trial_id"] += 1
            continue
        if trial_id.endswith("-warmup") and trial_id[:-7] in manifest:
            ignored_warmup += 1
            continue
        if trial_id not in expected:
            unknown[trial_id] += 1
            continue
        if entry.get("severity") != "INFO":
            invalid_severity[trial_id] += 1
        sequence = value.get("sequence")
        # Cloud Logging JSON numbers can be represented as integral doubles.
        if isinstance(sequence, float) and sequence.is_integer():
            sequence = int(sequence)
        if isinstance(sequence, bool) or not isinstance(sequence, int):
            malformed[trial_id] += 1
            continue
        counts[trial_id][sequence] += 1

    trials = {}
    for trial_id, config in expected.items():
        observed = counts[trial_id]
        count = config["count"]
        missing = [index for index in range(count) if index not in observed]
        duplicates = {str(index): amount for index, amount in sorted(observed.items()) if amount > 1}
        out_of_range = {str(index): amount for index, amount in sorted(observed.items()) if not 0 <= index < count}
        trials[trial_id] = {
            **config,
            "received": sum(observed.values()),
            "unique_received": len(observed),
            "missing": missing,
            "duplicates": duplicates,
            "out_of_range": out_of_range,
            "invalid_severity": invalid_severity[trial_id],
            "complete": not missing and not duplicates and not out_of_range
            and not malformed[trial_id] and not invalid_severity[trial_id],
        }
    passed = all(trial["complete"] for trial in trials.values()) and not malformed and not unknown and not invalid_severity
    return {
        "run_id": run_id,
        "passed": passed,
        "expected_records": sum(config["count"] for config in expected.values()),
        "received_records": sum(trial["received"] for trial in trials.values()),
        "raw_records": total,
        "ignored_warmup_records": ignored_warmup,
        "ignored_unrelated_records": ignored_unrelated,
        "excluded_trials": sorted(set(manifest) - set(expected)),
        "malformed_records": dict(malformed),
        "invalid_severity_records": dict(invalid_severity),
        "unknown_trials": dict(unknown),
        "trials": trials,
    }


class LoggingReader:
    """Read complete snapshots with a shared request-rate limiter."""

    def __init__(
        self,
        project: str,
        token: str,
        deadline: float,
        request_interval: float = MIN_REQUEST_INTERVAL,
        page_size: int = 10000,
        time_range: tuple[str, str] | None = None,
        opener: Callable[..., Any] = urllib.request.urlopen,
        clock: Callable[[], float] = time.monotonic,
        sleep: Callable[[float], None] = time.sleep,
    ) -> None:
        self.project = project
        self.token = token
        self.deadline = deadline
        self.request_interval = max(MIN_REQUEST_INTERVAL, request_interval)
        self.page_size = page_size
        self.time_range = time_range
        self.opener = opener
        self.clock = clock
        self.sleep = sleep
        self.next_request = 0.0
        self.requests = 0

    def snapshot(self, run_id: str) -> Iterable[dict[str, Any]]:
        page_token = ""
        seen_tokens = set()
        while True:
            wait = max(0.0, self.next_request - self.clock())
            if self.clock() + wait >= self.deadline:
                raise TimeoutError("delivery verification deadline reached during pagination")
            if wait:
                self.sleep(wait)
            body: dict[str, Any] = {
                "resourceNames": [f"projects/{self.project}"],
                "filter": logging_filter(run_id, self.time_range),
                "orderBy": "timestamp desc",
                "pageSize": self.page_size,
            }
            if page_token:
                body["pageToken"] = page_token
            request = urllib.request.Request(
                API_URL + "?" + urllib.parse.urlencode({"fields": RESPONSE_FIELDS}),
                data=json.dumps(body).encode("utf-8"),
                headers={"Authorization": f"Bearer {self.token}", "Content-Type": "application/json"},
                method="POST",
            )
            self.next_request = self.clock() + self.request_interval
            self.requests += 1
            try:
                with self.opener(request, timeout=min(60.0, self.deadline - self.clock())) as response:
                    result = json.load(response)
            except urllib.error.HTTPError as error:
                # Never print the request, headers, token, or server response.
                if error.code in (429, 500, 502, 503, 504):
                    self.next_request = self.clock() + max(5.0, self.request_interval)
                    continue
                raise RuntimeError(f"Cloud Logging API returned HTTP {error.code}") from None
            for entry in result.get("entries", []):
                if not isinstance(entry, dict):
                    raise ValueError("Cloud Logging returned a malformed entry")
                yield entry
            page_token = result.get("nextPageToken", "")
            if not page_token:
                return
            if page_token in seen_tokens:
                raise ValueError("Cloud Logging repeated a pagination token")
            seen_tokens.add(page_token)


def save_snapshot(
    entries: Iterable[dict[str, Any]],
    output: Path,
) -> Iterable[dict[str, Any]]:
    with gzip.open(output, "wt", encoding="utf-8") as stream:
        for entry in entries:
            stream.write(json.dumps(entry, separators=(",", ":")) + "\n")
            yield entry


def access_token(project: str) -> str:
    executable = shutil.which("gcloud")
    if not executable:
        raise RuntimeError("gcloud must be installed and authenticated")
    result = subprocess.run(
        [executable, "auth", "print-access-token", f"--project={project}"],
        text=True, capture_output=True, check=False,
    )
    if result.returncode or not result.stdout.strip():
        raise RuntimeError("gcloud could not obtain an access token")
    return result.stdout.strip()


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project", required=True)
    parser.add_argument("--run-id", required=True)
    inputs = parser.add_mutually_exclusive_group(required=True)
    inputs.add_argument("--manifest", type=Path)
    inputs.add_argument("--results", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--max-wait", type=float, default=600.0)
    parser.add_argument("--poll-seconds", type=float, default=15.0)
    parser.add_argument("--page-size", type=int, default=10000)
    parser.add_argument("--start-time", help="Override the run start (RFC 3339, timezone required)")
    parser.add_argument("--end-time", help="Override the run finish (RFC 3339, timezone required)")
    args = parser.parse_args(argv)
    if args.max_wait <= 0 or args.poll_seconds < MIN_REQUEST_INTERVAL:
        parser.error("max-wait must be positive; poll-seconds must be at least 1.4")
    if not 1 <= args.page_size <= 100000:
        parser.error("page-size must be between 1 and 100000")
    args.output.mkdir(parents=True, exist_ok=True)
    summary_path = args.output / "delivery-summary.json"
    started = time.monotonic()
    summary: dict[str, Any] = {"run_id": args.run_id, "passed": False}
    reader = None
    time_range = None
    try:
        manifest = read_manifest(args.manifest or args.results)
        if not logged_trials(manifest):
            raise ValueError("manifest contains no real cloud-logged trials to verify")
        time_range = read_time_range(args.manifest or args.results, args.start_time, args.end_time)
        reader = LoggingReader(args.project, access_token(args.project), started + args.max_wait,
                               page_size=args.page_size, time_range=time_range)
        attempt = 0
        while time.monotonic() < reader.deadline:
            attempt += 1
            snapshot_path = args.output / f"records-{attempt:03d}.jsonl.gz"
            summary = assess_entries(save_snapshot(reader.snapshot(args.run_id), snapshot_path), manifest, args.run_id)
            summary["snapshot_file"] = snapshot_path.name
            summary["attempts"] = attempt
            if summary["passed"]:
                break
            if summary["malformed_records"] or summary["invalid_severity_records"] or summary["unknown_trials"] or any(
                trial["duplicates"] or trial["out_of_range"] for trial in summary["trials"].values()
            ):
                break
            remaining = reader.deadline - time.monotonic()
            if remaining > 0:
                time.sleep(min(args.poll_seconds, remaining))
        if not summary["passed"]:
            summary.setdefault("error", "complete delivery was not verified")
    except (OSError, ValueError, RuntimeError, TimeoutError) as error:
        summary["passed"] = False
        summary["error"] = str(error)
    summary["elapsed_seconds"] = time.monotonic() - started
    summary["api_requests"] = reader.requests if reader else 0
    summary["query_time_range"] = time_range
    summary_path.write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(json.dumps({
        "passed": summary["passed"],
        "expected_records": summary.get("expected_records", 0),
        "received_records": summary.get("received_records", 0),
        "summary_file": str(summary_path),
    }))
    return 0 if summary["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
