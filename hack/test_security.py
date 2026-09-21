"""Security invariants across every workflow, including future additions."""
import pathlib
import re
import unittest

import yaml

ROOT = pathlib.Path(__file__).resolve().parent.parent


def workflow(path):
    data = yaml.safe_load(path.read_text())
    # PyYAML uses YAML 1.1, where the unquoted Actions key `on` becomes True.
    data['on'] = data.pop(True, data.get('on'))
    return data


class WorkflowSecurityTests(unittest.TestCase):
    def test_all_workflows_obey_security_boundaries(self):
        for path in sorted((ROOT / '.github/workflows').glob('*.*ml')):
            with self.subTest(workflow=path.name):
                data = workflow(path)
                self.assertEqual(data['permissions'], {})
                triggers = data['on']
                self.assertNotIn('pull_request_target', triggers)
                self.assertNotIn('workflow_run', triggers)
                for name, job in data['jobs'].items():
                    with self.subTest(job=name):
                        permissions = job.get('permissions', {})
                        if 'write' in permissions.values():
                            self.assertTrue(job.get('environment'), 'write jobs need deployment protection')
                        for step in job.get('steps', []):
                            action = step.get('uses', '')
                            if action:
                                self.assertRegex(action, r'^[\w.-]+/[\w./-]+@[a-f0-9]{40}$')
                            if action.startswith('actions/checkout@'):
                                self.assertIs(step['with']['persist-credentials'], False)

    def test_release_only_publishes_approved_main_commit_without_caches(self):
        data = workflow(ROOT / '.github/workflows/release.yaml')
        self.assertEqual(set(data['on']), {'workflow_dispatch'})
        self.assertIn("github.ref == 'refs/heads/main'", data['jobs']['validate']['if'])
        self.assertIn("github.repository == 'ewhauser/celld-operator'", data['jobs']['validate']['if'])
        self.assertEqual(data['jobs']['publish']['environment'], 'release')
        self.assertEqual(data['jobs']['release']['environment'], 'release-artifacts')
        self.assertEqual(data['jobs']['release']['needs'], ['publish', 'qualify'])
        for job in data['jobs'].values():
            for step in job.get('steps', []):
                action = step.get('uses', '')
                inputs = step.get('with', {})
                self.assertNotIn('actions/cache', action)
                if action.startswith('actions/checkout@'):
                    self.assertEqual(inputs['ref'], '${{ github.sha }}')
                if action.startswith('actions/setup-go@'):
                    self.assertIs(inputs.get('cache'), False)
                if action.startswith('docker/build-push-action@'):
                    self.assertIs(inputs['no-cache'], True)
                    self.assertNotIn('cache-from', inputs)
                    self.assertNotIn('cache-to', inputs)
        release_text = (ROOT / '.github/workflows/release.yaml').read_text()
        self.assertNotIn('github.ref_name', release_text)
        self.assertIn('--draft', release_text)
        self.assertIn('--draft=false', release_text)
        self.assertIn('actions/attest-build-provenance@', release_text)
        self.assertNotIn('--certificate-identity-regexp', release_text)
        self.assertNotIn('type=cache', (ROOT / 'Dockerfile').read_text())

    def test_every_ci_tool_has_an_immutable_digest(self):
        import json
        pins = json.loads((ROOT / 'hack/ci-tools.json').read_text())
        for name, pin in pins.items():
            with self.subTest(tool=name):
                self.assertTrue(pin['url'].startswith('https://'))
                self.assertNotIn('/latest/', pin['url'])
                self.assertIsNotNone(re.fullmatch('[a-f0-9]{64}', pin['sha256']))
