"""Publication guard tests; no registry or GitHub requests are sent."""
import importlib.util
import pathlib
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('release_guard', pathlib.Path(__file__).with_name('check-release-absent.py'))
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


class PublicationGuardTests(unittest.TestCase):
    def run_guard(self, artifact_status):
        calls = []
        def response(url, headers):
            calls.append(url)
            if '/token?' in url:
                return 200, b'{"token":"test-only"}'
            return artifact_status(url), b''
        with patch.dict('os.environ', {'REPOSITORY': 'Owner/Repo', 'RELEASE_VERSION': 'v1.2.3',
                                      'GH_TOKEN': 'test-only', 'GH_USER': 'test'}), patch.object(guard, 'request', response):
            guard.main()
        return calls

    def test_all_absent(self):
        calls = self.run_guard(lambda _: 404)
        self.assertEqual(len(calls), 6)
        self.assertIn('https://api.github.com/repos/Owner/Repo/git/ref/tags/v1.2.3', calls)
        self.assertIn('https://ghcr.io/v2/owner/charts/celld-operator/manifests/1.2.3', calls)

    def test_each_partial_publication_refuses(self):
        for fragment in ['/releases/', '/git/ref/tags/', '/owner/repo/manifests/', '/charts/']:
            with self.subTest(fragment=fragment), self.assertRaises(SystemExit):
                self.run_guard(lambda url: 200 if fragment in url else 404)

    def test_unexpected_status_refuses(self):
        with self.assertRaises(SystemExit):
            self.run_guard(lambda _: 403)

    def test_network_error_propagates(self):
        with patch.object(guard, 'request', side_effect=OSError('offline')), self.assertRaises(OSError):
            guard.require_absent('https://ghcr.io/', {}, 'test')

    def test_invalid_versions_fail_before_network_access(self):
        for version in ['v01.2.3', '1.2.3', 'v1.2.3\nforged=true', 'v1.2.3-..', 'v1.2.3-01']:
            with self.subTest(version=version), patch.dict('os.environ', {
                'REPOSITORY': 'Owner/Repo', 'RELEASE_VERSION': version,
                'GH_TOKEN': 'test-only', 'GH_USER': 'test',
            }), patch.object(guard, 'request') as request, self.assertRaises(SystemExit):
                guard.main()
            request.assert_not_called()


if __name__ == '__main__':
    unittest.main()
