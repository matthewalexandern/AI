#!/usr/bin/env python3
"""Validate the real Swift helper against independent macOS OS information."""
import argparse
import json
from pathlib import Path
import platform
import subprocess


def verify(helper):
    if platform.system() != "Darwin":
        raise RuntimeError("Native resource verification requires a macOS host")
    total = int(subprocess.check_output(["sysctl", "-n", "hw.memsize"], text=True))
    samples = []
    for _ in range(3):
        probe = subprocess.run([str(Path(helper).resolve())], capture_output=True, text=True, check=True, timeout=10)
        result = json.loads(probe.stdout)
        assert result["schema_version"] == 1, result
        for key in ("ram_total_bytes", "ram_available_bytes", "vram_total_bytes", "vram_available_bytes"):
            assert type(result[key]) is int and result[key] >= 0, (key, result)
        assert result["ram_total_bytes"] == total > 0, result
        assert result["ram_available_bytes"] <= total, result
        assert result["vram_available_bytes"] <= result["vram_total_bytes"], result
        assert type(result["unified_memory"]) is bool, result
        assert type(result["metal_available"]) is bool, result
        assert result["gpu_memory_is_estimate"] is True, result
        assert result["ram_available_kind"] == "free_including_speculative_plus_inactive", result
        assert result["gpu_memory_kind"] == "metal_recommended_working_set", result
        assert "not_global_free_vram" in result["gpu_available_kind"], result
        assert result["cpu_arch"] == platform.machine(), result
        assert isinstance(result["os_version"], str) and result["os_version"], result
        if result["metal_available"]:
            assert result["vram_total_bytes"] > 0 and result["gpu_name"], result
        else:
            assert result["vram_total_bytes"] == result["vram_available_bytes"] == 0, result
            assert result["unified_memory"] is False, result
        samples.append(result)
    return {"status": "passed", "host": "macOS", "independent_ram_total_bytes": total, "samples": samples}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--helper", required=True, type=Path)
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    encoded = json.dumps(verify(args.helper), indent=2) + "\n"
    if args.report:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(encoded, encoding="utf-8")
    print(encoded, end="")
