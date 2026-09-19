#!/usr/bin/env python3
"""Local directional release compatibility experiment, never cloud or Kubernetes."""
import json
import pathlib
import sys
import signal
import time
import run as harness

SOURCE = 'ghcr.io/denoland/celld@sha256:ce8bbc3c26a16c9ee00e3ce0501f36bfea2663b5af8285a08fc16a54568060a5'
TARGET = harness.IMAGE

def main():
    out = pathlib.Path(sys.argv[1]).resolve()
    out.mkdir(parents=True, exist_ok=True)
    signal.signal(signal.SIGALRM, lambda *_: (_ for _ in ()).throw(TimeoutError("version experiment exceeded 420s")))
    signal.alarm(420)
    harness.IMAGE = SOURCE
    harness.COMMIT = '10cb1303dac710dcb3b557e318e08c855261f68b'
    run = harness.Run(out)
    try:
        run.setup()
        print('source v0.4.1 deployed', flush=True)
        for node in ('a', 'b'):
            run.start(node, 'fleet', ('-e','CELLD_DRAIN_TOKEN_WAIT_MS=3000'))
            if run.ready(node) is None:
                raise RuntimeError('source startup failed')
        run.writes('a', 12)
        run.state('a', 'v041')
        before = run.metadata('v041')
        run.stop('a', 'TERM')
        generation = before['nodes']['nodes/a.json']['ownership_index_generation']
        recovered = run.recover('a', generation, 'source-a-sealed', seconds=60)
        if not recovered['candidate_complete']:
            raise RuntimeError('source a did not reach sealed boundary')
        run.stop('b', 'TERM')
        sealed = run.metadata('all-stopped')
        for node in ('a', 'b'):
            record = sealed['nodes']['nodes/' + node + '.json']
            if record.get('log', {}).get('state') != 'sealed':
                raise RuntimeError('source log not sealed; transition unsafe')
            name = run.nodes[node][0]
            harness.docker('rm', name)
            run.containers.remove(name)
        print('both source processes exited; both logs sealed', flush=True)
        deadline = time.monotonic() + 45
        while True:
            expiry = run.metadata('source-expiry')
            if all(n['expires_ms'] <= expiry['observed_unix_ms'] for n in expiry['nodes'].values()):
                break
            if time.monotonic() >= deadline:
                raise RuntimeError('source leases did not expire')
            time.sleep(1)
        print('all observed source leases expired', flush=True)
        harness.IMAGE = TARGET
        harness.docker('pull', TARGET, timeout=180)
        run.save('target-image', json.loads(harness.docker('image', 'inspect', TARGET)))
        for node in ('a', 'b'):
            run.start(node, 'fleet')
            if run.ready(node) is None:
                raise RuntimeError('target startup failed')
        run.state('a', 'v050')
        after = run.metadata('v050')
        for node in ('a', 'b'):
            key = 'nodes/' + node + '.json'
            if before['nodes'][key]['ownership_index_generation'] == after['nodes'][key]['ownership_index_generation']:
                raise RuntimeError('target generation was not replaced')
        result = {'source': SOURCE, 'target': TARGET, 'direction': 'upgrade-only', **run.verify('b')}
        run.writes('b', 6)
        result['post_upgrade'] = run.verify('a')
        if any(key.endswith('.loss.json') for key in after['log_keys']):
            raise RuntimeError('loss marker after transition')
        run.save('result', result)
        print(json.dumps(result), flush=True)
    finally:
        run.cleanup()

if __name__ == '__main__':
    main()
