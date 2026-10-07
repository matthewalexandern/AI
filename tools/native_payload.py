#!/usr/bin/env python3
"""Build a verified native payload on its actual target OS/architecture.

Release Linux payloads must be built on the Alma/RHEL 9 baseline (glibc 2.34).
This tool rejects a newer ELF requirement rather than relabelling it portable.
"""
import argparse
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tarfile
import tempfile
import urllib.request
import gzip
import importlib.util
from contextlib import contextmanager

ROOT = Path(__file__).resolve().parents[1]
VERSION = "0.1.0"
LLAMA_COMMIT = "8345f333951c661d166b00e6f9362e553768f292"
LLAMA_SHA256 = "84050e783dbd0c4a4678f90f253b0881df04e90a98c20f73ae3709751c4a4850"
LLAMA_URL = "https://codeload.github.com/ggml-org/llama.cpp/tar.gz/" + LLAMA_COMMIT
GO_VERSION = "go1.27.1"
SYSTEM_LINUX = re.compile(r"^(?:linux-vdso|ld-linux|lib(?:c|m|pthread|dl|rt|resolv|util|anl)\.so)")
DRIVER_LINUX = re.compile(r"^lib(?:cuda|nvidia-ml)\.so")
SYSTEM_WINDOWS = {x + ".DLL" for x in "KERNEL32 NTDLL USER32 ADVAPI32 WS2_32 SHELL32 OLE32 OLEAUT32 CRYPT32 BCRYPT VERSION SHLWAPI SECUR32 IPHLPAPI POWRPROF PSAPI GDI32 UCRTBASE COMDLG32 COMCTL32 WINMM WLDAP32 NORMALIZ SETUPAPI CFGMGR32 RPCRT4 IMM32 DWMAPI MSIMG32 WINHTTP WININET WTSAPI32".split()}


def sha256(path):
    h = hashlib.sha256()
    with Path(path).open("rb") as f:
        for block in iter(lambda: f.read(1024 * 1024), b""):
            h.update(block)
    return h.hexdigest()


def host_target():
    systems = {"Linux": "linux", "Darwin": "darwin", "Windows": "windows"}
    arches = {"x86_64": "amd64", "AMD64": "amd64", "aarch64": "arm64", "arm64": "arm64", "ARM64": "arm64"}
    if platform.system() not in systems or platform.machine() not in arches:
        raise ValueError("Unsupported native build host")
    return systems[platform.system()] + "-" + arches[platform.machine()]


def run(command, **kwargs):
    print("Running:", " ".join(str(x) for x in command), flush=True)
    return subprocess.run([str(x) for x in command], check=True, **kwargs)


def capture(command):
    return subprocess.check_output([str(x) for x in command], text=True, stderr=subprocess.STDOUT).strip()


class HTTPSRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, response, code, message, headers, new_url):
        if not new_url.lower().startswith("https://"):
            raise ValueError("Native source download redirected away from HTTPS")
        return super().redirect_request(request, response, code, message, headers, new_url)


def source_archive(cache, supplied):
    cache.mkdir(parents=True, exist_ok=True)
    archive = Path(supplied) if supplied else cache / ("llama-" + LLAMA_COMMIT + ".tar.gz")
    if archive.exists() and sha256(archive) == LLAMA_SHA256:
        return archive
    if supplied:
        raise ValueError("Supplied llama source archive failed pinned SHA256 verification")
    with tempfile.NamedTemporaryFile(dir=cache, prefix=".download-", delete=False) as f:
        temporary = Path(f.name)
        try:
            request = urllib.request.Request(LLAMA_URL, headers={"User-Agent": "Mini-Fabrics-Native-Builder/0.1.0"})
            with urllib.request.build_opener(HTTPSRedirect()).open(request, timeout=120) as response:
                if response.url.split(":", 1)[0] != "https":
                    raise ValueError("Source download redirected away from HTTPS")
                shutil.copyfileobj(response, f)
            f.flush()
            if sha256(temporary) != LLAMA_SHA256:
                raise ValueError("Downloaded llama source failed pinned SHA256 verification")
            os.replace(temporary, archive)
        finally:
            temporary.unlink(missing_ok=True)
    return archive


def extract_source(archive, destination):
    destination.mkdir()
    with tarfile.open(archive, "r:gz") as content:
        for entry in content:
            parts = entry.name.split("/")[1:]
            if not parts or parts == [""]:
                continue
            if any(x in ("", ".", "..") for x in parts) or "\\" in entry.name or ":" in entry.name:
                raise ValueError("Unsafe pinned source archive path")
            path = destination.joinpath(*parts)
            if entry.isdir():
                path.mkdir(parents=True, exist_ok=True)
            elif entry.isfile():
                path.parent.mkdir(parents=True, exist_ok=True)
                with path.open("xb") as f:
                    shutil.copyfileobj(content.extractfile(entry), f)
            else:
                raise ValueError("Pinned source archive contains unsupported links or special files")


def linux_dependencies(binary):
    text = capture(["ldd", binary])
    result = []
    for line in text.splitlines():
        line = line.strip()
        if not line or "statically linked" in line:
            continue
        name = line.split()[0]
        if SYSTEM_LINUX.match(Path(name).name) or DRIVER_LINUX.match(name):
            continue
        if "not found" in line:
            raise ValueError("Unresolved native runtime dependency: " + name)
        match = re.search(r"=>\s+(/\S+)", line)
        if not match:
            raise ValueError("Cannot identify native dependency: " + line)
        result.append((name, Path(match.group(1))))
    return result


def windows_dependencies(binary):
    text = capture(["dumpbin", "/dependents", binary])
    result = []
    for name in re.findall(r"^\s+([\w.\-]+\.dll)\s*$", text, re.I | re.M):
        upper = name.upper()
        if upper in SYSTEM_WINDOWS or upper.startswith(("API-MS-WIN-", "EXT-MS-WIN-")):
            continue
        if upper in ("NVCUDA.DLL", "NVML.DLL"):
            continue  # vendor driver, not an application redistributable
        candidates = [binary.parent / name]
        candidates += [Path(directory) / name for directory in os.environ.get("PATH", "").split(os.pathsep)]
        if os.environ.get("CUDA_PATH"):
            candidates.append(Path(os.environ["CUDA_PATH"]) / "bin" / name)
        found = next((p for p in candidates if p.is_file()), None)
        if not found:
            raise ValueError("Unresolved native DLL: " + name)
        result.append((name, found))
    return result


def mac_dependencies(binary):
    result = []
    for line in capture(["otool", "-L", binary]).splitlines()[1:]:
        dependency = line.strip().split(" (", 1)[0]
        if dependency.startswith(("/usr/lib/", "/System/Library/")):
            continue
        if dependency.startswith("@loader_path/"):
            source = binary.parent / dependency[len("@loader_path/"):]
        elif dependency.startswith("@executable_path/"):
            source = binary.parent / dependency[len("@executable_path/"):]
        elif dependency.startswith("@rpath/"):
            name = dependency[len("@rpath/"):]
            candidates = [binary.parent / name, Path("/opt/homebrew/lib") / name, Path("/usr/local/lib") / name]
            source = next((p for p in candidates if p.is_file()), None)
        else:
            source = Path(dependency)
        if source is None or not source.is_file():
            raise ValueError("Unresolved native macOS dependency: " + dependency)
        result.append((dependency, source))
    return result


def dependency_closure(binary, destination, goos):
    resolver = {"linux": linux_dependencies, "darwin": mac_dependencies, "windows": windows_dependencies}[goos]
    pending, visited, copied = [binary], set(), []
    while pending:
        current = pending.pop()
        if current.resolve() in visited:
            continue
        visited.add(current.resolve())
        for original, source in resolver(current):
            name = source.name if goos == "darwin" else original
            target = destination / name
            if not target.exists():
                shutil.copy2(source, target)
                copied.append(name)
                pending.append(target)
            elif sha256(target) != sha256(source):
                raise ValueError("Conflicting dependency basename: " + name)
            if goos == "darwin":
                run(["install_name_tool", "-change", original, "@loader_path/" + name, current])
                run(["install_name_tool", "-id", "@loader_path/" + name, target])
    if goos == "darwin":
        for artifact in [binary] + [destination / x for x in copied]:
            run(["codesign", "--force", "--sign", "-", artifact])
    return copied


def glibc_requirement(files):
    versions = set()
    for file in files:
        text = capture(["objdump", "-T", file])
        # Versioned definitions in a bundled library are not loader requirements.
        # Only undefined imports require a symbol supplied by the host's glibc.
        for line in text.splitlines():
            if "*UND*" in line:
                versions.update(re.findall(r"GLIBC_([0-9]+(?:\.[0-9]+)+)", line))
    return max(versions, key=lambda v: tuple(map(int, v.split(".")))) if versions else None


@contextmanager
def build_workspace(parent, keep=False):
    stage = Path(tempfile.mkdtemp(prefix=".native-", dir=parent))
    print("Native build workspace:", stage, flush=True)
    try:
        yield stage
    finally:
        if keep:
            print("Preserved native build workspace:", stage, flush=True)
        else:
            shutil.rmtree(stage)


def deterministic_archive(directory):
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.PAX_FORMAT) as archive:
        for path in sorted(directory.rglob("*")):
            if path.is_symlink():
                raise ValueError("Native payload cannot contain symlinks")
            if not path.is_file():
                continue
            entry = tarfile.TarInfo("native/" + path.relative_to(directory).as_posix())
            entry.size, entry.mtime = path.stat().st_size, 0
            entry.mode = 0o755 if os.access(path, os.X_OK) and "licenses" not in path.parts else 0o644
            with path.open("rb") as content:
                archive.addfile(entry, content)
    compressed = io.BytesIO()
    with gzip.GzipFile(fileobj=compressed, mode="wb", filename="", mtime=0) as f:
        f.write(raw.getvalue())
    return compressed.getvalue()


def copy_licenses(payload, source, go, extras):
    licenses = payload / "licenses"
    licenses.mkdir()
    shutil.copy2(ROOT / "LICENSE", licenses / "mini-fabrics-LICENSE")
    shutil.copy2(Path(capture([go, "env", "GOROOT"])) / "LICENSE", licenses / "Go-LICENSE")
    shutil.copy2(source / "LICENSE", licenses / "llama.cpp-LICENSE")
    # Inventory Go module license texts from the exact locked build cache.
    text = capture([go, "list", "-mod=readonly", "-m", "-json", "all"])
    decoder, offset = json.JSONDecoder(), 0
    while text[offset:].strip():
        offset += len(text[offset:]) - len(text[offset:].lstrip())
        module, count = decoder.raw_decode(text[offset:]); offset += count
        directory = module.get("Dir")
        if not directory or module.get("Main"):
            continue
        found = []
        for candidate in Path(directory).iterdir():
            if candidate.is_file() and candidate.name.upper().startswith(("LICENSE", "COPYING", "NOTICE", "UNLICENSE")):
                found.append(candidate)
        if not found:
            raise ValueError("Missing license text for locked Go dependency " + module["Path"])
        for file in found:
            shutil.copy2(file, licenses / (module["Path"].replace("/", "_") + "-" + file.name))
    for index, file in enumerate(extras):
        shutil.copy2(file, licenses / ("native-dependency-" + str(index) + "-" + Path(file).name))
    # GCC static runtime exception and notices are part of the redistribution.
    if platform.system() == "Linux":
        for path in [Path("/usr/share/doc/gcc-14-base/copyright"), Path("/usr/share/licenses/gcc/COPYING.RUNTIME"), Path("/usr/share/licenses/libstdc++/COPYING.RUNTIME")]:
            if path.is_file():
                shutil.copy2(path, licenses / ("gcc-" + path.parent.name + "-" + path.name))


def compiler_provenance(builddir):
    cache = (builddir / "CMakeCache.txt").read_text()
    match = re.search(r"^CMAKE_CXX_COMPILER:FILEPATH=(.+)$", cache, re.M)
    if not match:
        raise ValueError("CMake did not record a native C++ compiler")
    compiler = match.group(1)
    command = [compiler] if Path(compiler).name.lower() in ("cl", "cl.exe") else [compiler, "--version"]
    result = subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    text = result.stdout.strip()
    if not text:
        raise ValueError("Cannot establish native compiler provenance")
    return {"executable": compiler, "version_output": text, "cache_sha256": hashlib.sha256(cache.replace(str(builddir), "<BUILD>").replace(str(builddir.parent), "<BUILD_ROOT>").encode()).hexdigest()}


def build(args):
    target = args.target or host_target()
    if target != host_target():
        raise ValueError("Native llama payloads must be built/tested on their actual host target; cross-labelled payloads are rejected")
    goos, goarch = target.split("-")
    go = args.go or shutil.which("go")
    if not go or GO_VERSION not in capture([go, "version"]).split():
        raise ValueError("Release native builds require the pinned Go 1.27.1 toolchain")
    args.output.mkdir(parents=True, exist_ok=True)
    archive = source_archive(args.cache, args.llama_archive)
    with build_workspace(args.output.parent, getattr(args, "keep_build", False)) as stage:
        source, payload = stage / "llama", stage / "native"
        extract_source(archive, source)
        (payload / "bin").mkdir(parents=True)
        suffix = ".exe" if goos == "windows" else ""
        environment = dict(os.environ, CGO_ENABLED="0", GOOS=goos, GOARCH=goarch, GOAMD64="v1", GOARM64="v8.0", GOTOOLCHAIN="local", SOURCE_DATE_EPOCH="0")
        run([go, "build", "-mod=readonly", "-buildvcs=false", "-trimpath", "-ldflags=-s -w -X main.version=" + VERSION, "-o", payload / "bin" / ("fabrics" + suffix), "./cmd/fabrics"], cwd=ROOT, env=environment)
        backends = list(dict.fromkeys(args.backends.split(",")))
        if "cpu" not in backends or any(x not in ("cpu", "metal", "cuda", "vulkan") for x in backends):
            raise ValueError("Every native payload requires CPU; optional backends: metal,cuda,vulkan")
        if "metal" in backends and goos != "darwin":
            raise ValueError("Metal native payloads require macOS")
        paths, dependencies, compiler_records, build_flags, glibc = {}, {}, {}, {}, None
        for backend in backends:
            builddir = stage / ("build-" + backend)
            flags = ["-DCMAKE_BUILD_TYPE=Release", "-DBUILD_SHARED_LIBS=OFF", "-DGGML_STATIC=OFF", "-DGGML_NATIVE=OFF", "-DGGML_OPENMP=OFF", "-DGGML_AVX=OFF", "-DGGML_AVX2=OFF", "-DGGML_FMA=OFF", "-DGGML_F16C=OFF", "-DLLAMA_BUILD_SERVER=ON", "-DLLAMA_BUILD_TESTS=OFF", "-DLLAMA_OPENSSL=OFF", "-DLLAMA_BUILD_UI=OFF", "-DLLAMA_USE_PREBUILT_UI=OFF", "-DLLAMA_SUBPROCESS=OFF", "-DGGML_CUDA=OFF", "-DGGML_METAL=OFF", "-DGGML_VULKAN=OFF", "-DCMAKE_BUILD_WITH_INSTALL_RPATH=ON", "-DCMAKE_INSTALL_RPATH=$ORIGIN", "-DCMAKE_INSTALL_RPATH_USE_LINK_PATH=OFF"]
            if backend != "cpu":
                flags.append("-DGGML_" + backend.upper() + "=ON")
            if backend == "metal":
                flags.append("-DGGML_METAL_EMBED_LIBRARY=ON")
            if goarch == "arm64":
                flags.append("-DGGML_CPU_ARM_ARCH=armv8-a")
            if goos == "darwin":
                flags.append("-DCMAKE_OSX_DEPLOYMENT_TARGET=" + ("13.0"))
            if goos == "linux":
                flags.append("-DCMAKE_EXE_LINKER_FLAGS=-static-libstdc++ -static-libgcc -Wl,--disable-new-dtags")
            if goos == "windows":
                flags += ["-DCMAKE_POLICY_DEFAULT_CMP0091=NEW", "-DCMAKE_MSVC_RUNTIME_LIBRARY=MultiThreaded"]
                if goarch == "arm64":
                    if not shutil.which("clang-cl"):
                        raise ValueError("Portable Windows ARM64 llama builds require clang-cl; MSVC ARM64 forces host-native ISA")
                    flags += ["-DCMAKE_C_COMPILER=clang-cl", "-DCMAKE_CXX_COMPILER=clang-cl", "-DCMAKE_C_COMPILER_TARGET=aarch64-pc-windows-msvc", "-DCMAKE_CXX_COMPILER_TARGET=aarch64-pc-windows-msvc"]
                if shutil.which("ninja"):
                    flags += ["-G", "Ninja"]
            if goos != "windows":
                flags += ["-DCMAKE_C_FLAGS=-ffile-prefix-map=" + str(stage) + "=/build", "-DCMAKE_CXX_FLAGS=-ffile-prefix-map=" + str(stage) + "=/build"]
            compiler_env = dict(os.environ, SOURCE_DATE_EPOCH="0")
            for variable in ("CFLAGS", "CXXFLAGS", "LDFLAGS"):
                compiler_env.pop(variable, None)
            run([args.cmake, "-S", source, "-B", builddir] + flags, env=compiler_env)
            compiler_records[backend] = compiler_provenance(builddir)
            build_flags[backend] = [flag.replace(str(stage), "/build") for flag in flags]
            run([args.cmake, "--build", builddir, "--config", "Release", "--target", "llama-server", "--parallel", str(args.jobs)])
            built = builddir / "bin" / ("llama-server" + suffix)
            if not built.is_file():
                built = builddir / "bin" / "Release" / ("llama-server" + suffix)
            dest = payload / "backends" / backend
            dest.mkdir(parents=True)
            binary = dest / built.name
            shutil.copy2(built, binary)
            dependencies[backend] = dependency_closure(binary, dest, goos)
            if dependencies[backend] and not args.dependency_license:
                raise ValueError("Redistributed native dependencies require --dependency-license authoritative license texts")
            paths[backend] = binary.relative_to(payload).as_posix()
            if goos == "linux":
                required = glibc_requirement([p for p in dest.iterdir() if p.is_file()])
                if required and tuple(map(int, required.split("."))) > tuple(map(int, args.max_glibc.split("."))):
                    raise ValueError("Payload requires glibc " + required + "; rebuild on Alma/RHEL 9 baseline rather than claiming glibc " + args.max_glibc)
                if required and (not glibc or tuple(map(int, required.split("."))) > tuple(map(int, glibc.split(".")))):
                    glibc = required
            run([binary, "--version"])
        if goos == "darwin":
            helper = ROOT / "native/macos/SystemInfo.swift"
            if not helper.is_file():
                raise ValueError("Release macOS payload requires the native RAM/VRAM helper source")
            swift_env = dict(os.environ, MACOSX_DEPLOYMENT_TARGET="13.0")
            run(["xcrun", "swiftc", "-O", "-framework", "Foundation", "-framework", "Metal", helper, "-o", payload / "bin/fabrics-system-info"], env=swift_env)
            json.loads(capture([payload / "bin/fabrics-system-info"]))
        copy_licenses(payload, source, go, args.dependency_license)
        files = {p.relative_to(payload).as_posix(): sha256(p) for p in sorted(payload.rglob("*")) if p.is_file()}
        archive_data = deterministic_archive(payload)
        manifest = {"format_version": 1, "version": VERSION, "platform": goos + "/" + goarch, "backends": backends, "default_backend": "cpu", "backend_paths": paths, "files": files, "archive_sha256": hashlib.sha256(archive_data).hexdigest(), "llama_commit": LLAMA_COMMIT, "llama_source_sha256": LLAMA_SHA256, "go_version": capture([go, "version"]), "go_sum_sha256": sha256(ROOT / "go.sum"), "go_mod_sha256": sha256(ROOT / "go.mod"), "runtime_source_files": {p.relative_to(ROOT).as_posix(): sha256(p) for d in (ROOT / "cmd/fabrics", ROOT / "internal") for p in sorted(d.rglob("*.go")) if not p.name.endswith("_test.go")}, "model_catalog_sha256": sha256(ROOT / "installer/models.json"), "cpu_isa": "portable baseline; no host-native AVX/FMA", "dependencies": dependencies, "glibc_minimum": glibc, "macos_minimum": "13.0" if goos == "darwin" else None, "cmake_version": capture([args.cmake, "--version"]).splitlines()[0], "compilers": compiler_records, "build_flags": build_flags, "source_date_epoch": 0, "driver_requirements": [x for x in backends if x in ("cuda", "vulkan")]}
        archive_dest, manifest_dest = args.output / (target + ".tar.gz"), args.output / (target + ".json")
        (stage / archive_dest.name).write_bytes(archive_data)
        (stage / manifest_dest.name).write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
        specification = importlib.util.spec_from_file_location("native_publication", ROOT / "tools/package.py")
        publication = importlib.util.module_from_spec(specification)
        specification.loader.exec_module(publication)
        publication.publish_files(args.output, stage, [archive_dest.name, manifest_dest.name])
        print("Native payload:", archive_dest, manifest_dest, flush=True)
        return manifest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", help="actual host OS-arch; defaults to host")
    parser.add_argument("--output", type=Path, default=ROOT / "native-payloads")
    parser.add_argument("--cache", type=Path, default=ROOT / ".native-cache")
    parser.add_argument("--llama-archive", type=Path, help="existing source archive; pinned SHA256 checked")
    parser.add_argument("--go", default=shutil.which("go"))
    parser.add_argument("--cmake", default="cmake")
    parser.add_argument("--backends", default="cpu", help="cpu always required; optional metal,cuda,vulkan")
    parser.add_argument("--jobs", type=int, default=min(4, os.cpu_count() or 1))
    parser.add_argument("--max-glibc", default="2.34", help="Linux release baseline; do not raise for RHEL9-compatible releases")
    parser.add_argument("--dependency-license", action="append", type=Path, default=[])
    parser.add_argument("--keep-build", action="store_true", help="preserve build workspace, including failed binaries, for diagnostics")
    args = parser.parse_args()
    if not 1 <= args.jobs <= 64:
        parser.error("--jobs must be 1..64")
    build(args)


if __name__ == "__main__":
    main()
