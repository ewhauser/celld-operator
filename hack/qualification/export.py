#!/usr/bin/env python3
"""Export metadata/results only; exclude runtime logs, binaries and app payloads."""
import argparse
import hashlib
import json
import pathlib

parser = argparse.ArgumentParser()
parser.add_argument('run', type=pathlib.Path)
parser.add_argument('destination', type=pathlib.Path)
args = parser.parse_args()
args.destination.mkdir(parents=True, exist_ok=False)
manifest = {'source_run': args.run.name, 'files': {}}
files = sorted(args.run.glob('*-metadata.json')) + sorted(args.run.glob('*-state.json')) + sorted(args.run.glob('*-health-*.json'))
files += [args.run / n for n in ('results.json', 'version.json', 'acknowledged-ledger.json', 'harness.json')]
for source in files:
    if not source.exists():
        continue
    raw = source.read_bytes()
    value = json.loads(raw)
    redactions = []
    if source.name.endswith('-state.json'):
        # Cell/deployment identifiers are irrelevant to the runtime adapter.
        for key in ('deployment', 'residents', 'published'):
            if key in value:
                del value[key]
                redactions.append(key)
    data = (json.dumps(value, indent=2) + '\n').encode()
    (args.destination / source.name).write_bytes(data)
    manifest['files'][source.name] = {'source_sha256': hashlib.sha256(raw).hexdigest(),
                                    'export_sha256': hashlib.sha256(data).hexdigest(),
                                    'removed_fields': redactions}
image = json.loads((args.run / 'image.json').read_text())[0]
manifest['runtime'] = {'architecture': image['Architecture'], 'labels': image['Config']['Labels'],
                       'repo_digests': image['RepoDigests']}
(args.destination / 'provenance.json').write_text(json.dumps(manifest, indent=2) + '\n')
