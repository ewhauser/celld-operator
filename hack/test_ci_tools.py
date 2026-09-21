"""Untrusted downloaded bytes must not become executable before verification."""
import hashlib
import importlib.util
import io
import pathlib
import tarfile
import unittest

spec = importlib.util.spec_from_file_location('ci_tools', pathlib.Path(__file__).with_name('install-ci-tools.py'))
tools = importlib.util.module_from_spec(spec)
spec.loader.exec_module(tools)


class ToolIntegrityTests(unittest.TestCase):
    def test_checksum_mismatch_rejected_before_archive_parsing(self):
        with self.assertRaisesRegex(ValueError, 'checksum mismatch'):
            tools.verified_binary(b'not an archive', {'sha256': '0' * 64, 'member': 'helm'})

    def test_raw_binary(self):
        data = b'verified executable'
        self.assertEqual(tools.verified_binary(data, {'sha256': hashlib.sha256(data).hexdigest()}), data)

    def test_only_expected_regular_archive_member_is_read(self):
        for symlink in [False, True]:
            with self.subTest(symlink=symlink):
                stream = io.BytesIO()
                with tarfile.open(fileobj=stream, mode='w:gz') as archive:
                    member = tarfile.TarInfo('linux-amd64/helm')
                    if symlink:
                        member.type = tarfile.SYMTYPE
                        member.linkname = '/etc/passwd'
                        archive.addfile(member)
                    else:
                        member.size = 2
                        archive.addfile(member, io.BytesIO(b'ok'))
                data = stream.getvalue()
                pin = {'sha256': hashlib.sha256(data).hexdigest(), 'member': 'linux-amd64/helm'}
                if symlink:
                    with self.assertRaisesRegex(ValueError, 'regular file'):
                        tools.verified_binary(data, pin)
                else:
                    self.assertEqual(tools.verified_binary(data, pin), b'ok')
