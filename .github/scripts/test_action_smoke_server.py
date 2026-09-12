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

"""Executable protocol and candidate-pin checks for the action smoke fixture."""

import base64
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import textwrap
import threading
import time
import unittest
from urllib.error import HTTPError
from urllib.parse import urlencode
from urllib.request import Request, urlopen

import action_smoke_server as smoke


def encoded(value):
    return base64.urlsafe_b64encode(json.dumps(value).encode()).decode().rstrip("=")


class ProtocolTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory()
        cls.directory = Path(cls.temporary.name)
        cls.config = smoke.generate_fixture(cls.directory, "http://127.0.0.1:12345")

    @classmethod
    def tearDownClass(cls):
        cls.temporary.cleanup()

    def setUp(self):
        self.fixture = smoke.Fixture(self.config)

    def app_jwt(self, **changes):
        claims = {"iss": self.config["app_id"], "iat": int(time.time()) - 60,
                  "exp": int(time.time()) + 540, **changes}
        message = encoded({"alg": "RS256"}) + "." + encoded(claims)
        signed = subprocess.run(
            ["openssl", "dgst", "-sha256", "-sign", self.config["private_key_path"]],
            input=message.encode(), capture_output=True, check=True,
        ).stdout
        return message + "." + base64.urlsafe_b64encode(signed).decode().rstrip("=")

    def sts_body(self):
        return {
            "audience": self.fixture.audience,
            "grantType": "urn:ietf:params:oauth:grant-type:token-exchange",
            "requestedTokenType": "urn:ietf:params:oauth:token-type:access_token",
            "scope": smoke.SCOPE,
            "subjectTokenType": "urn:ietf:params:oauth:token-type:jwt",
            "subjectToken": smoke.OIDC_TOKEN,
        }

    def test_signed_app_installation_and_scoped_token(self):
        headers = {"Authorization": "Bearer " + self.app_jwt()}
        status, result = self.fixture.handle("GET", "/repos/fixture-owner/fixture-repository/installation", headers, {})
        self.assertEqual((status, result["id"]), (200, 123))
        status, result = self.fixture.handle("POST", "/app/installations/123/access_tokens", headers,
            {"repositories": ["fixture-repository"], "permissions": {"contents": "read"}})
        self.assertEqual((status, result["token"]), (201, smoke.APP_TOKEN))

    def test_app_rejects_wrong_signature_and_claims(self):
        for token in [self.app_jwt()[:-10] + "AAAAAAAAAA", self.app_jwt(iss="wrong"),
                      self.app_jwt(exp=0), self.app_jwt(iat=0), "malformed"]:
            with self.subTest(token=token[:8]), self.assertRaises(ValueError):
                self.fixture.verify_app_jwt("Bearer " + token)

    def test_app_rejects_broader_repositories_or_permissions(self):
        headers = {"Authorization": "Bearer " + self.app_jwt()}
        for body in [{}, {"repositories": ["other"], "permissions": {"contents": "read"}},
                     {"repositories": ["fixture-repository"], "permissions": {"contents": "write"}}]:
            with self.subTest(body=body), self.assertRaises(ValueError):
                self.fixture.handle("POST", "/app/installations/123/access_tokens", headers, body)

    def test_oidc_requires_authorization_and_audience(self):
        target = "/oidc?" + urlencode({"audience": "https://iam.googleapis.com/" + self.config["provider"]})
        headers = {"Authorization": "Bearer " + self.config["oidc_request_token"]}
        self.assertEqual(self.fixture.handle("GET", target, headers, {})[1], {"value": smoke.OIDC_TOKEN})
        for route, header in [("/oidc", headers), (target, {})]:
            with self.assertRaises(ValueError):
                self.fixture.handle("GET", route, header, {})

    def test_sts_checks_every_exchange_field(self):
        self.assertEqual(self.fixture.handle("POST", "/sts/v1/token", {}, self.sts_body())[1]["access_token"], smoke.FEDERATED_TOKEN)
        for key in self.sts_body():
            body = self.sts_body()
            body[key] = "wrong"
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.fixture.handle("POST", "/sts/v1/token", {}, body)

    def test_impersonation_checks_token_scope_and_lifetime(self):
        target = "/iamcredentials/v1/projects/-/serviceAccounts/" + self.config["service_account"] + ":generateAccessToken"
        headers = {"Authorization": "Bearer " + smoke.FEDERATED_TOKEN}
        body = {"scope": [smoke.SCOPE], "lifetime": "3600s", "delegates": []}
        self.assertEqual(self.fixture.handle("POST", target, headers, body)[1]["accessToken"], smoke.ACCESS_TOKEN)
        for key, wrong in [("scope", ["unexpected"]), ("lifetime", "7200s"), ("delegates", ["other"]), ("extra", True)]:
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.fixture.handle("POST", target, headers, {**body, key: wrong})
        with self.assertRaises(ValueError):
            self.fixture.handle("POST", target, {}, body)

    def test_unknown_request_is_rejected(self):
        with self.assertRaises(ValueError):
            self.fixture.handle("POST", "/unexpected", {}, {})

    def test_revocation_requires_fixture_token(self):
        self.assertEqual(self.fixture.handle("DELETE", "/installation/token", {"Authorization": "token " + smoke.APP_TOKEN}, {})[0], 204)
        with self.assertRaises(ValueError):
            self.fixture.handle("DELETE", "/installation/token", {}, {})

    def test_server_records_rejections(self):
        server = smoke.make_server()
        server.fixture = self.fixture
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            endpoint = f"http://127.0.0.1:{server.server_port}"
            with self.assertRaises(HTTPError):
                urlopen(endpoint + "/unexpected", timeout=5)
            with urlopen(endpoint + "/__status", timeout=5) as response:
                status = json.load(response)
            self.assertEqual(status["errors"], ["Unexpected request: GET /unexpected"])
        finally:
            server.shutdown()
            thread.join(timeout=5)
            server.server_close()

    def test_verify_rejects_missing_execution(self):
        server = smoke.make_server()
        server.fixture = self.fixture
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            with tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                (directory / "fixture.json").write_text(json.dumps({**self.config, "base_url": f"http://127.0.0.1:{server.server_port}"}))
                with self.assertRaisesRegex(ValueError, "did not execute"):
                    smoke.verify(directory, directory / "missing.json", smoke.APP_TOKEN, smoke.ACCESS_TOKEN)
        finally:
            server.shutdown()
            thread.join(timeout=5)
            server.server_close()


class CandidatePinTests(unittest.TestCase):
    def test_privileged_consumers_use_the_smoke_tested_revision(self):
        workflows = Path(__file__).resolve().parents[1] / "workflows"
        smoke_workflow = (workflows / "ci-action-smoke.yml").read_text(encoding="utf-8")
        for action in ["actions/create-github-app-token", "google-github-actions/auth", "google-github-actions/setup-gcloud",
                       "actions/upload-artifact", "actions/download-artifact"]:
            pattern = re.compile(r"uses:\s*['\"]?" + re.escape(action) + r"@([0-9a-f]{40})(?:\s|['\"])" )
            smoke_pins = set(pattern.findall(smoke_workflow))
            self.assertEqual(len(smoke_pins), 1, f"Expected one immutable smoke pin for {action}")
            for workflow in workflows.glob("*.yml"):
                pins = set(pattern.findall(workflow.read_text(encoding="utf-8")))
                self.assertTrue(pins <= smoke_pins, f"{workflow.name} uses an untested revision of {action}")


class ArtifactRoundTripTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = (Path(__file__).resolve().parents[1] / "workflows/ci-action-smoke.yml").read_text(encoding="utf-8")

    def step(self, name):
        return self.workflow.split("      - name: " + name + "\n", 1)[1].split("\n      - name:", 1)[0]

    def execute(self, name, directory):
        body = textwrap.dedent(self.step(name).split("        run: |\n", 1)[1])
        python = body.split("python - <<'PY'\n", 1)[1].rsplit("\nPY", 1)[0]
        return subprocess.run(
            [sys.executable, "-c", python],
            env={**os.environ, "RUNNER_TEMP": str(directory), "GITHUB_RUN_ID": "123", "GITHUB_RUN_ATTEMPT": "2"},
            capture_output=True, text=True,
        )

    def test_download_verification_checks_real_contents_and_rejects_missing_or_extra_files(self):
        for mutation in ("none", "missing", "changed", "extra"):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as temporary:
                directory = Path(temporary)
                created = self.execute("Create Artifact Round Trip Fixture", directory)
                self.assertEqual(created.returncode, 0, created.stderr)
                root = directory / "action-artifact-smoke"
                source = root / "source/result.txt"
                self.assertLess(source.stat().st_size, 100)
                destination = root / "download"
                destination.mkdir()
                if mutation != "missing":
                    shutil.copyfile(source, destination / "result.txt")
                if mutation == "changed":
                    (destination / "result.txt").write_bytes(b"different run")
                if mutation == "extra":
                    (destination / "unexpected.txt").write_bytes(b"unexpected")
                verified = self.execute("Verify Artifact Round Trip", directory)
                self.assertEqual(verified.returncode == 0, mutation == "none", verified.stderr)

    def test_artifact_storage_is_bounded_and_download_is_bound_to_upload(self):
        upload = self.step("Exercise Artifact Upload")
        download = self.step("Exercise Artifact Download")
        self.assertIn("retention-days: 1\n", upload)
        self.assertIn("if-no-files-found: error\n", upload)
        self.assertIn("path: ${{ runner.temp }}/action-artifact-smoke/source/result.txt\n", upload)
        self.assertIn("artifact-ids: ${{ steps.artifact_upload.outputs.artifact-id }}\n", download)
        for body in (upload, download):
            self.assertNotIn("github-token:", body)
            self.assertNotIn("continue-on-error:", body)
            self.assertNotIn("        if:", body)
        cleanup = self.step("Stop Local Protocol Fixtures")
        self.assertIn("if: always()", cleanup)
        self.assertIn('"${RUNNER_TEMP:?}/action-artifact-smoke"', cleanup)


class ActionResultGuardTests(unittest.TestCase):
    def test_actual_workflow_requires_all_expected_action_steps(self):
        workflow = (Path(__file__).resolve().parents[1] / "workflows/ci-action-smoke.yml").read_text(encoding="utf-8")
        step = workflow.split("      - name: Require Successful Action Execution\n", 1)[1]
        self.assertCountEqual(re.findall(r"steps\.([a-z_]+)\.outcome", step), [
            "gcloud_setup", "gcloud_smoke", "artifact_fixture", "artifact_upload",
            "artifact_download", "artifact_verify", "fixture", "app_token", "auth", "verify", "cleanup",
        ])
        body = textwrap.dedent(step.split("        run: |\n", 1)[1])
        git_bash = Path("C:/Program Files/Git/bin/bash.exe")
        bash = str(git_bash) if git_bash.exists() else shutil.which("bash")
        if not bash:
            self.skipTest("bash is required to execute the action result guard")

        def execute(results):
            return subprocess.run(
                [bash, "--noprofile", "--norc", "-c", body],
                env={"PATH": os.environ.get("PATH", ""), "SystemRoot": os.environ.get("SystemRoot", ""),
                     "RESULTS": " ".join(results), "GITHUB_OUTPUT": "/dev/null"},
                capture_output=True, text=True, check=False,
            ).returncode

        self.assertEqual(execute(["success"] * 11), 0)
        for lane in range(11):
            for outcome in ("failure", "skipped", "cancelled", "neutral", ""):
                with self.subTest(lane=lane, outcome=outcome):
                    results = ["success"] * 11
                    results[lane] = outcome
                    self.assertNotEqual(execute(results), 0)


if __name__ == "__main__":
    unittest.main()
