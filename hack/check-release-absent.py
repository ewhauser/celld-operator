#!/usr/bin/env python3
"""Refuse publication over any existing release artifact, including partial runs."""
import base64
import json
import os
import re
import urllib.error
import urllib.parse
import urllib.request


def request(url, headers):
    try:
        with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=30) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        if error.code == 404:
            return 404, b''
        raise SystemExit(f'Release preflight failed: HTTP {error.code} from {urllib.parse.urlparse(url).netloc}') from None


def require_absent(url, headers, label):
    status, _ = request(url, headers)
    if status != 404:
        raise SystemExit(f'Refusing to overwrite existing {label}; inspect partial publications and use a new version.')


def main():
    repository = os.environ['REPOSITORY']
    version = os.environ['RELEASE_VERSION']
    token = os.environ['GH_TOKEN']
    username = os.environ['GH_USER']
    if not re.fullmatch(r'[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', repository):
        raise SystemExit('Invalid repository')
    number = r'(?:0|[1-9][0-9]*)'
    prerelease = r'(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)'
    if not re.fullmatch(rf'v{number}\.{number}\.{number}(?:-{prerelease}(?:\.{prerelease})*)?', version):
        raise SystemExit('Invalid release tag')
    require_absent(f'https://api.github.com/repos/{repository}/releases/tags/{version}',
                   {'Authorization': f'Bearer {token}', 'Accept': 'application/vnd.github+json'}, 'GitHub release')
    require_absent(f'https://api.github.com/repos/{repository}/git/ref/tags/{version}',
                   {'Authorization': f'Bearer {token}', 'Accept': 'application/vnd.github+json'}, 'release tag')
    owner = repository.split('/')[0].lower()
    basic = base64.b64encode(f'{username}:{token}'.encode()).decode()
    for name, tag in [(repository.lower(), version), (f'{owner}/charts/celld-operator', version[1:])]:
        query = urllib.parse.urlencode({'service': 'ghcr.io', 'scope': f'repository:{name}:pull,push'})
        status, raw = request('https://ghcr.io/token?' + query, {'Authorization': 'Basic ' + basic})
        if status != 200:
            raise SystemExit('Registry authorization unavailable; refusing publication')
        authorization = json.loads(raw)['token']
        require_absent(f'https://ghcr.io/v2/{name}/manifests/{tag}',
                       {'Authorization': 'Bearer ' + authorization,
                        'Accept': 'application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.list.v2+json'},
                       f'OCI artifact {name}:{tag}')
    print('Release, Git tag, image tag and chart version are absent.')


if __name__ == '__main__':
    main()
