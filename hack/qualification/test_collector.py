"""Fault tests for capture completeness: do not label partial S3 evidence complete."""
import io
import pathlib
import tempfile
import unittest
from unittest.mock import Mock, patch

from run import Run


class CollectorTest(unittest.TestCase):
    def collect(self, nodes, logs, body=b'{"node":"a"}'):
        with tempfile.TemporaryDirectory() as directory:
            run = Run(pathlib.Path(directory))
            run.s3 = Mock()
            run.s3.get_paginator.return_value.paginate.side_effect = [nodes, logs]
            run.s3.get_object.return_value = {'Body': io.BytesIO(body)}
            return run.metadata('test')

    def test_complete_empty_scan(self):
        result = self.collect([{'IsTruncated': False}], [{'IsTruncated': False}])
        self.assertTrue(result['complete'])
        self.assertEqual(result['log_keys'], [])

    def test_truncated_without_continuation(self):
        for bad in ({'IsTruncated': True}, {}, {'IsTruncated': False, 'NextContinuationToken': 'x'}):
            with self.subTest(page=bad), self.assertRaises(RuntimeError):
                self.collect([{'IsTruncated': False}], [bad])

    def test_pagination_preserves_historical_loss(self):
        result = self.collect([{'IsTruncated': False}], [
            {'IsTruncated': True, 'NextContinuationToken': 'next', 'Contents': [{'Key': 'log/a/bundle'}]},
            {'IsTruncated': False, 'Contents': [{'Key': 'log/old/g.e1.loss.json'}]},
        ])
        self.assertIn('log/old/g.e1.loss.json', result['log_keys'])

    def test_prefix_escape_and_duplicate(self):
        for keys in (['private/secret'], ['log/a', 'log/a']):
            with self.subTest(keys=keys), self.assertRaises(RuntimeError):
                self.collect([{'IsTruncated': False}], [{'IsTruncated': False, 'Contents': [{'Key': k} for k in keys]}])

    def test_missing_final_page(self):
        for pages in ([], [{'IsTruncated': True, 'NextContinuationToken': 'next'}]):
            with self.subTest(pages=pages), self.assertRaises(RuntimeError):
                self.collect([{'IsTruncated': False}], pages)

    def test_pages_after_completion(self):
        with self.assertRaises(RuntimeError):
            self.collect([{'IsTruncated': False}], [{'IsTruncated': False}, {'IsTruncated': False}])

    def test_cleanup_continues_after_log_and_volume_failures(self):
        with tempfile.TemporaryDirectory() as directory:
            run = Run(pathlib.Path(directory))
            run.containers = ['test-container']
            run.volumes = ['test-volume']
            def fail_some(*args, **kwargs):
                if args[0] == 'logs' or args[:2] == ('volume', 'rm'):
                    raise RuntimeError('injected failure')
                return ''
            with patch('run.docker', side_effect=fail_some) as command:
                with self.assertRaises(RuntimeError):
                    run.cleanup()
            calls = [call.args for call in command.call_args_list]
            self.assertIn(('rm', '-f', 'test-container'), calls)
            self.assertIn(('network', 'rm', run.name), calls)

    def test_oversize_node(self):
        with self.assertRaises(RuntimeError):
            self.collect([{'IsTruncated': False, 'Contents': [{'Key': 'nodes/a.json'}]}], [], b' ' * 1048577)


if __name__ == '__main__':
    unittest.main()
