import importlib.util
import os
from pathlib import Path
import signal
import subprocess
import sys
import unittest

spec = importlib.util.spec_from_file_location("smoke", Path(__file__).with_name("smoke.py"))
smoke = importlib.util.module_from_spec(spec)
spec.loader.exec_module(smoke)


class ModelResultTests(unittest.TestCase):
    def result(self, answer="2 + 2 equals 4.", missing=None):
        return {"answer": answer, "assessment": {"confidence": 0.6, "needs_revision": False, "notes": "Model-reported assessment.", "missing": [] if missing is None else missing},
                "assessed": True, "revised": False,
                "decision": {"requested_mode": "balanced", "mode": "balanced", "reason": "Explicit mode selection.", "call_budget": 4, "calls": 2, "phases": ["answer", "assessment"], "duration_ms": 10}}

    def test_unhelpful_arithmetic_critique_is_preserved_as_quality_warning(self):
        reported = ["Missing evidence for arithmetic."]
        warnings = smoke.check_arithmetic(self.result(missing=reported))
        self.assertEqual(len(warnings), 1)
        self.assertEqual(warnings[0]["check"], "arithmetic_assessment")
        self.assertEqual(warnings[0]["reported_missing"], reported)
        self.assertEqual(smoke.check_arithmetic(self.result()), [])

    def test_wrong_answers_and_malformed_assessments_still_fail(self):
        with self.assertRaisesRegex(AssertionError, "failed the arithmetic check"):
            smoke.check_arithmetic(self.result(answer="2 + 2 equals 5.", missing=["Some context."]))
        with self.assertRaisesRegex(AssertionError, "missing-information list is invalid"):
            smoke.check_arithmetic(self.result(missing="not a list"))

    def test_explicit_mode_and_call_budget_are_checked(self):
        result = self.result()
        with self.assertRaisesRegex(AssertionError, "requested cognition mode"):
            smoke.check_result(result, "deep")
        result["decision"]["calls"] = 5
        with self.assertRaisesRegex(AssertionError, "call counts"):
            smoke.check_result(result, "balanced")

    def test_fast_mode_reports_unassessed_instead_of_fake_model_confidence(self):
        result = self.result()
        result["decision"].update({"mode": "fast", "requested_mode": "adaptive", "call_budget": 1, "calls": 1, "phases": ["answer"]})
        result["assessed"] = False
        result["assessment"].update({"confidence": 0, "notes": "Not assessed in fast mode; confidence is not available."})
        self.assertEqual(smoke.check_arithmetic(result, "adaptive"), [])
        result["assessment"]["confidence"] = 0.6
        with self.assertRaisesRegex(AssertionError, "assessment unavailable"):
            smoke.check_result(result, "adaptive")


class BackendEvidenceTests(unittest.TestCase):
    cpu = "load_backend: loaded CPU backend from /native/libggml-cpu.so\nload_tensors: CPU_Mapped model buffer size = 512.00 MiB\n"

    def gpu(self, backend, buffer=None, layers=25):
        name = buffer or {"cuda": "CUDA0", "metal": "Metal_Mapped", "vulkan": "Vulkan0"}[backend]
        return f"load_backend: loaded {backend.upper()} backend from /native/backend\nload_tensors: offloaded {layers}/25 layers to GPU\nload_tensors: {name} model buffer size = 512.00 MiB\n"

    def test_cpu_inference_is_not_reported_as_gpu_testing(self):
        evidence = smoke.backend_evidence({"backend": "cpu"}, self.cpu, "cpu")
        self.assertFalse(evidence["offload_confirmed"])
        self.assertEqual(evidence["hardware_tested"], "CPU inference; no GPU offload")
        with self.assertRaisesRegex(AssertionError, "configured GPU backend"):
            smoke.backend_evidence({"backend": "cpu"}, self.cpu, require_gpu=True)

    def test_each_gpu_requires_positive_layers_and_matching_buffers(self):
        for backend in ("cuda", "metal", "vulkan"):
            with self.subTest(backend=backend):
                log = "\x1b[32m" + self.gpu(backend) + "\x1b[0m"
                evidence = smoke.backend_evidence({"backend": backend}, log, backend, True)
                self.assertTrue(evidence["offload_confirmed"])
                self.assertEqual(evidence["offloaded_layers"], 25)
                self.assertEqual(evidence["observed_gpu_backends"], [backend])
                self.assertTrue(evidence["startup_evidence_lines"])

    def test_backend_discovery_host_buffers_and_zero_offload_cannot_pass(self):
        for log in (
            self.cpu + "load_backend: loaded CUDA backend from /native/cuda\n",
            self.cpu + self.gpu("cuda", buffer="CUDA_Host"),
            self.cpu + self.gpu("cuda", layers=0),
            self.cpu + "load_tensors: CUDA0 model buffer size = 512.00 MiB\n",
            self.cpu + self.gpu("vulkan"),
        ):
            with self.subTest(log=log), self.assertRaisesRegex(AssertionError, "did not confirm"):
                smoke.backend_evidence({"backend": "cuda"}, log, "cuda", True)

    def test_wrong_backend_and_absent_load_evidence_fail(self):
        with self.assertRaisesRegex(AssertionError, "does not match expected"):
            smoke.backend_evidence({"backend": "cpu"}, self.cpu, "cuda")
        with self.assertRaisesRegex(AssertionError, "observed GPU"):
            smoke.backend_evidence({"backend": "cpu"}, self.gpu("cuda"), "cpu")
        with self.assertRaisesRegex(AssertionError, "loaded CPU model buffer"):
            smoke.backend_evidence({"backend": "cpu"}, "server ready\n", "cpu")
        with self.assertRaisesRegex(AssertionError, "Invalid layer-offload"):
            smoke.backend_evidence({"backend": "cuda"}, self.gpu("cuda", layers=26), "cuda")

    def test_software_vulkan_adapter_is_not_claimed_as_gpu_hardware(self):
        log = self.gpu("vulkan") + "llama_model_load: using device Vulkan0 (llvmpipe software rasterizer) (id) - 1024 MiB free\n"
        with self.assertRaisesRegex(AssertionError, "software GPU adapter"):
            smoke.backend_evidence({"backend": "vulkan"}, log, "vulkan", True)


@unittest.skipIf(os.name == "nt", "Windows smoke reports listener shutdown without enumerating descendants")
class ProcessGroupTests(unittest.TestCase):
    def test_reaped_process_group_passes(self):
        process = subprocess.Popen([sys.executable, "-c", "pass"], start_new_session=True)
        self.assertEqual(process.wait(timeout=10), 0)
        smoke.check_process_group_shutdown(process, timeout=0.1)

    def test_process_without_listener_still_fails_and_is_cleaned_up(self):
        process = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"], start_new_session=True)
        try:
            with self.assertRaisesRegex(AssertionError, "owned process survived"):
                smoke.check_process_group_shutdown(process, timeout=0.1)
            self.assertEqual(process.wait(timeout=10), -signal.SIGKILL)
        finally:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=10)


if __name__ == "__main__":
    unittest.main()
