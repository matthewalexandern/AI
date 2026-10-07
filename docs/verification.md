# Verification record

Checked on 7 October 2026 in a Debian 13 Linux amd64 cloud workspace. Cross-compilation, fixture tests, and RHEL-compatible containers have distinct scopes from native desktop, GPU, and RHEL VM execution.

## Current validation

| Check | Observed result |
| --- | --- |
| Go tests, race detector, vet | All packages passed, including real SQLite FTS5, schema migration, bounded cognition, child lifecycle, and CLI/API memory operations. |
| Memory lifecycle | Inspect, delete, export, consistent WAL backup, JSON/SQLite merge and replacement restore passed. Invalid restores leave existing data intact; deleted IDs are not reused. |
| Python tests | 25 packaging/backend-proof tests and four Linux Hyper-V protocol fixtures passed. |
| PowerShell harness | Parser and command/evidence fixtures passed under verified PowerShell 7.6.6 on Linux. This does not execute Windows or Hyper-V cmdlets. |
| Cross builds | Six installer target builds passed. Native payload building refuses cross-labeling another host's llama binary. |
| Historical real CPU inference | Qwen2.5 0.5B and 1.5B produced correct arithmetic and recalled notes with valid assessment JSON on the earlier pinned backend. These are not v0.1 native-payload results. |
| GPT-OSS 20B download | Pinned MXFP4 file downloaded and SHA256 verified; real v0.1 inference validation is in progress. |
| RHEL-compatible baseline | Native v0.6.0 compilation completed in AlmaLinux 9. Payload validation rejected an unexpected glibc 2.35 requirement; diagnosis is in progress. No passing installer is claimed from that attempt. |
| GitHub platform CI | Linux, macOS, and Windows tests passed, including native Swift adapter and Windows PowerShell 5.1 harness checks. Packaged native inference jobs are pending. |

## Release gates

Versioned release assembly requires all six actual-host payloads and native CPU inference checks. Linux payloads require glibc 2.34 or earlier, with additional clean Ubuntu 22.04 and AlmaLinux 9 container installs. macOS verifies the Swift adapter and packages CPU/Metal variants. Windows runs PowerShell harness checks and native CPU inference. GPU evidence requires real offloaded layers and matching model buffers; CPU fallback cannot pass the GPU gate.

The single native installer embeds runtime binaries, application libraries, licenses, and integrity manifests. Go/CMake/compiler tools are unnecessary for ordinary installation. Models remain separate verified downloads. The source installer and optional package-manager dependency installation are explicit development paths.

The real-model check, `tools/smoke.py`, uses an isolated memory home. It checks arithmetic, saved-preference recall and IDs, assessment availability/schema, persisted cognition metadata, reopening SQLite, parent exit, closed listeners, and an empty owned POSIX process group. Retained logs support CPU/GPU backend evidence. Windows does not yet independently enumerate remaining descendants.

## Limits

Schema-valid judgments do not establish assessment quality. Small-model tests sometimes produced incorrect critiques of correct answers; reports distinguish model-quality warnings from runtime failures. Confidence is advisory and does not route cognition.

Native macOS, Windows, ARM hardware, actual RHEL VMs, and CUDA/Metal/Vulkan GPU inference have not been executed in this cloud workspace. The user selected a Windows desktop with Hyper-V; tested protocol scripts are ready, but no desktop terminal is connected. Hyper-V Linux guests test CPU operation, not GPU passthrough. ROCm is absent. Only backend variants explicitly included in an installer are usable.

Forgotten memories are logically deleted; historical backups, disk pages, and copied facts may retain them. Filesystem publication rolls back reported errors, not sudden power loss. Backup files must be protected by the operator.

The visible AI, MCPServers, and Patching repositories contained no larger prior runtime; this implementation is new. Cloud configuration publication and fresh-task restoration are separate from current-instance validation.
