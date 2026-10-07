#!/usr/bin/env bash
# Invoked over existing authenticated SSH by Invoke-LinuxGuestTest.ps1.
set -euo pipefail
if [[ $# != 3 || "$1" != /tmp/mini-fabrics-test-* || ! "$2" =~ ^[a-fA-F0-9]{64}$ ]]; then
    printf '%s\n' 'Expected generated work directory, installer SHA256, and catalog model.' >&2
    exit 2
fi
work_directory="$1"
installer_sha="$2"
model="$3"
case "$model" in qwen2.5-0.5b|qwen2.5-1.5b|qwen2.5-3b) ;; *) exit 2 ;; esac
umask 077
evidence="$work_directory/evidence"
mkdir -p "$evidence"
if ! command -v python3 >/dev/null; then
    printf '%s\n' 'Install Python 3.9+ inside the guest before running this test (Ubuntu: python3; RHEL: python3.9 or newer).' | tee "$evidence/prerequisite-error.log" >&2
    exit 1
fi
stage='prerequisites'
write_report() {
    result="$?"
    trap - EXIT
    python3 - "$evidence/guest-report.json" "$result" "$stage" "$installer_sha" "$model" <<'PY'
import json
from pathlib import Path
import platform
import sys
destination, code, stage, checksum, model = sys.argv[1:]
os_release = {}
for line in Path('/etc/os-release').read_text().splitlines():
    if '=' in line:
        key, value = line.split('=', 1)
        if key in ('ID', 'VERSION_ID', 'PRETTY_NAME'):
            os_release[key] = value.strip('"')
report = {'status': 'passed' if code == '0' else 'failed', 'exit_code': int(code),
          'last_stage': stage, 'backend': 'cpu', 'model': model,
          'installer_sha256': checksum, 'architecture': platform.machine(),
          'os_release': os_release, 'memory_isolation': 'unique installation prefix and smoke temporary memory database',
          'gpu_validated': False}
Path(destination).write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
PY
    exit "$result"
}
trap write_report EXIT
python3 -c 'import sys; assert sys.version_info >= (3, 9), "Python 3.9+ is required"'
test "$(uname -s)" = Linux
stage='installer_checksum'
printf '%s  %s\n' "$installer_sha" "$work_directory/installer" | sha256sum --check --status
chmod u+x "$work_directory/installer"
stage='install'
"$work_directory/installer" --prefix "$work_directory/install" --model "$model" --backend cpu --non-interactive --jobs 2 2>&1 | tee "$evidence/install.log"
stage='doctor'
"$work_directory/install/bin/fabrics" --home "$work_directory/install" doctor 2>&1 | tee "$evidence/doctor.log"
stage='smoke'
python3 "$work_directory/smoke.py" --runtime "$work_directory/install/bin/fabrics" \
    --home "$work_directory/install" --mode balanced --expect-backend cpu \
    --log-directory "$evidence/logs" --report "$evidence/smoke.json" 2>&1 | tee "$evidence/smoke.log"
stage='complete'
