"""Local fixtures for native artifact reuse; no GitHub or native builds are run."""
import copy
import hashlib
import io
import json
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest
import zipfile

import verify_native_reuse as reuse


class RunVerificationTests(unittest.TestCase):
    def fixtures(self):
        repository = {"id": 1234, "full_name": "fixture/mini-fabrics"}
        run = {
            "id": 42, "repository": repository,
            "head_repository": dict(repository), "head_sha": "a" * 40,
            "path": ".github/workflows/release.yml", "event": "push",
            "head_branch": "v0.1.0-rc.1", "status": "completed",
            "conclusion": "failure", "run_attempt": 1,
            "html_url": "https://github.com/fixture/mini-fabrics/actions/runs/42",
        }
        jobs = []
        for name, required_steps in reuse.NATIVE_SPECS.values():
            jobs.append({"id": len(jobs) + 1, "name": name,
                         "status": "completed", "conclusion": "success",
                         "run_id": 42, "run_attempt": 1, "head_sha": "a" * 40,
                         "steps": [{"name": step, "status": "completed", "conclusion": "success"}
                                   for step in required_steps]})
        for name in reuse.SOURCE_JOBS:
            jobs.append({"id": len(jobs) + 1, "name": name,
                         "status": "completed", "conclusion": "success",
                         "run_id": 42, "run_attempt": 1, "head_sha": "a" * 40,
                         "steps": []})
        return run, jobs

    def verify(self, run, jobs):
        return reuse.verify_run(run, jobs, "fixture/mini-fabrics", "42")

    def test_completed_native_jobs_can_survive_unrelated_workflow_failure(self):
        run, jobs = self.fixtures()
        self.assertIsInstance(self.verify(run, jobs), dict)
        run["conclusion"] = "success"
        self.assertIsInstance(self.verify(run, jobs), dict)

    def test_wrong_repository_event_ref_attempt_and_incomplete_run_rejected(self):
        changes = [
            ("id", 43), ("event", "pull_request"), ("head_branch", "main"),
            ("head_branch", "v0.1.0-rc.evil"), ("run_attempt", 2),
            ("status", "in_progress"), ("conclusion", "cancelled"),
            ("path", ".github/workflows/native.yml"),
            ("head_repository", {"id": 9999, "full_name": "fixture/mini-fabrics"}),
            ("head_repository", {"id": 1234, "full_name": "other/mini-fabrics"}),
            ("repository", {"id": 1234, "full_name": "other/mini-fabrics"}),
        ]
        for key, value in changes:
            with self.subTest(key=key, value=value):
                run, jobs = self.fixtures()
                run[key] = value
                with self.assertRaises((ValueError, RuntimeError)):
                    self.verify(run, jobs)

    def test_missing_failed_or_skipped_required_job_rejected(self):
        for index in range(len(self.fixtures()[1])):
            for result in ("missing", "failure", "skipped", "in_progress"):
                with self.subTest(index=index, result=result):
                    run, jobs = self.fixtures()
                    if result == "missing":
                        jobs.pop(index)
                    elif result == "in_progress":
                        jobs[index]["status"] = result
                    else:
                        jobs[index]["conclusion"] = result
                    with self.assertRaises((ValueError, RuntimeError)):
                        self.verify(run, jobs)

    def test_missing_failed_or_skipped_native_build_smoke_step_rejected(self):
        for index, job in enumerate(self.fixtures()[1]):
            for step_index in range(len(job["steps"])):
                for result in ("missing", "failure", "skipped", "in_progress"):
                    with self.subTest(job=index, step=step_index, result=result):
                        run, jobs = self.fixtures()
                        if result == "missing":
                            jobs[index]["steps"].pop(step_index)
                        elif result == "in_progress":
                            jobs[index]["steps"][step_index]["status"] = result
                        else:
                            jobs[index]["steps"][step_index]["conclusion"] = result
                        with self.assertRaises((ValueError, RuntimeError)):
                            self.verify(run, jobs)


@unittest.skipUnless(shutil.which("git"), "Git is required to compare source snapshots")
class SourceVerificationTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-reuse-source-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        checkout = Path(reuse.__file__).resolve().parents[1]
        paths = set(reuse.CONSTRUCTION_INPUTS)
        paths.add(".github/workflows/native.yml")
        for folder in ("cmd/fabrics", "internal", "native"):
            paths.update(path.relative_to(checkout).as_posix()
                         for path in (checkout / folder).rglob("*")
                         if path.is_file() and path.suffix in (".go", ".swift"))
        for name in paths:
            destination = self.root / name
            destination.parent.mkdir(parents=True, exist_ok=True)
            # The verifier runs on Linux against Git LF bytes. Construct that
            # checkout even when these unit tests run from a Windows CRLF tree.
            destination.write_bytes((checkout / name).read_bytes().replace(b"\r\n", b"\n"))
        self.git("init", "--quiet")
        self.git("config", "core.autocrlf", "false")
        self.git("add", ".")
        self.commit()
        self.producer = self.git("rev-parse", "HEAD")

    def git(self, *arguments):
        return subprocess.check_output(["git", "-C", str(self.root), *arguments],
                                       text=True, encoding="utf-8", stderr=subprocess.STDOUT).strip()

    def commit(self):
        self.git("-c", "user.name=Reuse Fixture", "-c", "user.email=fixture@example.invalid",
                 "-c", "commit.gpgSign=false", "commit", "--quiet", "-m", "fixture snapshot")

    def test_identical_build_sources_and_unrelated_documentation_change_are_reusable(self):
        self.assertIsInstance(reuse.verify_sources(self.root, self.producer), dict)
        (self.root / "README.md").write_text("Documentation-only follow-up.\n", encoding="utf-8")
        self.git("add", "README.md")
        self.commit()
        self.assertIsInstance(reuse.verify_sources(self.root, self.producer), dict)

    def test_committed_construction_input_change_is_rejected(self):
        changed = self.root / "tools/native_payload.py"
        changed.write_bytes(changed.read_bytes() + b"\n# Changed build recipe.\n")
        self.git("add", "tools/native_payload.py")
        self.commit()
        with self.assertRaises((ValueError, RuntimeError)):
            reuse.verify_sources(self.root, self.producer)

    def test_uncommitted_runtime_and_native_changes_are_rejected(self):
        for suffix in (".go", ".swift"):
            with self.subTest(suffix=suffix):
                changed = next(path for path in self.root.rglob("*" + suffix)
                               if not path.name.endswith("_test.go"))
                original = changed.read_bytes()
                try:
                    changed.write_bytes(original + b"\n// Uncommitted source mutation.\n")
                    with self.assertRaises((ValueError, RuntimeError)):
                        reuse.verify_sources(self.root, self.producer)
                finally:
                    changed.write_bytes(original)

    def test_new_untracked_runtime_source_is_rejected(self):
        (self.root / "internal" / "reuse_fixture.go").write_text("package internal\n", encoding="utf-8")
        with self.assertRaises((ValueError, RuntimeError)):
            reuse.verify_sources(self.root, self.producer)

    def test_windows_snapshot_records_crlf_rendering_of_original_git_bytes(self):
        state = reuse.verify_sources(self.root, self.producer)
        original = (self.root / "go.mod").read_bytes()
        self.assertEqual(state["source_state"]["go_mod_sha256"], hashlib.sha256(original).hexdigest())
        self.assertEqual(state["windows_crlf_state"]["go_mod_sha256"],
                         hashlib.sha256(original.replace(b"\n", b"\r\n")).hexdigest())
        for name in state["source_state"]["runtime_source_files"]:
            rendered = (self.root / name).read_bytes().replace(b"\n", b"\r\n")
            self.assertEqual(state["windows_crlf_state"]["runtime_source_files"][name],
                             hashlib.sha256(rendered).hexdigest())


class ArchiveVerificationTests(unittest.TestCase):
    target = "windows-amd64"

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-reuse-archive-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.members = {
            "native-payloads/windows-amd64.json": b"manifest fixture",
            "native-payloads/windows-amd64.tar.gz": b"payload fixture",
            "native-smoke-windows-amd64.json": b"smoke fixture",
        }

    def archive(self, members=None):
        archive = self.root / "artifact.zip"
        with zipfile.ZipFile(archive, "w") as output:
            for name, data in (members if members is not None else self.members).items():
                output.writestr(name, data)
        return archive, hashlib.sha256(archive.read_bytes()).hexdigest()

    def test_verified_archive_preserves_all_three_original_paths(self):
        archive, checksum = self.archive()
        destination = self.root / "extracted"
        reuse.extract_artifact(archive, destination, self.target, checksum)
        self.assertEqual({path.relative_to(destination).as_posix(): path.read_bytes()
                          for path in destination.rglob("*") if path.is_file()}, self.members)

    def test_wrong_archive_digest_is_rejected_before_extraction(self):
        archive, _ = self.archive()
        destination = self.root / "extracted"
        with self.assertRaises((ValueError, RuntimeError)):
            reuse.extract_artifact(archive, destination, self.target, "0" * 64)
        self.assertFalse(any(path.is_file() for path in destination.rglob("*")))

    def test_missing_extra_and_traversing_entries_are_rejected(self):
        missing = dict(self.members)
        missing.pop("native-smoke-windows-amd64.json")
        variants = [missing, dict(self.members, unexpected=b"extra"),
                    dict(self.members, **{"../outside": b"escape"})]
        for index, members in enumerate(variants):
            with self.subTest(index=index):
                archive, checksum = self.archive(members)
                with self.assertRaises((ValueError, RuntimeError)):
                    reuse.extract_artifact(archive, self.root / str(index), self.target, checksum)
                self.assertFalse((self.root / "outside").exists())


class PayloadVerificationTests(unittest.TestCase):
    target = "windows-amd64"

    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="native-reuse-payload-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.payloads = self.root / "native-payloads"
        self.payloads.mkdir()
        self.expected = reuse.NATIVE.source_state(reuse.ROOT)
        files = {"bin/fabrics.exe": b"fixture runtime, never executed",
                 "backends/cpu/llama-server.exe": b"fixture inference binary, never executed"}
        raw = io.BytesIO()
        with tarfile.open(fileobj=raw, mode="w:gz") as archive:
            for name, data in files.items():
                entry = tarfile.TarInfo("native/" + name)
                entry.size = len(data)
                archive.addfile(entry, io.BytesIO(data))
        data = raw.getvalue()
        (self.payloads / (self.target + ".tar.gz")).write_bytes(data)
        self.manifest = dict(copy.deepcopy(self.expected), format_version=1, version="0.1.0",
                             platform="windows/amd64", backends=["cpu"], default_backend="cpu",
                             backend_paths={"cpu": "backends/cpu/llama-server.exe"},
                             files={name: hashlib.sha256(value).hexdigest() for name, value in files.items()},
                             archive_sha256=hashlib.sha256(data).hexdigest(),
                             llama_commit=reuse.NATIVE.LLAMA_COMMIT,
                             llama_source_sha256=reuse.NATIVE.LLAMA_SHA256,
                             go_version="go version " + reuse.NATIVE.GO_VERSION + " windows/amd64")
        catalog = json.loads((reuse.ROOT / "installer/models.json").read_text(encoding="utf-8"))
        model = next(item for item in catalog if item["name"] == "qwen2.5-1.5b")
        arithmetic = self.result("2 + 2 equals 4.")
        recall = self.result("Your favorite planet is Neptune.")
        recall["memories"] = [{"id": 1, "content": "The user's favorite planet is Neptune."}]
        self.report = {
            "status": "passed", "runtime_checks": "passed", "mode": "balanced", "backend": "cpu",
            "model": model["name"] + "-" + model["sha256"] + ".gguf",
            "backend_evidence": {"host_os": "Windows", "host_arch": "AMD64",
                                 "configured_backend": "cpu", "expected_backend": "cpu",
                                 "offload_confirmed": False, "offloaded_layers": 0,
                                 "startup_evidence_lines": ["load_tensors: CPU_Mapped model buffer size = 512.00 MiB"]},
            "arithmetic": arithmetic, "recall": recall,
            "persisted_turn": {"answer": recall["answer"], "reflection": json.dumps(recall["assessment"]),
                               "recall_ids": [1], "cognition": json.dumps({"assessed": True, "decision": recall["decision"]})},
            "reopen": "persisted turn unchanged",
            "shutdown": "parent exited and inference listeners closed; surviving Windows descendants not independently enumerated",
        }
        self.write()

    @staticmethod
    def result(answer):
        return {"answer": answer, "assessment": {"confidence": 0.6, "needs_revision": False,
                                                "notes": "Fixture assessment.", "missing": []},
                "assessed": True, "revised": False,
                "decision": {"requested_mode": "balanced", "mode": "balanced", "reason": "Explicit mode.",
                             "call_budget": 4, "calls": 2, "phases": ["answer", "assessment"], "duration_ms": 10}}

    def write(self):
        (self.payloads / (self.target + ".json")).write_text(json.dumps(self.manifest), encoding="utf-8")
        (self.root / ("native-smoke-" + self.target + ".json")).write_text(json.dumps(self.report), encoding="utf-8")

    def verify(self):
        return reuse.verify_payload(self.root, self.target, self.expected)

    def test_original_payload_and_native_cpu_smoke_are_verified_without_execution(self):
        checked = self.verify()
        self.assertEqual(checked["archive_sha256"], self.manifest["archive_sha256"])
        self.assertEqual(checked["smoke_status"], "passed")
        self.assertEqual(checked["model"], self.report["model"])

    def test_source_snapshot_source_pin_and_toolchain_mutations_are_rejected(self):
        original = copy.deepcopy(self.manifest)
        changes = {"go_mod_sha256": "0" * 64, "runtime_source_files": {}, "native_source_files": {},
                   "llama_commit": "0" * 40, "llama_source_sha256": "0" * 64,
                   "go_version": "go version go1.27.1 linux/amd64", "default_backend": "cuda"}
        for key, value in changes.items():
            with self.subTest(key=key):
                self.manifest = dict(copy.deepcopy(original), **{key: value})
                self.write()
                with self.assertRaises((ValueError, RuntimeError)):
                    self.verify()

    def test_changed_payload_archive_is_rejected(self):
        archive = self.payloads / (self.target + ".tar.gz")
        archive.write_bytes(archive.read_bytes() + b"changed")
        with self.assertRaises((ValueError, RuntimeError)):
            self.verify()

    def test_failed_wrong_host_or_model_and_inconsistent_smoke_are_rejected(self):
        original = copy.deepcopy(self.report)
        variants = []
        for key, value in (("status", "failed"), ("model", "unverified.gguf"),
                           ("reopen", "not checked"), ("shutdown", "not checked")):
            variants.append(dict(copy.deepcopy(original), **{key: value}))
        for key, value in (("host_os", "Linux"), ("host_arch", "ARM64"),
                           ("offloaded_layers", 2), ("startup_evidence_lines", ["server ready"])):
            report = copy.deepcopy(original)
            report["backend_evidence"][key] = value
            variants.append(report)
        for key in ("arithmetic", "recall"):
            report = copy.deepcopy(original)
            report[key]["answer"] = "Wrong answer."
            variants.append(report)
        report = copy.deepcopy(original)
        report["persisted_turn"]["answer"] = "Different persisted answer."
        variants.append(report)
        for index, report in enumerate(variants):
            with self.subTest(index=index):
                self.report = report
                self.write()
                with self.assertRaises((ValueError, RuntimeError, AssertionError)):
                    self.verify()


if __name__ == "__main__":
    unittest.main()
