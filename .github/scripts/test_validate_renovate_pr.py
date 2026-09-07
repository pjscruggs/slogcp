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

from __future__ import annotations

import copy
import subprocess
import unittest
from unittest.mock import patch

import validate_renovate_pr as policy


BASE = "b" * 40
HEAD = "a" * 40
MODULE = """module example.org/library

go 1.26.0

toolchain go1.26.6

require example.org/runtime v1.0.0

require (
    example.org/indirect v0.5.0 // indirect
)
"""
FIXED = MODULE.replace("runtime v1.0.0", "runtime v1.1.0")
VERSION = 'package library\n\nconst Version = "v1.2.3"\n'
PATCH_VERSION = VERSION.replace("v1.2.3", "v1.2.4")


def event() -> dict:
    return {
        "repository": {"full_name": "example/library"},
        "pull_request": {
            "user": {"login": "renovate[bot]"},
            "base": {"ref": "main", "repo": {"full_name": "example/library"}},
            "head": {
                "sha": HEAD,
                "ref": "renovate/root-runtime-vulnerability",
                "repo": {"full_name": "example/library"},
            },
            "title": "Update runtime [security]",
            "labels": [{"name": "security"}],
        },
    }


class CandidateScopeTests(unittest.TestCase):
    def inspect(self, **changes) -> str:
        fixture = {
            "base_module": MODULE,
            "head_module": FIXED,
            "base_version": VERSION,
            "head_version": PATCH_VERSION,
            "changed_paths": ["go.mod", "go.sum", "version.go"],
            "security_hint": True,
        }
        fixture.update(changes)
        return policy.inspect_candidate(**fixture)

    def test_root_security_repair_prepares_one_patch_and_allows_example_closure(
        self,
    ) -> None:
        self.assertEqual(
            self.inspect(
                changed_paths=[
                    "go.mod",
                    "go.sum",
                    "version.go",
                    ".examples/client/go.sum",
                ]
            ),
            "security_patch",
        )

    def test_explicit_indirect_security_repair_is_release_eligible(self) -> None:
        self.assertEqual(
            self.inspect(
                head_module=MODULE.replace("indirect v0.5.0", "indirect v0.5.1")
            ),
            "security_patch",
        )

    def test_added_indirect_security_minimum_is_release_eligible(self) -> None:
        self.assertEqual(
            self.inspect(
                head_module=MODULE + "\nrequire example.org/fixed v0.1.0 // indirect\n"
            ),
            "security_patch",
        )

    def test_tools_examples_and_toolchain_do_not_release(self) -> None:
        for paths in (
            [".github/tools/go.mod", ".github/tools/go.sum"],
            [".examples/client/go.mod", ".examples/client/go.sum"],
            [".github/workflows/validation.yml"],
            ["go.mod"],
        ):
            with self.subTest(paths=paths):
                self.assertEqual(
                    self.inspect(
                        head_module=MODULE.replace("go1.26.6", "go1.27.1"),
                        head_version=VERSION,
                        changed_paths=paths,
                    ),
                    "non_releasing",
                )

    def test_security_label_without_dependency_repair_does_not_create_release(
        self,
    ) -> None:
        with self.assertRaisesRegex(ValueError, "without a root dependency repair"):
            self.inspect(head_module=MODULE)

    def test_root_sum_churn_is_not_a_dependency_repair(self) -> None:
        self.assertEqual(
            self.inspect(
                head_module=MODULE, head_version=VERSION, changed_paths=["go.sum"]
            ),
            "non_releasing",
        )

    def test_unmarked_routine_root_minimum_update_is_rejected(self) -> None:
        with self.assertRaisesRegex(ValueError, "require a security repair"):
            self.inspect(security_hint=False)

    def test_removing_a_requirement_alone_is_not_a_security_repair(self) -> None:
        with self.assertRaisesRegex(ValueError, "require a security repair"):
            self.inspect(
                head_module=MODULE.replace("require example.org/runtime v1.0.0\n", "")
            )

    def test_missing_repeated_or_nonpatch_version_change_is_rejected(self) -> None:
        for value in ("v1.2.3", "v1.2.5", "v1.3.0", "v2.0.0"):
            with (
                self.subTest(value=value),
                self.assertRaisesRegex(ValueError, "next patch"),
            ):
                self.inspect(head_version=VERSION.replace("v1.2.3", value))

    def test_base_version_moving_requires_a_regenerated_patch(self) -> None:
        with self.assertRaisesRegex(ValueError, "next patch"):
            self.inspect(base_version=PATCH_VERSION)
        self.assertEqual(
            self.inspect(
                base_version=PATCH_VERSION,
                head_version=VERSION.replace("v1.2.3", "v1.2.5"),
            ),
            "security_patch",
        )

    def test_go_floor_or_module_identity_change_is_rejected(self) -> None:
        for before, after in (
            ("go 1.26.0", "go 1.27.0"),
            ("example.org/library", "example.org/other"),
        ):
            with self.subTest(after=after), self.assertRaises(ValueError):
                self.inspect(head_module=FIXED.replace(before, after))

    def test_root_security_repair_cannot_include_source_or_tool_changes(self) -> None:
        for path in ("adapter.go", ".github/tools/go.mod", ".examples/client/main.go"):
            with (
                self.subTest(path=path),
                self.assertRaisesRegex(ValueError, "unrelated files"),
            ):
                self.inspect(changed_paths=["go.mod", "go.sum", "version.go", path])

    def test_version_file_may_change_only_its_version_value(self) -> None:
        for candidate in (
            PATCH_VERSION + "// unrelated\n",
            PATCH_VERSION.replace("const", "var"),
        ):
            with (
                self.subTest(candidate=candidate),
                self.assertRaisesRegex(ValueError, "only the Version value"),
            ):
                self.inspect(head_version=candidate)

    def test_var_version_declaration_and_alternate_example_directory_are_supported(
        self,
    ) -> None:
        self.assertEqual(
            self.inspect(
                base_version=VERSION.replace("const", "var"),
                head_version=PATCH_VERSION.replace("const", "var"),
                changed_paths=["go.mod", "version.go", "examples/client/go.mod"],
                examples_dir="examples",
            ),
            "security_patch",
        )

    def test_duplicate_directives_requirements_and_versions_are_rejected(self) -> None:
        for candidate in (
            FIXED + "\ngo 1.26.0\n",
            FIXED + "\nrequire example.org/runtime v1.1.0\n",
        ):
            with self.subTest(candidate=candidate), self.assertRaises(ValueError):
                self.inspect(head_module=candidate)
        with self.assertRaisesRegex(ValueError, "one canonical"):
            self.inspect(head_version=PATCH_VERSION + 'const Version = "v1.2.4"\n')


class EventBindingTests(unittest.TestCase):
    def git_reply(self, *args: str) -> str:
        return {
            ("rev-parse", "HEAD"): HEAD + "\n",
            ("merge-base", "--is-ancestor", BASE, HEAD): "",
            (
                "diff",
                "--name-only",
                "--no-renames",
                "-z",
                BASE,
                HEAD,
            ): "go.mod\0go.sum\0version.go\0",
            ("show", f"{BASE}:go.mod"): MODULE,
            ("show", f"{HEAD}:go.mod"): FIXED,
            ("show", f"{BASE}:version.go"): VERSION,
            ("show", f"{HEAD}:version.go"): PATCH_VERSION,
        }[args]

    def test_exact_current_base_and_head_are_used_for_every_content_read(self) -> None:
        with patch.object(policy, "git", side_effect=self.git_reply) as git:
            self.assertEqual(policy.validate_event(event(), BASE), "security_patch")
        git.assert_any_call("merge-base", "--is-ancestor", BASE, HEAD)
        git.assert_any_call("show", f"{BASE}:version.go")
        git.assert_any_call("show", f"{HEAD}:version.go")

    def test_human_prs_and_non_pr_events_keep_their_existing_review_path(self) -> None:
        human = event()
        human["pull_request"]["user"]["login"] = "maintainer"
        with patch.object(policy, "git") as git:
            self.assertEqual(policy.validate_event(human, BASE), "not_renovate")
            self.assertEqual(policy.validate_event({}, BASE), "not_renovate")
            git.assert_not_called()

    def test_foreign_head_wrong_base_and_invalid_sha_are_rejected(self) -> None:
        for path, value in (
            (("head", "repo"), {"full_name": "other/fork"}),
            (("head", "repo"), None),
            (("base", "ref"), "release"),
            (("head", "sha"), "not-a-sha"),
        ):
            candidate = copy.deepcopy(event())
            candidate["pull_request"][path[0]][path[1]] = value
            with (
                self.subTest(path=path, value=value),
                patch.object(policy, "git") as git,
            ):
                with self.assertRaises(ValueError):
                    policy.validate_event(candidate, BASE)
                git.assert_not_called()

    def test_wrong_checkout_or_stale_base_fails(self) -> None:
        with patch.object(policy, "git", return_value="c" * 40):
            with self.assertRaisesRegex(ValueError, "exact candidate"):
                policy.validate_event(event(), BASE)

        def stale_base(*args: str) -> str:
            if args[0] == "merge-base":
                raise subprocess.CalledProcessError(1, ["git", *args])
            return self.git_reply(*args)

        with patch.object(policy, "git", side_effect=stale_base):
            with self.assertRaises(subprocess.CalledProcessError):
                policy.validate_event(event(), BASE)


if __name__ == "__main__":
    unittest.main()
