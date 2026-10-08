import base64
import hashlib
import importlib.util
import io
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location("packager", Path(__file__).with_name("package.py"))
packager = importlib.util.module_from_spec(spec)
spec.loader.exec_module(packager)


class PackagingTests(unittest.TestCase):
    def test_bundle_is_repeatable_and_contains_build_inputs(self):
        first = packager.runtime_bundle()
        self.assertEqual(first, packager.runtime_bundle())
        with tarfile.open(fileobj=io.BytesIO(first), mode="r:gz") as archive:
            names = archive.getnames()
            self.assertIn("mini-fabrics/go.mod", names)
            self.assertIn("mini-fabrics/go.sum", names)
            self.assertIn("mini-fabrics/LICENSE", names)
            self.assertIn("mini-fabrics/cmd/fabrics/main.go", names)
            self.assertFalse(any(name.endswith("_test.go") for name in names))
            self.assertTrue(all(entry.mtime == 0 for entry in archive))

    def test_payload_does_not_exceed_windows_command_limit(self):
        # The payload alone exceeds CreateProcess' command-line limit.
        payload = b"runtime sources" * 5000
        calls = []

        def build(command, **kwargs):
            calls.append(command)
            self.assertLess(sum(len(str(arg)) + 3 for arg in command), 32767)
            self.assertFalse(any("runtimeArchiveBase64=" in arg for arg in command))
            source = Path(command[-1]).read_text()
            self.assertIn(base64.b64encode(payload).decode()[:65536], source)
            self.assertIn(hashlib.sha256(payload).hexdigest(), source)
            target = Path(command[command.index("-o") + 1])
            target.write_bytes(kwargs["env"]["GOOS"].encode() * 500000)

        with tempfile.TemporaryDirectory() as directory, mock.patch.object(packager, "runtime_bundle", return_value=payload), mock.patch.object(packager.subprocess, "run", side_effect=build), mock.patch.object(packager.hashlib, "file_digest", side_effect=AssertionError("Python 3.9 has no file_digest"), create=True):
            output = Path(directory) / "dist"
            packager.build_installers("go", ["linux-amd64", "windows-arm64"], output, packager.ROOT / "installer/models.json", source_installer=True)
            self.assertEqual(len(calls), 2)
            for line in (output / "SHA256SUMS").read_text().splitlines():
                digest, name = line.split("  ")
                self.assertEqual(digest, hashlib.sha256((output / name).read_bytes()).hexdigest())

    def test_partial_build_refuses_to_mix_existing_targets_without_mutation(self):
        for target, unselected in (("linux-amd64", "mini-fabrics-installer-windows-arm64.exe"), ("windows-amd64", "mini-fabrics-installer-linux-arm64")):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as directory:
                output = Path(directory) / "dist"
                output.mkdir()
                original = {
                    packager.installer_name(target): b"previous selected installer",
                    unselected: b"previous unselected installer",
                    "SHA256SUMS": b"previous checksums",
                    "runtime-source.sha256": b"previous source digest",
                }
                for name, content in original.items():
                    (output / name).write_bytes(content)
                with mock.patch.object(packager, "runtime_bundle") as bundle, mock.patch.object(packager.subprocess, "run") as build:
                    with self.assertRaisesRegex(ValueError, "--output.*all targets"):
                        packager.build_installers("go", [target], output, packager.ROOT / "installer/models.json", source_installer=True)
                    bundle.assert_not_called()
                    build.assert_not_called()
                self.assertEqual({path.name: path.read_bytes() for path in output.iterdir()}, original)
                self.assertEqual(list(Path(directory).iterdir()), [output])

    def test_failed_cross_build_preserves_published_artifacts(self):
        calls = 0

        def build(command, **kwargs):
            nonlocal calls
            calls += 1
            if calls == 2:
                raise subprocess.CalledProcessError(1, command)
            Path(command[command.index("-o") + 1]).write_bytes(b"new installer")

        with tempfile.TemporaryDirectory() as directory, mock.patch.object(packager.subprocess, "run", side_effect=build):
            output = Path(directory) / "dist"
            output.mkdir()
            original = {"mini-fabrics-installer-linux-amd64": b"old installer", "SHA256SUMS": b"old checksums", "runtime-source.sha256": b"old source digest"}
            for name, content in original.items():
                (output / name).write_bytes(content)
            with self.assertRaises(subprocess.CalledProcessError):
                packager.build_installers("go", ["linux-amd64", "windows-amd64"], output, packager.ROOT / "installer/models.json", source_installer=True)
            self.assertEqual({p.name: p.read_bytes() for p in output.iterdir()}, original)

    def test_publication_failure_restores_old_release(self):
        with tempfile.TemporaryDirectory() as directory:
            output, staged = Path(directory) / "dist", Path(directory) / "stage"
            output.mkdir(); staged.mkdir()
            for name in ("installer", "SHA256SUMS"):
                (output / name).write_bytes(b"old " + name.encode())
                (staged / name).write_bytes(b"new " + name.encode())
            replace = packager.os.replace

            def fail_metadata(source, destination):
                if Path(source) == staged / "SHA256SUMS":
                    raise PermissionError("simulated metadata publication failure")
                return replace(source, destination)

            with mock.patch.object(packager.os, "replace", side_effect=fail_metadata), self.assertRaises(PermissionError):
                packager.publish_files(output, staged, ["installer", "SHA256SUMS"])
            for name in ("installer", "SHA256SUMS"):
                self.assertEqual((output / name).read_bytes(), b"old " + name.encode())



class NativePackagingTests(unittest.TestCase):
    def payload(self, directory, corrupt=False):
        import json
        contents = {'bin/fabrics': b'runtime', 'backends/cpu/llama-server': b'llama', 'licenses/LICENSE': b'license'}
        raw = io.BytesIO()
        with tarfile.open(fileobj=raw, mode='w:gz') as archive:
            for name, data in contents.items():
                entry = tarfile.TarInfo('native/' + name); entry.size = len(data)
                archive.addfile(entry, io.BytesIO(data))
        archive_data = raw.getvalue()
        manifest = {'format_version':1, 'version':'0.1.0', 'platform':'linux/amd64', 'backends':['cpu'], 'default_backend':'cpu', 'backend_paths':{'cpu':'backends/cpu/llama-server'}, 'files':{name:hashlib.sha256(data).hexdigest() for name,data in contents.items()}, 'archive_sha256':hashlib.sha256(archive_data).hexdigest()}
        if corrupt: manifest['files']['bin/fabrics'] = '0' * 64
        (directory / 'linux-amd64.tar.gz').write_bytes(archive_data)
        (directory / 'linux-amd64.json').write_text(json.dumps(manifest))
        return archive_data

    def test_release_refuses_compiler_dependent_installer_by_default(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, '--payloads'):
                packager.build_installers('go',['linux-amd64'],Path(directory)/'dist',packager.ROOT/'installer/models.json')
            self.assertFalse((Path(directory)/'dist').exists())

    def test_native_payload_integrity_and_backend_closure(self):
        with tempfile.TemporaryDirectory() as directory:
            self.payload(Path(directory))
            packager.load_native_payload(Path(directory),'linux-amd64')
            self.payload(Path(directory),corrupt=True)
            with self.assertRaisesRegex(ValueError,'file integrity'):
                packager.load_native_payload(Path(directory),'linux-amd64')

    def test_native_build_embeds_verified_bytes_without_source_bundle(self):
        def build(command,**kwargs):
            source = Path(command[-1]).read_text()
            self.assertIn('nativeArchiveBase64',source)
            self.assertNotIn('runtimeArchiveBase64',source)
            Path(command[command.index('-o')+1]).write_bytes(b'native-installer')
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(packager.subprocess,'run',side_effect=build), mock.patch.object(packager,'runtime_bundle',side_effect=AssertionError('release must not build sources')):
            payloads = Path(directory)/'payloads';payloads.mkdir();self.payload(payloads)
            output = Path(directory)/'dist'
            packager.build_installers('go',['linux-amd64'],output,packager.ROOT/'installer/models.json',payloads=payloads)
            import json
            self.assertEqual(json.loads((output/'release-provenance.json').read_text())['mode'],'native')

if __name__ == "__main__":
    unittest.main()
