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

"""Generate temporary .e2e module files and enforce slogcp graph constraints."""

from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import stat
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any


METADATA_FILE_NAME = "go.module.json"
SLOGCP_MODULE_PATH = "github.com/pjscruggs/slogcp"
ADAPTER_MODULE_PATH = "github.com/pjscruggs/slogcp-grpc-adapter"
LOCAL_E2E_PREFIX = "github.com/pjscruggs/slogcp-e2e-internal/"
LOCAL_PJSCRUGGS_PREFIX = "github.com/pjscruggs/"
GO_BINARY = os.environ.get("GO_BINARY", "go")
WORKSPACE_FILE_NAME = "go.work"
STABLE_VERSION_PATTERN = re.compile(r"^v\d+\.\d+\.\d+$")
MAX_RECONCILIATION_ROUNDS = 6


@dataclass(frozen=True)
class Candidate:
    path: str
    current: str


@dataclass(frozen=True)
class Attempt:
    ok: bool
    mismatches: dict[str, tuple[str, str]]
    requested: dict[str, str]
    selected: dict[str, str]
    module_graph: dict[str, str]
    output: str
    reason: str
    error: str | None


class DependencyResolutionError(RuntimeError):
    def __init__(self, message: str, *, details: dict[str, Any]) -> None:
        super().__init__(message)
        self.details = details


@dataclass(frozen=True)
class WorkspaceState:
    root: Path
    members: tuple[Path, ...]
    editable_members: tuple[Path, ...]
    module_paths: dict[Path, str]
    baselines: dict[Path, tuple[str, str | None]]


def run_command(
    command: list[str],
    *,
    cwd: Path,
    env: dict[str, str],
) -> subprocess.CompletedProcess[str]:
    return subprocess.run(
        command,
        cwd=str(cwd),
        env=env,
        text=True,
        capture_output=True,
        check=False,
    )


def go_command(*args: str) -> list[str]:
    return [GO_BINARY, *args]


def env_with_gowork(env: dict[str, str], value: str | Path) -> dict[str, str]:
    updated = dict(env)
    updated["GOWORK"] = str(value)
    return updated


def parse_go_version(go_version: str) -> tuple[int, int, int]:
    parts = go_version.split(".")
    if len(parts) != 3 or not all(part.isdigit() for part in parts):
        raise ValueError(
            f"expected full Go version MAJOR.MINOR.PATCH, got {go_version!r}"
        )
    return int(parts[0]), int(parts[1]), int(parts[2])


def go_major_minor(go_version: str) -> str:
    major, minor, _patch = parse_go_version(go_version)
    return f"{major}.{minor}"


def decode_json_stream(payload: str) -> list[dict[str, Any]]:
    decoder = json.JSONDecoder()
    items: list[dict[str, Any]] = []
    index = 0
    length = len(payload)

    while index < length:
        while index < length and payload[index].isspace():
            index += 1
        if index >= length:
            break
        item, index = decoder.raw_decode(payload, index)
        if not isinstance(item, dict):
            raise ValueError("expected JSON object in go list output")
        items.append(item)

    return items


def load_metadata(module_dir: Path) -> dict[str, Any]:
    metadata_path = module_dir / METADATA_FILE_NAME
    if not metadata_path.is_file():
        raise FileNotFoundError(f"module metadata not found: {metadata_path}")

    with metadata_path.open("r", encoding="utf-8") as handle:
        data = json.load(handle)

    if not isinstance(data, dict):
        raise ValueError(f"{metadata_path} must contain a JSON object")

    module_path = data.get("module_path")
    if not isinstance(module_path, str) or not module_path:
        raise ValueError(f"{metadata_path} is missing a non-empty module_path")

    seed_requirements = data.get("seed_requirements", {})
    if not isinstance(seed_requirements, dict):
        raise ValueError(f"{metadata_path} seed_requirements must be an object")

    pinned_modules = data.get("pinned_modules", [])
    if not isinstance(pinned_modules, list):
        raise ValueError(f"{metadata_path} pinned_modules must be a list")

    return data


def go_mod_json(module_dir: Path, env: dict[str, str]) -> dict[str, Any]:
    completed = run_command(
        go_command("mod", "edit", "-json"),
        cwd=module_dir,
        env=env_with_gowork(env, "off"),
    )
    if completed.returncode != 0:
        raise RuntimeError(
            f"go mod edit -json failed for {module_dir}:\n"
            f"{completed.stdout}{completed.stderr}"
        )
    data = json.loads(completed.stdout)
    if not isinstance(data, dict):
        raise ValueError(f"go mod edit -json returned non-object for {module_dir}")
    return data


def module_path(module_dir: Path, env: dict[str, str]) -> str:
    metadata = go_mod_json(module_dir, env)
    module = metadata.get("Module")
    if isinstance(module, dict) and isinstance(module.get("Path"), str):
        return str(module["Path"])
    raise RuntimeError(f"{module_dir} does not have a module path")


def build_module_graph(module_dir: Path, env: dict[str, str]) -> dict[str, str]:
    completed = run_command(go_command("list", "-m", "-json", "all"), cwd=module_dir, env=env)
    if completed.returncode != 0:
        raise RuntimeError(
            "go list -m -json all failed for "
            f"{module_dir}:\n{completed.stdout}{completed.stderr}"
        )

    module_map: dict[str, str] = {}
    for item in decode_json_stream(completed.stdout):
        if item.get("Main"):
            continue

        path = item.get("Path")
        if not isinstance(path, str) or not path:
            continue

        replace = item.get("Replace")
        version = item.get("Version")
        if isinstance(replace, dict):
            replace_version = replace.get("Version")
            if isinstance(replace_version, str) and replace_version:
                version = replace_version

        if isinstance(version, str) and version:
            module_map[path] = version

    return module_map


def build_package_module_graph(module_dir: Path, env: dict[str, str]) -> dict[str, str]:
    completed = run_command(
        go_command(
            "list",
            "-deps",
            "-test",
            "-f",
            "{{with .Module}}{{.Path}} {{.Version}}{{end}}",
            "./...",
        ),
        cwd=module_dir,
        env=env,
    )
    if completed.returncode != 0:
        raise RuntimeError(
            "go list -deps -test failed for "
            f"{module_dir}:\n{completed.stdout}{completed.stderr}"
        )

    graph: dict[str, str] = {}
    for line in completed.stdout.splitlines():
        parts = line.strip().split()
        if len(parts) >= 2:
            graph[parts[0]] = parts[1]
    return graph


def selected_dependency_graph(
    module_dir: Path,
    env: dict[str, str],
    *,
    ceiling_scope: str,
) -> dict[str, str]:
    if ceiling_scope == "package":
        return build_package_module_graph(module_dir, env)
    return build_module_graph(module_dir, env)


def resolve_version(source: dict[str, Any], slogcp_reference: str) -> str:
    version = source.get("version")
    if isinstance(version, str) and version:
        return version

    version_source = source.get("version_source")
    if version_source == "slogcp_reference":
        return slogcp_reference

    raise ValueError(f"unable to resolve version for pinned module: {source!r}")


def render_go_mod(
    *,
    module_path: str,
    go_version: str,
    requirements: dict[str, str],
    replace_directives: dict[str, str],
) -> str:
    lines = [
        f"module {module_path}",
        "",
        f"go {go_major_minor(go_version)}",
        f"toolchain go{go_version}",
    ]

    if requirements:
        lines.extend(["", "require ("])
        for path in sorted(requirements):
            lines.append(f"\t{path} {requirements[path]}")
        lines.append(")")

    if replace_directives:
        lines.append("")
        for path in sorted(replace_directives):
            lines.append(f"replace {path} => {replace_directives[path]}")

    lines.append("")
    return "\n".join(lines)


def render_go_work(*, go_version: str, workspace_members: list[str]) -> str:
    lines = [
        f"go {go_version}",
        f"toolchain go{go_version}",
        "",
        "use (",
    ]
    for member in workspace_members:
        lines.append(f"\t{member}")
    lines.extend([")", ""])
    return "\n".join(lines)


def sync_local_slogcp_metadata(
    *,
    module_dir: Path,
    pinned_modules: list[dict[str, Any]],
    slogcp_dir: Path,
) -> None:
    for item in pinned_modules:
        module_path_value = item.get("module_path")
        replace_path = item.get("replace_path")
        if module_path_value != SLOGCP_MODULE_PATH or not isinstance(replace_path, str):
            continue

        destination = module_dir / replace_path
        if not destination.is_dir():
            continue

        source_go_mod = slogcp_dir / "go.mod"
        if source_go_mod.is_file():
            (destination / "go.mod").write_text(
                source_go_mod.read_text(encoding="utf-8"),
                encoding="utf-8",
            )

        source_go_sum = slogcp_dir / "go.sum"
        destination_go_sum = destination / "go.sum"
        if source_go_sum.is_file():
            destination_go_sum.write_text(
                source_go_sum.read_text(encoding="utf-8"),
                encoding="utf-8",
            )
        elif destination_go_sum.exists():
            destination_go_sum.unlink()


def make_tree_writable(root: Path) -> None:
    for path in (root, *root.rglob("*")):
        try:
            path.chmod(path.stat().st_mode | stat.S_IWRITE | stat.S_IWUSR)
        except OSError:
            pass


def copy_module_source(source_dir: Path, destination_dir: Path) -> None:
    if destination_dir.exists():
        if (destination_dir / "go.mod").is_file():
            return
        raise RuntimeError(
            f"adapter replacement path exists but has no go.mod: {destination_dir}"
        )

    shutil.copytree(
        source_dir,
        destination_dir,
        ignore=shutil.ignore_patterns(".git"),
    )
    make_tree_writable(destination_dir)


def download_module_source(
    *,
    module_dir: Path,
    module_path_value: str,
    version: str,
    env: dict[str, str],
) -> Path:
    completed = run_command(
        go_command("mod", "download", "-json", f"{module_path_value}@{version}"),
        cwd=module_dir,
        env=env_with_gowork(env, "off"),
    )
    if completed.returncode != 0:
        raise RuntimeError(
            f"go mod download failed for {module_path_value}@{version}:\n"
            f"{completed.stdout}{completed.stderr}"
        )

    data = json.loads(completed.stdout)
    source_dir = data.get("Dir")
    if not isinstance(source_dir, str) or not source_dir:
        raise RuntimeError(
            f"go mod download did not return a source directory for "
            f"{module_path_value}@{version}"
        )

    source_path = Path(source_dir)
    if not source_path.is_dir():
        raise RuntimeError(
            f"downloaded module source directory not found for "
            f"{module_path_value}@{version}: {source_path}"
        )
    return source_path


def materialize_adapter_sources(
    *,
    module_dir: Path,
    pinned_modules: list[dict[str, Any]],
    slogcp_reference: str,
    env: dict[str, str],
) -> list[dict[str, Any]]:
    fixtures: list[dict[str, Any]] = []

    for item in pinned_modules:
        if not isinstance(item, dict):
            continue
        if item.get("module_path") != ADAPTER_MODULE_PATH:
            continue

        replace_path = item.get("replace_path")
        if not isinstance(replace_path, str) or not replace_path:
            continue

        version = resolve_version(item, slogcp_reference)
        destination_dir = (module_dir / replace_path).resolve()
        fixture: dict[str, Any] = {
            "parent_module_dir": str(module_dir),
            "module_path": ADAPTER_MODULE_PATH,
            "version": version,
            "replace_path": replace_path,
            "destination": str(destination_dir),
        }

        if (destination_dir / "go.mod").is_file():
            fixture["source"] = "local"
            fixtures.append(fixture)
            continue

        source_dir = download_module_source(
            module_dir=module_dir,
            module_path_value=ADAPTER_MODULE_PATH,
            version=version,
            env=env,
        )
        copy_module_source(source_dir, destination_dir)
        fixture["source"] = "module-cache"
        fixture["download_dir"] = str(source_dir)
        fixtures.append(fixture)

    return fixtures


def has_generated_module_metadata(module_dir: Path) -> bool:
    return (module_dir / METADATA_FILE_NAME).is_file()


def discover_workspace_members(
    *,
    module_dir: Path,
    pinned_modules: list[dict[str, Any]],
) -> list[str]:
    members = ["."]
    seen_members = {"."}

    for item in pinned_modules:
        replace_path = item.get("replace_path")
        if not isinstance(replace_path, str) or not replace_path:
            continue

        candidate_dir = module_dir / replace_path
        if not (candidate_dir / "go.mod").is_file():
            continue

        normalized = replace_path.replace("\\", "/")
        if normalized in seen_members:
            continue

        seen_members.add(normalized)
        members.append(normalized)

    return members


def workspace_member_dirs(module_dir: Path, env: dict[str, str]) -> list[Path]:
    workspace_file = module_dir / WORKSPACE_FILE_NAME
    if not workspace_file.is_file():
        return [module_dir.resolve()]

    completed = run_command(
        go_command("work", "edit", "-json"),
        cwd=module_dir,
        env=env_with_gowork(env, workspace_file),
    )
    if completed.returncode != 0:
        raise RuntimeError(
            f"go work edit -json failed for {module_dir}:\n"
            f"{completed.stdout}{completed.stderr}"
        )

    metadata = json.loads(completed.stdout)
    members: list[Path] = []
    for item in metadata.get("Use", []):
        disk_path = item.get("DiskPath") if isinstance(item, dict) else None
        if not isinstance(disk_path, str) or not disk_path:
            continue
        candidate = Path(disk_path)
        if not candidate.is_absolute():
            candidate = module_dir / candidate
        members.append(candidate.resolve())

    root = module_dir.resolve()
    if root not in members:
        members.insert(0, root)
    return members


def snapshot_workspace(module_dir: Path, env: dict[str, str]) -> WorkspaceState:
    members = tuple(workspace_member_dirs(module_dir, env))
    module_paths = {member: module_path(member, env) for member in members}
    editable_members = tuple(
        member for member in members if module_paths.get(member) != SLOGCP_MODULE_PATH
    )
    baselines: dict[Path, tuple[str, str | None]] = {}
    for member in editable_members:
        go_mod = (member / "go.mod").read_text(encoding="utf-8")
        go_sum_path = member / "go.sum"
        go_sum = go_sum_path.read_text(encoding="utf-8") if go_sum_path.is_file() else None
        baselines[member] = (go_mod, go_sum)
    return WorkspaceState(
        root=module_dir.resolve(),
        members=members,
        editable_members=editable_members,
        module_paths=module_paths,
        baselines=baselines,
    )


def restore_workspace_files(workspace: WorkspaceState) -> None:
    for member, (go_mod, go_sum) in workspace.baselines.items():
        (member / "go.mod").write_text(go_mod, encoding="utf-8")
        go_sum_path = member / "go.sum"
        if go_sum is None:
            if go_sum_path.exists():
                go_sum_path.unlink()
        else:
            go_sum_path.write_text(go_sum, encoding="utf-8")


def module_require_paths(module_dir: Path, env: dict[str, str]) -> set[str]:
    metadata = go_mod_json(module_dir, env)
    paths: set[str] = set()
    for item in metadata.get("Require", []):
        path = item.get("Path")
        if isinstance(path, str):
            paths.add(path)
    return paths


def direct_requirements(module_dir: Path, env: dict[str, str]) -> dict[str, str]:
    metadata = go_mod_json(module_dir, env)
    requirements: dict[str, str] = {}
    for item in metadata.get("Require", []):
        path = item.get("Path")
        version = item.get("Version")
        if (
            isinstance(path, str)
            and isinstance(version, str)
            and not bool(item.get("Indirect"))
        ):
            requirements[path] = version
    return requirements


def apply_graph_requirements(
    module_dir: Path,
    env: dict[str, str],
    versions: dict[str, str],
) -> subprocess.CompletedProcess[str]:
    command = go_command(
        "get",
        *[f"{path}@{version}" for path, version in sorted(versions.items())],
    )
    return run_command(command, cwd=module_dir, env=env_with_gowork(env, "off"))


def edit_replaces(
    module_dir: Path,
    env: dict[str, str],
    replacements: dict[str, Path],
) -> subprocess.CompletedProcess[str]:
    command = go_command("mod", "edit")
    for path, target in sorted(replacements.items()):
        relative = os.path.relpath(target, module_dir).replace(os.sep, "/")
        if not relative.startswith((".", "/")):
            relative = f"./{relative}"
        command.append(f"-replace={path}={relative}")
    return run_command(command, cwd=module_dir, env=env_with_gowork(env, "off"))


def sync_local_workspace_replaces(workspace: WorkspaceState, env: dict[str, str]) -> None:
    for member in workspace.editable_members:
        require_paths = module_require_paths(member, env)
        replacements: dict[str, Path] = {}
        for target, target_module_path in workspace.module_paths.items():
            if target == member:
                continue
            if target_module_path in require_paths:
                replacements[target_module_path] = target
        if replacements:
            completed = edit_replaces(member, env, replacements)
            if completed.returncode != 0:
                raise RuntimeError(
                    f"go mod edit replace failed in {member}\n"
                    f"{completed.stdout}{completed.stderr}"
                )


def tidy_module(module_dir: Path, env: dict[str, str]) -> subprocess.CompletedProcess[str]:
    return run_command(
        go_command("mod", "tidy"),
        cwd=module_dir,
        env=env_with_gowork(env, "off"),
    )


def direct_candidates(
    workspace: WorkspaceState,
    env: dict[str, str],
    reference_graph: dict[str, str],
) -> list[Candidate]:
    graph = build_module_graph(workspace.root, env_with_gowork(env, workspace.root / WORKSPACE_FILE_NAME))
    seen: set[str] = set()
    candidates: list[Candidate] = []

    for member in workspace.editable_members:
        metadata = go_mod_json(member, env)
        for item in metadata.get("Require", []):
            path = item.get("Path")
            version = item.get("Version")
            indirect = bool(item.get("Indirect"))
            if indirect or not isinstance(path, str) or not isinstance(version, str):
                continue
            if path in seen:
                continue
            if path == SLOGCP_MODULE_PATH or path.startswith(LOCAL_E2E_PREFIX):
                continue
            if path.startswith(LOCAL_PJSCRUGGS_PREFIX):
                continue
            if path in reference_graph:
                continue

            seen.add(path)
            candidates.append(Candidate(path=path, current=graph.get(path, version)))

    return candidates


def available_stable_versions(
    module_path_value: str,
    env: dict[str, str],
    cwd: Path,
) -> list[str]:
    completed = run_command(
        go_command("list", "-m", "-versions", module_path_value),
        cwd=cwd,
        env=env_with_gowork(env, "off"),
    )
    if completed.returncode != 0:
        return []
    parts = completed.stdout.strip().split()
    return [part for part in parts[1:] if STABLE_VERSION_PATTERN.match(part)]


def compare_shared_graph(
    graph: dict[str, str],
    reference_graph: dict[str, str],
) -> dict[str, tuple[str, str]]:
    mismatches: dict[str, tuple[str, str]] = {}
    for path, reference_version in sorted(reference_graph.items()):
        selected_version = graph.get(path)
        if selected_version and selected_version != reference_version:
            mismatches[path] = (reference_version, selected_version)
    return mismatches


def candidate_requests_by_member(
    workspace: WorkspaceState,
    env: dict[str, str],
    versions: dict[str, str],
) -> dict[Path, dict[str, str]]:
    requests: dict[Path, dict[str, str]] = {
        member: {} for member in workspace.editable_members
    }
    for member in workspace.editable_members:
        require_paths = module_require_paths(member, env)
        requests[member] = {
            path: version
            for path, version in sorted(versions.items())
            if path in require_paths
        }
    return requests


def add_reference_requests_for_mismatches(
    workspace: WorkspaceState,
    env: dict[str, str],
    requests_by_member: dict[Path, dict[str, str]],
    mismatches: dict[str, tuple[str, str]],
) -> tuple[bool, list[str]]:
    added = False
    assigned: set[str] = set()

    for member in workspace.editable_members:
        member_graph = build_module_graph(member, env_with_gowork(env, "off"))
        member_requests = requests_by_member.setdefault(member, {})
        for path, (reference_version, _selected_version) in sorted(mismatches.items()):
            if path not in member_graph:
                continue
            assigned.add(path)
            if member_requests.get(path) == reference_version:
                continue
            member_requests[path] = reference_version
            added = True

    unassigned = sorted(set(mismatches) - assigned)
    return added, unassigned


def mismatch_report(
    mismatches: dict[str, tuple[str, str]],
) -> dict[str, dict[str, str]]:
    return {
        path: {"reference": reference, "selected": selected}
        for path, (reference, selected) in sorted(mismatches.items())
    }


def attempt_report(attempt: Attempt) -> dict[str, Any]:
    return {
        "ok": attempt.ok,
        "reason": attempt.reason,
        "error": attempt.error,
        "requested": attempt.requested,
        "selected": attempt.selected,
        "mismatches": mismatch_report(attempt.mismatches),
        "module_graph": attempt.module_graph,
        "output": attempt.output,
    }


def dependency_resolution_error(prefix: str, attempt: Attempt) -> DependencyResolutionError:
    lines = [prefix]
    if attempt.error:
        lines.append(attempt.error.rstrip())
    if attempt.mismatches:
        lines.extend(
            f"  {path}: slogcp={reference} generated={selected}"
            for path, (reference, selected) in sorted(attempt.mismatches.items())
        )
    if attempt.reason == "requested_version_not_selected":
        lines.extend(
            f"  {path}: requested={version} selected={attempt.selected.get(path, '<missing>')}"
            for path, version in sorted(attempt.requested.items())
            if attempt.selected.get(path) != version
        )
    return DependencyResolutionError(
        "\n".join(lines),
        details=attempt_report(attempt),
    )


def try_versions(
    workspace: WorkspaceState,
    env: dict[str, str],
    versions: dict[str, str],
    *,
    reference_graph: dict[str, str],
    ceiling_scope: str,
) -> Attempt:
    restore_workspace_files(workspace)
    sync_local_workspace_replaces(workspace, env)
    requests_by_member = candidate_requests_by_member(workspace, env, versions)
    output_parts: list[str] = []
    last_mismatches: dict[str, tuple[str, str]] = {}
    last_module_graph: dict[str, str] = {}
    last_selected: dict[str, str] = {}
    workspace_env = env_with_gowork(env, workspace.root / WORKSPACE_FILE_NAME)

    for round_index in range(MAX_RECONCILIATION_ROUNDS):
        if round_index > 0:
            restore_workspace_files(workspace)
            sync_local_workspace_replaces(workspace, env)

        for member in workspace.editable_members:
            member_versions = requests_by_member.get(member, {})
            if not member_versions:
                continue
            completed = apply_graph_requirements(member, env, member_versions)
            output_parts.append(completed.stdout + completed.stderr)
            if completed.returncode != 0:
                requested_text = " ".join(
                    f"{path}@{version}" for path, version in sorted(member_versions.items())
                )
                return Attempt(
                    ok=False,
                    mismatches=last_mismatches,
                    requested=dict(versions),
                    selected=last_selected,
                    module_graph=last_module_graph,
                    output="".join(output_parts),
                    reason="go_get_failed",
                    error=(
                        f"go get failed in {member} for {requested_text}:\n"
                        f"{completed.stdout}{completed.stderr}"
                    ),
                )

        for member in workspace.editable_members:
            completed = tidy_module(member, env)
            output_parts.append(completed.stdout + completed.stderr)
            if completed.returncode != 0:
                return Attempt(
                    ok=False,
                    mismatches=last_mismatches,
                    requested=dict(versions),
                    selected=last_selected,
                    module_graph=last_module_graph,
                    output="".join(output_parts),
                    reason="go_mod_tidy_failed",
                    error=(
                        f"go mod tidy failed in {member}:\n"
                        f"{completed.stdout}{completed.stderr}"
                    ),
                )

        selected_graph = selected_dependency_graph(
            workspace.root,
            workspace_env,
            ceiling_scope=ceiling_scope,
        )
        last_module_graph = build_module_graph(workspace.root, workspace_env)
        last_selected = {
            path: selected
            for path in sorted(versions)
            if (selected := last_module_graph.get(path)) is not None
        }
        last_mismatches = compare_shared_graph(selected_graph, reference_graph)
        if not last_mismatches:
            exact_request_satisfied = all(
                last_selected.get(path) == version for path, version in versions.items()
            )
            return Attempt(
                ok=exact_request_satisfied,
                mismatches={},
                requested=dict(versions),
                selected=last_selected,
                module_graph=last_module_graph,
                output="".join(output_parts),
                reason=("accepted" if exact_request_satisfied else "requested_version_not_selected"),
                error=None,
            )

        added, unassigned = add_reference_requests_for_mismatches(
            workspace,
            env,
            requests_by_member,
            last_mismatches,
        )
        if unassigned:
            return Attempt(
                ok=False,
                mismatches=last_mismatches,
                requested=dict(versions),
                selected=last_selected,
                module_graph=last_module_graph,
                output="".join(output_parts),
                reason="unassigned_shared_dependency_mismatch",
                error=(
                    "shared dependency mismatches could not be assigned to an "
                    "editable workspace member: " + ", ".join(unassigned)
                ),
            )
        if not added:
            return Attempt(
                ok=False,
                mismatches=last_mismatches,
                requested=dict(versions),
                selected=last_selected,
                module_graph=last_module_graph,
                output="".join(output_parts),
                reason="constraints_stalled",
                error="shared dependency reconciliation made no progress",
            )

    return Attempt(
        ok=False,
        mismatches=last_mismatches,
        requested=dict(versions),
        selected=last_selected,
        module_graph=last_module_graph,
        output="".join(output_parts),
        reason="maximum_reconciliation_rounds_exceeded",
        error=(
            "shared dependency reconciliation exceeded "
            f"{MAX_RECONCILIATION_ROUNDS} rounds"
        ),
    )


def find_highest_compatible(
    workspace: WorkspaceState,
    env: dict[str, str],
    candidates: list[Candidate],
    reference_graph: dict[str, str],
    *,
    ceiling_scope: str,
) -> dict[str, Any]:
    requested = {candidate.path: candidate.current for candidate in candidates}
    rejected: dict[str, dict[str, Any]] = {}

    if not candidates:
        final = try_versions(
            workspace,
            env,
            {},
            reference_graph=reference_graph,
            ceiling_scope=ceiling_scope,
        )
        if not final.ok:
            raise dependency_resolution_error(
                "generated workspace violates slogcp shared dependency parity:",
                final,
            )
        return {
            "requested": {},
            "selected": {},
            "rejected": rejected,
            "module_graph": final.module_graph,
        }

    for candidate in candidates:
        versions = available_stable_versions(candidate.path, env, workspace.root)
        if candidate.current not in versions:
            versions.append(candidate.current)

        first_rejection: tuple[str, Attempt] | None = None
        for version in reversed(versions):
            trial = dict(requested)
            trial[candidate.path] = version
            attempt = try_versions(
                workspace,
                env,
                trial,
                reference_graph=reference_graph,
                ceiling_scope=ceiling_scope,
            )
            if attempt.ok:
                requested[candidate.path] = version
                break
            if first_rejection is None:
                first_rejection = (version, attempt)
        else:
            requested[candidate.path] = candidate.current

        if first_rejection:
            version, attempt = first_rejection
            rejected[candidate.path] = {
                "requested_version": version,
                "selected_version": attempt.selected.get(candidate.path),
                "reason": attempt.reason,
                "error": attempt.error,
                "mismatches": mismatch_report(attempt.mismatches),
            }

    final = try_versions(
        workspace,
        env,
        requested,
        reference_graph=reference_graph,
        ceiling_scope=ceiling_scope,
    )
    if not final.ok:
        raise dependency_resolution_error(
            "final generated workspace violates slogcp shared dependency parity:",
            final,
        )

    return {
        "requested": requested,
        "selected": final.selected,
        "rejected": rejected,
        "module_graph": final.module_graph,
    }


def collect_shared_dependency_mismatches(
    module_graph: dict[str, str],
    slogcp_graph: dict[str, str],
) -> dict[str, str]:
    mismatches: dict[str, str] = {}

    for path in sorted(module_graph):
        slogcp_version = slogcp_graph.get(path)
        if not slogcp_version:
            continue

        module_version = module_graph[path]
        if module_version != slogcp_version:
            mismatches[path] = module_version

    return mismatches


def verify_workspace_parity(
    *,
    module_dir: Path,
    pinned_modules: list[dict[str, Any]],
    env: dict[str, str],
    reference_graph: dict[str, str],
    ceiling_scope: str,
) -> None:
    slogcp_replace_paths: list[str] = []
    for item in pinned_modules:
        if item.get("module_path") != SLOGCP_MODULE_PATH:
            continue
        replace_path = item.get("replace_path")
        if isinstance(replace_path, str) and replace_path:
            slogcp_replace_paths.append(replace_path)

    if not slogcp_replace_paths:
        return

    workspace_file = module_dir / WORKSPACE_FILE_NAME
    graph_env = env_with_gowork(env, workspace_file)
    module_graph = selected_dependency_graph(
        module_dir,
        graph_env,
        ceiling_scope=ceiling_scope,
    )
    mismatches = collect_shared_dependency_mismatches(module_graph, reference_graph)
    if not mismatches:
        return

    mismatch_lines = [
        f"  {path}: module={module_graph[path]} slogcp={reference_graph[path]}"
        for path in sorted(mismatches)
    ]
    raise RuntimeError(
        "workspace parity check failed for "
        f"{module_dir}:\n" + "\n".join(mismatch_lines)
    )


def upgrade_slogcp_direct_dependencies(
    *,
    slogcp_dir: Path,
    env: dict[str, str],
    ceiling_scope: str,
) -> dict[str, Any]:
    before_direct = direct_requirements(slogcp_dir, env)
    before_module_graph = build_module_graph(slogcp_dir, env_with_gowork(env, "off"))
    before_package_graph = selected_dependency_graph(
        slogcp_dir,
        env_with_gowork(env, "off"),
        ceiling_scope=ceiling_scope,
    )

    if before_direct:
        completed = run_command(
            go_command("get", *[f"{path}@latest" for path in sorted(before_direct)]),
            cwd=slogcp_dir,
            env=env_with_gowork(env, "off"),
        )
        if completed.returncode != 0:
            raise RuntimeError(
                "go get latest slogcp direct dependencies failed:\n"
                f"{completed.stdout}{completed.stderr}"
            )
        tidy = tidy_module(slogcp_dir, env)
        if tidy.returncode != 0:
            raise RuntimeError(
                "go mod tidy failed after slogcp dependency upgrade:\n"
                f"{tidy.stdout}{tidy.stderr}"
            )
        upgrade_output = completed.stdout + completed.stderr + tidy.stdout + tidy.stderr
    else:
        upgrade_output = ""

    after_direct = direct_requirements(slogcp_dir, env)
    changed = {
        path: {"before": before_direct.get(path), "after": after_direct.get(path)}
        for path in sorted(set(before_direct) | set(after_direct))
        if before_direct.get(path) != after_direct.get(path)
    }

    return {
        "direct_before": before_direct,
        "direct_after": after_direct,
        "direct_changed": changed,
        "module_graph_before": before_module_graph,
        "module_graph_after": build_module_graph(slogcp_dir, env_with_gowork(env, "off")),
        "package_graph_before": before_package_graph,
        "package_graph_after": selected_dependency_graph(
            slogcp_dir,
            env_with_gowork(env, "off"),
            ceiling_scope=ceiling_scope,
        ),
        "output": upgrade_output,
    }


def generate_module(
    *,
    module_dir: Path,
    go_version: str,
    slogcp_reference: str,
    slogcp_dir: Path,
    env: dict[str, str],
    generated_dirs: set[Path],
    reference_graph: dict[str, str],
    ceiling_scope: str,
    report: dict[str, Any],
) -> None:
    module_dir = module_dir.resolve()
    if module_dir in generated_dirs:
        return

    report["active_module_dir"] = str(module_dir)
    metadata = load_metadata(module_dir)
    pinned_modules = metadata.get("pinned_modules", [])
    seed_requirements = metadata.get("seed_requirements", {})
    module_report: dict[str, Any] = {
        "module_dir": str(module_dir),
        "module_path": str(metadata["module_path"]),
        "workspace_members": [],
        "normalization": {},
        "constrained_dependencies": {},
        "status": "running",
    }
    report.setdefault("modules", []).append(module_report)

    adapter_fixtures = materialize_adapter_sources(
        module_dir=module_dir,
        pinned_modules=pinned_modules,
        slogcp_reference=slogcp_reference,
        env=env,
    )
    if adapter_fixtures:
        report.setdefault("adapter_fixtures", []).extend(adapter_fixtures)

    for item in pinned_modules:
        if not isinstance(item, dict):
            raise ValueError(f"invalid pinned_modules entry in {module_dir}: {item!r}")
        replace_path = item.get("replace_path")
        if not isinstance(replace_path, str) or not replace_path:
            continue

        dependency_dir = (module_dir / replace_path).resolve()
        if has_generated_module_metadata(dependency_dir):
            generate_module(
                module_dir=dependency_dir,
                go_version=go_version,
                slogcp_reference=slogcp_reference,
                slogcp_dir=slogcp_dir,
                env=env,
                generated_dirs=generated_dirs,
                reference_graph=reference_graph,
                ceiling_scope=ceiling_scope,
                report=report,
            )

    report["active_module_dir"] = str(module_dir)
    report["failed_stage"] = "initial_module_tidy"

    sync_local_slogcp_metadata(
        module_dir=module_dir,
        pinned_modules=pinned_modules,
        slogcp_dir=slogcp_dir,
    )

    requirements = dict(seed_requirements)
    replace_directives: dict[str, str] = {}

    for item in pinned_modules:
        if not isinstance(item, dict):
            continue
        module_path_value = item.get("module_path")
        if not isinstance(module_path_value, str) or not module_path_value:
            raise ValueError(f"invalid pinned module path in {module_dir}: {item!r}")

        requirements[module_path_value] = resolve_version(item, slogcp_reference)

        replace_path = item.get("replace_path")
        if isinstance(replace_path, str) and replace_path:
            dependency_dir = module_dir / replace_path
            if dependency_dir.exists():
                replace_directives[module_path_value] = replace_path

    go_mod_text = render_go_mod(
        module_path=str(metadata["module_path"]),
        go_version=go_version,
        requirements=requirements,
        replace_directives=replace_directives,
    )
    (module_dir / "go.mod").write_text(go_mod_text, encoding="utf-8")

    go_sum_path = module_dir / "go.sum"
    if go_sum_path.exists():
        go_sum_path.unlink()

    completed = tidy_module(module_dir, env)
    if completed.returncode != 0:
        raise RuntimeError(
            "go mod tidy failed for "
            f"{module_dir}:\n{completed.stdout}{completed.stderr}"
        )

    workspace_members = discover_workspace_members(
        module_dir=module_dir,
        pinned_modules=pinned_modules,
    )
    module_report["workspace_members"] = workspace_members

    if len(workspace_members) > 1:
        workspace_file = module_dir / WORKSPACE_FILE_NAME
        workspace_file.write_text(
            render_go_work(
                go_version=go_version,
                workspace_members=workspace_members,
            ),
            encoding="utf-8",
        )
        workspace = snapshot_workspace(module_dir, env)
        module_paths = {str(path): value for path, value in workspace.module_paths.items()}
        module_report["workspace_module_paths"] = module_paths

        has_slogcp = any(value == SLOGCP_MODULE_PATH for value in workspace.module_paths.values())
        if has_slogcp:
            report["failed_stage"] = "workspace_floor_normalization"
            normalization = try_versions(
                workspace,
                env,
                {},
                reference_graph=reference_graph,
                ceiling_scope=ceiling_scope,
            )
            module_report["normalization"] = attempt_report(normalization)
            if not normalization.ok:
                raise dependency_resolution_error(
                    "unable to normalize generated workspace to slogcp shared "
                    "dependency parity:",
                    normalization,
                )

            workspace = snapshot_workspace(module_dir, env)
            report["failed_stage"] = "candidate_resolution"
            module_report["constrained_dependencies"] = find_highest_compatible(
                workspace,
                env,
                direct_candidates(workspace, env, reference_graph),
                reference_graph,
                ceiling_scope=ceiling_scope,
            )

        report["failed_stage"] = "workspace_parity_verification"
        verify_workspace_parity(
            module_dir=module_dir,
            pinned_modules=pinned_modules,
            env=env,
            reference_graph=reference_graph,
            ceiling_scope=ceiling_scope,
        )
        build_module_graph(module_dir, env_with_gowork(env, workspace_file))
    else:
        workspace_file = module_dir / WORKSPACE_FILE_NAME
        if workspace_file.exists():
            workspace_file.unlink()

    module_report["status"] = "success"
    generated_dirs.add(module_dir)


def collect_go_build_context(slogcp_dir: Path, env: dict[str, str]) -> dict[str, str]:
    completed = run_command(
        go_command("env", "-json", "GOOS", "GOARCH", "CGO_ENABLED"),
        cwd=slogcp_dir,
        env=env_with_gowork(env, "off"),
    )
    if completed.returncode != 0:
        return {"error": (completed.stdout + completed.stderr).strip()}
    data = json.loads(completed.stdout)
    return {
        key: str(value)
        for key, value in data.items()
        if key in {"GOOS", "GOARCH", "CGO_ENABLED"}
    }


def write_dependency_report(report_path: Path, report: dict[str, Any]) -> None:
    report_path.parent.mkdir(parents=True, exist_ok=True)
    temporary_path = report_path.with_name(
        f".{report_path.name}.{os.getpid()}.tmp"
    )
    try:
        temporary_path.write_text(
            json.dumps(report, indent=2, sort_keys=True) + "\n",
            encoding="utf-8",
        )
        os.replace(temporary_path, report_path)
    finally:
        if temporary_path.exists():
            temporary_path.unlink()


def main() -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Generate temporary .e2e go.mod/go.sum files plus local go.work files "
            "and enforce shared dependency ceilings against the staged slogcp checkout."
        )
    )
    parser.add_argument("--module-dir", action="append", required=True)
    parser.add_argument("--go-version", required=True)
    parser.add_argument("--slogcp-dir", required=True)
    parser.add_argument("--slogcp-reference", required=True)
    parser.add_argument(
        "--dependency-mode",
        choices=["floor", "latest-slogcp"],
        default="floor",
    )
    parser.add_argument(
        "--slogcp-shared-ceiling-scope",
        choices=["package", "module"],
        default="package",
    )
    parser.add_argument("--emit-dependency-report")
    args = parser.parse_args()

    module_dirs = [Path(path).resolve() for path in args.module_dir]
    slogcp_dir = Path(args.slogcp_dir).resolve()
    go_version = args.go_version.strip()
    slogcp_reference = args.slogcp_reference.strip()

    env = os.environ.copy()
    env["GOTOOLCHAIN"] = f"go{go_version}"
    env["GOWORK"] = "off"

    report: dict[str, Any] = {
        "dependency_mode": args.dependency_mode,
        "go_version": go_version,
        "slogcp_reference": slogcp_reference,
        "slogcp_shared_ceiling_scope": args.slogcp_shared_ceiling_scope,
        "status": "running",
        "failed_stage": "input_validation",
        "adapter_fixtures": [],
        "modules": [],
    }

    exit_code = 0
    try:
        for module_dir in module_dirs:
            if not module_dir.is_dir():
                raise FileNotFoundError(f"module directory not found: {module_dir}")
        if not slogcp_dir.is_dir():
            raise FileNotFoundError(f"slogcp directory not found: {slogcp_dir}")
        if not slogcp_reference:
            raise ValueError("slogcp reference must not be empty")
        parse_go_version(go_version)

        report["build_context"] = collect_go_build_context(slogcp_dir, env)
        report["failed_stage"] = "slogcp_reference_graph"
        if args.dependency_mode == "latest-slogcp":
            report["slogcp_upgrade"] = upgrade_slogcp_direct_dependencies(
                slogcp_dir=slogcp_dir,
                env=env,
                ceiling_scope=args.slogcp_shared_ceiling_scope,
            )
        else:
            report["slogcp_upgrade"] = {
                "direct_before": direct_requirements(slogcp_dir, env),
                "direct_after": direct_requirements(slogcp_dir, env),
                "direct_changed": {},
                "module_graph_before": build_module_graph(
                    slogcp_dir,
                    env_with_gowork(env, "off"),
                ),
                "module_graph_after": build_module_graph(
                    slogcp_dir,
                    env_with_gowork(env, "off"),
                ),
                "package_graph_before": selected_dependency_graph(
                    slogcp_dir,
                    env_with_gowork(env, "off"),
                    ceiling_scope=args.slogcp_shared_ceiling_scope,
                ),
                "package_graph_after": selected_dependency_graph(
                    slogcp_dir,
                    env_with_gowork(env, "off"),
                    ceiling_scope=args.slogcp_shared_ceiling_scope,
                ),
            }

        reference_graph = report["slogcp_upgrade"]["package_graph_after"]
        if args.slogcp_shared_ceiling_scope == "module":
            reference_graph = report["slogcp_upgrade"]["module_graph_after"]

        generated_dirs: set[Path] = set()
        for module_dir in module_dirs:
            report["failed_stage"] = "module_generation"
            report["active_module_dir"] = str(module_dir)
            generate_module(
                module_dir=module_dir,
                go_version=go_version,
                slogcp_reference=slogcp_reference,
                slogcp_dir=slogcp_dir,
                env=env,
                generated_dirs=generated_dirs,
                reference_graph=reference_graph,
                ceiling_scope=args.slogcp_shared_ceiling_scope,
                report=report,
            )
            print(f"generated module files for {module_dir}")
        report["status"] = "success"
        report["failed_stage"] = None
        report.pop("active_module_dir", None)
    except Exception as exc:  # pragma: no cover - exercised via CLI
        report["status"] = "failure"
        active_module_dir = report.get("active_module_dir")
        for module_report in reversed(report["modules"]):
            if module_report.get("module_dir") == active_module_dir:
                module_report["status"] = "failure"
                break
        error: dict[str, Any] = {
            "type": type(exc).__name__,
            "message": str(exc),
        }
        if isinstance(exc, DependencyResolutionError):
            error["details"] = exc.details
        report["error"] = error
        print(str(exc), file=sys.stderr)
        exit_code = 1
    finally:
        if args.emit_dependency_report:
            try:
                write_dependency_report(Path(args.emit_dependency_report), report)
            except Exception as exc:  # pragma: no cover - exercised via CLI
                print(f"failed to write dependency report: {exc}", file=sys.stderr)
                exit_code = 1

    return exit_code


if __name__ == "__main__":
    raise SystemExit(main())
