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

import io
import json
import subprocess
import tempfile
import unittest
from pathlib import Path
from unittest.mock import Mock, patch

import readme_benchmarks as bench


SHA = "a" * 40
FINGERPRINT = "b" * 64
GO_VERSION = "go version go1.27.1 linux/amd64"
IMAGE_OS = "ubuntu24"
IMAGE_VERSION = "20260906.1.0"
RUNNER_ENV = {"ImageOS": IMAGE_OS, "ImageVersion": IMAGE_VERSION}
REPOSITORY = "pjscruggs/slogcp"
CPU = "AMD EPYC 7763 64-Core Processor"
PLACEHOLDER = "Intro\r\n\r\n" + bench.START + "\r\nPending.\r\n" + bench.END + "\r\n\r\nTail\r\n"


def output_fixture() -> str:
    lines = ["goos: linux", "goarch: amd64", "pkg: " + bench.PACKAGE, "cpu: " + CPU]
    for name in bench.BENCHMARK_NAMES:
        for sample in range(bench.SAMPLE_COUNT):
            lines.append(f"{name}\t1000\t{100 + sample}.0 ns/op\t{sample % 2} B/op\t0 allocs/op")
    return "\n".join(lines + ["PASS", "ok  " + bench.PACKAGE + " 60.123s", ""])


def provenance_fixture() -> dict:
    return {
        "schema": 1, "source_sha": SHA,
        "source_url": f"https://github.com/{REPOSITORY}/commit/{SHA}",
        "workflow_url": f"https://github.com/{REPOSITORY}/actions/runs/12345",
        "repository": REPOSITORY, "input_fingerprint": FINGERPRINT,
        "go_version": GO_VERSION, "goos": "linux", "goarch": "amd64",
        "cpu": CPU, "runner": bench.RUNNER, "command": list(bench.COMMAND),
        "environment": dict(bench.BENCHMARK_ENV), "sample_count": 10,
        "image_os": IMAGE_OS, "image_version": IMAGE_VERSION,
    }


def report_fixture() -> str:
    return bench.render_report(bench.parse_output(output_fixture()), provenance_fixture())


class OutputTests(unittest.TestCase):
    def test_accepts_all_cases_all_samples_and_zero_allocations(self):
        parsed = bench.parse_output(output_fixture())
        self.assertEqual(set(parsed["samples"]), set(bench.BENCHMARK_NAMES))
        self.assertTrue(all(len(values) == 10 for values in parsed["samples"].values()))
        self.assertEqual(parsed["samples"][bench.BENCHMARK_NAMES[0]][0]["allocs_per_op"], 0)

    def test_accepts_explicit_single_cpu_suffix(self):
        output = output_fixture()
        for name in bench.BENCHMARK_NAMES:
            output = output.replace(name + "\t", name + "-1\t")
        bench.parse_output(output)

    def test_rejects_partial_runs_even_with_pass(self):
        lines = output_fixture().splitlines()
        for output in (
            "\n".join(lines[:-2]),
            "\n".join(lines[:-1]),
            "\n".join(lines[:4] + lines[14:]),
            "\n".join(lines[:4] + lines[5:]),
            output_fixture().replace("PASS\n", ""),
        ):
            with self.subTest(output=output[-100:]), self.assertRaises(ValueError):
                bench.parse_output(output)

    def test_rejects_extra_samples_and_unselected_cases(self):
        output = output_fixture()
        row = output.splitlines()[4]
        for extra in (
            row,
            row.replace("/Typical", "/ErrorReportingAttrs"),
            row.replace("/Typical", "/ReplaceAttrEnabled"),
            row.replace("/Typical", "/Typical-2"),
            row.replace("BenchmarkJSONHandlerCore", "BenchmarkOther"),
        ):
            with self.subTest(extra=extra), self.assertRaises(ValueError):
                bench.parse_output(output.replace("PASS", extra + "\nPASS"))

    def test_rejects_invalid_metrics(self):
        for before, after in (
            ("100.0 ns/op", "0 ns/op"),
            ("100.0 ns/op", "NaN ns/op"),
            ("100.0 ns/op", "Inf ns/op"),
            ("100.0 ns/op", "1e999 ns/op"),
            ("100.0 ns/op", "-1 ns/op"),
            ("100.0 ns/op", "100 us/op"),
            ("100.0 ns/op", "100 ns/op 50 MB/s"),
            ("0 B/op", "-1 B/op"),
            ("0 B/op", "NaN B/op"),
            ("0 B/op", "0.5 B/op"),
            ("0 B/op", str(2**63) + " B/op"),
            ("0 allocs/op", "-1 allocs/op"),
            ("\t1000\t", "\t0\t"),
            ("\t1000\t", "\t+1000\t"),
        ):
            with self.subTest(after=after), self.assertRaises(ValueError):
                bench.parse_output(output_fixture().replace(before, after, 1))

    def test_rejects_bad_headers_failures_and_trailing_output(self):
        output = output_fixture()
        for changed in (
            output.replace("goos: linux", "goos: windows"),
            output.replace("goarch: amd64", "goarch: arm64"),
            output.replace("pkg: " + bench.PACKAGE, "pkg: other"),
            output.replace("cpu: " + CPU + "\n", ""),
            output.replace("cpu: " + CPU, "cpu: CPU `markdown`"),
            output.replace("goos: linux", "goos: linux\ngoos: linux"),
            output.replace("PASS", "FAIL"),
            output.replace("PASS", "PASS\nPASS"),
            output.replace(" 60.123s", " (cached)"),
            output + "FAIL\n",
            output.replace("PASS", "PASS\n" + output.splitlines()[4]),
        ):
            with self.subTest(changed=changed[-100:]), self.assertRaises(ValueError):
                bench.parse_output(changed)


class ReportTests(unittest.TestCase):
    def test_reports_every_case_medians_ranges_and_durable_provenance(self):
        report = report_fixture()
        prose = " ".join(report.split())
        for name, _ in bench.CASES:
            self.assertIn("(`" + name + "`)", report)
        self.assertEqual(report.count("104.5 | 100.0–109.0 | 0.5 | 0 |"), 6)
        self.assertIn("rather than a confidence interval", prose)
        self.assertIn("prebuilt records", prose)
        self.assertIn("Timestamp emission and service context", prose)
        self.assertIn("output I/O", prose)
        self.assertIn("-benchtime=1s -count=10 -cpu=1 -timeout=10m", report)
        self.assertNotIn("HighAttrCount", report)
        self.assertNotIn("ErrorReportingAttrs", report)
        self.assertEqual(bench.previous_provenance(report), provenance_fixture())

    def test_generated_prose_wraps_without_splitting_cpu_and_uses_reference_links(self):
        report = report_fixture()
        in_code = False
        for line in report.splitlines():
            if line.startswith("```"):
                in_code = not in_code
            if not in_code and not line.startswith(("<!--", "|", "[slogcp-readme-benchmark-")):
                self.assertLessEqual(len(line), 80, line)
        self.assertIn("`" + CPU + "`", report)
        self.assertIn("[slogcp-readme-benchmark-source]: " + provenance_fixture()["source_url"], report)
        self.assertIn("[slogcp-readme-benchmark-run]: " + provenance_fixture()["workflow_url"], report)

    def test_line_length_exception_applies_only_to_machine_provenance(self):
        lines = report_fixture().splitlines()
        directive = "<!-- markdownlint-disable-next-line MD013 -->"
        self.assertEqual(lines.count(directive), 1)
        self.assertTrue(lines[lines.index(directive) + 1].startswith(bench.PROVENANCE))
        self.assertFalse(any("markdownlint-disable " in line for line in lines))

    def test_preserves_every_byte_outside_block_and_is_idempotent(self):
        report = report_fixture()
        updated = bench.replace_report(PLACEHOLDER, report)
        self.assertEqual(updated, "Intro\r\n\r\n" + report + "\r\n\r\nTail\r\n")
        self.assertEqual(bench.replace_report(updated, report), updated)
        self.assertEqual(report_fixture(), report)

    def test_rejects_missing_duplicate_reversed_or_inline_markers(self):
        for readme in (
            "none", bench.START, bench.END,
            PLACEHOLDER + "\n" + bench.START,
            PLACEHOLDER + "\n" + bench.END,
            bench.END + "\n" + bench.START,
            "prefix " + bench.START + "\n" + bench.END,
            bench.START + " suffix\n" + bench.END,
        ):
            with self.subTest(readme=readme), self.assertRaises(ValueError):
                bench.replace_report(readme, report_fixture())

    def test_rejects_unbounded_replacement_and_missing_provenance(self):
        for replacement in (
            "outside\n" + report_fixture(), report_fixture() + "\noutside",
            bench.START + "\nempty\n" + bench.END,
            report_fixture().replace("prebuilt records", "X" * bench.MAX_BLOCK_BYTES),
        ):
            with self.subTest(replacement=replacement[:40]), self.assertRaises(ValueError):
                bench.replace_report(PLACEHOLDER, replacement)

    def test_rejects_malformed_or_duplicate_provenance(self):
        report = report_fixture()
        provenance_line = next(line for line in report.splitlines() if line.startswith(bench.PROVENANCE))
        for changed in (
            report.replace(provenance_line, provenance_line + "\n" + provenance_line),
            report.replace(provenance_line, bench.PROVENANCE + "{} -->"),
            report.replace(provenance_line, bench.PROVENANCE + "not-json -->"),
            report.replace('"schema":1', '"schema":1,"schema":1'),
            report + "\n" + provenance_line,
            PLACEHOLDER + provenance_line,
        ):
            with self.subTest(changed=changed[:100]), self.assertRaises(ValueError):
                bench.previous_provenance(changed)

    def test_same_inputs_skip_despite_new_source_commit(self):
        self.assertTrue(bench.needs_report(PLACEHOLDER, FINGERPRINT, REPOSITORY))
        self.assertFalse(bench.needs_report(report_fixture(), FINGERPRINT, REPOSITORY))
        self.assertTrue(bench.needs_report(report_fixture(), "c" * 64, REPOSITORY))
        self.assertTrue(bench.needs_report(report_fixture(), FINGERPRINT, "pjscruggs/slogcp-test"))

    def test_private_mirror_refresh_keeps_original_provenance_unmodified(self):
        report = report_fixture()
        before = bench.previous_provenance(report)
        self.assertTrue(bench.needs_report(report, FINGERPRINT, "pjscruggs/slogcp-test"))
        self.assertEqual(bench.previous_provenance(report), before)
        self.assertEqual(before["repository"], REPOSITORY)

    def test_rejects_bad_provenance_before_rendering(self):
        for key, value in (
            ("source_sha", "short"), ("source_sha", "A" * 40),
            ("source_url", "https://github.com/pjscruggs/slogcp/commit/" + "c" * 40),
            ("workflow_url", "https://example.com/actions/runs/123"),
            ("workflow_url", "https://github.com/pjscruggs/slogcp-test/actions/runs/123"),
            ("workflow_url", "https://github.com/pjscruggs/slogcp/actions/runs/123?x=1"),
            ("workflow_url", "https://github.com/pjscruggs/slogcp/actions/runs/0"),
            ("repository", "../repo"), ("repository", "owner/repo/extra"),
            ("input_fingerprint", "short"), ("go_version", "go version devel linux/amd64"),
            ("go_version", "go version go1.27.1 windows/amd64"),
            ("goos", "darwin"), ("goarch", "arm64"), ("runner", "ubuntu-24.04"),
            ("image_os", ""), ("image_version", ""),
            ("image_os", "ubuntu24\nextra"), ("image_version", "`injected`"),
            ("cpu", ""), ("cpu", "CPU\nextra"), ("cpu", "CPU `injected`"),
            ("command", list(bench.COMMAND) + ["-race"]),
            ("environment", dict(bench.BENCHMARK_ENV, GOFLAGS="-race")),
            ("sample_count", 9), ("schema", True), ("sample_count", True),
        ):
            with self.subTest(key=key, value=value), self.assertRaises(ValueError):
                provenance = provenance_fixture()
                provenance[key] = value
                bench.render_report(bench.parse_output(output_fixture()), provenance)

    def test_rejects_environment_mismatch_missing_samples_and_nan_in_render_api(self):
        for mutation in ("cpu", "missing_case", "missing_sample", "nan", "zero", "boolean"):
            with self.subTest(mutation=mutation), self.assertRaises(ValueError):
                parsed = bench.parse_output(output_fixture())
                name = bench.BENCHMARK_NAMES[0]
                if mutation == "cpu":
                    parsed["environment"]["cpu"] = "Other CPU"
                elif mutation == "missing_case":
                    del parsed["samples"][name]
                elif mutation == "missing_sample":
                    parsed["samples"][name].pop()
                else:
                    parsed["samples"][name][0]["ns_per_op"] = {
                        "nan": float("nan"), "zero": 0, "boolean": True,
                    }[mutation]
                bench.render_report(parsed, provenance_fixture())


class ArtifactTests(unittest.TestCase):
    def write_artifact(self, directory: Path) -> dict:
        report = {"provenance": provenance_fixture(), "benchmarks": bench.parse_output(output_fixture())}
        (directory / "report.json").write_text(json.dumps(report), encoding="utf-8")
        (directory / "benchmark.txt").write_text(output_fixture(), encoding="utf-8")
        return report

    def test_validates_raw_artifact_before_publication(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            report = self.write_artifact(directory)
            self.assertEqual(bench.load_report(directory), report)
            report["benchmarks"]["samples"][bench.BENCHMARK_NAMES[0]][0]["ns_per_op"] = 1
            (directory / "report.json").write_text(json.dumps(report), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "differs from the raw"):
                bench.load_report(directory)

    def test_rejects_missing_raw_oversized_or_duplicate_json(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            self.write_artifact(directory)
            (directory / "benchmark.txt").unlink()
            with self.assertRaises(ValueError):
                bench.load_report(directory)
            self.write_artifact(directory)
            (directory / "benchmark.txt").write_text("X" * (bench.MAX_ARTIFACT_BYTES + 1), encoding="utf-8")
            with self.assertRaises(ValueError):
                bench.load_report(directory)
            self.write_artifact(directory)
            path = directory / "report.json"
            path.write_text(path.read_text(encoding="utf-8").replace('"schema": 1', '"schema": 1, "schema": 1'), encoding="utf-8")
            with self.assertRaises(ValueError):
                bench.load_report(directory)


class SourceTests(unittest.TestCase):
    def tree(self, root_blob="a" * 40, docs_blob="b" * 40) -> str:
        entries = [(path, "c" * 40) for path in sorted(bench.REQUIRED_INPUT_PATHS)]
        entries.extend((
            ("json_handler_bench_test.go", root_blob), ("json_handler.go", root_blob),
            ("slogcpasync/async_handler.go", root_blob), ("internal/helper.s", root_blob),
            ("README.md", docs_blob), ("docs/USAGE.md", docs_blob),
            (".benchmarks/cloud_test.go", docs_blob), ("examples/go.mod", docs_blob),
            ("examples/main.go", docs_blob),
        ))
        return "\0".join("100644 blob " + oid + "\t" + path for path, oid in entries) + "\0"

    def fingerprint(self, tree: str, sha=SHA, go_version=GO_VERSION, image_os=IMAGE_OS, image_version=IMAGE_VERSION) -> str:
        with patch.object(bench, "git", return_value=tree):
            return bench.fingerprint_inputs(Path("repo"), sha, go_version, bench.RUNNER, image_os, image_version)

    def test_fingerprint_tracks_inputs_not_docs_or_head(self):
        initial = self.fingerprint(self.tree())
        self.assertEqual(initial, self.fingerprint(self.tree(docs_blob="d" * 40), sha="e" * 40))
        self.assertNotEqual(initial, self.fingerprint(self.tree(root_blob="d" * 40)))
        self.assertNotEqual(initial, self.fingerprint(self.tree(), go_version="go version go1.27.2 linux/amd64"))
        self.assertNotEqual(initial, self.fingerprint(self.tree().replace("c" * 40, "e" * 40, 1)))

    def test_runner_image_updates_trigger_measurement(self):
        initial = self.fingerprint(self.tree())
        self.assertNotEqual(initial, self.fingerprint(self.tree(), image_os="ubuntu26"))
        self.assertNotEqual(initial, self.fingerprint(self.tree(), image_version="20260907.1.0"))
        with self.assertRaises(ValueError):
            self.fingerprint(self.tree(), image_os="")

    def test_fingerprint_includes_subpackage_and_assembly_changes(self):
        initial = self.fingerprint(self.tree())
        for path in ("slogcpasync/async_handler.go", "internal/helper.s"):
            changed = self.tree().replace("a" * 40 + "\t" + path, "d" * 40 + "\t" + path)
            with self.subTest(path=path):
                self.assertNotEqual(initial, self.fingerprint(changed))

    def test_missing_input_or_symlink_refused(self):
        for tree in (
            self.tree().replace(".github/workflows/benchmarks.yml", "unrelated"),
            self.tree().replace("100644", "120000", 1),
        ):
            with self.subTest(tree=tree[:100]), self.assertRaises(ValueError):
                self.fingerprint(tree)

    def test_dirty_or_mismatched_source_is_refused(self):
        repo = Path("repo").resolve()
        for head, status in (("b" * 40, ""), (SHA, " M json_handler.go"), (SHA, "?? untracked.go")):
            with self.subTest(head=head, status=status):
                with patch.object(bench, "git", side_effect=[str(repo), head, status]):
                    with self.assertRaises(ValueError):
                        bench.assert_clean_source(repo, SHA)
        with patch.object(bench, "git", side_effect=[str(repo), SHA, ""]):
            bench.assert_clean_source(repo, SHA)


class ExecutionTests(unittest.TestCase):
    def test_run_writes_only_report_block_and_validated_artifacts(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repo, artifacts = root / "repo", root / "artifacts"
            repo.mkdir()
            (repo / "README.md").write_bytes(PLACEHOLDER.encode("utf-8"))
            process = Mock(returncode=0)
            process.communicate.return_value = (output_fixture(), "")
            with patch.object(bench, "assert_clean_source") as clean:
                with patch.object(bench, "benchmark_environment", return_value=(RUNNER_ENV, GO_VERSION)):
                    with patch.object(bench, "fingerprint_inputs", return_value=FINGERPRINT):
                        with patch.object(bench.subprocess, "Popen", return_value=process), patch("sys.stdout", new_callable=io.StringIO):
                            result = bench.main([
                                "run", "--repo", str(repo), "--source-sha", SHA,
                                "--repository", REPOSITORY, "--output-dir", str(artifacts),
                                "--workflow-url", provenance_fixture()["workflow_url"],
                            ])
            self.assertEqual(result, 0)
            self.assertEqual(clean.call_count, 2)
            self.assertEqual((repo / "README.md").read_bytes(), bench.replace_report(PLACEHOLDER, report_fixture()).encode("utf-8"))
            self.assertEqual(bench.load_report(artifacts)["provenance"], provenance_fixture())
            self.assertEqual({path.name for path in repo.iterdir()}, {"README.md"})

    def test_bad_run_url_or_artifact_path_never_starts_benchmarks(self):
        with tempfile.TemporaryDirectory() as temporary:
            repo = Path(temporary)
            (repo / "README.md").write_bytes(PLACEHOLDER.encode("utf-8"))
            for output_dir, url in (
                (repo / "artifacts", provenance_fixture()["workflow_url"]),
                (repo.parent / "artifacts", "https://example.com/bad"),
            ):
                with self.subTest(output_dir=output_dir, url=url):
                    with patch.object(bench, "assert_clean_source"), patch.object(bench, "benchmark_environment", return_value=(RUNNER_ENV, GO_VERSION)):
                        with patch.object(bench, "fingerprint_inputs", return_value=FINGERPRINT), patch.object(bench, "execute_benchmarks") as execute:
                            with self.assertRaises(ValueError):
                                bench.main([
                                    "run", "--repo", str(repo), "--source-sha", SHA,
                                    "--repository", REPOSITORY, "--output-dir", str(output_dir),
                                    "--workflow-url", url,
                                ])
                            execute.assert_not_called()
            self.assertEqual((repo / "README.md").read_bytes(), PLACEHOLDER.encode("utf-8"))

    def test_source_change_during_benchmark_prevents_readme_update(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            repo = root / "repo"
            repo.mkdir()
            (repo / "README.md").write_bytes(PLACEHOLDER.encode("utf-8"))
            with patch.object(bench, "assert_clean_source", side_effect=[None, ValueError("source changed")]):
                with patch.object(bench, "benchmark_environment", return_value=(RUNNER_ENV, GO_VERSION)):
                    with patch.object(bench, "fingerprint_inputs", return_value=FINGERPRINT):
                        with patch.object(bench, "execute_benchmarks", return_value=output_fixture()):
                            with self.assertRaisesRegex(ValueError, "source changed"):
                                bench.main([
                                    "run", "--repo", str(repo), "--source-sha", SHA,
                                    "--repository", REPOSITORY, "--output-dir", str(root / "artifacts"),
                                    "--workflow-url", provenance_fixture()["workflow_url"],
                                ])
            self.assertEqual((repo / "README.md").read_bytes(), PLACEHOLDER.encode("utf-8"))

    def test_only_benchmarks_run_and_temporary_directory_is_cleaned(self):
        process = Mock(returncode=0)
        process.communicate.return_value = (output_fixture(), "")
        with tempfile.TemporaryDirectory() as temporary:
            output_dir = Path(temporary)
            with patch.object(bench.subprocess, "Popen", return_value=process) as popen:
                self.assertEqual(bench.execute_benchmarks(Path("repo"), {}, output_dir), output_fixture())
            args, kwargs = popen.call_args
            self.assertEqual(args[0], bench.COMMAND)
            self.assertIn("-run=^$", args[0])
            self.assertNotIn("-race", args[0])
            self.assertNotIn("-cover", args[0])
            self.assertTrue(kwargs["start_new_session"])
            self.assertFalse(Path(kwargs["env"]["GOTMPDIR"]).exists())
            self.assertTrue((output_dir / "benchmark.txt").is_file())

    def test_failed_go_run_is_rejected_even_with_plausible_output(self):
        process = Mock(returncode=1)
        process.communicate.return_value = (output_fixture(), "failure")
        with tempfile.TemporaryDirectory() as temporary:
            with patch.object(bench.subprocess, "Popen", return_value=process):
                with self.assertRaisesRegex(ValueError, "exit code 1"):
                    bench.execute_benchmarks(Path("repo"), {}, Path(temporary))

    def test_timeout_kills_process_group_and_cleans_temporary_directory(self):
        process = Mock(pid=1234)
        process.communicate.side_effect = [subprocess.TimeoutExpired(bench.COMMAND, 720), ("", "")]
        with tempfile.TemporaryDirectory() as temporary:
            with patch.object(bench.subprocess, "Popen", return_value=process) as popen:
                # create=True keeps this Linux-only cleanup test runnable on Windows.
                with patch.object(bench.os, "killpg", create=True) as kill:
                    with patch.object(bench.signal, "SIGKILL", 9, create=True):
                        with self.assertRaises(subprocess.TimeoutExpired):
                            bench.execute_benchmarks(Path("repo"), {}, Path(temporary))
            kill.assert_called_once_with(1234, 9)
            self.assertFalse(Path(popen.call_args.kwargs["env"]["GOTMPDIR"]).exists())

    def test_check_and_run_skip_unchanged_without_benchmark_execution(self):
        with tempfile.TemporaryDirectory() as temporary:
            repo = Path(temporary)
            (repo / "README.md").write_text(report_fixture(), encoding="utf-8")
            for operation, expected in (("check", 3), ("run", 0)):
                args = [operation, "--repo", str(repo), "--source-sha", SHA, "--repository", REPOSITORY]
                if operation == "run":
                    args.extend(("--workflow-url", provenance_fixture()["workflow_url"], "--output-dir", str(repo.parent / "unused")))
                with self.subTest(operation=operation):
                    with patch.object(bench, "assert_clean_source"), patch.object(bench, "benchmark_environment", return_value=(RUNNER_ENV, GO_VERSION)):
                        with patch.object(bench, "fingerprint_inputs", return_value=FINGERPRINT), patch.object(bench, "execute_benchmarks") as execute:
                            with patch("sys.stdout", new_callable=io.StringIO):
                                self.assertEqual(bench.main(args), expected)
                            execute.assert_not_called()


if __name__ == "__main__":
    unittest.main()
