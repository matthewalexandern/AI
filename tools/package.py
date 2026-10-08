#!/usr/bin/env python3
"""Build versioned single-file installers from verified native payloads."""
import argparse
import base64
import gzip
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]
SUPPORTED = ["linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64", "windows-amd64", "windows-arm64"]


def installer_name(target):
    return "mini-fabrics-installer-" + target + (".exe" if target.startswith("windows-") else "")


def file_sha256(path):
    # hashlib.file_digest is unavailable in Python 3.9/3.10, shipped by common
    # RHEL and Ubuntu installations. Stream large executables on all versions.
    digest = hashlib.sha256()
    with path.open("rb") as artifact:
        for block in iter(lambda: artifact.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def runtime_bundle():
    files = [ROOT / "go.mod", ROOT / "go.sum", ROOT / "LICENSE"]
    for directory in ("cmd/fabrics", "internal"):
        files.extend(p for p in (ROOT / directory).rglob("*.go") if not p.name.endswith("_test.go"))
    raw = io.BytesIO()
    with tarfile.open(fileobj=raw, mode="w", format=tarfile.PAX_FORMAT) as archive:
        for source in sorted(files):
            if source.is_symlink() or not source.is_file():
                raise ValueError(f"Runtime source must be a regular file: {source}")
            data = source.read_bytes()
            entry = tarfile.TarInfo("mini-fabrics/" + source.relative_to(ROOT).as_posix())
            entry.size, entry.mode, entry.mtime = len(data), 0o644, 0
            archive.addfile(entry, io.BytesIO(data))
    compressed = io.BytesIO()
    with gzip.GzipFile(fileobj=compressed, mode="wb", filename="", mtime=0) as stream:
        stream.write(raw.getvalue())
    return compressed.getvalue()


def publish_files(output, staged, names):
    """Publish a complete successful build, rolling back ordinary I/O failures."""
    output.mkdir(parents=True, exist_ok=True)
    for name in names:
        target = output / name
        if target.is_symlink() or (target.exists() and not target.is_file()):
            raise ValueError(f"Preserving non-regular output path: {target}")
    backup = staged / "previous"
    backup.mkdir()
    saved, published = [], []
    try:
        for name in names:
            target = output / name
            if target.exists():
                os.replace(target, backup / name)
                saved.append(name)
            os.replace(staged / name, target)
            published.append(name)
    except OSError as original:
        errors = []
        for name in reversed(published):
            try:
                (output / name).unlink()
            except OSError as error:
                errors.append(str(error))
        for name in reversed(saved):
            try:
                os.replace(backup / name, output / name)
            except OSError as error:
                errors.append(str(error))
        if errors:
            # Retain recovery copies outside the staging directory's cleanup.
            recovery = Path(tempfile.mkdtemp(prefix="package-recovery-", dir=output.parent)) / "previous"
            os.replace(backup, recovery)
            raise RuntimeError(f"Publish failed and rollback needs attention; saved previous files in {recovery}: {errors}") from original
        raise


def payload_source(values):
    # Keep the process command line small and avoid one enormous Go constant.
    source = 'package main\n\nimport payloadstrings "strings"\n\nfunc init() {\n'
    for name, value in values.items():
        chunks = [value[n:n + 65536] for n in range(0, len(value), 65536)]
        source += "\t" + name + " = payloadstrings.Join([]string{\n"
        source += "".join("\t\t" + json.dumps(chunk) + ",\n" for chunk in chunks)
        source += '\t}, "")\n'
    return source + "}\n"


def load_native_payload(directory, target):
    archive_path, manifest_path = directory / (target + ".tar.gz"), directory / (target + ".json")
    if archive_path.is_symlink() or manifest_path.is_symlink():
        raise ValueError("Native payload inputs must be regular files")
    archive, encoded = archive_path.read_bytes(), manifest_path.read_bytes()
    manifest = json.loads(encoded)
    platform = "/".join(target.split("-")[:2])
    if manifest.get("format_version") != 1 or manifest.get("version") != "0.1.0" or manifest.get("platform") != platform:
        raise ValueError(f"Native payload version/platform mismatch for {target}")
    if manifest.get("archive_sha256") != hashlib.sha256(archive).hexdigest():
        raise ValueError(f"Native payload archive checksum mismatch for {target}")
    expected = manifest.get("files", {})
    if not isinstance(expected, dict) or not expected:
        raise ValueError("Native payload has no file integrity manifest")
    seen = set()
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as content:
        for entry in content:
            if entry.isdir() and entry.name.rstrip("/") == "native":
                continue
            if not entry.name.startswith("native/") or (not entry.isfile() and not entry.isdir()):
                raise ValueError("Native payload contains links, special files, or unexpected roots")
            name = entry.name[len("native/"):].rstrip("/") if entry.isdir() else entry.name[len("native/"):]
            if not name or "\\" in name or ":" in name or any(part in ("", ".", "..") for part in name.split("/")) or name in seen:
                raise ValueError("Unsafe or duplicate native payload entry")
            if entry.isdir():
                continue
            digest = hashlib.sha256()
            with content.extractfile(entry) as artifact:
                for block in iter(lambda: artifact.read(1024 * 1024), b""):
                    digest.update(block)
            if name not in expected or digest.hexdigest() != expected[name]:
                raise ValueError(f"Native payload file integrity mismatch: {name}")
            seen.add(name)
    if seen != set(expected):
        raise ValueError("Native payload file set does not match manifest")
    executable = "bin/fabrics.exe" if target.startswith("windows-") else "bin/fabrics"
    if executable not in expected or not manifest.get("backends") or "cpu" not in manifest["backends"]:
        raise ValueError("Complete native payload requires runtime executable and CPU backend")
    paths = manifest.get("backend_paths", {})
    if set(paths) != set(manifest["backends"]) or any(path not in expected for path in paths.values()):
        raise ValueError("Native payload backend paths do not match files")
    return archive, encoded


def build_installers(go, targets, output, catalog, payloads=None, source_installer=False):
    output = Path(output).resolve()
    unselected = [installer_name(target) for target in SUPPORTED if target not in targets]
    remaining = [name for name in unselected if (output / name).exists() or (output / name).is_symlink()]
    if remaining:
        raise ValueError(
            f"Partial build would leave unselected installers in {output}: {', '.join(remaining)}. "
            "Use --output with a separate directory or build all targets (omit --targets); existing files are preserved."
        )
    output.parent.mkdir(parents=True, exist_ok=True)
    if payloads is None and not source_installer:
        raise ValueError("Release installers require --payloads with native binaries; use --source-installer only for development source builds")
    bundle = runtime_bundle() if source_installer else b""
    digest = hashlib.sha256(bundle).hexdigest() if bundle else ""
    catalog_data = Path(catalog).read_bytes()
    if not isinstance(json.loads(catalog_data), list):
        raise ValueError("Catalog must contain a JSON array")
    installer_source = (ROOT / "installer/install.go").read_bytes()
    native = {target: load_native_payload(Path(payloads), target) for target in targets} if not source_installer else {}
    with tempfile.TemporaryDirectory(prefix=".mini-fabrics-package-", dir=output.parent) as directory:
        staged = Path(directory)
        source = staged / "source"
        source.mkdir()
        (source / "install.go").write_bytes(installer_source)
        checksums, names = [], []
        for target in targets:
            goos, goarch = target.split("-")[:2]
            values = {"modelCatalogBase64": base64.b64encode(catalog_data).decode()}
            if source_installer:
                values.update(runtimeArchiveBase64=base64.b64encode(bundle).decode(), runtimeArchiveSHA256=digest)
            else:
                archive, manifest = native[target]
                values.update(nativeArchiveBase64=base64.b64encode(archive).decode(), nativeArchiveSHA256=hashlib.sha256(archive).hexdigest(), nativeManifestBase64=base64.b64encode(manifest).decode())
            (source / "payload.go").write_text(payload_source(values), encoding="utf-8")
            name = installer_name(target)
            destination = staged / name
            environment = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0", GOWORK="off")
            subprocess.run([go, "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", str(destination), str(source / "install.go"), str(source / "payload.go")], cwd=ROOT, env=environment, check=True)
            checksum = file_sha256(destination)
            checksums.append(f"{checksum}  {name}\n")
            names.append(name)
        (staged / "SHA256SUMS").write_text("".join(checksums), encoding="utf-8")
        metadata = {"version": "0.1.0", "mode": "source" if source_installer else "native", "targets": targets,
                    "installer_source_sha256": hashlib.sha256(installer_source).hexdigest(),
                    "catalog_sha256": hashlib.sha256(catalog_data).hexdigest(),
                    "native_payload_sha256": {t: hashlib.sha256(native[t][0]).hexdigest() for t in native}}
        (staged / "release-provenance.json").write_text(json.dumps(metadata, indent=2, sort_keys=True) + "\n", encoding="utf-8")
        extras = ["release-provenance.json"]
        if source_installer:
            (staged / "runtime-source.sha256").write_text(digest + "\n", encoding="utf-8")
            extras.append("runtime-source.sha256")
        for name in extras:
            checksums.append(f"{file_sha256(staged / name)}  {name}\n")
        (staged / "SHA256SUMS").write_text("".join(checksums), encoding="utf-8")
        publish_files(output, staged, names + ["SHA256SUMS"] + extras)
    for name in names:
        print(output / name)
    print("Installer mode:", "development source" if source_installer else "native payload")
    return digest


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--targets", default=",".join(SUPPORTED), help="comma-separated OS-arch targets")
    parser.add_argument("--output", type=Path, default=ROOT / "dist")
    parser.add_argument("--catalog", type=Path, default=ROOT / "installer/models.json", help="trusted immutable model catalog JSON (default: installer/models.json)")
    parser.add_argument("--payloads", type=Path, help="directory containing TARGET.tar.gz and TARGET.json verified native payloads")
    parser.add_argument("--source-installer", action="store_true", help="explicit development mode: installer builds sources and needs compiler prerequisites")
    parser.add_argument("--go", default=shutil.which("go"), help="Go compiler executable")
    args = parser.parse_args()
    if not args.go:
        parser.error("Install Go 1.24+ or pass --go /path/to/go")
    targets = list(dict.fromkeys(args.targets.split(",")))
    if any(target not in SUPPORTED for target in targets):
        parser.error("Supported targets: " + ", ".join(SUPPORTED))
    if args.payloads and args.source_installer:
        parser.error("Choose --payloads or --source-installer")
    build_installers(args.go, targets, args.output, args.catalog, args.payloads, args.source_installer)


if __name__ == "__main__":
    main()
