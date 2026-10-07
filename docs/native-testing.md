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
`-IdentityFile` when needed. The script accepts a DNS name or IPv4 address; IPv6
address literals are not supported. `-Model` selects one of the catalog models;
the default is `qwen2.5-0.5b`.

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

```powershell
.\tools\hyperv\Invoke-WindowsHostTest.ps1 `
  -Installer .\dist\mini-fabrics-installer-windows-amd64.exe `
  -InstallerSHA256 $trustedWindowsInstallerSHA256 -Backend cpu `
  -ReportDirectory .\test-evidence\windows-cpu

.\tools\hyperv\Invoke-WindowsHostTest.ps1 `
  -Installer .\dist\mini-fabrics-installer-windows-amd64.exe `
  -InstallerSHA256 $trustedWindowsInstallerSHA256 -Backend cuda `
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
