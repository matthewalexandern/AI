# Native platform testing

Mini Fabrics v0.1 keeps orchestration and memory ownership in Go. Its macOS
resource adapter is Swift, using Foundation, Darwin, and Metal. A successful
cross-build does not establish native execution or GPU compatibility. Preserve
the installer checksum, test report, and runtime logs for each tested machine.

| Host | Required native evidence | What the result establishes |
| --- | --- | --- |
| Windows desktop, CPU | Native installer, doctor, CPU smoke | Windows installation, inference, memory and shutdown |
| Ubuntu Hyper-V guest | Native Linux installer, CPU smoke | Ubuntu guest CPU operation |
| RHEL Hyper-V guest | Native Linux installer, CPU smoke | That RHEL version's guest CPU operation |
| Windows physical GPU host | Explicit CUDA or Vulkan smoke with `--require-gpu` | Actual supported GPU offload on that host |
| Apple hardware | Swift helper verification, native installer, CPU and Metal smoke | Native macOS behavior; Metal only when real offload is recorded |

The Hyper-V guests below are CPU test environments. Their success does not
establish CUDA, Vulkan, GPU passthrough, GPU partitioning, or macOS compatibility.
No desktop was accessed and no Windows feature was enabled by creating these
scripts in the Linux cloud workspace. Platform-specific runs remain necessary.
The Linux amd64 release requires **glibc 2.34 or later** and an **x86-64-v2** CPU
because of its AlmaLinux 9 static C++ runtime libraries. Use Ubuntu 22.04 or
later, or RHEL 9 or later. A Hyper-V guest must expose those CPU features;
virtualization does not lower the installer's instruction-set requirement.

For the Windows desktop, run the PowerShell commands below locally, or connect
that desktop as an explicitly configured self-hosted runner. Selecting Hyper-V
in this chat does not connect a desktop terminal to the cloud task. Hyper-V
enablement still runs only on that Windows machine and never restarts it
automatically.

To put the checkout on that desktop, run this in local Windows PowerShell with
Git installed. An existing checkout is reused without changing its branch or
files; an unrelated existing directory is preserved and rejected:

```powershell
$desktopPath = [Environment]::GetFolderPath('Desktop')
if ([string]::IsNullOrWhiteSpace($desktopPath)) { throw 'Windows did not report a Desktop folder.' }
$checkoutPath = Join-Path $desktopPath 'MiniFabrics'
if (Test-Path -LiteralPath $checkoutPath) {
    if (-not (Test-Path -LiteralPath $checkoutPath -PathType Container) -or
        -not (Test-Path -LiteralPath (Join-Path $checkoutPath '.git'))) {
        throw 'The existing Desktop/MiniFabrics path is not a checkout; it was preserved.'
    }
    $origin = & git -C $checkoutPath remote get-url origin
    if ($LASTEXITCODE -ne 0 -or $origin -notin @(
        'https://github.com/matthewalexandern/AI.git',
        'https://github.com/matthewalexandern/AI',
        'git@github.com:matthewalexandern/AI.git')) {
        throw 'The existing checkout does not have the expected AI origin; it was preserved.'
    }
    Write-Host 'Reusing the existing AI checkout; its files and current branch are unchanged.'
} else {
    & git clone --branch mini-fabrics-v0.1.0 --single-branch https://github.com/matthewalexandern/AI.git $checkoutPath
    if ($LASTEXITCODE -ne 0) { throw 'Clone failed. Inspect the Git error before retrying; existing files were not deleted.' }
}
Set-Location -LiteralPath $checkoutPath
```

The cloud terminal cannot execute this against an unconnected Windows desktop.
If an existing checkout predates these scripts, update it deliberately after
preserving any local work; the reuse path above does not pull or reset it.

The standard six-platform release workflow currently builds CPU payloads on all
platforms and additionally includes Metal in macOS packages. It does **not**
bundle CUDA or Vulkan in the Windows/Linux release installers. Build and validate
those GPU variants using the separate manual workflow described below; merely
passing `--backend cuda` to a CPU-only package cannot add CUDA support.

## Run one local admin session

`tools/hyperv/Invoke-NativeMatrix.ps1` runs a closed JSON plan for Windows CPU
verification, optional Hyper-V enablement/VM creation, and Ubuntu/RHEL guest CPU
tests. Launch it from ordinary Windows PowerShell: it validates and prints the
plan, then requests **one UAC approval for the process session**. Every approved
Hyper-V step runs in that elevated session. Starting from an Administrator
terminal requires no additional elevation. The launcher pins the plan, scripts,
installers, and ISO checksums and stops when inputs change.

Download the [v0.1.0-rc.4 release](https://github.com/matthewalexandern/AI/releases/tag/v0.1.0-rc.4)
assets `mini-fabrics-installer-windows-amd64.exe` and
`mini-fabrics-installer-linux-amd64` into `C:\MiniFabrics\release` (or adjust the
plan paths). The example pins their verified RC4 SHA256 values. For another
release or architecture, replace both the filenames and trusted checksums.

Copy the example and edit the local paths, VM names,
guest addresses, ordinary SSH usernames, and verified known-hosts/private-key
file paths. Do not put passwords or key contents in the JSON. Relative paths are
resolved from the plan's directory. The example targets x64 Windows and existing
Ubuntu/RHEL guests; ARM hosts need matching installers, media, and architecture.

```powershell
Copy-Item .\tools\hyperv\native-matrix.example.json .\matrix.local.json
# Edit matrix.local.json for your desktop before either invocation.
.\tools\hyperv\Invoke-NativeMatrix.ps1 -PlanPath .\matrix.local.json -ValidateOnly
.\tools\hyperv\Invoke-NativeMatrix.ps1 -PlanPath .\matrix.local.json
```

Validation checks the closed plan schema, files, and SHA256 values without UAC
or host changes. Running requires Python 3.9+ for Windows evidence and OpenSSH
for guests. Guest OS installation, Python, verified host fingerprints, and
key-based login must already be prepared as described below. `provision` entries
create verified-ISO VMs; creation alone is reported as `prepared`, not a passed
Linux test. Creation does not install the guest OS unattended.

If the guests do not exist yet, save a provisioning-only plan such as this as
`provision.local.json`. Replace the ISO path and publisher SHA256 before
validation. Add a second entry with a different name and verified RHEL media to
prepare RHEL too; the launcher accepts at most two guests per plan.

```json
{
  "schema_version": 1,
  "enable_hyperv": true,
  "report_root": "C:\\MiniFabrics\\test-evidence\\provision-01",
  "windows": null,
  "guests": [],
  "provision": [{
    "distribution": "Ubuntu",
    "iso_path": "C:\\ISOs\\ubuntu-server-amd64.iso",
    "iso_sha256": "REPLACE_WITH_VERIFIED_PUBLISHER_SHA256",
    "name": "MiniFabrics-Ubuntu",
    "vm_root": "D:\\MiniFabricsVMs",
    "switch_name": "Default Switch",
    "memory_bytes": 25769803776,
    "processors": 4,
    "disk_bytes": 68719476736,
    "start": true
  }]
}
```

Run that plan, install the guest OS/Python/SSH interactively, then run the test
plan with a fresh `report_root`. To use one UAC approval across both phases,
open **Windows PowerShell as Administrator once** and keep that window open
while preparing the guests. Invoke both plans there; the launcher reuses the
existing admin token. Launching each phase from a new ordinary terminal can
request a separate approval. A required host reboot still ends the session.

The default test model is your preferred GPT-OSS 20B with a 1,800-second deadline
per turn. A fixed 6 GiB guest cannot fit it; allocate sufficient RAM/disk (for
example, 24 GiB guest RAM plus host headroom) or explicitly choose the small
`qwen2.5-1.5b` validation fixture. Guest tests run sequentially and preserve
existing VM power states. Two running 24 GiB guests still consume memory
simultaneously; size the host or run separate plans with only the intended VM
running. A named test VM must already be a running Generation 2 VM before its
test begins. Hyper-V integration services must report its IPv4 address through
`Get-VMNetworkAdapter`; empty IP results block testing. Prepare the guest's
Hyper-V integration/KVP daemon if needed.

On a smaller host, use a Windows-only plan (`guests: []`) before starting the
large guests, then guest-only plans (`windows: null`) with one VM running at a
time. Keep the same Administrator window open to reuse its approval; start or
stop your own VMs deliberately between plans.

If enabling Hyper-V requires a restart, the report says `blocked_reboot` and
stops without rebooting Windows. An elevated process cannot survive that
restart; a later launch can require another UAC approval. There is no permanent
administrator service or scheduled task. OS install/SSH readiness failures are
reported as blocked or failed and do not become successful platform tests.

`matrix-report.json` records the host OS/CPU, source/input checksums, stages,
completed targets, and failures under the new `report_root`. Existing evidence,
VMs, and disks are preserved. A Hyper-V guest counts as passed only when its SSH
destination matches the named VM's reported IPv4 address and retrieved evidence
matches the requested distribution and architecture. An AlmaLinux result does
not count as actual RHEL. Hyper-V does not establish macOS, physical GPU, or a
different native CPU architecture's compatibility.

The harness also has small protocol checks that do not change Hyper-V state:

```sh
# Linux guest-runner fixtures; these do not perform model inference.
python3 -m unittest discover -s tools/hyperv -p 'test_*.py'
```

```powershell
# PowerShell parser, exit-code/logging, and evidence-preservation checks.
.\tools\hyperv\Test-Harness.ps1
```

Run the PowerShell checks in Windows PowerShell 5.1 as well as modern PowerShell
when validating Windows compatibility. Passing them on Linux does not validate
Windows-only cmdlets or native Windows installation.

## Swift resource adapter

On macOS with Xcode Command Line Tools and Python 3.9+, run from the checkout:

```sh
bash native/macos/test.sh --report swift-resource-report.json
```

This compiles the actual helper with warnings treated as errors, samples it three
times, and compares physical memory against `sysctl hw.memsize`. Native packaging
also compiles `native/macos/SystemInfo.swift` into `bin/fabrics-system-info`.
The JSON contract contains:

| Field | Meaning |
| --- | --- |
| `ram_total_bytes` | Physical memory from Foundation |
| `ram_available_bytes` | Darwin free pages, including speculative pages, plus inactive pages |
| `vram_total_bytes` | Default Metal device's recommended working-set budget |
| `vram_available_bytes` | That budget minus allocations made by the helper process |
| `unified_memory` | Metal reports that CPU and GPU share physical memory |
| `metal_available` | A default Metal device was returned |

Darwin's free-page counter already includes speculative pages; they are not
added again. Inactive pages are an estimate of reclaimable memory. Wired memory
and overlapping purgeable counters are not added to available memory.

Metal does not provide device-wide free VRAM through this API. The helper's
`gpu_memory_is_estimate` and kind fields mark its figures as a working-set budget,
not proof that other processes have left that much memory free. On unified-memory
devices, RAM and Metal budgets describe overlapping resources: never add them.
Model-fit planning must cap the Metal budget by available RAM and keep headroom.
A failed helper invocation must lead to a conservative fallback, not unlimited
resources. Sampling alone does not prove that any particular model will load.

Run macOS integration tests on Apple hardware or macOS CI. For an installed
Metal configuration, require measured offload:

```sh
python3 tools/smoke.py --runtime "$prefix/bin/fabrics" --home "$prefix" \
  --mode balanced --expect-backend metal --require-gpu \
  --log-directory metal-evidence --report metal-evidence.json
```

A virtual macOS CI runner can compile and test Swift even when Metal is absent;
it cannot satisfy the Metal GPU gate without a real supported device.

## Prepare a Windows Hyper-V host

The direct commands in this section and the following VM-creation section are
the manual alternative to the matrix launcher. When using a plan, let that
launcher perform these steps in its existing elevated session.

Use Windows Pro, Enterprise, or Education with hardware virtualization and SLAT
enabled in UEFI/BIOS. Use guest ISOs and installers matching the host's supported
guest architecture; the standard workflow is x64 Windows with x86-64 guests.
This harness uses an existing virtual switch. It does not alter physical network
adapters, firewall rules, firmware, boot settings, or GPU assignments.

Open **PowerShell as Administrator** and inspect readiness first:

```powershell
.\tools\hyperv\Enable-HyperV.ps1
```

If the report requires enablement and you want to make that host change:

```powershell
.\tools\hyperv\Enable-HyperV.ps1 -Enable
```

Enabling uses `-NoRestart`. If `reboot_required` is true, save your work and restart
Windows manually before continuing. The VM script requires an enabled feature
and a running hypervisor, so it will stop if a restart remains outstanding.
`-WhatIf` previews changes; it does not enable the feature or create a VM.

## Create Ubuntu and RHEL CPU guests

Download Ubuntu and RHEL installation media through their official distribution
channels. Obtain the expected SHA256 from the publisher through its trusted
checksum/signature process. RHEL media, entitlements, and any subscription setup
remain the operator's responsibility. The script does not automate RHEL license
acceptance or registration. Do not use a checksum computed only from the same
untrusted ISO as your trusted expected value.

Provide those verified checksum values using local PowerShell variables:

```powershell
# Set $ubuntuPublisherSHA256 and $rhelPublisherSHA256 to independently verified values.
.\tools\hyperv\New-LinuxTestVM.ps1 -Distribution Ubuntu `
  -ISOPath C:\ISOs\ubuntu-server-amd64.iso -ExpectedSHA256 $ubuntuPublisherSHA256 `
  -Name MiniFabrics-Ubuntu -VMRoot D:\MiniFabricsVMs -SwitchName 'Default Switch' -Start

.\tools\hyperv\New-LinuxTestVM.ps1 -Distribution RHEL `
  -ISOPath C:\ISOs\rhel-x86_64-dvd.iso -ExpectedSHA256 $rhelPublisherSHA256 `
  -Name MiniFabrics-RHEL -VMRoot D:\MiniFabricsVMs -SwitchName 'Default Switch' -Start
```

The ISO checksum is checked before attaching media or changing a VM. New guests
use Generation 2, the Microsoft UEFI certificate authority Secure Boot template,
two virtual CPUs, fixed 6 GiB RAM, and a dynamically allocated 48 GiB disk.
Use `-Processors`, `-MemoryBytes`, and `-DiskBytes` to adjust these values. At
creation, a 2 GiB available host RAM reserve is required. Dynamic memory,
automatic checkpoints, and automatic startup are disabled for predictable tests.

If a VM with that name exists, its disks, firmware, networking, and ISO are
preserved; `-Start` can start an existing stopped VM. An existing directory or
VHD without a matching VM is preserved and creation stops. Partial creation
failures leave resources for inspection. There is no automatic deletion or
cleanup of VMs or virtual disks. The existing VM's operating system and resource
configuration are not revalidated by a repeated creation command.

Open each guest console with `vmconnect.exe localhost MiniFabrics-Ubuntu` or
`vmconnect.exe localhost MiniFabrics-RHEL`. Install interactively, then arrange
network access and SSH key authentication for an ordinary guest user. Python
3.9+, Bash, and `sha256sum` are needed for the test harness; native release
installers do not need development compilers. Ubuntu provides Python via
`python3`; select an appropriate Python 3.9+ package on the chosen RHEL version.
Guest prerequisite installation is an explicit operator step.

Check the guest IP in its console or with `Get-VMNetworkAdapter`. Connect once
with OpenSSH, independently verify its displayed fingerprint against the guest's
host public key, and establish the existing key-based login. The test runner uses
`BatchMode=yes` and `StrictHostKeyChecking=yes`. It will fail rather than request
a password or bypass host-key verification. Keep private keys and passwords out
of chat and scripts.

## Run and retain Linux guest evidence

Obtain the matching native Linux installer and its trusted release SHA256.
The runner checks that hash locally and again after transferring to the guest:

```powershell
.\tools\hyperv\Invoke-LinuxGuestTest.ps1 `
  -HostName 192.168.1.120 -UserName tester `
  -Installer .\dist\mini-fabrics-installer-linux-amd64 `
  -InstallerSHA256 $trustedInstallerSHA256 `
  -ReportDirectory .\test-evidence\ubuntu-cpu
```

Repeat with the RHEL guest address and a new report directory. Use `-Port` and
`-IdentityFile` and `-KnownHostsFile` when needed. To record a named Hyper-V test,
also supply `-VMName`, `-ExpectedDistribution Ubuntu|RHEL`, and
`-ExpectedArchitecture x86_64|aarch64`. Without the VM binding it is an SSH Linux
CPU test. Retrieved guest/smoke JSON must prove completion, model/deadline,
native CPU evidence, recall provenance, database reopening, and shutdown; SSH
exit zero alone does not establish a passed guest test.

The script accepts a DNS name or IPv4 address; IPv6
address literals are not supported. `-Model` selects one of the catalog models;
the default smoke fixture is `qwen2.5-1.5b`. The smaller 0.5B model remains an
explicit option, but its weaker recall/assessment can fail the strict smoke
check. Both guest and Windows host runners accept `gpt-oss-20b` and
`gpt-oss-120b`, as well as all three Qwen sizes.

Use `-TurnTimeoutSeconds 900` for a slower CPU run. The default is 300 seconds;
accepted values are integers from 10 through 3600. This limit applies to each
assistant turn, not the whole installation, SSH session, or smoke suite. The
smoke test performs multiple turns. Startup has its separate existing deadline.
The selected model and per-turn timeout are retained in the controller/guest
reports, including failures.

A balanced turn can use four model calls when revision and reassessment are
needed. On slow portable CPU builds, 900 seconds may still be insufficient; use
`-TurnTimeoutSeconds 1800` for a longer full-review budget, or another value up to
3600. This changes the deadline without relaxing assessment or persistence checks.

For a guest with sufficient available RAM and disk, select GPT-OSS explicitly:

```powershell
.\tools\hyperv\Invoke-LinuxGuestTest.ps1 `
  -HostName 192.168.1.120 -UserName tester `
  -Installer .\dist\mini-fabrics-installer-linux-amd64 `
  -InstallerSHA256 $trustedInstallerSHA256 `
  -Model gpt-oss-20b -TurnTimeoutSeconds 900 `
  -ReportDirectory .\test-evidence\ubuntu-gpt20-cpu
```

The default 6 GiB guest is sized for the small smoke fixture, not GPT-OSS. Increase
guest RAM before testing GPT-OSS 20B. GPT-OSS 120B needs substantially more RAM
and a larger disk than the default 48 GiB: its model weights alone are about
59 GiB. The test runners keep installer memory-fit checks enabled and do not
override low-memory failures. Extending the turn deadline does not make a model
fit in available memory.

Each run creates its own `/tmp/mini-fabrics-test-*` directory, runs the complete
installer with `--backend cpu`, runs doctor, then tests inference, isolated
memory, persistence, and shutdown in balanced mode. It retrieves
`install.log`, `doctor.log`, `smoke.log`, `smoke.json`, `guest-report.json`, and
retained runtime logs. `controller-report.json` also records SSH/transfer failures.
Existing evidence directories are rejected rather than overwritten. Installation
files and guest evidence remain until the operator removes that specific test
directory; smoke memories use a separate temporary database.

An early SSH or Python-prerequisite failure may prevent generation of the full
guest report. The controller still records failure and retains available logs.
Run a new test after resolving the reported prerequisite; a missing or partial
report is not a pass.

## Test the Windows physical CPU and GPU host

Use a native release installer for that Windows machine, with its trusted
checksum. Python 3.9+ and the checkout's smoke script are test dependencies.
Run this as an ordinary user; it does not enable Hyper-V or install GPU drivers.
The default smoke model is Qwen2.5 1.5B with a 300-second per-turn deadline.
For a slow GPT-OSS CPU run that needs revision/reassessment, the 900-second
example below can be increased to `-TurnTimeoutSeconds 1800`.

```powershell
.\tools\hyperv\Invoke-WindowsHostTest.ps1 `
  -Installer .\dist\mini-fabrics-installer-windows-amd64.exe `
  -InstallerSHA256 $trustedWindowsInstallerSHA256 -Backend cpu `
  -ReportDirectory .\test-evidence\windows-cpu

.\tools\hyperv\Invoke-WindowsHostTest.ps1 `
  -Installer .\dist\mini-fabrics-installer-windows-amd64.exe `
  -InstallerSHA256 $trustedWindowsInstallerSHA256 -Backend cpu `
  -Model gpt-oss-20b -TurnTimeoutSeconds 900 `
  -ReportDirectory .\test-evidence\windows-gpt20-cpu

.\tools\hyperv\Invoke-WindowsHostTest.ps1 `
  -Installer .\gpu-candidate\mini-fabrics-installer-windows-amd64-cuda.exe `
  -InstallerSHA256 $trustedCUDACandidateSHA256 -Backend cuda `
  -ReportDirectory .\test-evidence\windows-cuda
```

Use `-Backend vulkan` on a supported Vulkan device and payload. CUDA requires
an appropriate NVIDIA driver/device; Vulkan requires a supported driver/device.
The selected backend must exist in the packaged payload. Every run creates its
own installation. Existing user memory is not opened by the smoke test.

GPU success requires all smoke checks plus positive layer offload and matching
accelerator/model-buffer evidence in the actual runtime log. A detected GPU,
available compiler, configured backend, or successful CPU fallback alone is not
a GPU pass. Inspect `smoke.json` and `logs/smoke-*/runtime.log`; the report's
`runtime_log` field identifies the log. `host-report.json` records the requested
backend and whether the GPU evidence gate was required. Keep these artifacts
alongside the release checksum and record the real host/driver in the test record.
`host-report.json` also records the selected model and `turn_timeout_seconds`.
Use `-Model gpt-oss-120b` only on a host with sufficient RAM/VRAM and disk; the same
10–3600 second timeout bounds and memory-fit checks apply.

## Build a complete GPU candidate on its native host

[gpu-smoke.yml](../.github/workflows/gpu-smoke.yml) is a manually dispatched build,
package, and hardware-validation workflow. It replaces the earlier download-only
GPU check. It builds from the checkout selected for dispatch; the native builder
verifies the pinned llama.cpp source SHA256 and uses locked Go dependencies and
the pinned Go toolchain. Dispatch trusted repository revisions on these hosts.

Register the physical GPU machine as a self-hosted GitHub Actions runner for this
repository. Keep its normal OS/architecture labels, and add
`mini-fabrics-gpu-cuda`, `mini-fabrics-gpu-vulkan`, or `mini-fabrics-gpu-metal`
according to the hardware being tested. The workflow schedules only a runner
matching the selected OS, architecture, and backend label, then checks the actual
host architecture before building. A queued job without a matching runner is
waiting for hardware, not a completed platform test.

| Candidate host | Prerequisites already installed on the host |
| --- | --- |
| Windows x64 CUDA | NVIDIA GPU/driver, supported CUDA toolkit including `nvcc`, CMake, Visual Studio C++ Build Tools; toolkit binaries visible to the runner |
| Windows x64/ARM64 Vulkan | Supported GPU/driver, Vulkan SDK including `glslc`, CMake, native Visual Studio tools; ARM64 also requires `clang-cl` |
| Alma/RHEL 9 x64/ARM64 CUDA or Vulkan | Python 3.9+ as `python3`, CMake, C/C++ toolchain and static GCC runtimes, binutils, GPU driver and matching toolkit/SDK |
| macOS x64/ARM64 Metal | Apple hardware with a real Metal device, macOS 13+, Xcode Command Line Tools/Swift, CMake |

The workflow provisions its pinned Go compiler, and Python on Windows/macOS. The
Linux host supplies Python directly, avoiding assumptions that hosted Ubuntu
Python binaries work on RHEL. Linux GPU builds retain the glibc 2.34 baseline. Use the frozen signed AlmaLinux 9.0 compiler repositories shown in the native workflow; newer rolling GCC packages can import backported libc symbols. Strict binary validation catches this;
building on a newer distribution and relabelling it RHEL-compatible is rejected.
CUDA on macOS and Windows ARM64 is not offered. macOS uses the Metal path.

Configure the **runner process environment** variable
`MINI_FABRICS_GPU_LICENSES` with a JSON array of absolute paths to authoritative
redistribution license/notice files for every bundled native dependency. For
CUDA, include the applicable NVIDIA CUDA redistribution terms and notices; for
Vulkan, include the loader and any other redistributed library licenses. Supply
real files from the installed SDK/vendor distribution. Merely supplying a file
does not establish redistribution rights; the host operator must verify coverage
for the actual dependency inventory.

For example, construct the JSON in PowerShell from verified local paths before
starting the runner:

```powershell
# Replace these variables with actual authoritative license-file paths.
$env:MINI_FABRICS_GPU_LICENSES = ConvertTo-Json -Compress -InputObject @($cudaLicensePath, $additionalNoticePath)
```

For a service runner, configure that service's environment and restart the runner
service so it receives the value. A variable in an unrelated terminal is not
injected into an already running service. CUDA/Vulkan builds fail early when
license files are absent. The workflow passes each file through
`--dependency-license`; the resulting payload includes the texts and hashes.
Metal uses system frameworks and needs no additional native dependency license
unless the actual build introduces another redistributable.

In GitHub Actions, select **Build and verify GPU release candidate (self-hosted)**,
choose the real OS, architecture, backend, catalog model, and compiler-job limit,
then run the workflow. GPT-OSS 20B is available when the CPU and GPU capacity estimates allow it; Qwen provides a smaller installation test. It builds both CPU and the selected GPU backend, packages
their binaries and dependency closure into one installer, verifies package
checksums, and performs fresh CPU and GPU installations. Runtime subprocesses
receive ordinary OS executable paths without development SDK paths or dynamic
library-path overrides. Drivers remain a host prerequisite. Both installations
must pass memory/persistence/shutdown smoke; GPU smoke additionally requires
positive offload on the selected backend.

Every completed attempt retains a `gpu-evidence-*` artifact with available
build/install logs, runtime logs, native device information, and the workflow report. Only a
successful attempt uploads `gpu-candidate-*`, containing:

- `mini-fabrics-installer-<os>-<arch>-<backend>[.exe]`, the complete single installer;
- `SHA256SUMS`, native dependency/build provenance, and release provenance;
- `gpu-validation.json`, identifying the tested commit, model, hardware, and
  positive offload evidence.

The backend suffix keeps candidate names distinct from the standard CPU/Metal
release artifacts. This workflow has read-only repository permissions and never
publishes or updates a GitHub release. Review the candidate checksums, dependency
licenses, and recorded hardware coverage before a separate manual prerelease or
release-publication action. It does not insert untested CUDA/Vulkan payloads into
the six-platform release workflow. Successful testing covers the recorded GPU
and driver, not every device supported in principle by that backend.

The workflow definition and local protocol checks are prepared here. A genuine
CUDA, Vulkan, or Metal result requires dispatching onto the corresponding native
hardware; the Linux cloud task has not produced such a result.
