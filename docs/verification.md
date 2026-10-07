# Verification record

Checked on 7 October 2026 in a Debian 13 Linux amd64 cloud workspace. Cross-compilation, fixture tests, and RHEL-compatible containers have distinct scopes from native desktop, GPU, and RHEL VM execution.

## Current validation

| Check | Observed result |
| --- | --- |
| Go tests, race detector, vet | All packages passed, including real SQLite FTS5, schema migration, bounded cognition, child lifecycle, and CLI/API memory operations. |
| Memory lifecycle | Inspect, delete, export, consistent WAL backup, JSON/SQLite merge and replacement restore passed. Invalid restores leave existing data intact; deleted IDs are not reused. |
| Python tests | 50 packaging/backend-proof/artifact-reuse tests and nine Linux Hyper-V protocol fixtures passed. They validate bounded binary ELF CPU-property parsing, checksums, unchanged construction inputs, original native job evidence, bounded timeouts, GPT-OSS selection, command forwarding, and retained evidence; they do not run a VM. The ELF parser also passed on the actual frozen AlmaLinux 9.0/Python 3.9 toolchain. |
| PowerShell harness | Parser and command/evidence fixtures passed under verified PowerShell 7.6.6 on Linux, including eight controller-forwarding cases and four rejected timeout values. This does not execute Windows or Hyper-V cmdlets. |
| Cross builds | Six installer target builds passed. Native payload building refuses cross-labeling another host's llama binary. |
| Native Qwen 1.5B CPU inference | The pinned v0.6.0 payload passed balanced arithmetic, FTS5 preference recall, strict assessment parsing, persisted cognition/provenance, reopening, and shutdown. Arithmetic and recall took approximately 65 and 62 seconds on this cloud CPU. Critiques were sometimes incorrect despite correct answers. |
| Native Qwen 0.5B CPU inference | Arithmetic passed, but the recall assessment exhausted its token budget and was correctly rejected. Native release gates therefore use Qwen 1.5B. |
| Native GPT-OSS 20B CPU inference | The pinned MXFP4 file was SHA256 verified. Balanced arithmetic and preference recall passed with correct answers, valid assessments, four-call revision/reassessment, persisted cognition/recall IDs, database reopening, and owned-process shutdown. Arithmetic took 463 seconds and recall 883 seconds; CPU full review should allow a 30-minute turn deadline on this host. No OOM events occurred. |
| Explicit fast GPT-OSS check | Arithmetic passed, but the unrevised preference answer failed the Neptune recall assertion. Its visible text was not retained, so the precise cause is unconfirmed. Fast and balanced use the same sampled first-answer path; the successful balanced recall required revision. Future smoke runs retain visible API results before assertions. |
| Reusable cloud setup | The complete setup script passed installation, development build, Go tests, Python tests, and balanced Qwen 1.5B inference/recall/persistence/reopening/shutdown against the installed pinned backend. The fixture is checksum-verified and isolated; the main home keeps GPT-OSS and its memory. Small-model critiques still showed inaccuracies despite correct answers. |
| RHEL-compatible baseline | The signed frozen AlmaLinux 9.0 compiler produced the pinned native payload with a maximum glibc 2.34 requirement. Its only Linux shared dependencies are libc, libm, and the system loader. ELF notes declare x86-64-v2, including the distribution's static C++ libraries. Compiler-free installation and native CPU inference passed in this Debian workspace; actual RHEL VM execution is pending. |
| Repeated native installation and packaging | Installation with an empty PATH and a repeated update passed. The corrected installer verified x86-64-v2 on every Linux processor. Two builds of the same payload produced identical installer bytes (local SHA256 `aadb7caf3c1282aaf30368fc58e2be4960c3c2838d8de95b444ab675035efdb4`). This verifies packaging reproducibility, not independent reproducibility of every native compiler build. |
| GitHub platform CI | Linux, macOS, and Windows source tests passed, including the native Swift adapter and Windows PowerShell 5.1 harness. RC3 run [37684833788](https://github.com/matthewalexandern/AI/actions/runs/37684833788) passed native CPU installation, arithmetic, memory recall, persistence, reopening, and shutdown on all six targets: Linux, macOS, and Windows, each amd64/arm64. Its clean Linux jobs failed before installation because artifacts were downloaded into an extra directory; release assembly was blocked. The consumer paths are corrected and fresh clean gates remain required. |

## Release gates

Versioned release assembly requires all six actual-host payloads and native CPU inference checks. Linux payloads must import no glibc symbols newer than 2.34; Linux amd64 additionally declares and checks x86-64-v2. Additional clean Ubuntu 22.04 and AlmaLinux 9 containers test installation. macOS verifies the Swift adapter and packages CPU/Metal variants. Windows runs PowerShell harness checks and native CPU inference. GPU evidence requires real offloaded layers and matching model buffers; CPU fallback cannot pass the GPU gate.

The single native installer embeds runtime binaries, application libraries, licenses, and integrity manifests. Go/CMake/compiler tools are unnecessary for ordinary installation. Models remain separate verified downloads. The source installer and optional package-manager dependency installation are explicit development paths.

Release assets include the six original native manifests, installer checksums, packaging provenance, and any verified native-run reuse report. These files are checksummed before GitHub build attestation and publication. Apple notarization and Windows Authenticode signing are not configured. Download quarantine, SmartScreen, and other desktop approval behavior are outside the GitHub runner checks and remain part of native user acceptance testing.

The real-model check, `tools/smoke.py`, uses an isolated memory home. It checks arithmetic, saved-preference recall and IDs, assessment availability/schema, persisted cognition metadata, reopening SQLite, parent exit, closed listeners, and an empty owned POSIX process group. Retained logs support CPU/GPU backend evidence. Visible API results are saved before assertions so failed answers remain reviewable after the temporary database is removed; raw reasoning fields are excluded. Windows does not yet independently enumerate remaining descendants.

## Limits

Schema-valid judgments do not establish assessment quality. Small-model tests sometimes produced incorrect critiques of correct answers; reports distinguish model-quality warnings from runtime failures. Confidence is advisory and does not route cognition.

Native macOS, Windows, and ARM CPU checks ran on GitHub's corresponding native hosts. Actual RHEL VMs and CUDA/Metal/Vulkan GPU inference remain untested. The user selected a Windows desktop with Hyper-V; tested protocol scripts are ready, but no desktop terminal is connected. Hyper-V Linux guests test CPU operation, not GPU passthrough. ROCm is absent. Only backend variants explicitly included in an installer are usable.

Forgotten memories are logically deleted; historical backups, disk pages, and copied facts may retain them. Filesystem publication rolls back reported errors, not sudden power loss. Backup files must be protected by the operator.

The visible AI, MCPServers, and Patching repositories contained no larger prior runtime; this implementation is new. Cloud configuration publication and fresh-task restoration are separate from current-instance validation.
