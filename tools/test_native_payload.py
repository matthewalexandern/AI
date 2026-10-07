import importlib.util
import io
import hashlib
import os
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
        output = '0000 DF *UND* (GLIBC_2.9) old_symbol\n0000 DF *UND* (GLIBC_2.34) new_symbol\n0000 g DF .text GLIBC_2.35 exported_symbol\n0000 DF *UND* (GLIBC_PRIVATE) internal_symbol\n'
        with mock.patch.object(native,'capture',return_value=output):
            self.assertEqual(native.glibc_requirement([Path('server')]),'2.34')
    def test_failed_build_workspace_can_be_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, 'build failure'):
                with native.build_workspace(Path(directory), keep=True) as stage:
                    (stage / 'diagnostic').write_bytes(b'ELF')
                    raise ValueError('build failure')
            self.assertEqual((stage / 'diagnostic').read_bytes(), b'ELF')
            with native.build_workspace(Path(directory)) as cleaned:
                (cleaned / 'temporary').touch()
            self.assertFalse(cleaned.exists())
    def test_source_archive_rejects_traversal(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'source.tar.gz'
            with tarfile.open(path,'w:gz') as archive:
                entry=tarfile.TarInfo('root/../outside');entry.size=1;archive.addfile(entry,io.BytesIO(b'x'))
            with self.assertRaisesRegex(ValueError,'Unsafe'):
                native.extract_source(path,Path(directory)/'out')
    def test_provenance_rejects_source_mutation_during_build(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory)
            for name in ('go.mod','go.sum','installer/models.json','cmd/fabrics/main.go','native/macos/SystemInfo.swift'):
                file=root/name;file.parent.mkdir(parents=True,exist_ok=True);file.write_text('locked input')
            locked=native.source_state(root)
            native.verify_source_state(locked,root)
            (root/'cmd/fabrics/main.go').write_text('new implementation')
            with self.assertRaisesRegex(ValueError,'changed during'):
                native.verify_source_state(locked,root)

    def test_download_closes_file_before_publication(self):
        data=b'verified source'
        handles=[]
        real_named_temporary=native.tempfile.NamedTemporaryFile
        real_replace=os.replace
        def opened(*args,**kwargs):
            file=real_named_temporary(*args,**kwargs);handles.append(file);return file
        def publish(source,destination):
            self.assertTrue(handles[-1].closed)
            return real_replace(source,destination)
        class Response(io.BytesIO):
            url=native.LLAMA_URL
        opener=mock.Mock()
        opener.open.side_effect=lambda *args,**kwargs:Response(data)
        with tempfile.TemporaryDirectory() as directory, mock.patch.object(native,'LLAMA_SHA256',hashlib.sha256(data).hexdigest()), mock.patch.object(native.tempfile,'NamedTemporaryFile',side_effect=opened), mock.patch.object(native.os,'replace',side_effect=publish), mock.patch.object(native.urllib.request,'build_opener',return_value=opener):
            archive=native.source_archive(Path(directory),None)
            self.assertEqual(archive.read_bytes(),data)
            self.assertFalse(list(Path(directory).glob('.download-*')))

    def test_supplied_source_requires_exact_pin(self):
        with tempfile.TemporaryDirectory() as directory:
            path=Path(directory)/'bad.tar.gz';path.write_bytes(b'unverified')
            with self.assertRaisesRegex(ValueError,'SHA256'):
                native.source_archive(Path(directory)/'cache',path)

if __name__=='__main__':unittest.main()
