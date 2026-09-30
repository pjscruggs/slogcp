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

"""Confirm terminal E2E builds left no resources owned by their exact run ID."""

from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import time


BUILD_ID = re.compile(r"^[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}$")
TERMINAL = {"SUCCESS", "FAILURE", "TIMEOUT", "CANCELLED", "INTERNAL_ERROR"}
GCLOUD = "gcloud.cmd" if os.name == "nt" else "gcloud"


class SweepError(Exception):
    """The runner cannot prove that owned resources were removed."""


def gcloud_json(*args: str) -> object:
    try:
        result = subprocess.run([GCLOUD, *args, "--format=json"],
                                capture_output=True, text=True)
    except OSError as error:
        raise SweepError("Cloud CLI is unavailable") from error
    if result.returncode:
        raise SweepError("Cloud inventory or build status is unavailable")
    try:
        return json.loads(result.stdout)
    except json.JSONDecodeError as error:
        raise SweepError("Cloud returned malformed JSON") from error


def cloud_delete(*args: str) -> None:
    try:
        result = subprocess.run([GCLOUD, *args, "--quiet"],
                                capture_output=True, text=True)
    except OSError as error:
        raise SweepError("Cloud CLI is unavailable") from error
    if result.returncode:
        # A later inventory may establish that the resource was already absent.
        return


def normalized_run_id(run_id: str) -> tuple[str, str]:
    if not re.fullmatch(r"[A-Za-z0-9-]{8,80}", run_id):
        raise SweepError("E2E run ID is invalid")
    sanitized = re.sub(r"-+", "-", run_id.lower()).strip("-")
    if not sanitized[0].isalpha():
        sanitized = "r-" + sanitized
    return sanitized[:50], sanitized[:32]


def pubsub_name(base: str, short_id: str) -> str:
    if not re.fullmatch(r"[A-Za-z][A-Za-z0-9_-]{2,200}", base):
        raise SweepError("Pub/Sub resource base name is invalid")
    value = re.sub(r"-+", "-", re.sub(r"[^a-z0-9-]+", "-",
                                  f"{base}-{short_id}".lower())).strip("-")
    return value[:255] if value[0].isalpha() else ("e2e-" + value)[:255]


def resource_name(row: dict) -> str:
    if not isinstance(row, dict):
        raise SweepError("Cloud inventory contains a malformed resource")
    metadata = row.get("metadata") or {}
    value = metadata.get("name") or row.get("name")
    if not isinstance(value, str) or not value:
        raise SweepError("Cloud inventory omitted a resource name")
    return value.rsplit("/", 1)[-1]


def owned_resources(project: str, region: str, run_id: str,
                    topic_base: str, subscription_base: str) -> dict[str, set[str]]:
    suffix, short_id = normalized_run_id(run_id)
    services = gcloud_json("run", "services", "list", "--platform=managed",
                           f"--project={project}", f"--region={region}")
    jobs = gcloud_json("run", "jobs", "list", f"--project={project}",
                       f"--region={region}")
    topics = gcloud_json("pubsub", "topics", "list", f"--project={project}")
    subscriptions = gcloud_json("pubsub", "subscriptions", "list",
                                f"--project={project}")
    if not all(isinstance(rows, list) for rows in
               (services, jobs, topics, subscriptions)):
        raise SweepError("Cloud inventory is incomplete")
    expected_services = {f"{prefix}-{suffix}"[:63] for prefix in
                         ("core-log-app", "trace-target-app", "trace-http-app",
                          "trace-grpc-app")}
    selected_services = set()
    for row in services:
        name = resource_name(row)
        labels = (row.get("metadata") or {}).get("labels") or row.get("labels") or {}
        if name in expected_services or (
                short_id in name and isinstance(labels, dict) and
                "e2e-scenario" in labels):
            selected_services.add(name)
    job_prefix = f"e2e-hjob-{suffix}"[:38] + "-"
    selected_jobs = {name for row in jobs if
                     (name := resource_name(row)).startswith(job_prefix) and
                     re.fullmatch(r"[0-9]{10,}", name[len(job_prefix):])}
    topic_name = pubsub_name(topic_base, short_id)
    subscription_name = pubsub_name(subscription_base, short_id)
    selected_topics = {name for row in topics if
                       (name := resource_name(row)) == topic_name}
    selected_subscriptions = {name for row in subscriptions if
                             (name := resource_name(row)) == subscription_name}
    return {"service": selected_services, "job": selected_jobs,
            "subscription": selected_subscriptions, "topic": selected_topics}


def sweep(project: str, region: str, build_id: str, run_id: str,
          topic_base: str, subscription_base: str) -> dict[str, int]:
    if not BUILD_ID.fullmatch(build_id):
        raise SweepError("Cloud Build ID is invalid")
    if not re.fullmatch(r"[a-z][a-z0-9-]{2,62}", project) or not re.fullmatch(
            r"[a-z]+-[a-z]+[0-9]+", region):
        raise SweepError("Explicit cloud project or region is invalid")
    build = gcloud_json("builds", "describe", build_id, f"--project={project}",
                        f"--region={region}")
    if not isinstance(build, dict) or build.get("id") != build_id or \
            build.get("status") not in TERMINAL:
        raise SweepError("Cloud Build has not reached a verified terminal state")
    if build.get("substitutions", {}).get("_E2E_RUN_ID") != run_id:
        raise SweepError("Cloud Build does not own the requested E2E run ID")
    found = owned_resources(project, region, run_id, topic_base, subscription_base)
    for kind in ("job", "service", "subscription", "topic"):
        for name in sorted(found[kind]):
            if kind == "job":
                cloud_delete("run", "jobs", "delete", name,
                             f"--project={project}", f"--region={region}")
            elif kind == "service":
                cloud_delete("run", "services", "delete", name,
                             "--platform=managed", f"--project={project}",
                             f"--region={region}")
            else:
                cloud_delete("pubsub", kind + "s", "delete", name,
                             f"--project={project}")
    for attempt in range(3):
        remaining = owned_resources(project, region, run_id,
                                    topic_base, subscription_base)
        if not any(remaining.values()):
            return {kind: len(names) for kind, names in found.items()}
        if attempt < 2:
            time.sleep(3)
    raise SweepError("E2E owned resources remain after cleanup")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--project", required=True)
    parser.add_argument("--region", required=True)
    parser.add_argument("--build-id", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--topic-base", required=True)
    parser.add_argument("--subscription-base", required=True)
    args = parser.parse_args()
    try:
        removed = sweep(args.project, args.region, args.build_id, args.run_id,
                        args.topic_base, args.subscription_base)
    except SweepError as error:
        print(f"E2E cleanup not confirmed: {error}", file=sys.stderr)
        return 1
    print("E2E owned resource cleanup confirmed: " +
          ", ".join(f"{kind}={count}" for kind, count in removed.items()))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
