"""Publication guard tests; no registry or GitHub requests are sent."""
import importlib.util
import pathlib
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('release_guard', pathlib.Path(__file__).with_name('check-release-absent.py'))
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)

spec = importlib.util.spec_from_file_location('chart_push', pathlib.Path(__file__).with_name('push-release-chart.py'))
chart_push = importlib.util.module_from_spec(spec)
spec.loader.exec_module(chart_push)


class ChartPushTests(unittest.TestCase):
    transient = 'Error: failed to perform "Tag" on destination: sha256:' + 'a' * 64 + ': not found\n'

    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.chart = pathlib.Path(directory.name) / 'chart.tgz'
        self.chart.write_bytes(b'fixed archive')
        self.log = pathlib.Path(directory.name) / 'push.txt'

    def result(self, code, output):
        return subprocess.CompletedProcess([], code, stdout=output)

    def test_visibility_failure_then_success(self):
        success = 'Pushed: registry/chart:1.2.3\nDigest: sha256:' + 'b' * 64 + '\n'
        with patch.object(chart_push.subprocess, 'run', side_effect=[
            self.result(1, self.transient), self.result(0, success),
        ]) as run, patch.object(chart_push.time, 'sleep') as sleep:
            chart_push.push(self.chart, 'oci://registry', self.log)
        self.assertEqual(run.call_count, 2)
        self.assertEqual(run.call_args_list[0], run.call_args_list[1])
        sleep.assert_called_once_with(5)
        self.assertEqual(self.log.read_text(), success)

    def test_persistent_visibility_failure_stops(self):
        with patch.object(chart_push.subprocess, 'run', return_value=self.result(1, self.transient)) as run, \
                patch.object(chart_push.time, 'sleep') as sleep, self.assertRaises(SystemExit):
            chart_push.push(self.chart, 'oci://registry', self.log)
        self.assertEqual(run.call_count, 3)
        self.assertEqual([call.args[0] for call in sleep.call_args_list], [5, 15])

    def test_other_errors_do_not_retry(self):
        for error in ['unauthorized', 'denied', 'connection refused', 'manifest unknown']:
            with self.subTest(error=error), patch.object(chart_push.subprocess, 'run',
                    return_value=self.result(1, error)) as run, \
                    patch.object(chart_push.time, 'sleep') as sleep, self.assertRaises(SystemExit):
                chart_push.push(self.chart, 'oci://registry', self.log)
            self.assertEqual(run.call_count, 1)
            sleep.assert_not_called()

    def test_changed_chart_is_not_republished(self):
        with patch.object(chart_push.subprocess, 'run', return_value=self.result(1, self.transient)) as run, \
                patch.object(chart_push.time, 'sleep', side_effect=lambda _: self.chart.write_bytes(b'changed')), \
                self.assertRaisesRegex(SystemExit, 'chart changed'):
            chart_push.push(self.chart, 'oci://registry', self.log)
        self.assertEqual(run.call_count, 1)


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
