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

from __future__ import annotations

import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import unittest
import zipfile
from unittest import mock

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

from generate_go_module import (
    Attempt,
    WorkspaceState,
    apply_graph_requirements,
    available_stable_versions,
    build_module_graph,
    declared_direct_candidates,
    discover_workspace_members,
    enforce_declared_direct_versions,
    env_with_gowork,
    has_generated_module_metadata,
    render_go_work,
    restore_workspace_files,
    run_command,
    snapshot_workspace,
    tidy_module,
    try_versions,
    write_dependency_report,
)


class GenerateGoModuleTests(unittest.TestCase):
    @staticmethod
    def write_proxy_module(
        proxy_root: pathlib.Path,
        module_path: str,
        version: str,
        *,
        go_mod: str,
        files: dict[str, str],
    ) -> None:
        version_root = proxy_root / module_path / "@v"
        version_root.mkdir(parents=True, exist_ok=True)
        versions_path = version_root / "list"
        existing_versions = (
            versions_path.read_text(encoding="utf-8").splitlines()
            if versions_path.exists()
            else []
        )
        versions_path.write_text(
            "\n".join([*existing_versions, version]) + "\n",
            encoding="utf-8",
        )
        (version_root / f"{version}.info").write_text(
            json.dumps({"Version": version, "Time": "2026-01-01T00:00:00Z"}) + "\n",
            encoding="utf-8",
        )
        (version_root / f"{version}.mod").write_text(go_mod, encoding="utf-8")

        archive_prefix = f"{module_path}@{version}"
        with zipfile.ZipFile(
            version_root / f"{version}.zip",
            "w",
            compression=zipfile.ZIP_DEFLATED,
        ) as archive:
            archive.writestr(f"{archive_prefix}/go.mod", go_mod)
            for relative_path, content in sorted(files.items()):
                archive.writestr(f"{archive_prefix}/{relative_path}", content)

    def create_hidden_parent_fixture(
        self,
        root: pathlib.Path,
    ) -> tuple[pathlib.Path, dict[str, str]]:
        proxy_root = root / "proxy"
        shared_mod = "module example.com/shared\n\ngo 1.26.0\n"
        for version in ("v1.0.0", "v1.1.0"):
            self.write_proxy_module(
                proxy_root,
                "example.com/shared",
                version,
                go_mod=shared_mod,
                files={
                    "shared.go": (
                        "package shared\n\n"
                        f'const Version = "{version}"\n'
                    )
                },
            )

        for version, shared_version in (
            ("v1.0.0", "v1.0.0"),
            ("v1.1.0", "v1.1.0"),
        ):
            parent_mod = (
                "module example.com/parent\n\n"
                "go 1.26.0\n\n"
                f"require example.com/shared {shared_version}\n"
            )
            self.write_proxy_module(
                proxy_root,
                "example.com/parent",
                version,
                go_mod=parent_mod,
                files={
                    "parent.go": (
                        "package parent\n\n"
                        'import "example.com/shared"\n\n'
                        "const Version = shared.Version\n"
                    )
                },
            )

        module_dir = root / "target"
        module_dir.mkdir()
        (module_dir / "go.mod").write_text(
            "module example.com/target\n\n"
            "go 1.26.0\n\n"
            "require example.com/parent v1.1.0\n",
            encoding="utf-8",
        )
        (module_dir / "main.go").write_text(
            "package target\n\n"
            'import "example.com/parent"\n\n'
            "const Version = parent.Version\n",
            encoding="utf-8",
        )
        (module_dir / "go.work").write_text(
            render_go_work(go_version="1.26.0", workspace_members=["."]),
            encoding="utf-8",
        )

        fixture_env = os.environ.copy()
        fixture_env.update(
            {
                "CGO_ENABLED": "0",
                "GOCACHE": str(root / "go-cache"),
                "GOENV": "off",
                "GOFLAGS": "",
                "GOMODCACHE": str(root / "module-cache"),
                "GOPROXY": proxy_root.as_uri(),
                "GOSUMDB": "off",
                "GOTOOLCHAIN": "local",
                "GOWORK": "off",
            }
        )
        completed = tidy_module(module_dir, fixture_env)
        self.assertEqual(
            completed.returncode,
            0,
            msg=completed.stdout + completed.stderr,
        )
        return module_dir, fixture_env

    def test_has_generated_module_metadata_only_uses_root_metadata_file(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            adapter_root = pathlib.Path(tmp) / "slogcp-grpc-adapter"
            nested_e2e = adapter_root / ".e2e"
            nested_e2e.mkdir(parents=True)
            (nested_e2e / "go.module.json").write_text("{}", encoding="utf-8")

            self.assertFalse(has_generated_module_metadata(adapter_root))
            self.assertTrue(has_generated_module_metadata(nested_e2e))

    def test_discover_workspace_members_ignores_nested_adapter_e2e_go_mod(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            module_dir = pathlib.Path(tmp) / "trace-target-app"
            adapter_dir = module_dir / "slogcp-grpc-adapter"
            nested_e2e = adapter_dir / ".e2e"
            nested_e2e.mkdir(parents=True)
            (adapter_dir / "go.mod").write_text(
                "module github.com/pjscruggs/slogcp-grpc-adapter\n",
                encoding="utf-8",
            )
            (nested_e2e / "go.mod").write_text(
                "module github.com/pjscruggs/slogcp-grpc-adapter-e2e\n",
                encoding="utf-8",
            )

            members = discover_workspace_members(
                module_dir=module_dir,
                pinned_modules=[
                    {
                        "module_path": "github.com/pjscruggs/slogcp-grpc-adapter",
                        "replace_path": "./slogcp-grpc-adapter",
                    }
                ],
            )

            self.assertEqual(members, [".", "./slogcp-grpc-adapter"])

    def test_low_level_require_edit_does_not_downgrade_hidden_parent(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            module_dir, fixture_env = self.create_hidden_parent_fixture(pathlib.Path(tmp))

            completed = run_command(
                ["go", "mod", "edit", "-require=example.com/shared@v1.0.0"],
                cwd=module_dir,
                env=env_with_gowork(fixture_env, "off"),
            )
            self.assertEqual(completed.returncode, 0, msg=completed.stderr)
            completed = tidy_module(module_dir, fixture_env)
            self.assertEqual(completed.returncode, 0, msg=completed.stderr)

            graph = build_module_graph(module_dir, env_with_gowork(fixture_env, "off"))
            self.assertEqual(graph["example.com/parent"], "v1.1.0")
            self.assertEqual(graph["example.com/shared"], "v1.1.0")

    def test_graph_reconciliation_downgrades_hidden_parent(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            module_dir, fixture_env = self.create_hidden_parent_fixture(pathlib.Path(tmp))
            workspace = snapshot_workspace(module_dir, fixture_env)

            attempt = try_versions(
                workspace,
                fixture_env,
                {},
                reference_graph={"example.com/shared": "v1.0.0"},
                parity_scope="package",
            )

            self.assertTrue(attempt.ok, msg=attempt.error or attempt.output)
            self.assertEqual(attempt.reason, "accepted")
            self.assertEqual(attempt.module_graph["example.com/parent"], "v1.0.0")
            self.assertEqual(attempt.module_graph["example.com/shared"], "v1.0.0")

    def test_incompatible_exact_candidate_is_not_reported_as_selected(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            module_dir, fixture_env = self.create_hidden_parent_fixture(pathlib.Path(tmp))
            workspace = snapshot_workspace(module_dir, fixture_env)

            attempt = try_versions(
                workspace,
                fixture_env,
                {"example.com/parent": "v1.1.0"},
                reference_graph={"example.com/shared": "v1.0.0"},
                parity_scope="package",
            )

            self.assertFalse(attempt.ok)
            self.assertNotEqual(attempt.reason, "accepted")
            if attempt.reason == "requested_version_not_selected":
                self.assertNotEqual(attempt.selected.get("example.com/parent"), "v1.1.0")

    def test_graph_requests_are_sorted_in_one_transaction(self) -> None:
        completed = subprocess.CompletedProcess([], 0, "", "")
        with mock.patch("generate_go_module.run_command", return_value=completed) as run:
            apply_graph_requirements(
                pathlib.Path("fixture"),
                {},
                {"example.com/z": "v1.0.0", "example.com/a": "v2.0.0"},
            )

        command = run.call_args.args[0]
        self.assertEqual(
            command[1:],
            ["get", "example.com/a@v2.0.0", "example.com/z@v1.0.0"],
        )

    def test_reconciliation_can_progress_beyond_six_rounds(self) -> None:
        root = pathlib.Path("fixture").resolve()
        member = root / "member"
        reference_graph = {
            f"example.com/shared-{index}": "v1.0.0" for index in range(7)
        }
        selected_graphs = [
            {path: "v2.0.0"} for path in reference_graph
        ] + [dict(reference_graph)]
        workspace = WorkspaceState(
            root=root,
            members=(member,),
            editable_members=(member,),
            module_paths={member: "example.com/member"},
            baselines={member: ("module example.com/member\n", None)},
            baseline_directives={member: (None, None)},
            go_work_sum=None,
        )
        completed = subprocess.CompletedProcess([], 0, "", "")

        def add_one_constraint(
            _workspace: WorkspaceState,
            _env: dict[str, str],
            requests_by_member: dict[pathlib.Path, dict[str, str]],
            mismatches: dict[str, tuple[str, str]],
        ) -> tuple[bool, list[str]]:
            path = next(iter(mismatches))
            requests_by_member[member][path] = reference_graph[path]
            return True, []

        with (
            mock.patch("generate_go_module.restore_workspace_files"),
            mock.patch("generate_go_module.sync_local_workspace_replaces"),
            mock.patch(
                "generate_go_module.candidate_requests_by_member",
                return_value={member: {}},
            ),
            mock.patch(
                "generate_go_module.apply_graph_requirements",
                return_value=completed,
            ),
            mock.patch("generate_go_module.tidy_module", return_value=completed),
            mock.patch(
                "generate_go_module.changed_module_directives",
                return_value={},
            ),
            mock.patch(
                "generate_go_module.selected_dependency_graph",
                side_effect=selected_graphs,
            ),
            mock.patch(
                "generate_go_module.build_module_graph",
                return_value=dict(reference_graph),
            ),
            mock.patch(
                "generate_go_module.add_reference_requests_for_mismatches",
                side_effect=add_one_constraint,
            ),
        ):
            attempt = try_versions(
                workspace,
                {},
                {},
                reference_graph=reference_graph,
                parity_scope="package",
            )

        self.assertTrue(attempt.ok, msg=attempt.error)
        self.assertEqual(len(attempt.rounds), 8)

    def test_workspace_snapshot_restores_go_work_sum(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            module_dir, fixture_env = self.create_hidden_parent_fixture(pathlib.Path(tmp))
            go_work_sum = module_dir / "go.work.sum"
            go_work_sum.write_text("baseline\n", encoding="utf-8")
            workspace = snapshot_workspace(module_dir, fixture_env)

            go_work_sum.write_text("trial\n", encoding="utf-8")
            restore_workspace_files(workspace)
            self.assertEqual(go_work_sum.read_text(encoding="utf-8"), "baseline\n")

            go_work_sum.unlink()
            workspace_without_sum = snapshot_workspace(module_dir, fixture_env)
            go_work_sum.write_text("trial\n", encoding="utf-8")
            restore_workspace_files(workspace_without_sum)
            self.assertFalse(go_work_sum.exists())

    def test_declared_version_enforcement_does_not_enumerate_versions(self) -> None:
        workspace = mock.sentinel.workspace
        accepted = Attempt(
            ok=True,
            mismatches={},
            requested={"example.com/direct": "v1.2.3"},
            selected={"example.com/direct": "v1.2.3"},
            module_graph={"example.com/direct": "v1.2.3"},
            output="",
            reason="accepted",
            error=None,
        )
        candidate = mock.Mock(path="example.com/direct", current="v1.2.3")
        with (
            mock.patch("generate_go_module.try_versions", return_value=accepted),
            mock.patch("generate_go_module.available_stable_versions") as enumerate_versions,
        ):
            result = enforce_declared_direct_versions(
                workspace,
                {},
                [candidate],
                {},
                parity_scope="package",
            )

        enumerate_versions.assert_not_called()
        self.assertEqual(result["selection_policy"], "declared-direct-versions")

    def test_declared_candidates_use_reviewed_seed_versions(self) -> None:
        candidates = declared_direct_candidates(
            {
                "example.com/direct": "v1.2.3",
                "example.com/shared": "v9.9.9",
                "github.com/pjscruggs/local": "v1.0.0",
            },
            {"example.com/shared": "v1.0.0"},
        )

        self.assertEqual(
            [(candidate.path, candidate.current) for candidate in candidates],
            [("example.com/direct", "v1.2.3")],
        )

    def test_version_enumeration_failure_is_explicit(self) -> None:
        completed = subprocess.CompletedProcess([], 1, "", "proxy unavailable")
        with mock.patch("generate_go_module.run_command", return_value=completed):
            with self.assertRaisesRegex(RuntimeError, "proxy unavailable"):
                available_stable_versions(
                    "example.com/direct",
                    {},
                    pathlib.Path("fixture"),
                )

    def test_dependency_report_write_is_atomic_and_complete(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            report_path = pathlib.Path(tmp) / "reports" / "dependency-report.json"
            report = {
                "status": "failure",
                "failed_stage": "candidate_resolution",
                "error": {"type": "RuntimeError", "message": "fixture"},
            }

            write_dependency_report(report_path, report)

            self.assertEqual(json.loads(report_path.read_text(encoding="utf-8")), report)
            self.assertEqual(list(report_path.parent.glob("*.tmp")), [])

    def test_cli_writes_structured_report_on_failure(self) -> None:
        generator = pathlib.Path(__file__).resolve().with_name("generate_go_module.py")
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            report_path = root / "dependency-report.json"
            completed = subprocess.run(
                [
                    sys.executable,
                    str(generator),
                    "--module-dir",
                    str(root / "missing-module"),
                    "--go-version",
                    "1.26.5",
                    "--slogcp-dir",
                    str(root / "missing-slogcp"),
                    "--slogcp-reference",
                    "v1.2.3",
                    "--emit-dependency-report",
                    str(report_path),
                ],
                text=True,
                capture_output=True,
                check=False,
            )

            self.assertEqual(completed.returncode, 1)
            report = json.loads(report_path.read_text(encoding="utf-8"))
            self.assertEqual(report["status"], "failure")
            self.assertEqual(report["schema_version"], 1)
            self.assertEqual(report["failed_stage"], "input_validation")
            self.assertEqual(report["error"]["type"], "FileNotFoundError")

    def test_generate_and_upload_wrapper_preserves_exit_status(self) -> None:
        bash = shutil.which("bash")
        git_bash = pathlib.Path("C:/Program Files/Git/bin/bash.exe")
        if os.name == "nt" and git_bash.is_file():
            bash = str(git_bash)
        if not bash:
            self.skipTest("bash is not available")

        wrapper = (
            pathlib.Path(__file__).resolve().parents[1]
            / "cloudbuild"
            / "build-tools"
            / "generate-and-upload-modules.sh"
        )
        cases = [
            (0, 0, "valid", False, 0),
            (7, 0, "valid", False, 7),
            (7, 9, "valid", False, 7),
            (0, 9, "valid", False, 9),
            (7, 0, "missing", False, 7),
            (0, 0, "missing", False, 66),
            (0, 0, "malformed", False, 66),
            (0, 0, "missing", True, 66),
            (7, 0, "malformed", False, 7),
            (7, 0, "wrong-status", False, 7),
        ]

        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            fake_bin = root / "bin"
            fake_bin.mkdir()
            fake_gsutil = fake_bin / "gsutil"
            gsutil_log = root / "gsutil.log"
            fake_gsutil.write_text(
                "#!/usr/bin/env bash\n"
                "printf '%s\\n' \"$*\" >> \"$FAKE_GSUTIL_LOG\"\n"
                "exit \"${FAKE_GSUTIL_RC:-0}\"\n",
                encoding="utf-8",
            )
            fake_gsutil.chmod(0o755)

            for index, (
                generator_rc,
                upload_rc,
                report_kind,
                seed_stale_report,
                expected,
            ) in enumerate(cases):
                report_path = root / f"report-{index}.json"
                if seed_stale_report:
                    report_path.write_text(
                        '{"schema_version":1,"status":"success"}\n',
                        encoding="utf-8",
                    )
                expected_status = "success" if generator_rc == 0 else "failure"
                generator = (
                    "printf '{\"schema_version\":1,\"status\":\"%s\"}\\n' "
                    "\"$3\" > \"$1\"; exit \"$2\""
                )
                if report_kind == "missing":
                    generator = "exit \"$2\""
                elif report_kind == "malformed":
                    generator = "printf malformed > \"$1\"; exit \"$2\""
                elif report_kind == "wrong-status":
                    generator = (
                        "printf "
                        "'{\"schema_version\":1,\"status\":\"success\"}\\n' "
                        "> \"$1\"; exit \"$2\""
                    )
                case_env = os.environ.copy()
                case_env["FAKE_GSUTIL_RC"] = str(upload_rc)
                case_env["FAKE_GSUTIL_LOG"] = gsutil_log.as_posix()
                case_env["PATH"] = f"{fake_bin.as_posix()}:/usr/bin:/bin"
                case_env["PYTHON_BINARY"] = pathlib.Path(sys.executable).as_posix()
                completed = subprocess.run(
                    [
                        bash,
                        wrapper.as_posix(),
                        report_path.as_posix(),
                        "gs://fixture/dependency-report.json",
                        "--",
                        "sh",
                        "-c",
                        generator,
                        "fixture-generator",
                        report_path.as_posix(),
                        str(generator_rc),
                        expected_status,
                    ],
                    env=case_env,
                    text=True,
                    capture_output=True,
                    check=False,
                )
                self.assertEqual(
                    completed.returncode,
                    expected,
                    msg=(
                        f"generator={generator_rc} upload={upload_rc} "
                        f"report_kind={report_kind} stale={seed_stale_report}\n"
                        f"{completed.stdout}{completed.stderr}"
                    ),
                )
                if generator_rc != 0 and report_kind != "valid":
                    fallback = json.loads(report_path.read_text(encoding="utf-8"))
                    self.assertEqual(fallback["schema_version"], 1)
                    self.assertEqual(fallback["status"], "failure")
                    self.assertEqual(fallback["error"]["exit_code"], generator_rc)

            upload_commands = gsutil_log.read_text(encoding="utf-8")
            self.assertIn("x-goog-if-generation-match:0", upload_commands)


if __name__ == "__main__":
    unittest.main()
