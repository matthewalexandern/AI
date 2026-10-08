#!/usr/bin/env python3
"""Exercise real model inference, memory persistence, and owned-process shutdown."""
import argparse
import json
import math
import os
from pathlib import Path
import platform
import re
import signal
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request


def request(base, path, payload=None, timeout=300):
    data = None if payload is None else json.dumps(payload).encode()
    headers = {} if data is None else {"Content-Type": "application/json"}
    req = urllib.request.Request(base + path, data=data, headers=headers)
    # Local inference must never be routed through a configured HTTP proxy.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(req, timeout=timeout) as response:
            return json.load(response)
    except urllib.error.HTTPError as error:
        detail = error.read(65536).decode("utf-8", errors="replace")
        raise RuntimeError(f"Local API {path} returned HTTP {error.code}: {detail}") from error


MODES = ("adaptive", "fast", "balanced", "deep")
BACKENDS = ("cpu", "cuda", "metal", "vulkan")


def print_diagnostic(message):
    # Windows terminals can still use a legacy code page while model/log/config
    # data is UTF-8. Preserve diagnostics as escapes instead of masking the
    # original failure with a console UnicodeEncodeError.
    encoding = sys.stdout.encoding or "utf-8"
    print(message.encode(encoding, errors="backslashreplace").decode(encoding), flush=True)


def retained_log_path(log_directory):
    # Logs must outlive the isolated runtime home even when setup, inference,
    # shutdown, or database reopening raises. Never overwrite another run.
    if log_directory is None:
        directory = Path(tempfile.mkdtemp(prefix="mini-fabrics-smoke-logs-"))
    else:
        base = Path(log_directory).resolve()
        base.mkdir(parents=True, exist_ok=True)
        directory = Path(tempfile.mkdtemp(prefix="smoke-", dir=base))
    return directory / "runtime.log"


def retain_visible_result(path, results, phase, result):
    # Keep only the public runtime contract, never raw llama.cpp responses or
    # reasoning channels. Save before assertions so a wrong answer is reviewable
    # after the isolated memory database has been removed.
    visible = {key: result[key] for key in ("answer", "plan", "assessed", "revised") if key in result}
    for field, keys in (
        ("assessment", ("confidence", "needs_revision", "notes", "missing")),
        ("decision", ("requested_mode", "mode", "reason", "call_budget", "calls", "phases", "duration_ms")),
    ):
        value = result.get(field)
        if isinstance(value, dict):
            visible[field] = {key: value[key] for key in keys if key in value}
    if isinstance(result.get("memories"), list):
        visible["memories"] = [
            {key: entry[key] for key in ("id", "content", "kind", "created_at", "score") if key in entry}
            for entry in result["memories"] if isinstance(entry, dict)
        ]
    results[phase] = visible
    path.write_text(json.dumps(results, indent=2) + "\n", encoding="utf-8")


def check_result(result, expected_mode=None):
    if not isinstance(result.get("answer"), str) or not result["answer"].strip():
        raise AssertionError("The model did not produce an answer")
    assessment = result["assessment"]
    if set(assessment) != {"confidence", "needs_revision", "notes", "missing"}:
        raise AssertionError("Assessment fields do not match the contract")
    confidence = assessment["confidence"]
    if isinstance(confidence, bool) or not isinstance(confidence, (int, float)) or not 0 <= confidence <= 1:
        raise AssertionError("Assessment confidence is invalid")
    if not isinstance(assessment["needs_revision"], bool) or not isinstance(assessment["notes"], str) or not assessment["notes"].strip():
        raise AssertionError("Assessment flags or notes are invalid")
    if not isinstance(assessment["missing"], list) or not all(isinstance(value, str) and value.strip() for value in assessment["missing"]):
        raise AssertionError("Assessment missing-information list is invalid")
    decision = result.get("decision", {})
    mode = decision.get("mode")
    budgets = {"fast": 1, "balanced": 4, "deep": 5}
    if mode not in budgets or decision.get("requested_mode") not in MODES:
        raise AssertionError("Runtime scheduling decision is missing or invalid")
    if expected_mode is not None and decision["requested_mode"] != expected_mode:
        raise AssertionError("Runtime did not use the requested cognition mode")
    if decision["requested_mode"] != "adaptive" and mode != decision["requested_mode"]:
        raise AssertionError("Runtime ignored the explicit cognition mode")
    assessed = result.get("assessed")
    if not isinstance(assessed, bool) or assessed != (mode != "fast"):
        raise AssertionError("Assessment status does not match the selected mode")
    phases = ["answer"] if mode == "fast" else ["answer", "assessment"]
    if mode == "deep":
        phases.insert(0, "outline")
    revised = result.get("revised")
    if not isinstance(revised, bool) or revised and mode == "fast":
        raise AssertionError("Revision status does not match the selected mode")
    if revised:
        phases.extend(["revision", "assessment"])
    if type(decision.get("call_budget")) is not int or decision["call_budget"] != budgets[mode] or type(decision.get("calls")) is not int or decision["calls"] != len(phases) or decision.get("phases") != phases:
        raise AssertionError("Inference phases or call counts exceed the selected policy")
    if not isinstance(decision.get("reason"), str) or not decision["reason"].strip() or type(decision.get("duration_ms")) is not int or decision["duration_ms"] < 0:
        raise AssertionError("Scheduling reason or elapsed duration is invalid")
    if not assessed and (confidence != 0 or assessment["needs_revision"] or assessment["missing"] or "Not assessed" not in assessment["notes"]):
        raise AssertionError("Fast mode must explicitly report assessment unavailable")


def check_arithmetic(result, expected_mode=None):
    check_result(result, expected_mode)
    if not re.search(r"\b4\b", result["answer"]):
        raise AssertionError("The model failed the arithmetic check")
    if result["assessment"]["missing"]:
        # A structurally valid but unhelpful critique is a model-quality issue.
        # Preserve the evidence while continuing persistence and shutdown checks.
        return [{"check": "arithmetic_assessment", "message": "The model reported missing information for self-contained arithmetic", "reported_missing": result["assessment"]["missing"]}]
    return []


def backend_evidence(configuration, startup_log, expected_backend=None, require_gpu=False):
    """Require actual loaded model buffers, not merely GPU discovery or flags."""
    configured = configuration.get("backend", "cpu")
    if configured not in BACKENDS or expected_backend is not None and expected_backend not in BACKENDS:
        raise ValueError("Backend must be cpu, cuda, metal, or vulkan")
    if expected_backend is not None and configured != expected_backend:
        raise AssertionError(f"Configured backend {configured!r} does not match expected {expected_backend!r}")
    if require_gpu and configured == "cpu":
        raise AssertionError("GPU verification requires a configured GPU backend")
    clean = re.sub(r"\x1b\[[0-9;]*m", "", startup_log)
    offloads, buffers, loaded, devices, evidence = [], [], set(), [], []
    for line in clean.splitlines():
        offload = re.search(r"\b(?:llama_model_load_tensors|load_tensors):\s+offloaded\s+(\d+)/(\d+)\s+layers to GPU\b", line)
        buffer = re.search(r"\b(?:llama_model_load_tensors|load_tensors):\s+(\w+)\s+model buffer size\s*=\s*(\d+(?:\.\d+)?)\s+MiB\b", line)
        backend = re.search(r"\bload_backend:\s+loaded\s+(\w+)\s+backend\b", line)
        device = re.search(r"\busing device\s+(\w+)\s+\((.+)\)", line)
        if offload:
            count, total = map(int, offload.groups())
            if count > total:
                raise AssertionError("Invalid layer-offload count in runtime log")
            offloads.append((count, total))
        if buffer:
            buffers.append({"name": buffer[1], "mib": float(buffer[2])})
        if backend:
            loaded.add(backend[1].lower())
        if device:
            devices.append({"name": device[1], "description": device[2][:512]})
        if offload or buffer or backend or device:
            evidence.append(line[:1024])
    observed_gpu = set()
    cpu_mib = 0.0
    for buffer in buffers:
        name = buffer["name"].lower()
        if buffer["mib"] <= 0:
            continue
        if name.startswith("cpu"):
            cpu_mib += buffer["mib"]
        # CUDA_Host and Vulkan_Host are CPU staging allocations, not proof
        # of inference on a device. Metal's mapped/shared buffers are GPU-visible.
        if "host" not in name:
            for backend in ("cuda", "metal", "vulkan"):
                if name.startswith(backend):
                    observed_gpu.add(backend)
    count, total = max(offloads, default=(0, 0))
    confirmed = count > 0 and configured in observed_gpu
    if configured == "cpu":
        if count > 0 or observed_gpu:
            raise AssertionError("CPU verification observed GPU model offload")
        if cpu_mib <= 0:
            raise AssertionError("Runtime log did not confirm a loaded CPU model buffer")
    elif not confirmed:
        raise AssertionError(f"Runtime log did not confirm positive {configured} layer offload and matching GPU model buffers; CPU fallback cannot pass GPU verification")
    if observed_gpu - {configured}:
        raise AssertionError("Runtime model buffers do not match the configured GPU backend")
    if confirmed and any(re.search(r"llvmpipe|lavapipe|swiftshader|software rasterizer|microsoft basic render", device["description"], re.IGNORECASE) for device in devices):
        raise AssertionError("Runtime selected a software GPU adapter; this does not verify GPU hardware")
    return {"configured_backend": configured, "expected_backend": expected_backend, "gpu_required": require_gpu,
            "offload_confirmed": confirmed, "offloaded_layers": count, "total_layers": total,
            "loaded_backends": sorted(loaded), "observed_gpu_backends": sorted(observed_gpu),
            "model_buffers": buffers, "devices": devices, "startup_evidence_lines": evidence[:64],
            "hardware_tested": f"{configured} GPU offload" if confirmed else "CPU inference; no GPU offload",
            "host_os": platform.system(), "host_arch": platform.machine()}


def reachable(port):
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=1):
            return True
    except OSError:
        return False


def stop(process):
    if process.poll() is not None:
        return
    if os.name == "nt":
        process.send_signal(signal.CTRL_BREAK_EVENT)
    else:
        process.terminate()
    try:
        process.wait(timeout=15)
    except subprocess.TimeoutExpired:
        if os.name == "nt":
            subprocess.run(["taskkill", "/PID", str(process.pid), "/T", "/F"], capture_output=True)
        else:
            os.killpg(process.pid, signal.SIGKILL)
        process.wait(timeout=10)
        raise AssertionError("Runtime did not shut down gracefully")


def check_process_group_shutdown(process, timeout=5):
    """Verify no POSIX process remains in the runtime's original process group."""
    deadline = time.monotonic() + timeout
    while True:
        try:
            os.killpg(process.pid, 0)
        except ProcessLookupError:
            return
        if time.monotonic() >= deadline:
            # Clean up any surviving members of this smoke run's own group,
            # while still failing verification rather than accepting a kill.
            try:
                os.killpg(process.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            raise AssertionError("An owned process survived runtime shutdown")
        time.sleep(0.05)


def smoke(runtime, home, mode="balanced", log_directory=None, expected_backend=None, require_gpu=False, startup_timeout=240, turn_timeout=300):
    if mode not in MODES:
        raise ValueError("Cognition mode must be adaptive, fast, balanced, or deep")
    if not math.isfinite(startup_timeout) or not 10 <= startup_timeout <= 900:
        raise ValueError("Startup timeout must be between 10 and 900 seconds")
    runtime = Path(runtime).resolve()
    configuration = json.loads((Path(home) / "config.json").read_text(encoding="utf-8"))
    if not configuration.get("model_path"):
        raise ValueError("Install a GGUF model before running the real-model smoke check")
    configured_backend = configuration.get("backend", "cpu")
    if expected_backend is not None and configured_backend != expected_backend:
        raise AssertionError(f"Configured backend {configured_backend!r} does not match expected {expected_backend!r}")
    if require_gpu and configured_backend == "cpu":
        raise AssertionError("GPU verification requires a configured GPU backend")
    configuration["cognition_mode"] = mode
    # Isolate validation memories and config; retain the installed model/binary.
    with tempfile.TemporaryDirectory(prefix="mini-fabrics-smoke-") as directory:
        isolated = Path(directory)
        first, second = socket.socket(), socket.socket()
        try:
            first.bind(("127.0.0.1", 0))
            second.bind(("127.0.0.1", 0))
            api_port, engine_port = first.getsockname()[1], second.getsockname()[1]
        finally:
            first.close(); second.close()
        configuration["port"] = engine_port
        (isolated / "config.json").write_text(json.dumps(configuration), encoding="utf-8")
        base = f"http://127.0.0.1:{api_port}"
        process_options = {"creationflags": subprocess.CREATE_NEW_PROCESS_GROUP} if os.name == "nt" else {"start_new_session": True}
        log_path = retained_log_path(log_directory)
        results_path = log_path.with_name("api-results.json")
        visible_results = {}
        with log_path.open("wb") as log:
            process = subprocess.Popen([str(runtime), "--home", str(isolated), "serve", "--listen", f"127.0.0.1:{api_port}", "--startup-timeout", f"{startup_timeout:g}s", "--turn-timeout", f"{turn_timeout:g}s"], stdout=log, stderr=subprocess.STDOUT, **process_options)
            try:
                startup_begin = time.monotonic()
                deadline = startup_begin + startup_timeout
                while time.monotonic() < deadline:
                    if process.poll() is not None:
                        raise AssertionError(f"Runtime exited before readiness (status {process.returncode})")
                    try:
                        if request(base, "/health", timeout=2).get("status") == "ok":
                            break
                    except (OSError, urllib.error.URLError, RuntimeError):
                        pass
                    time.sleep(0.2)
                else:
                    raise AssertionError(f"Runtime did not become ready within {startup_timeout:g} seconds")

                log.flush()
                # Capture load evidence before inference can add generated text
                # to any diagnostics. Backend discovery alone is insufficient.
                evidence = backend_evidence(configuration, log_path.read_text(encoding="utf-8", errors="replace"), expected_backend, require_gpu)
                evidence["readiness_seconds"] = round(time.monotonic() - startup_begin, 3)

                arithmetic = request(base, "/v1/chat", {"session": "arithmetic", "input": "What is 2 + 2? Give one short sentence."}, timeout=turn_timeout + 30)
                retain_visible_result(results_path, visible_results, "arithmetic", arithmetic)
                model_quality_warnings = check_arithmetic(arithmetic, mode)

                memory = request(base, "/v1/memory", {"content": "The user's favorite planet is Neptune."})
                recall = request(base, "/v1/chat", {"session": "recall", "input": "What is my favorite planet? Use my saved preference and answer briefly."}, timeout=turn_timeout + 30)
                retain_visible_result(results_path, visible_results, "recall", recall)
                check_result(recall, mode)
                if "neptune" not in recall["answer"].lower():
                    raise AssertionError("The model failed to use its saved memory")
                if memory["id"] not in [entry["id"] for entry in recall["memories"]]:
                    raise AssertionError("The saved memory is missing from reported recall provenance")
                episodes = request(base, "/v1/episodes?session=recall")
                if len(episodes) != 1 or episodes[0]["answer"] != recall["answer"]:
                    raise AssertionError("Completed turn was not persisted correctly")
                if json.loads(episodes[0]["reflection"]) != recall["assessment"]:
                    raise AssertionError("Persisted assessment differs from the returned assessment")
                if memory["id"] not in episodes[0]["recall_ids"]:
                    raise AssertionError("Persisted recall provenance is incomplete")
                if json.loads(episodes[0]["cognition"]) != {"assessed": recall["assessed"], "decision": recall["decision"]}:
                    raise AssertionError("Persisted cognition decision differs from the returned decision")
            except Exception:
                log.flush()
                print_diagnostic(log_path.read_text(encoding="utf-8", errors="replace")[-6000:])
                print_diagnostic(f"Preserved runtime log: {log_path}")
                if results_path.exists():
                    print_diagnostic(f"Preserved visible API results: {results_path}")
                raise
            finally:
                stop(process)

        if process.returncode != 0:
            raise AssertionError(f"Runtime shutdown returned {process.returncode}")
        if os.name != "nt":
            check_process_group_shutdown(process)
            shutdown = "parent exited, inference listeners closed, POSIX process group empty"
        else:
            shutdown = "parent exited and inference listeners closed; surviving Windows descendants not independently enumerated"
        if reachable(api_port) or reachable(engine_port):
            raise AssertionError("An owned API or inference listener survived runtime shutdown")
        reopened = subprocess.run([str(runtime), "--home", str(isolated), "episodes", "--session", "recall"], capture_output=True, text=True, encoding="utf-8", check=True, timeout=30)
        if json.loads(reopened.stdout) != episodes:
            raise AssertionError("Persisted turn changed after reopening the database")
        return {"status": "passed_with_model_warnings" if model_quality_warnings else "passed", "runtime_checks": "passed", "model_quality_warnings": model_quality_warnings,
                "model": Path(configuration["model_path"]).name, "mode": mode, "backend": configured_backend,
                "backend_evidence": evidence, "hardware_tested": evidence["hardware_tested"], "runtime_log": str(log_path),
                "visible_results": str(results_path),
                "arithmetic": arithmetic, "recall": recall, "persisted_turn": episodes[0], "shutdown": shutdown, "reopen": "persisted turn unchanged"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runtime", required=True, type=Path)
    parser.add_argument("--home", required=True, type=Path)
    parser.add_argument("--report", type=Path)
    parser.add_argument("--mode", choices=MODES, default="balanced", help="cognition mode in an isolated configuration (default: balanced, including assessment)")
    parser.add_argument("--log-directory", type=Path, help="directory for retained runtime logs (default: a separate persistent system-temporary directory)")
    parser.add_argument("--expect-backend", choices=BACKENDS, help="require the configured and observed inference backend to match")
    parser.add_argument("--require-gpu", action="store_true", help="require positive GPU layer offload and matching GPU model buffers")
    parser.add_argument("--startup-timeout", type=float, default=240, help="model startup deadline in seconds, from 10 to 900 (default: 240)")
    parser.add_argument("--turn-timeout", type=int, default=300, help="per-turn inference deadline in seconds (10..3600)")
    args = parser.parse_args()
    if not 10 <= args.turn_timeout <= 3600:
        parser.error("turn-timeout must be between 10 and 3600 seconds")
    result = smoke(args.runtime, args.home, args.mode, args.log_directory, args.expect_backend, args.require_gpu, args.startup_timeout, args.turn_timeout)
    encoded = json.dumps(result, indent=2) + "\n"
    if args.report:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(encoded, encoding="utf-8")
    print(encoded)


if __name__ == "__main__":
    main()
