import importlib.util
import io
import hashlib
import os
import struct
from pathlib import Path
import tarfile
import tempfile
import unittest
from unittest import mock

spec=importlib.util.spec_from_file_location('native_payload',Path(__file__).with_name('native_payload.py'))
native=importlib.util.module_from_spec(spec);spec.loader.exec_module(native)

def elf_fixture(mask=3, descriptor=None, property_type=0xc0008002, duplicate_segment=False):
    if descriptor is None:
        descriptor = struct.pack('<III', property_type, 4, mask) + b'\0' * 4
    note = struct.pack('<III', 4, len(descriptor), 5) + b'GNU\0' + descriptor
    count = 2 if duplicate_segment else 1
    header = bytearray(64)
    header[:7] = b'\x7fELF\x02\x01\x01'
    struct.pack_into('<HHI', header, 16, 2, 62, 1)
    struct.pack_into('<Q', header, 32, 64)
    struct.pack_into('<HHH', header, 52, 64, 56, count)
    start = 64 + count * 56
    segments = []
    for kind in ([4, 0x6474e553] if duplicate_segment else [0x6474e553]):
        segments.append(struct.pack('<IIQQQQQQ', kind, 4, start, 0, 0, len(note), len(note), 8))
    return bytes(header) + b''.join(segments) + note

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
    def test_elf_cpu_isa_enforces_declared_minimum_for_every_library(self):
        with tempfile.TemporaryDirectory() as directory:
            binary,library = Path(directory)/'llama-server',Path(directory)/'libexample.so'
            binary.write_bytes(elf_fixture(3, duplicate_segment=True))
            # Verification must not depend on old/new/localized readelf output.
            with mock.patch.object(native, 'capture', side_effect=AssertionError('readelf text must not be used')):
                self.assertEqual(native.verify_elf_cpu_isa([binary]), {'llama-server': 'x86-64-v2'})
            for mask,higher in ((4,'x86-64-v3'),(8,'x86-64-v4')):
                library.write_bytes(elf_fixture(mask))
                with self.assertRaisesRegex(ValueError, 'requires '+higher):
                    native.verify_elf_cpu_isa([binary,library])
            for mask in (0,16,19):
                binary.write_bytes(elf_fixture(mask))
                with self.assertRaisesRegex(ValueError, 'Unrecognized'):
                    native.verify_elf_cpu_isa([binary])

    def test_elf_cpu_isa_rejects_malformed_headers_ranges_and_properties(self):
        malformed = []
        wrong_class = bytearray(elf_fixture());wrong_class[4]=1;malformed.append(wrong_class)
        wrong_machine = bytearray(elf_fixture());struct.pack_into('<H',wrong_machine,18,183);malformed.append(wrong_machine)
        truncated_segment = bytearray(elf_fixture());struct.pack_into('<Q',truncated_segment,64+32,65536);malformed.append(truncated_segment)
        truncated_note = bytearray(elf_fixture());struct.pack_into('<I',truncated_note,120+4,65536);malformed.append(truncated_note)
        malformed.append(elf_fixture(descriptor=struct.pack('<II',0xc0008002,8)+b'\0'*8))
        malformed.append(elf_fixture(descriptor=struct.pack('<III',0xc0008002,4,3)))  # missing eight-byte alignment padding
        malformed.append(elf_fixture(descriptor=(struct.pack('<III',0xc0008002,4,3)+b'\0'*4)*2))
        malformed.append(elf_fixture(property_type=0x1234))  # no ISA requirement property
        malformed.extend((b'',b'not ELF',elf_fixture()[:-1]))
        with tempfile.TemporaryDirectory() as directory:
            binary=Path(directory)/'llama-server'
            for content in malformed:
                binary.write_bytes(content)
                with self.subTest(size=len(content)):
                    with self.assertRaises(ValueError):
                        native.verify_elf_cpu_isa([binary])
    def test_failed_build_workspace_can_be_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaisesRegex(ValueError, 'build failure'):
                with native.build_workspace(Path(directory), keep=True) as stage:
                    (stage / 'diagnostic').write_bytes(b'ELF')
                    raise ValueError('build failure')
            self.assertEqual((stage / 'diagnostic').read_bytes(), b'ELF')
            with native.build_workspace(Path(directory)) as cleaned:
                (cleaned / 'temporary').write_bytes(b'readonly notice')
                (cleaned / 'temporary').chmod(0o444)
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

    def test_explicit_clang_compiler_cache_has_supported_provenance(self):
        with tempfile.TemporaryDirectory() as directory:
            build=Path(directory)
            (build/'CMakeCache.txt').write_text('CMAKE_CXX_COMPILER:STRING=C:/LLVM/bin/clang-cl.exe\n')
            process=mock.Mock(stdout='clang version 20; target aarch64-pc-windows-msvc')
            with mock.patch.object(native.subprocess,'run',return_value=process):
                record=native.compiler_provenance(build)
            self.assertEqual(record['executable'],'C:/LLVM/bin/clang-cl.exe')
            self.assertIn('aarch64',record['version_output'])

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
