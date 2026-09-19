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

"""Execute manual E2E authorization and immutable runner selection guards."""

import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import unittest


WORKFLOW = (Path(__file__).resolve().parents[1] / "workflows/manual-e2e-trigger.yml").read_text(
    encoding="utf-8"
)
HEAD = "b" * 40
MAIN = "d" * 40


def step(name):
    return WORKFLOW.split("      - name: " + name + "\n", 1)[1].split("\n      - name:", 1)[0]


def body(name, key):
    block = step(name)
    match = re.search(r"(?m)^( +)" + key + r": \|\n", block)
    indent = len(match[1]) + 2
    lines = []
    for line in block[match.end():].splitlines():
        if line.strip() and len(line) - len(line.lstrip()) < indent:
            break
        lines.append(line[indent:] if line.strip() else "")
    return "\n".join(lines)


class InfrastructureSelectionTests(unittest.TestCase):
    def execute(self, name="Select Reviewed Infrastructure SHA", **overrides):
        git_bash = Path("C:/Program Files/Git/bin/bash.exe")
        bash = str(git_bash) if git_bash.exists() else shutil.which("bash")
        if not bash:
            self.skipTest("bash is required to execute the manual E2E guards")
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "outputs"
            result = subprocess.run(
                [bash, "--noprofile", "--norc", "-c", body(name, "run")],
                env={**os.environ, "GITHUB_EVENT_NAME": "workflow_dispatch",
                     "GITHUB_REF": "refs/heads/main", "GITHUB_SHA": MAIN,
                     "DISPATCH_MODE": "pr", "DISPATCH_PR_NUMBER": "7",
                     "DISPATCH_REVIEWED_INFRASTRUCTURE_SHA": "",
                     "PR_HEAD_SHA": HEAD, "LOCAL_VALIDATION_OK": "true",
                     "GITHUB_OUTPUT": output.as_posix(), **overrides},
                capture_output=True, text=True,
            )
            selected = output.read_text() if output.exists() else ""
            return result, selected

    def test_default_runner_is_immutable_workflow_revision(self):
        for mode in ("pr", "main_latest"):
            result, output = self.execute(DISPATCH_MODE=mode)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(output, f"sha={MAIN}\n")

    def test_explicit_reviewed_runner_is_exact_validated_pr_head(self):
        result, output = self.execute(DISPATCH_REVIEWED_INFRASTRUCTURE_SHA=HEAD)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(output, f"sha={HEAD}\n")

    def test_untrusted_runner_selections_fail_without_output(self):
        cases = [
            {"DISPATCH_REVIEWED_INFRASTRUCTURE_SHA": "feature/runner"},
            {"DISPATCH_REVIEWED_INFRASTRUCTURE_SHA": HEAD[:7]},
            {"DISPATCH_REVIEWED_INFRASTRUCTURE_SHA": HEAD + "\nsha=evil"},
            {"DISPATCH_REVIEWED_INFRASTRUCTURE_SHA": "a" * 40},
            {"DISPATCH_MODE": "main_latest"},
            {"LOCAL_VALIDATION_OK": "false"},
            {"LOCAL_VALIDATION_OK": ""},
            {"PR_HEAD_SHA": "a" * 40},
            {"GITHUB_REF": "refs/heads/feature"},
            {"GITHUB_EVENT_NAME": "pull_request_target"},
        ]
        for case in cases:
            with self.subTest(case=case):
                result, output = self.execute(**{
                    "DISPATCH_REVIEWED_INFRASTRUCTURE_SHA": HEAD, **case,
                })
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(output, "")

    def test_input_gate_requires_main_and_rejects_latest_override(self):
        for case in ({"GITHUB_REF": "refs/heads/feature"},
                     {"GITHUB_EVENT_NAME": "push"},
                     {"DISPATCH_REVIEWED_INFRASTRUCTURE_SHA": "main"},
                     {"DISPATCH_MODE": "main_latest", "DISPATCH_PR_NUMBER": "",
                      "DISPATCH_REVIEWED_INFRASTRUCTURE_SHA": HEAD}):
            with self.subTest(case=case):
                result, _ = self.execute("Validate Inputs", **case)
                self.assertNotEqual(result.returncode, 0)
        for mode, number in (("pr", "7"), ("main_latest", "")):
            result, _ = self.execute("Validate Inputs", DISPATCH_MODE=mode,
                                     DISPATCH_PR_NUMBER=number)
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_selection_precedes_credentials_and_immutable_checkouts(self):
        selection = WORKFLOW.index("      - name: Select Reviewed Infrastructure SHA")
        self.assertLess(WORKFLOW.index("      - name: Require successful local validation"), selection)
        for name in ("Generate App Token for E2E", "Checkout Workflow Repository",
                     "Checkout Target Source", "Authenticate to Google Cloud"):
            self.assertLess(selection, WORKFLOW.index("      - name: " + name))
        self.assertNotIn("continue-on-error", step("Select Reviewed Infrastructure SHA"))
        self.assertIn("ref: ${{ steps.infrastructure.outputs.sha }}", step("Checkout Workflow Repository"))
        self.assertIn("ref: ${{ steps.target.outputs.head_sha }}", step("Checkout Target Source"))
        self.assertIn("INFRASTRUCTURE_SHA: ${{ steps.infrastructure.outputs.sha }}", step("Finalize E2E Check"))

    def test_no_finalization_token_is_minted_after_early_rejection(self):
        for name in ("Generate App Token for E2E Finalization", "Finalize E2E Check"):
            condition = re.search(r"if: \$\{\{ (.*?) \}\}", step(name))[1]
            for check_id, local_ok, pr_number, expected in (
                ("", "", "", False), ("", "true", "7", False),
                ("123", "true", "7", True), ("123", "false", "7", False),
                ("123", "", "", True),
            ):
                fixture = {"create_check": {"outputs": {"check_run_id": check_id}},
                           "target": {"outputs": {"pr_number": pr_number}},
                           "local_validation_gate": {"outputs": {"ok": local_ok}}}
                result = subprocess.run(
                    ["node", "-e", "const steps=JSON.parse(process.env.FIXTURE);"
                     "const always=()=>true;console.log(Boolean(" + condition + "));"],
                    env={**os.environ, "FIXTURE": json.dumps(fixture)},
                    capture_output=True, text=True, check=True,
                )
                self.assertEqual(result.stdout.strip(), str(expected).lower(), (name, fixture))


class ManualAuthorityTests(unittest.TestCase):
    def execute(self, name, **overrides):
        fixture = {"head_repo": "owner/repo", "head": HEAD, "status": "success",
                   "comparison": "ahead", "behind": 0, **overrides}
        harness = """
const f=JSON.parse(process.env.FIXTURE);
const outputs={},failures=[];
const context={repo:{owner:'owner',repo:'repo'}};
const core={info(){},setOutput:(key,value)=>outputs[key]=value,setFailed:value=>failures.push(value)};
const github={rest:{
  actions:{listWorkflowRuns:async()=>({data:{workflow_runs:[{
    id:10,run_attempt:1,head_sha:process.env.EXPECTED_SHA,event:'pull_request',
    path:'.github/workflows/validation_pipeline.yml',pull_requests:[{number:7}],
    status:'completed',conclusion:f.status}]}}),listJobsForWorkflowRun(){}},
  pulls:{get:async()=>({data:{state:'open',
    head:{sha:f.head,repo:{full_name:f.head_repo}},
    base:{ref:'main',repo:{full_name:'owner/repo'}}}})},
  git:{getRef:async()=>({data:{object:{sha:'dddddddddddddddddddddddddddddddddddddddd'}}})},
  repos:{compareCommits:async()=>({data:{status:f.comparison,behind_by:f.behind,
    merge_base_commit:{sha:'dddddddddddddddddddddddddddddddddddddddd'}}})}
},paginate:async()=>[{id:11,run_id:10,run_attempt:1,head_sha:process.env.EXPECTED_SHA,
  name:'Local Validation Ready for E2E',status:'completed',conclusion:'success'}]};
"""
        result = subprocess.run(
            ["node", "--input-type=module", "-e", harness +
             "\nawait (async()=>{\n" + body(name, "script") +
             "\n})();console.log(JSON.stringify({outputs,failures}));"],
            env={**os.environ, "FIXTURE": json.dumps(fixture), "PR_NUMBER": "7",
                 "EXPECTED_SHA": HEAD, "LOCAL_VALIDATION_CHECK_NAME": "Local Validation Ready for E2E"},
            check=True, capture_output=True, text=True,
        )
        return json.loads(result.stdout)

    def test_both_gates_require_current_validated_same_repository_head(self):
        for name in ("Require successful local validation before manual PR E2E",
                     "Revalidate PR authority after E2E"):
            self.assertEqual(self.execute(name)["outputs"]["ok"], "true")
            for case in ({"head_repo": "fork/repo"}, {"head_repo": ""},
                         {"head": "a" * 40}, {"status": "failure"},
                         {"comparison": "diverged", "behind": 1}):
                with self.subTest(name=name, case=case):
                    result = self.execute(name, **case)
                    self.assertNotEqual(result["outputs"].get("ok"), "true")


if __name__ == "__main__":
    unittest.main()
