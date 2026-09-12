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

"""Execute the workflow's classifiers and required-result aggregate."""

import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = (ROOT / ".github/workflows/validation_pipeline.yml").read_text(
    encoding="utf-8"
)


def step_body(name, key):
    lines = WORKFLOW.splitlines()
    start = lines.index(f"      - name: {name}")
    for offset in range(start + 1, len(lines)):
        if lines[offset].strip() == f"{key}: |":
            indent = len(lines[offset]) - len(lines[offset].lstrip()) + 2
            body = []
            for line in lines[offset + 1 :]:
                if line.strip() and len(line) - len(line.lstrip()) < indent:
                    break
                body.append(line[indent:] if line.strip() else "")
            return "\n".join(body)
        if lines[offset].startswith("      - name:"):
            break
    raise AssertionError(f"Missing {key} body for {name}")


class WorkflowPolicyTests(unittest.TestCase):
    def test_actual_planner_separates_tool_compiler_from_module_runtimes(self):
        shell = step_body("Plan Go validation", "run")
        script = shell.split("python3 <<'PY'\n", 1)[1].rsplit("\nPY", 1)[0]
        for latest in (False, True):
            with (
                self.subTest(latest=latest),
                tempfile.TemporaryDirectory() as temporary,
            ):
                root = Path(temporary)
                files = {
                    "go.mod": "module example.org/root\ngo 1.26.0\ntoolchain go1.27.1\n",
                    ".examples/demo/go.mod": "module example.org/demo\ngo 1.28.0\n",
                    ".github/tools/go.mod": "module example.org/tools\ngo 1.28.1\n",
                }
                for name, content in files.items():
                    path = root / name
                    path.parent.mkdir(parents=True, exist_ok=True)
                    path.write_text(content, encoding="utf-8")
                subprocess.run(["git", "init", "--quiet"], cwd=root, check=True)
                subprocess.run(["git", "add", "--", *files], cwd=root, check=True)
                output = root / "outputs"
                result = subprocess.run(
                    [sys.executable, "-c", script],
                    cwd=root,
                    env={
                        **os.environ,
                        "LATEST_GO": str(latest).lower(),
                        "GITHUB_OUTPUT": str(output),
                    },
                    check=True,
                    capture_output=True,
                    text=True,
                )
                planned = dict(
                    line.split("=", 1) for line in output.read_text().splitlines()
                )
                self.assertEqual(planned["root_floor_version"], "1.26.x")
                self.assertEqual(
                    planned["root_version"], "stable" if latest else "1.27.1"
                )
                self.assertEqual(
                    planned["tools_version"], "stable" if latest else "1.28.1"
                )
                self.assertEqual(
                    json.loads(planned["example_matrix"])["include"][0]["go_version"],
                    "stable" if latest else "1.28.0",
                )
                self.assertIn("CI tools compiler:", result.stdout)

    def classify(
        self,
        files,
        *,
        actor="renovate[bot]",
        labels=(),
        same_repo=True,
        title="Update dependency",
        head_ref="renovate/update",
        changed_files="inferred",
    ):
        pr = {
            "number": 1,
            "changed_files": len(files) if changed_files == "inferred" else changed_files,
            "user": {"login": actor},
            "title": title,
            "labels": [{"name": label} for label in labels],
            "head": {
                "ref": head_ref,
                "repo": {"full_name": "owner/repo" if same_repo else "fork/repo"},
            },
        }
        fixture = {"files": files, "pr": pr}
        results = []
        for step in ("Classify changed files", "Classify PR Scope"):
            script = (
                """
const fixture = JSON.parse(process.env.POLICY_FIXTURE);
const context = {eventName: 'pull_request', repo: {owner:'owner',repo:'repo'},
  payload: {pull_request:fixture.pr, repository:{full_name:'owner/repo'}}};
const github = {paginate:async()=>fixture.files,rest:{pulls:{listFiles(){}}}};
const outputs = {};
const core = {info(){},setOutput:(key,value)=>outputs[key]=value};
await (async()=>{
"""
                + step_body(step, "script")
                + "\n})();\nconsole.log(JSON.stringify(outputs));\n"
            )
            result = subprocess.run(
                ["node", "--input-type=module", "-e", script],
                env={**os.environ, "POLICY_FIXTURE": json.dumps(fixture)},
                capture_output=True,
                text=True,
                check=True,
            )
            results.append(json.loads(result.stdout)["mode"])
        self.assertEqual(
            results[0], results[1], "Local and trusted-base classification disagree"
        )
        return results[0]

    def test_target_gate_runs_only_for_candidate_events(self):
        target = re.search(
            r"(?ms)^  pull_request_target:\n(.*?)(?=^  \S|\Z)", WORKFLOW
        )
        self.assertIsNotNone(target)
        types = re.search(r"(?m)^    types: \[([^\]]+)\]$", target[1])
        self.assertIsNotNone(types)
        self.assertCountEqual(
            [value.strip() for value in types[1].split(",")],
            ["opened", "reopened", "synchronize"],
        )
        for label_event in ("github.event.label", "context.payload.label"):
            self.assertNotIn(label_event, WORKFLOW)
        manual = (ROOT / ".github/workflows/manual-e2e-trigger.yml").read_text(
            encoding="utf-8"
        )
        self.assertIn("  workflow_dispatch:\n", manual)
        self.assertNotIn("  pull_request_target:\n", manual)

    def test_security_classification_does_not_wait_for_labels(self):
        metadata = [{"filename": "go.mod"}, {"filename": "version.go"}]
        for hint in (
            {"title": "fix(deps): update module example.org/lib [SECURITY]"},
            {"head_ref": "renovate/security-example.org-lib"},
        ):
            with self.subTest(hint=hint):
                self.assertEqual(self.classify(metadata, **hint), "security_floor")
                self.assertEqual(
                    self.classify(metadata, actor="human", **hint), "normal"
                )
                self.assertEqual(
                    self.classify(metadata, same_repo=False, **hint), "normal"
                )

    def test_actual_classifiers_keep_module_scopes_separate(self):
        cases = [
            ([{"filename": ".examples/grpc/go.mod"}], "examples_dependency"),
            ([{"filename": ".github/tools/go.mod"}], "ci_tools"),
            (
                [
                    {"filename": "renovate.json"},
                    {"filename": ".github/scripts/test_renovate_policy.py"},
                ],
                "automation_config_only",
            ),
            ([{"filename": ".github/workflows/validation_pipeline.yml"}], "ci_only"),
            (
                [{"filename": ".e2e/services/e2e-harness/Dockerfile"}],
                "renovate_cloud_e2e",
            ),
            (
                [
                    {
                        "filename": "go.mod",
                        "patch": "-toolchain go1.27.0\n+toolchain go1.27.1",
                    }
                ],
                "toolchain_update",
            ),
        ]
        for files, expected in cases:
            with self.subTest(expected=expected):
                self.assertEqual(self.classify(files, labels=["security"]), expected)

    def test_security_scope_does_not_hide_library_source_changes(self):
        metadata = [{"filename": "go.mod"}, {"filename": "version.go"}]
        self.assertEqual(self.classify(metadata, labels=["security"]), "security_floor")
        self.assertEqual(
            self.classify(metadata + [{"filename": "handler.go"}], labels=["security"]),
            "renovate_cloud_e2e",
        )
        self.assertEqual(
            self.classify(metadata, labels=["security"], actor="human"), "normal"
        )
        self.assertEqual(
            self.classify(metadata, labels=["security"], same_repo=False), "normal"
        )

    def test_license_policy_is_local_but_does_not_hide_runtime_changes(self):
        policy = [{"filename": ".licenserc.yaml"}]
        for actor in ("human", "renovate[bot]"):
            with self.subTest(actor=actor):
                self.assertEqual(self.classify(policy, actor=actor), "ci_only")
                self.assertEqual(self.classify(policy + [{"filename": ".github/workflows/validation_pipeline.yml"}], actor=actor), "ci_only")
                for path in ("handler.go", "go.mod", ".e2e/services/e2e-harness/main.go"):
                    self.assertNotEqual(self.classify(policy + [{"filename": path}], actor=actor), "ci_only")

    def test_readme_modification_is_no_cloud_for_humans_and_bots(self):
        files = [{"filename": "README.md", "status": "modified"}]
        for actor in ("human", "benchmark[bot]", "renovate[bot]"):
            for same_repo in (True, False):
                with self.subTest(actor=actor, same_repo=same_repo):
                    self.assertEqual(
                        self.classify(files, actor=actor, same_repo=same_repo),
                        "readme_only",
                    )

    def test_readme_exception_requires_complete_unambiguous_file_scope(self):
        readme = {"filename": "README.md", "status": "modified"}
        cases = [
            [],
            [{"filename": "README.md"}],
            [{**readme, "previous_filename": "handler.go"}],
            [{**readme, "filename": "docs/README.md"}],
        ]
        cases.extend(
            [[{**readme, "status": status}] for status in ("added", "removed", "renamed", "copied")]
        )
        cases.extend(
            [readme, {"filename": path, "status": "modified"}]
            for path in (
                "handler.go", "go.mod", "go.sum", "version.go",
                ".github/workflows/validation_pipeline.yml", ".benchmarks/main.go",
                "docs/USAGE.md", ".e2e/services/e2e-harness/main.go",
            )
        )
        for files in cases:
            for actor in ("human", "benchmark[bot]", "renovate[bot]"):
                with self.subTest(files=files, actor=actor):
                    self.assertNotEqual(self.classify(files, actor=actor), "readme_only")
        for count in (None, 0, 2, 3001, "1"):
            with self.subTest(reported_count=count):
                self.assertNotEqual(
                    self.classify([readme], changed_files=count), "readme_only"
                )

    def readme_step_enabled(self, name, *, validation_ok="true"):
        fixture = {
            "pr_scope": {"mode": "readme_only"},
            "trigger_type": {"should_auto_trigger": "true"},
            "wait_validation": {"ok": validation_ok},
            "app_token_secrets": {"available": "true"},
            "have_secrets": {"ok": "true"},
            "cloud_build": {"build_status": "SUCCESS"},
        }
        script = """
const steps = Object.fromEntries(Object.entries(JSON.parse(process.env.STEPS)).map(
  ([name, outputs]) => [name, {outputs}]));
const always = () => true;
const success = () => true;
console.log(Boolean(
""" + step_body(name, "if") + "\n));"
        result = subprocess.run(
            ["node", "--input-type=module", "-e", script],
            env={**os.environ, "STEPS": json.dumps(fixture)},
            check=True, capture_output=True, text=True,
        )
        return result.stdout.strip() == "true"

    def test_readme_uses_existing_local_validation_and_freshness_gates(self):
        self.assertTrue(self.readme_step_enabled("Wait for scoped local validation to succeed"))
        for name in (
            "Revalidate PR authority before E2E acceptance",
            "Generate App Token for No-Cloud Finalization",
            "Finalize stable E2E check for updates that do not require cloud E2E",
        ):
            for ok in ("true", "false", ""):
                with self.subTest(step=name, validation_ok=ok):
                    self.assertEqual(self.readme_step_enabled(name, validation_ok=ok), ok == "true")
        for name in (
            "Checkout Target Source For Cloud E2E",
            "Detect GCP E2E secret availability",
            "Run E2E Cloud Build",
            "Generate App Token for Cloud E2E Finalization",
            "Finalize stable E2E check after Cloud Build",
        ):
            with self.subTest(step=name):
                self.assertFalse(self.readme_step_enabled(name))

    def test_same_repository_readme_automatically_enters_policy_path(self):
        condition = step_body("Determine Automatic Policy Path", "run").split(
            "if [[", 1
        )[1].split("]]; then", 1)[0]
        for mode in ("readme_only", "normal"):
            for same_repo in (True, False):
                with self.subTest(mode=mode, same_repo=same_repo):
                    expression = condition
                    for key, value in (
                        ("github.event.pull_request.head.repo.full_name", "owner/repo" if same_repo else "fork/repo"),
                        ("github.repository", "owner/repo"),
                        ("steps.pr_scope.outputs.mode", mode),
                    ):
                        expression = expression.replace("${{ " + key + " }}", value)
                    result = subprocess.run(
                        ["node", "--input-type=module", "-e", "console.log(Boolean(" + expression + "));"],
                        check=True, capture_output=True, text=True,
                    )
                    self.assertEqual(result.stdout.strip() == "true", same_repo and mode == "readme_only")

    def test_readme_finalization_rejects_stale_or_missing_validation(self):
        script = step_body(
            "Finalize stable E2E check for updates that do not require cloud E2E", "script"
        )
        for expression, value in (
            ("steps.create_run_check.outputs.check_run_id", "17"),
            ("steps.run_id.outputs.e2e_run_id", "fixture-run"),
            ("steps.pr_scope.outputs.mode", "readme_only"),
        ):
            script = script.replace("${{ " + expression + " }}", value)
        harness = """
const context = {repo:{owner:'owner',repo:'repo'}};
const result = {updates:[],outputs:{},failed:false};
const github = {rest:{checks:{update:async args=>result.updates.push(args)}}};
const core = {setOutput:(key,value)=>result.outputs[key]=value,
  setFailed:()=>{result.failed=true}};
"""
        for ok in ("true", "false", ""):
            with self.subTest(validation_ok=ok):
                completed = subprocess.run(
                    ["node", "--input-type=module", "-e", harness + script + "\nconsole.log(JSON.stringify(result));"],
                    env={**os.environ, "FINAL_VALIDATION_OK": ok,
                         "FINAL_VALIDATION_SUMMARY": "validation fixture", "CHECK_RUN_NAME": "E2E Tests (GCP)"},
                    check=True, capture_output=True, text=True,
                )
                result = json.loads(completed.stdout)
                self.assertEqual(len(result["updates"]), 1)
                self.assertEqual(result["updates"][0]["conclusion"], "success" if ok == "true" else "failure")
                self.assertEqual(result["outputs"]["ok"], "true" if ok == "true" else "false")
                self.assertEqual(result["failed"], ok != "true")

    def aggregate(self, **overrides):
        env = {
            **os.environ,
            "SCOPE_MODE": "automation_config_only",
            "GO_VALIDATION_MODE": "requirements",
            "GO_PLAN_RESULT": "success",
            "CI_ACTION_RESULT": "success",
            "CI_ACTION_PASSED": "true",
            "ROOT_FLOOR_VALIDATION_RESULT": "success",
            "ROOT_VALIDATION_RESULT": "success",
            "E2E_HARNESS_VALIDATION_RESULT": "success",
            "HAS_EXAMPLES": "true",
            "EXAMPLE_VALIDATION_RESULT": "success",
            **overrides,
        }
        bash = "bash"
        if os.name == "nt":
            bash = str(
                Path(os.environ.get("ProgramFiles", "C:/Program Files"))
                / "Git/bin/bash.exe"
            )
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "outputs"
            env["GITHUB_OUTPUT"] = output.as_posix()
            result = subprocess.run(
                [bash, "-s"],
                input=step_body("Evaluate scoped local validation result", "run"),
                env=env,
                text=True,
                capture_output=True,
            )
            content = output.read_text() if output.exists() else ""
            return result.returncode, content

    def test_actual_aggregate_accepts_real_success(self):
        self.assertEqual(self.aggregate(), (0, "validation_passed=true\n"))
        self.assertEqual(self.aggregate(SCOPE_MODE="readme_only"), (0, "validation_passed=true\n"))

    def test_actual_aggregate_rejects_every_unsuccessful_expected_lane(self):
        for lane in (
            "GO_PLAN_RESULT",
            "CI_ACTION_RESULT",
            "ROOT_FLOOR_VALIDATION_RESULT",
            "ROOT_VALIDATION_RESULT",
            "E2E_HARNESS_VALIDATION_RESULT",
            "EXAMPLE_VALIDATION_RESULT",
        ):
            for result in ("failure", "skipped", "cancelled", ""):
                with self.subTest(lane=lane, result=result):
                    for scope in ("automation_config_only", "readme_only"):
                        code, output = self.aggregate(SCOPE_MODE=scope, **{lane: result})
                        self.assertNotEqual(code, 0)
                        self.assertNotIn("validation_passed=true", output)

    def test_deliberately_absent_examples_are_not_a_failure(self):
        self.assertEqual(
            self.aggregate(HAS_EXAMPLES="false", EXAMPLE_VALIDATION_RESULT="skipped"),
            (0, "validation_passed=true\n"),
        )
        self.assertNotEqual(
            self.aggregate(HAS_EXAMPLES="", EXAMPLE_VALIDATION_RESULT="skipped")[0], 0
        )

    def test_latest_go_lane_does_not_repeat_unrelated_action_smokes(self):
        self.assertEqual(
            self.aggregate(
                GO_VALIDATION_MODE="latest",
                CI_ACTION_RESULT="skipped",
                CI_ACTION_PASSED="",
            ),
            (0, "validation_passed=true\n"),
        )
        self.assertNotEqual(self.aggregate(CI_ACTION_PASSED="")[0], 0)

    def test_validation_does_not_write_commits_or_merge_pull_requests(self):
        for forbidden in (
            "git push",
            "git commit",
            "github.rest.pulls.merge",
            "github.rest.repos.createOrUpdateFileContents",
            "allow_go_sum_only_drift",
            "security_release_version",
        ):
            self.assertNotIn(forbidden, WORKFLOW)
        self.assertIn("python3 -m unittest discover -s .github/scripts", WORKFLOW)
        self.assertIn("validate_renovate_pr.py --base", WORKFLOW)
        self.assertIn(
            "jobs.local_validation_policy.outputs.validation_passed", WORKFLOW
        )


class LicensePolicyTests(unittest.TestCase):
    def test_license_validation_uses_versioned_policy(self):
        self.assertNotIn("date +%Y", WORKFLOW)
        self.assertNotIn("steps.year.outputs.YEAR", WORKFLOW)
        self.assertNotRegex(WORKFLOW, r"sed[^\n]*copyright-year:")
        self.assertIn("header check", WORKFLOW)
        self.assertIn(".licenserc.yaml", WORKFLOW)


if __name__ == "__main__":
    unittest.main()
