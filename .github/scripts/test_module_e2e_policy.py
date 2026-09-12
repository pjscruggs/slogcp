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

"""Execute optional module cloud authorization against controlled GitHub state."""

import json
import os
from pathlib import Path
import subprocess
import textwrap
import unittest


WORKFLOW = (Path(__file__).resolve().parents[1] / "workflows/module-candidate-e2e.yml").read_text(encoding="utf-8")
HEAD = "a" * 40
BASE = "b" * 40
INFRASTRUCTURE = "c" * 40


class ModuleE2EPolicyTests(unittest.TestCase):
    def authorize(self, **overrides):
        fixture = {
            "event": "pull_request_target", "kind": "pubsub", "repository": "slogcp-pubsub",
            "head": HEAD, "base": BASE, "head_repository": "pjscruggs/slogcp-pubsub",
            "comparison_base": BASE, "behind": 0, "conclusion": "success",
            "check_app": "github-actions", "project": "fixture", "expected_project": "fixture",
            "infrastructure": INFRASTRUCTURE, "ref": "refs/heads/main",
        }
        fixture.update(overrides)
        step = WORKFLOW.split("      - name: Authorize candidate and wait for current local validation\n", 1)[1].split("\n      - name:", 1)[0]
        script = textwrap.dedent(step.split("          script: |\n", 1)[1])
        harness = r'''
const f = JSON.parse(process.env.FIXTURE);
const outputs = {};
const context = {repo: {owner: 'pjscruggs', repo: f.repository}, eventName: f.event,
 ref: f.ref, sha: 'a'.repeat(40), payload: {pull_request: f.event === 'pull_request_target' ? {number: 1, head: {sha: 'a'.repeat(40)}} : undefined}};
const core = {setOutput: (key, value) => outputs[key] = value};
const setTimeout = callback => callback();
const github = {rest: {
 repos: {
  getContent: async ({ref}) => {
   if (ref !== 'd'.repeat(40)) throw Error('wrong caller source revision');
   return {data: {content: Buffer.from('uses: pjscruggs/slogcp/.github/workflows/module-candidate-e2e.yml@' + f.infrastructure).toString('base64')}};
  },
  compareCommitsWithBasehead: async () => ({data: {behind_by: f.behind, merge_base_commit: {sha: f.comparison_base}}})
 },
 git: {getRef: async () => ({data: {object: {sha: f.base}}})},
 pulls: {get: async () => ({data: {state: 'open', head: {sha: f.head, repo: {full_name: f.head_repository}},
  base: {ref: 'main', repo: {full_name: 'pjscruggs/' + f.repository}}}})},
 checks: {listForRef: async () => ({data: {check_runs: [{id: 1, name: 'Module Local Validation Policy',
  app: {slug: f.check_app}, status: 'completed', conclusion: f.conclusion}]}})}
}};
(async () => { SCRIPT
})().then(() => console.log(JSON.stringify(outputs))).catch(error => {console.error(error.message); process.exitCode = 1;});
'''.replace("SCRIPT", script)
        return subprocess.run(
            ["node", "-e", harness], capture_output=True, text=True,
            env={**os.environ, "FIXTURE": json.dumps(fixture), "CANDIDATE_KIND": fixture["kind"],
                 "EXPECTED_PROJECT": fixture["expected_project"], "GCP_PROJECT_ID": fixture["project"],
                 "LOCAL_CHECK_NAME": "Module Local Validation Policy", "CALLER_WORKFLOW_SHA": "d" * 40},
        )

    def test_current_validated_subject_uses_the_trusted_caller_pin(self):
        result = self.authorize()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), {"sha": HEAD, "base": BASE, "infrastructure_revision": INFRASTRUCTURE})

    def test_wrong_identity_and_failed_or_skipped_validation_are_rejected(self):
        for changes in (
            {"kind": "grpc"}, {"head": "f" * 40}, {"head_repository": "foreign/fork"},
            {"comparison_base": "e" * 40}, {"behind": 1}, {"project": "wrong"},
            {"infrastructure": "main"}, {"check_app": "other-app"},
            *({"conclusion": outcome} for outcome in ("failure", "skipped", "cancelled", "neutral")),
        ):
            with self.subTest(changes=changes):
                result = self.authorize(**changes)
                self.assertNotEqual(result.returncode, 0, result.stdout)

    def test_release_subject_must_be_main(self):
        self.assertEqual(self.authorize(event="push").returncode, 0)
        self.assertNotEqual(self.authorize(event="push", ref="refs/heads/topic").returncode, 0)
        self.assertNotEqual(self.authorize(event="pull_request").returncode, 0)


if __name__ == "__main__":
    unittest.main()
