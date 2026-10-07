Mini Fabrics is a Go-owned local runtime with pinned llama.cpp v0.6.0 inference, SQLite FTS5 memory, and bounded adaptive cognition. Native installers embed compiled runtime/backend binaries, application dependencies, licenses, and integrity manifests. No Go, CMake, or compiler is needed for ordinary installation; model weights download separately and GPU drivers remain host prerequisites.

GPT-OSS 20B MXFP4 is the preferred model when it fits the memory estimate. GPT-OSS 120B is an explicit option; Qwen models support smaller systems. Confidence is advisory. Fast answers are explicitly unassessed; balanced/deep assessments remain model judgments, even when valid JSON. A small model may exhaust its assessment token budget; use a stronger model or fast mode. Slow CPU workflows can use `--turn-timeout 15m`.

Memory inspect/forget/export/backup/restore operates without inference. Consistent backups include committed WAL state; restores validate before atomic publication. Schema v1 upgrades preserve history, and ordinary new memories do not reuse forgotten IDs. Deletion is logical and does not erase historical backups or old disk pages.

Release assembly is gated on actual native CPU installation and inference checks on six OS/architecture targets, plus compiler-free Ubuntu22.04 and AlmaLinux9 containers. macOS packages CPU and Metal backends and a Swift hardware adapter; Linux/Windows standard installers contain CPU. CUDA/Vulkan candidates are built and positively tested separately on connected self-hosted GPU hardware.

This release candidate does not establish Metal/CUDA/Vulkan hardware compatibility or successful Windows-desktop/Hyper-V VM execution. Those host checks remain pending. ROCm is not implemented. See docs/native-testing.md for the Windows PowerShell and Hyper-V harnesses and docs/verification.md for the observed test record.

Choose the matching installer for your operating system and architecture, then verify its SHA256 against SHA256SUMS. Native build/source provenance accompanies the assets.
