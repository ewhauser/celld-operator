#!/usr/bin/env python3
"""Package a release chart with immutable controller and launcher images."""
import argparse
import pathlib
import re
import shutil
import subprocess
import tempfile

p = argparse.ArgumentParser()
p.add_argument('--version', required=True)
p.add_argument('--image', required=True)
p.add_argument('--digest', required=True)
p.add_argument('--output', default='dist')
a = p.parse_args()
if not re.fullmatch(r'v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?', a.version):
    p.error('version must be a semantic version')
if not re.fullmatch(r'sha256:[0-9a-f]{64}', a.digest):
    p.error('digest must be sha256 plus 64 lowercase hex digits')
if not re.fullmatch(r'[a-z0-9][a-z0-9./_-]+', a.image):
    p.error('image must be a lowercase repository without a tag')
version = a.version.removeprefix('v')
output = pathlib.Path(a.output).resolve()
output.mkdir(parents=True, exist_ok=True)
with tempfile.TemporaryDirectory(prefix='celld-release-') as temp:
    chart = pathlib.Path(temp) / 'celld-operator'
    shutil.copytree('charts/celld-operator', chart)
    values = chart / 'values.yaml'
    data = values.read_text()
    for pattern, replacement in [
        (r'^  repository:.*$', f'  repository: {a.image}'),
        (r'^  digest:.*$', f'  digest: {a.digest}'),
        (r'^launcherImage:.*$', f'launcherImage: {a.image}@{a.digest}'),
    ]:
        data, count = re.subn(pattern, replacement, data, flags=re.MULTILINE)
        if count != 1:
            raise SystemExit(f'expected exactly one match for {pattern}')
    values.write_text(data)
    subprocess.run(['helm', 'lint', str(chart), '--strict'], check=True)
    subprocess.run(['helm', 'package', str(chart), '--version', version, '--app-version', version, '--destination', str(output)], check=True)
