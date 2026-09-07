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

"""Loopback protocol fixture for secretless GitHub action consumer tests."""

from __future__ import annotations

import argparse
import base64
from collections import Counter
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import subprocess
import tempfile
import threading
import time
from urllib.parse import parse_qs, unquote, urlsplit
from urllib.request import Request, urlopen


APP_TOKEN = "fixture-installation-token"
FEDERATED_TOKEN = "fixture-federated-token"
ACCESS_TOKEN = "fixture-access-token"
OIDC_TOKEN = "fixture.oidc.token"
SCOPE = "https://www.googleapis.com/auth/cloud-platform"


def b64decode(value: str) -> bytes:
    return base64.urlsafe_b64decode(value + "=" * (-len(value) % 4))


def generate_fixture(directory: Path, base_url: str) -> dict:
    directory.mkdir(parents=True, exist_ok=True)
    private_key = directory / "app-private.pem"
    public_key = directory / "app-public.pem"
    subprocess.run(
        ["openssl", "genpkey", "-algorithm", "RSA", "-pkeyopt",
         "rsa_keygen_bits:2048", "-out", str(private_key)],
        check=True, capture_output=True,
    )
    private_key.chmod(0o600)
    subprocess.run(
        ["openssl", "pkey", "-in", str(private_key), "-pubout", "-out", str(public_key)],
        check=True, capture_output=True,
    )
    return {
        "base_url": base_url,
        "private_key_path": str(private_key.resolve()),
        "public_key_path": str(public_key.resolve()),
        "app_id": "123456",
        "owner": "fixture-owner",
        "repository": "fixture-repository",
        "project_id": "ci-fixture-project",
        "provider": "projects/123456789/locations/global/workloadIdentityPools/fixture/providers/github",
        "service_account": "fixture@ci-fixture-project.iam.gserviceaccount.com",
        "oidc_request_token": "fixture-request-token",
    }


class Fixture:
    """Check the few requests made by the actual action implementations."""

    def __init__(self, config: dict):
        self.config = config
        self.counts: Counter = Counter()
        self.errors: list[str] = []

    @property
    def audience(self) -> str:
        return "//iam.googleapis.com/" + self.config["provider"]

    def verify_app_jwt(self, authorization: str) -> None:
        scheme, _, token = authorization.partition(" ")
        if scheme.lower() not in {"bearer", "token"}:
            raise ValueError("App JWT authorization scheme is missing")
        try:
            header, payload, signature = token.split(".")
            metadata = json.loads(b64decode(header))
            claims = json.loads(b64decode(payload))
            signature_bytes = b64decode(signature)
        except (ValueError, UnicodeError) as exc:
            raise ValueError("App JWT is malformed") from exc
        if metadata.get("alg") != "RS256" or str(claims.get("iss")) != self.config["app_id"]:
            raise ValueError("App JWT algorithm or issuer is incorrect")
        now = time.time()
        if not now - 180 <= claims.get("iat", 0) <= now + 30:
            raise ValueError("App JWT issued-at time is incorrect")
        if not now < claims.get("exp", 0) <= now + 660:
            raise ValueError("App JWT expiry is incorrect")
        with tempfile.TemporaryDirectory() as temporary:
            signature_file = Path(temporary) / "signature.bin"
            signature_file.write_bytes(signature_bytes)
            verified = subprocess.run(
                ["openssl", "dgst", "-sha256", "-verify", self.config["public_key_path"],
                 "-signature", str(signature_file)],
                input=f"{header}.{payload}".encode(), capture_output=True,
            )
        if verified.returncode:
            raise ValueError("App JWT signature is incorrect")

    def handle(self, method: str, target: str, headers: dict, body: dict) -> tuple[int, dict]:
        parsed = urlsplit(target)
        route = unquote(parsed.path)
        auth = headers.get("Authorization", "")
        owner = self.config["owner"]
        repository = self.config["repository"]
        if method == "GET" and route == f"/repos/{owner}/{repository}/installation":
            self.verify_app_jwt(auth)
            self.counts["installation"] += 1
            return 200, {"id": 123, "app_slug": "fixture-app"}
        if method == "POST" and route == "/app/installations/123/access_tokens":
            self.verify_app_jwt(auth)
            if body != {"repositories": [repository], "permissions": {"contents": "read"}}:
                raise ValueError("App token repository or permission boundary is incorrect")
            self.counts["app-token"] += 1
            return 201, {
                "token": APP_TOKEN,
                "expires_at": (datetime.now(timezone.utc) + timedelta(hours=1)).isoformat(),
                "permissions": {"contents": "read"},
                "repositories": [{"id": 456, "name": repository, "full_name": f"{owner}/{repository}"}],
            }
        if method == "DELETE" and route == "/installation/token":
            if auth.lower() != f"token {APP_TOKEN}".lower():
                raise ValueError("Revocation authorization is incorrect")
            self.counts["revoke"] += 1
            return 204, {}
        if method == "GET" and route == "/oidc":
            if auth != "Bearer " + self.config["oidc_request_token"]:
                raise ValueError("OIDC request authorization is incorrect")
            if parse_qs(parsed.query).get("audience") != ["https://iam.googleapis.com/" + self.config["provider"]]:
                raise ValueError("OIDC audience is incorrect")
            self.counts["oidc"] += 1
            return 200, {"value": OIDC_TOKEN}
        if method == "POST" and route == "/sts/v1/token":
            expected = {
                "audience": self.audience,
                "grantType": "urn:ietf:params:oauth:grant-type:token-exchange",
                "requestedTokenType": "urn:ietf:params:oauth:token-type:access_token",
                "scope": SCOPE,
                "subjectTokenType": "urn:ietf:params:oauth:token-type:jwt",
                "subjectToken": OIDC_TOKEN,
            }
            if body != expected:
                raise ValueError("STS token-exchange contract is incorrect")
            self.counts["sts"] += 1
            return 200, {"access_token": FEDERATED_TOKEN, "token_type": "Bearer", "expires_in": 3600}
        if method == "POST" and route == f"/iamcredentials/v1/projects/-/serviceAccounts/{self.config['service_account']}:generateAccessToken":
            if auth != "Bearer " + FEDERATED_TOKEN:
                raise ValueError("Service account impersonation authorization is incorrect")
            if body.get("scope") != [SCOPE] or body.get("lifetime") != "3600s" or body.get("delegates", []) != []:
                raise ValueError("Service account impersonation scope, lifetime, or delegates is incorrect")
            if set(body) - {"scope", "lifetime", "delegates"}:
                raise ValueError("Unexpected impersonation fields")
            self.counts["access-token"] += 1
            return 200, {
                "accessToken": ACCESS_TOKEN,
                "expireTime": (datetime.now(timezone.utc) + timedelta(hours=1)).isoformat(),
            }
        raise ValueError(f"Unexpected request: {method} {route}")


def make_server() -> ThreadingHTTPServer:
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, _format, *_args):
            pass

        def process(self):
            if self.command == "GET" and self.path == "/__status":
                self.respond(200, {"counts": dict(self.server.fixture.counts), "errors": self.server.fixture.errors})
                return
            if self.command == "POST" and self.path == "/__shutdown":
                self.respond(200, {})
                threading.Thread(target=self.server.shutdown, daemon=True).start()
                return
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if not 0 <= length <= 65536:
                    raise ValueError("Invalid request size")
                body = json.loads(self.rfile.read(length)) if length else {}
                status, result = self.server.fixture.handle(self.command, self.path, self.headers, body)
            except (ValueError, TypeError) as exc:
                self.server.fixture.errors.append(str(exc))
                status, result = 400, {"error": str(exc)}
            self.respond(status, result)

        def respond(self, status, body):
            encoded = json.dumps(body).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(encoded)))
            self.end_headers()
            self.wfile.write(encoded)

        do_GET = process
        do_POST = process
        do_DELETE = process

    return ThreadingHTTPServer(("127.0.0.1", 0), Handler)


def verify(directory: Path, credentials_file: Path | None, app_token: str, access_token: str | None,
           auth_token: str = FEDERATED_TOKEN, github_only: bool = False) -> dict:
    config = json.loads((directory / "fixture.json").read_text())
    with urlopen(config["base_url"] + "/__status", timeout=5) as response:
        status = json.load(response)
    if status["errors"]:
        raise ValueError("Fixture rejected requests: " + "; ".join(status["errors"]))
    required = ["installation", "app-token"]
    if not github_only:
        required += ["oidc", "sts", "access-token"]
    if any(status["counts"].get(route, 0) < 1 for route in required):
        raise ValueError("An expected action request did not execute")
    if app_token != APP_TOKEN:
        raise ValueError("Action token outputs are incorrect")
    if github_only:
        return status
    if access_token != ACCESS_TOKEN or auth_token != FEDERATED_TOKEN or credentials_file is None:
        raise ValueError("Action token outputs or credential file are incorrect")
    credentials = json.loads(credentials_file.read_text())
    expected = {
        "type": "external_account",
        "audience": "//iam.googleapis.com/" + config["provider"],
        "subject_token_type": "urn:ietf:params:oauth:token-type:jwt",
        "token_url": config["base_url"] + "/sts/v1/token",
        "service_account_impersonation_url": config["base_url"] + "/iamcredentials/v1/projects/-/serviceAccounts/" + config["service_account"] + ":generateAccessToken",
    }
    if any(credentials.get(key) != value for key, value in expected.items()):
        raise ValueError("Generated external-account credentials are incorrect")
    source = credentials.get("credential_source", {})
    parsed = urlsplit(source.get("url", ""))
    if f"{parsed.scheme}://{parsed.netloc}{parsed.path}" != config["base_url"] + "/oidc":
        raise ValueError("Generated OIDC source URL is incorrect")
    if parse_qs(parsed.query).get("audience") != ["https://iam.googleapis.com/" + config["provider"]]:
        raise ValueError("Generated OIDC audience is incorrect")
    if source.get("headers") != {"Authorization": "Bearer " + config["oidc_request_token"]}:
        raise ValueError("Generated OIDC request headers are incorrect")
    if source.get("format") != {"type": "json", "subject_token_field_name": "value"}:
        raise ValueError("Generated OIDC response format is incorrect")
    return status


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=["serve", "verify", "stop"])
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--credentials-file", type=Path)
    parser.add_argument("--app-token")
    parser.add_argument("--access-token")
    parser.add_argument("--auth-token", default=FEDERATED_TOKEN)
    parser.add_argument("--github-only", action="store_true")
    args = parser.parse_args()
    if args.command == "serve":
        server = make_server()
        config = generate_fixture(args.directory, f"http://127.0.0.1:{server.server_port}")
        server.fixture = Fixture(config)
        (args.directory / "fixture.json").write_text(json.dumps(config))
        try:
            server.serve_forever()
        finally:
            server.server_close()
    elif args.command == "verify":
        if args.app_token is None or (not args.github_only and (not args.credentials_file or args.access_token is None)):
            parser.error("verify requires credentials-file, app-token and access-token")
        print(json.dumps(verify(args.directory, args.credentials_file, args.app_token, args.access_token, args.auth_token, args.github_only)))
    else:
        config = json.loads((args.directory / "fixture.json").read_text())
        with urlopen(Request(config["base_url"] + "/__shutdown", data=b"{}", method="POST"), timeout=5):
            pass


if __name__ == "__main__":
    main()
