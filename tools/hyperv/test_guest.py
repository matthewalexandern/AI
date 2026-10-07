"""Linux-only protocol tests with fixture commands; these are not model smoke tests."""
import hashlib
import json
from pathlib import Path
import platform
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parent


@unittest.skipUnless(platform.system() == "Linux", "Guest harness runs on Linux")
class GuestHarnessTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="mini-fabrics-test-", dir="/tmp")
        self.addCleanup(self.temporary.cleanup)
        self.work = Path(self.temporary.name)
        self.installer = self.work / "installer"
        self.installer.write_text(
            "#!/usr/bin/env python3\n"
            "from pathlib import Path\nimport sys\n"
            "prefix = Path(sys.argv[sys.argv.index('--prefix') + 1])\n"
            "assert prefix.parent == Path(__file__).parent\n"
            "assert sys.argv[sys.argv.index('--backend') + 1] == 'cpu'\n"
            "runtime = prefix / 'bin' / 'fabrics'\n"
            "runtime.parent.mkdir(parents=True)\n"
            "runtime.write_text('#!/bin/sh\\nprintf \\\"fixture doctor\\\\n\\\"\\n')\n"
            "runtime.chmod(0o700)\n"
            "print('fixture installation')\n",
            encoding="utf-8",
        )
        (self.work / "smoke.py").write_text(
            "import json\nfrom pathlib import Path\nimport sys\n"
            "def argument(name): return sys.argv[sys.argv.index(name) + 1]\n"
            "assert argument('--expect-backend') == 'cpu'\n"
            "assert argument('--mode') == 'balanced'\n"
            "assert Path(argument('--home')).parent == Path(__file__).parent\n"
            "logs = Path(argument('--log-directory'))\n"
            "logs.mkdir(parents=True)\n"
            "(logs / 'runtime.log').write_text('fixture log')\n"
            "Path(argument('--report')).write_text(json.dumps({'fixture': True}))\n"
            "print('fixture smoke')\n",
            encoding="utf-8",
        )

    def run_harness(self, checksum=None):
        digest = checksum or hashlib.sha256(self.installer.read_bytes()).hexdigest()
        return subprocess.run(
            ["bash", str(ROOT / "guest-test.sh"), str(self.work), digest, "qwen2.5-0.5b"],
            capture_output=True, text=True, timeout=20,
        )

    def report(self):
        return json.loads((self.work / "evidence" / "guest-report.json").read_text())

    def test_complete_fixture_pipeline_retains_reports_and_logs(self):
        process = self.run_harness()
        self.assertEqual(process.returncode, 0, process.stdout + process.stderr)
        report = self.report()
        self.assertEqual(report["status"], "passed")
        self.assertEqual(report["last_stage"], "complete")
        self.assertFalse(report["gpu_validated"])
        self.assertTrue((self.work / "evidence" / "smoke.json").is_file())
        self.assertEqual((self.work / "evidence" / "logs" / "runtime.log").read_text(), "fixture log")

    def test_wrong_checksum_never_executes_installer(self):
        process = self.run_harness("0" * 64)
        self.assertNotEqual(process.returncode, 0)
        self.assertFalse((self.work / "install").exists())
        self.assertEqual(self.report()["last_stage"], "installer_checksum")
        self.assertEqual(self.report()["status"], "failed")

    def test_installer_failure_retains_exit_code_and_log(self):
        self.installer.write_text("#!/bin/sh\nprintf 'fixture installation failure\\n' >&2\nexit 17\n", encoding="utf-8")
        process = self.run_harness()
        self.assertEqual(process.returncode, 17, process.stdout + process.stderr)
        self.assertEqual(self.report()["exit_code"], 17)
        self.assertEqual(self.report()["last_stage"], "install")
        self.assertIn("fixture installation failure", (self.work / "evidence" / "install.log").read_text())

    def test_smoke_failure_does_not_become_success(self):
        (self.work / "smoke.py").write_text("import sys\nprint('fixture smoke failure')\nsys.exit(19)\n", encoding="utf-8")
        process = self.run_harness()
        self.assertEqual(process.returncode, 19, process.stdout + process.stderr)
        self.assertEqual(self.report()["last_stage"], "smoke")
        self.assertEqual(self.report()["status"], "failed")
        self.assertIn("fixture smoke failure", (self.work / "evidence" / "smoke.log").read_text())


if __name__ == "__main__":
    unittest.main()
