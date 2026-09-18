#!/usr/bin/env python3
"""Regenerate without changing the working tree, including untracked generated files."""
import pathlib
import subprocess
import tempfile

root = pathlib.Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix='celld-generated-') as tmp:
    tmp = pathlib.Path(tmp)
    subprocess.run(['go', 'run', 'sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1',
                    'object', 'crd', 'paths=./api/...',
                    f'output:crd:dir={tmp}', f'output:object:dir={tmp}'], cwd=root, check=True)
    expected = list((root / 'config/crd').glob('*.yaml')) + list((root / 'api').rglob('zz_generated.deepcopy.go'))
    for path in expected:
        if path.read_bytes() != (tmp / path.name).read_bytes():
            raise SystemExit(f'generated file differs: {path}; run make generate')
    print('Generated CRDs and deepcopy code match.')
