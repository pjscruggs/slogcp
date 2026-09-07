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
from pathlib import Path
import re
import subprocess
import tempfile
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
        for action in ["actions/create-github-app-token", "google-github-actions/auth", "google-github-actions/setup-gcloud"]:
            pattern = re.compile(r"uses:\s*['\"]?" + re.escape(action) + r"@([0-9a-f]{40})(?:\s|['\"])" )
            smoke_pins = set(pattern.findall(smoke_workflow))
            self.assertEqual(len(smoke_pins), 1, f"Expected one immutable smoke pin for {action}")
            for workflow in workflows.glob("*.yml"):
                pins = set(pattern.findall(workflow.read_text(encoding="utf-8")))
                self.assertTrue(pins <= smoke_pins, f"{workflow.name} uses an untested revision of {action}")


class ActionResultGuardTests(unittest.TestCase):
    def test_actual_workflow_requires_all_expected_action_steps(self):
        import os
        import shutil
        import textwrap

        workflow = (Path(__file__).resolve().parents[1] / "workflows/ci-action-smoke.yml").read_text(encoding="utf-8")
        step = workflow.split("      - name: Require Successful Action Execution\n", 1)[1]
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

        self.assertEqual(execute(["success"] * 7), 0)
        for lane in range(7):
            for outcome in ("failure", "skipped", "cancelled", "neutral", ""):
                with self.subTest(lane=lane, outcome=outcome):
                    results = ["success"] * 7
                    results[lane] = outcome
                    self.assertNotEqual(execute(results), 0)


if __name__ == "__main__":
    unittest.main()
