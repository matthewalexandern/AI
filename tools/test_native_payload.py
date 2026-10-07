import importlib.util
import io
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest import mock

spec=importlib.util.spec_from_file_location('native_payload',Path(__file__).with_name('native_payload.py'))
native=importlib.util.module_from_spec(spec);spec.loader.exec_module(native)

class NativePayloadTests(unittest.TestCase):
    def test_deterministic_archive_contains_only_regular_files(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);(root/'bin').mkdir();(root/'bin/fabrics').write_bytes(b'fixed executable')
            first=native.deterministic_archive(root)
            self.assertEqual(first,native.deterministic_archive(root))
            with tarfile.open(fileobj=io.BytesIO(first),mode='r:gz') as archive:
                self.assertEqual(archive.getnames(),['native/bin/fabrics'])
                self.assertTrue(all(entry.mtime==0 and entry.isfile() for entry in archive))
    def test_dependency_closure_excludes_platform_glibc_but_keeps_runtime_libraries(self):
        text='linux-vdso.so.1 (0x0)\nlibc.so.6 => /lib/libc.so.6 (0x1)\nlibexample.so.1 => /opt/libexample.so.1 (0x2)\n/lib64/ld-linux-x86-64.so.2 (0x3)\n'
        with mock.patch.object(native,'capture',return_value=text):
            self.assertEqual(native.linux_dependencies(Path('/tmp/server')),[('libexample.so.1',Path('/opt/libexample.so.1'))])
        with mock.patch.object(native,'capture',return_value='libmissing.so => not found'):
            with self.assertRaisesRegex(ValueError,'Unresolved'):
                native.linux_dependencies(Path('/tmp/server'))
    def test_glibc_requirements_are_numeric(self):
        with mock.patch.object(native,'capture',return_value='GLIBC_2.9 GLIBC_2.34 GLIBC_PRIVATE'):
            self.assertEqual(native.glibc_requirement([Path('server')]),'2.34')
    def test_source_archive_rejects_traversal(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'source.tar.gz'
            with tarfile.open(path,'w:gz') as archive:
                entry=tarfile.TarInfo('root/../outside');entry.size=1;archive.addfile(entry,io.BytesIO(b'x'))
            with self.assertRaisesRegex(ValueError,'Unsafe'):
                native.extract_source(path,Path(directory)/'out')
    def test_supplied_source_requires_exact_pin(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'bad.tar.gz';path.write_bytes(b'unverified')
            with self.assertRaisesRegex(ValueError,'SHA256'):
                native.source_archive(Path(directory)/'cache',path)

if __name__=='__main__':unittest.main()
