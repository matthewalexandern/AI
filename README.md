# Mini Fabrics v0.1

A local runtime owned by Go, with llama.cpp inference, SQLite FTS5 memory, and bounded adaptive cognition. On macOS a Swift adapter uses Foundation, Darwin, and Metal for native resource detection. The controller produces visible answers and concise assessments; confidence remains an advisory model self-report.

## Install

Each release installer is **one native executable for its operating system and architecture**. It contains the compiled Go runtime, native llama.cpp backend binaries, required application libraries, dependency licenses, and integrity manifests. Ordinary installation needs no Go, CMake, or C++ compiler. GPU drivers and operating-system libraries remain host prerequisites. Model weights are selected separately and verified when downloaded; they are not embedded in the installer.

Linux amd64 release payloads require an **x86-64-v2 CPU** and glibc 2.34 or later, matching the AlmaLinux/RHEL 9 build baseline. The installer checks every processor's required features before running bundled binaries. Linux ARM64 uses the ARMv8-A baseline. Disabling host-native AVX/FMA does not lower the static C++ libraries' CPU minimum.

Download the matching artifact from [v0.1.0-rc.4](https://github.com/matthewalexandern/AI/releases/tag/v0.1.0-rc.4), check it against `SHA256SUMS`, then run. This candidate passed native CPU checks on all six targets and clean Ubuntu/AlmaLinux installs; physical GPU and Hyper-V acceptance tests remain pending.

```sh
# Ubuntu 22.04+ / RHEL 9-compatible Linux, x86-64-v2 CPU; use linux-arm64 on ARM
chmod +x ./mini-fabrics-installer-linux-amd64
./mini-fabrics-installer-linux-amd64 --doctor
./mini-fabrics-installer-linux-amd64 --model gpt-oss-20b --non-interactive

# macOS 13+, Apple silicon; use darwin-amd64 on Intel
chmod +x ./mini-fabrics-installer-darwin-arm64
./mini-fabrics-installer-darwin-arm64 --model auto --non-interactive
```

```powershell
# Windows / PowerShell; use windows-arm64 on ARM
.\mini-fabrics-installer-windows-amd64.exe --doctor
.\mini-fabrics-installer-windows-amd64.exe --model gpt-oss-20b --non-interactive
```

Run without model options for interactive selection. GPT-OSS 20B MXFP4 is the preferred recommendation when it fits the conservative memory estimate. GPT-OSS 120B MXFP4 is an explicit option. Their downloads are approximately 11.28 GiB and 59.03 GiB. Qwen2.5 Instruct 0.5B, 1.5B, and 3B Q4_K_M provide smaller choices. `--model auto` selects a fitting recommendation; it explains any Qwen fallback. Explicit choices are preserved, and known insufficient RAM/VRAM blocks the download unless `--allow-low-memory` is supplied. Estimates reserve workspace/context headroom and do not guarantee successful allocation.

The installer measures available RAM and, where supported, GPU memory. The Swift adapter labels Metal figures as a working-set estimate and never adds unified GPU memory to RAM. Unknown capacity is reported as unknown. Model revisions and LFS SHA256 values are pinned in [models.json](installer/models.json).

`--backend auto` selects a usable backend present in the package: CUDA with a working NVIDIA driver, Metal on supported Apple hardware, or CPU. Bundled CUDA does not require `nvcc`. `--backend cuda|metal|vulkan` explicitly selects a packaged variant. A package without the requested variant cannot provide it; automatic CPU fallback states why. GPU testing requires actual layer offload, not merely detection. ROCm is not implemented.

Existing GGUF files and custom downloads are supported:

```sh
./mini-fabrics-installer-linux-amd64 --model /absolute/path/model.gguf --non-interactive
./mini-fabrics-installer-linux-amd64 --model-url https://example.com/model.gguf --model-sha256 TRUSTED_SHA256 --non-interactive
```

A local GGUF remains at its supplied path. Downloads use content-addressed filenames. Reinstallation preserves memory and configuration tuning. Publication stages binaries, configuration, licenses, and manifests, and rolls back reported filesystem failures; sudden power loss is outside that guarantee. `--skip-model` preserves the configured model during a runtime update.

## Run and adaptive cognition

Default installation home is the user configuration directory plus `mini-fabrics`: `~/.config/mini-fabrics` on Linux, `~/Library/Application Support/mini-fabrics` on macOS, and `%APPDATA%\mini-fabrics` on Windows. Use installer `--prefix DIRECTORY` to change it.

```sh
fabrics --home /path/to/mini-fabrics doctor
fabrics --home /path/to/mini-fabrics chat --session personal --turn-timeout 30m
fabrics --home /path/to/mini-fabrics ask --mode adaptive --json "Explain SQLite FTS5 briefly."
fabrics --home /path/to/mini-fabrics ask --mode deep --json "Compare these options and review the tradeoffs."
```

```powershell
$prefix = Join-Path $env:APPDATA 'mini-fabrics'
& "$prefix\bin\fabrics.exe" --home $prefix chat --session personal
```

Adaptive scheduling uses request structure, length, recalled evidence, and history. It chooses fast for simple requests, balanced for contextual answers, and deep for analysis or multipart tasks. Explicit `--mode fast|balanced|deep` overrides the choice. `cognition_mode` in `config.json` sets the default.

| Workflow | Inference budget | Assessment |
| --- | --- | --- |
| Fast | One answer call | Explicitly unassessed; the compatibility confidence field is zero, not a measured judgment. |
| Balanced | Answer and assessment, with at most one revision/reassessment; maximum four calls | JSON Schema constrained and application validated. |
| Deep | Outline, answer, assessment, with at most one revision/reassessment; maximum five calls | Same bounded validation. |

JSON results include `assessed` and the scheduling `decision`: requested/selected modes, reason, call budget, actual calls, phases, and duration. Episodes persist the same metadata. Model confidence never selects the workflow or triggers escalation. Failed, canceled, truncated, or malformed turns save no partial conversation. Ctrl+C stops the owned inference child; Unix also handles SIGTERM. `--startup-timeout 240s` permits slower model loading. Turns default to five minutes; `--turn-timeout 30m` allows slower CPU full review (bounded from 10 seconds to 60 minutes). GPT-OSS 20B balanced arithmetic and recall took approximately 7.7 and 14.7 minutes on the portable four-core cloud build. GPU validation is separate.

## Memory management

Memory commands work without loading a model:

```sh
fabrics --home /path/to/mini-fabrics remember "I prefer concise answers and metric units."
fabrics --home /path/to/mini-fabrics recall "metric units"
fabrics --home /path/to/mini-fabrics episodes --session personal
fabrics --home /path/to/mini-fabrics memory list --limit 20
fabrics --home /path/to/mini-fabrics memory inspect 1
fabrics --home /path/to/mini-fabrics memory forget 1
fabrics --home /path/to/mini-fabrics memory export memories.json
fabrics --home /path/to/mini-fabrics memory backup memories.sqlite
fabrics --home /path/to/mini-fabrics memory restore memories.json
fabrics --home /path/to/mini-fabrics memory restore --replace memories.sqlite
```

Forgetting a note removes that record and its search entry. Forgetting an episode deletes its associated turn, history, and episode, and prunes obsolete recall IDs from surviving provenance. Ordinary new memories do not reuse forgotten IDs. This is logical deletion: historical backups, old disk pages, and separately stored copies of facts are outside its scope.

Export preserves complete notes, turns, messages, cognition metadata, and provenance. Backup uses a consistent SQLite snapshot, including committed WAL data. Neither overwrites an existing file. Restore accepts JSON exports or SQLite backups, validates them in staging, and commits atomically. Default restore merges and remaps IDs; `--replace` replaces local memory and preserves snapshot IDs. Schema v1 databases migrate to v2 without discarding existing turns.

History is session-specific; searchable memories are shared within a home. Use separate homes for separate memory stores. Storage is local and unencrypted; retrieval is lexical FTS5/BM25. Retrieved content is quoted as untrusted reference data.

## Local API

```sh
fabrics --home /path/to/mini-fabrics serve --listen 127.0.0.1:8080 --mode adaptive
curl -fsS http://127.0.0.1:8080/health
curl -fsS http://127.0.0.1:8080/v1/chat -H 'Content-Type: application/json' \
  -d '{"session":"personal","input":"What do you remember about my preferences?"}'
```

Additional endpoints: `POST /v1/memory`, `GET /v1/memory?q=...`, `GET /v1/memory/ID`, `DELETE /v1/memory/ID`, `GET /v1/memories?limit=20&offset=0`, and `GET /v1/episodes?session=...`. Both servers bind to loopback; the API rejects browser-origin requests and has no user authentication. The runtime has no external action or tool execution interface.

## Develop, build, and verify

Development uses Go 1.24+ and Python 3.9+. Release builds pin Go 1.27.1 and llama.cpp v0.6.0, commit `8345f333951c661d166b00e6f9362e553768f292`, with source archive SHA256 `84050e783dbd0c4a4678f90f253b0881df04e90a98c20f73ae3709751c4a4850`. Runtime SQLite is pure Go and needs no system SQLite or CGO.

```sh
go mod download
go test ./...
go test -race ./...
go vet ./...
python3 -m unittest discover -s tools -p 'test_*.py'
go build -trimpath -o bin/fabrics ./cmd/fabrics

# Native payloads must be built on their actual target host.
# Linux release builds use the frozen signed AlmaLinux 9.0 vault and require libstdc++-static from CRB; rolling Alma9 toolchains can require newer libc symbols.
python3 tools/native_payload.py --output native-payloads --backends cpu
python3 tools/package.py --payloads native-payloads --targets linux-amd64 --output dist/linux-only
```

Complete packaging requires every requested target's verified native payload; it refuses missing payloads or mixed target generations. Native payloads carry dependency licenses, hashes, backend paths, compiler/platform provenance, and the Linux glibc requirement. Packaging uses normalized source paths, fixed timestamps, and deterministic archives. Checksums and provenance accompany versioned releases.

Development source installation is explicit and requires CMake and a C++ compiler:

```sh
python3 tools/package.py --source-installer --targets linux-amd64 --output dist/source
./dist/source/mini-fabrics-installer-linux-amd64 --build-from-source --install-deps \
  --model gpt-oss-20b --non-interactive
# Or run the single Go source against your checkout:
go run installer/install.go --source . --doctor
```

`--install-deps` opts into detected package-manager prerequisite installation. It is unnecessary for native release installers. Doctor only reports and never installs. Supported managers are apt/apt-get, dnf/yum/microdnf, Homebrew, winget, Chocolatey, and Scoop; unknown managers receive manual guidance.

After installing a model, retain real inference and process evidence:

```sh
python3 tools/smoke.py --runtime /path/to/mini-fabrics/bin/fabrics \
  --home /path/to/mini-fabrics --mode balanced --expect-backend cpu \
  --log-directory test-evidence --report test-evidence/report.json
```

GPU runs require `--expect-backend cuda|metal|vulkan --require-gpu`, positive offloaded layers, and matching GPU model buffers in the real log. See [native testing and Hyper-V](docs/native-testing.md), [architecture](docs/architecture.md), and [verification](docs/verification.md). Small-model critiques may be inaccurate despite valid JSON; smoke records model-quality warnings separately from runtime failures. The available earlier GitHub repositories contained no larger runtime to inherit; Mini Fabrics is a new implementation.
