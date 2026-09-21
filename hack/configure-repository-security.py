#!/usr/bin/env python3
"""Apply the reviewed GitHub controls. Requires repository administration access."""
import argparse
import json
import pathlib
import subprocess


def api(path, method='GET', payload=None):
    command = ['gh', 'api', '--method', method, path]
    if payload is not None:
        command.extend(['--input', '-'])
    result = subprocess.run(command, input=json.dumps(payload) if payload is not None else None,
                            text=True, check=True, capture_output=True)
    return json.loads(result.stdout) if result.stdout.strip() else None


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--apply', action='store_true', help='write the checked-in settings to GitHub')
    args = parser.parse_args()
    config = json.loads((pathlib.Path(__file__).resolve().parent.parent / '.github/repository-security.json').read_text())
    if not args.apply:
        print(json.dumps(config, indent=2))
        return
    root = 'repos/' + config['repository']
    # No bypass actors, long-lived credentials, or workflow review approval.
    api(root + '/actions/permissions', 'PUT', config['actions'])
    api(root + '/actions/permissions/workflow', 'PUT', config['workflow'])
    api(root + '/actions/permissions/fork-pr-contributor-approval', 'PUT', config['fork_approval'])
    api(root + '/immutable-releases', 'PUT')
    api(root + '/private-vulnerability-reporting', 'PUT')
    api(root + '/vulnerability-alerts', 'PUT')
    api(root + '/automated-security-fixes', 'PUT')
    api(root, 'PATCH', {'security_and_analysis': {
        'secret_scanning': {'status': 'enabled'},
        'secret_scanning_push_protection': {'status': 'enabled'},
    }})
    for name, settings in config['release_environments'].items():
        environment = root + '/environments/' + name
        api(environment, 'PUT', settings)
        policies = api(environment + '/deployment-branch-policies')['branch_policies']
        if not any(p['name'] == 'main' and p['type'] == 'branch' for p in policies):
            api(environment + '/deployment-branch-policies', 'POST', {'name': 'main', 'type': 'branch'})
        for policy in policies:
            if policy['name'] != 'main' or policy['type'] != 'branch':
                api(environment + '/deployment-branch-policies/' + str(policy['id']), 'DELETE')
    existing = {r['name']: r['id'] for r in api(root + '/rulesets')}
    for ruleset in config['rulesets']:
        if ruleset['name'] in existing:
            api(root + '/rulesets/' + str(existing[ruleset['name']]), 'PUT', ruleset)
        else:
            api(root + '/rulesets', 'POST', ruleset)
    print('Applied repository security controls. Read back settings to verify enforcement.')


if __name__ == '__main__':
    try:
        main()
    except subprocess.CalledProcessError as error:
        raise SystemExit(error.stderr.strip()) from None
