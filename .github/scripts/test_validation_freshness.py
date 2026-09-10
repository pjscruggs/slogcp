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

"""Execute the actual final freshness guard with controlled GitHub responses."""

import copy
import json
import os
from pathlib import Path
import re
import subprocess
import unittest


ROOT = Path(__file__).resolve().parents[2]
WORKFLOW = (ROOT / ".github/workflows/validation_pipeline.yml").read_text(encoding="utf-8")


def step(name):
    block = WORKFLOW.split("      - name: " + name + "\n", 1)[1]
    return block.split("\n      - name:", 1)[0]


def script(name):
    lines = step(name).split("          script: |\n", 1)[1].splitlines()
    body = []
    for line in lines:
        if line.strip() and not line.startswith("            "):
            break
        body.append(line[12:] if line.strip() else "")
    return "\n".join(body)


class FinalFreshnessTests(unittest.TestCase):
    def fixture(self):
        pr = {
            "number": 7, "state": "open",
            "head": {"sha": "a" * 40, "repo": {"full_name": "owner/repo"}},
            "base": {"sha": "b" * 40, "ref": "main", "repo": {"full_name": "owner/repo"}},
        }
        return {
            "expected": copy.deepcopy(pr), "pr": pr,
            "base": "b" * 40, "current": "b" * 40,
            "run": {"id": 10, "workflow_id": 5, "run_attempt": 2},
            "runs": [],
        }

    def execute(self, fixture):
        harness = """
const f = JSON.parse(process.env.FIXTURE);
const context = {repo:{owner:'owner',repo:'repo'},runId:10,payload:{pull_request:f.expected}};
const core = {info: message => console.log(message)};
const github = {rest:{
  actions:{
    getWorkflowRun:async()=>({data:f.run}),
    listWorkflowRuns:async args=>{
      if (args.workflow_id !== f.run.workflow_id || args.head_sha !== f.expected.head.sha ||
          args.event !== 'pull_request') throw Error('Wrong validation run query');
      return {data:{workflow_runs:f.runs}};
    }
  },
  pulls:{get:async()=>{if(f.apiError)throw Error('HTTP 403');return {data:f.pr}}},
  git:{getRef:async()=>({data:{object:{sha:f.current}}})}
}};
"""
        return subprocess.run(
            ["node", "--input-type=module", "-e", harness + script("Recheck validated PR identity")],
            env={**os.environ, "FIXTURE": json.dumps(fixture),
                 "VALIDATED_BASE": fixture["base"], "GITHUB_RUN_ATTEMPT": "2"},
            capture_output=True, text=True,
        )

    def test_current_subject_passes(self):
        result = self.execute(self.fixture())
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Validated current PR head", result.stdout)

    def test_stale_missing_or_wrong_identity_fails(self):
        for field in ("base", "head", "closed", "retarget", "base_repo",
                      "head_repo", "pr_base", "missing_base", "attempt", "api_error"):
            fixture = self.fixture()
            if field == "base":
                fixture["current"] = "c" * 40
            elif field == "head":
                fixture["pr"]["head"]["sha"] = "c" * 40
            elif field == "closed":
                fixture["pr"]["state"] = "closed"
            elif field == "retarget":
                fixture["pr"]["base"]["ref"] = "other"
            elif field == "base_repo":
                fixture["pr"]["base"]["repo"]["full_name"] = "other/repo"
            elif field == "head_repo":
                fixture["pr"]["head"]["repo"]["full_name"] = "other/repo"
            elif field == "pr_base":
                fixture["pr"]["base"]["sha"] = "c" * 40
            elif field == "missing_base":
                fixture["base"] = ""
            elif field == "attempt":
                fixture["run"]["run_attempt"] = 3
            else:
                fixture["apiError"] = True
            with self.subTest(field=field):
                result = self.execute(fixture)
                self.assertNotEqual(result.returncode, 0)
                self.assertNotIn("Validated current PR head", result.stdout)

    def test_newer_run_blocks_even_if_older_run_was_successful(self):
        for conclusion in ("success", "failure", None):
            fixture = self.fixture()
            fixture["runs"] = [{"id": 11, "conclusion": conclusion,
                                "pull_requests": [{"number": 7}]}]
            with self.subTest(conclusion=conclusion):
                self.assertNotEqual(self.execute(fixture).returncode, 0)

    def test_other_pr_does_not_supersede_this_run(self):
        fixture = self.fixture()
        fixture["runs"] = [{"id": 11, "pull_requests": [{"number": 8}]}]
        self.assertEqual(self.execute(fixture).returncode, 0)

    def test_guard_precedes_accepting_output_and_runs_for_reused_pr_workflows(self):
        guard = step("Recheck validated PR identity")
        self.assertIn("if: github.event_name == 'pull_request'", guard)
        self.assertNotIn("continue-on-error", guard)
        self.assertLess(WORKFLOW.index("      - name: Recheck validated PR identity"),
                        WORKFLOW.index('echo "validation_passed=true"'))
        self.assertIn("validated_base: \u0024{{ steps.current_base.outputs.sha }}", WORKFLOW)
        self.assertIn('echo "sha=$current_base" >> "$GITHUB_OUTPUT"', WORKFLOW)

    def test_reusable_callers_grant_actions_read(self):
        for name in ("auto-release.yml", "latest-go-canary.yml"):
            content = (ROOT / ".github/workflows" / name).read_text(encoding="utf-8")
            jobs = re.split(r"(?m)^  [a-zA-Z_][a-zA-Z_0-9]*:\s*$", content)
            calls = [job for job in jobs if "uses: ./.github/workflows/validation_pipeline.yml" in job]
            self.assertEqual(len(calls), 1)
            self.assertIn("actions: read", calls[0])


class NoCloudFreshnessTests(unittest.TestCase):
    def execute(self, body, *, final_ok="true"):
        # Evaluate the workflow JavaScript, substituting only workflow expressions.
        body = re.sub(r"\$\{\{.*?\}\}", "fixture", body)
        harness = """
const context = {repo:{owner:'owner',repo:'repo'}};
const updates = [];
const outputs = {};
const failures = [];
const core = {setOutput:(key,value)=>outputs[key]=value,setFailed:value=>failures.push(value)};
const github = {rest:{checks:{update:async value=>updates.push(value)}}};
"""
        result = subprocess.run(
            ["node", "--input-type=module", "-e", harness + body +
             "\nconsole.log(JSON.stringify({updates,outputs,failures}));"],
            env={**os.environ, "FINAL_VALIDATION_OK": final_ok,
                 "FINAL_VALIDATION_SUMMARY": "Authority result", "CHECK_RUN_NAME": "E2E Tests (GCP)"},
            capture_output=True, text=True, check=True,
        )
        return json.loads(result.stdout)

    def test_no_cloud_acceptance_requires_final_authority(self):
        body = script("Finalize stable E2E check for updates that do not require cloud E2E")
        for ok in ("true", "false", ""):
            with self.subTest(ok=ok):
                result = self.execute(body, final_ok=ok)
                self.assertEqual(result["updates"][0]["conclusion"],
                                 "success" if ok == "true" else "failure")
                self.assertEqual(result["outputs"]["ok"], "true" if ok == "true" else "false")
                self.assertEqual(bool(result["failures"]), ok != "true")

    def test_authority_guard_runs_for_no_cloud_modes_without_cloud_secrets(self):
        block = step("Revalidate PR authority before E2E acceptance")
        condition = block.split("        if: |\n", 1)[1].split("        id:", 1)[0].strip()
        for mode in ("ci_only", "ci_tools", "automation_config_only", "examples_dependency",
                     "toolchain_update", "security_floor", "renovate_cloud_e2e", "normal"):
            for local_ok in ("true", "false"):
                fixture = {
                    "trigger_type": {"outputs": {"should_auto_trigger": "true"}},
                    "pr_scope": {"outputs": {"mode": mode}},
                    "wait_validation": {"outputs": {"ok": local_ok}},
                    "have_secrets": {"outputs": {"ok": "false"}},
                    "cloud_build": {"outputs": {"build_status": ""}},
                }
                result = subprocess.run(
                    ["node", "-e", "const steps=JSON.parse(process.env.FIXTURE);"
                     "const always=()=>true; console.log(Boolean(" + condition + "));"],
                    env={**os.environ, "FIXTURE": json.dumps(fixture)},
                    capture_output=True, text=True, check=True,
                )
                expected = local_ok == "true" and mode in {
                    "ci_only", "ci_tools", "automation_config_only", "examples_dependency", "toolchain_update"
                }
                self.assertEqual(result.stdout.strip(), str(expected).lower(), (mode, local_ok))


if __name__ == "__main__":
    unittest.main()
