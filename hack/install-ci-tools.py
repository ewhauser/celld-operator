#!/usr/bin/env python3
"""Install reviewed Linux/amd64 tools, checking bytes before extraction or use."""
import hashlib
import io
import json
import os
import pathlib
import platform
import sys
import tarfile
import urllib.request


def verified_binary(data, pin):
    if hashlib.sha256(data).hexdigest() != pin['sha256']:
        raise ValueError('CI tool checksum mismatch')
    if 'member' in pin:
        with tarfile.open(fileobj=io.BytesIO(data), mode='r:gz') as archive:
            member = archive.getmember(pin['member'])
            if not member.isfile():
                raise ValueError('CI tool archive member must be a regular file')
            return archive.extractfile(member).read()
    return data


def main():
    if platform.system() != 'Linux' or platform.machine() != 'x86_64':
        raise SystemExit('These CI tool pins are for Linux/amd64 runners')
    pins = json.loads(pathlib.Path(__file__).with_name('ci-tools.json').read_text())
    directory = pathlib.Path(os.environ['RUNNER_TEMP']) / 'celld-ci-tools'
    directory.mkdir(exist_ok=True)
    for name in sys.argv[1:]:
        pin = pins[name]
        with urllib.request.urlopen(pin['url'], timeout=60) as response:
            binary = verified_binary(response.read(), pin)
        if name == 'buildx':
            target = pathlib.Path.home() / '.docker/cli-plugins/docker-buildx'
            target.parent.mkdir(parents=True, exist_ok=True)
        else:
            target = directory / name
        target.write_bytes(binary)
        target.chmod(0o755)
    with open(os.environ['GITHUB_PATH'], 'a') as path:
        path.write(f'{directory}\n')


if __name__ == '__main__':
    main()
