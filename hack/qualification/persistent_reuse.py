#!/usr/bin/env python3
"""Local-only counterexample: lease replacement does not exclude a disk opener.

This is deliberately not a data-loss or Kubernetes/EBS qualification test.
"""
import argparse
import json
import pathlib
import signal
import time

from run import Run, docker


def experiment(run):
    run.setup()
    run.start('a', 'fleet')
    if run.ready('a') is None:
        raise RuntimeError('original runtime not ready')
    run.start('b', 'fleet')
    if run.ready('b') is None:
        raise RuntimeError('survivor runtime not ready')
    def record():
        response = run.s3.get_object(Bucket='qualification', Key='nodes/a.json')
        return json.loads(response['Body'].read())

    before = record()
    run.save('before-node', before)
    original = run.nodes['a'][0]
    old = original + '-old'
    # Retain a real invocation and its mounted filesystem. This models the
    # authority gap after a pod API object disappears, not force-deleting a
    # resource in any existing Kubernetes cluster.
    docker('pause', original)
    docker('rename', original, old)
    run.containers[run.containers.index(original)] = old
    time.sleep(12)
    run.start('a', 'fleet')  # exact node name and exact retained volume
    ready = run.ready('a', seconds=90)
    after = record()
    run.save('replacement-node', after)
    inspections = json.loads(docker('inspect', old, original))
    mounted = [next(m['Name'] for m in c['Mounts'] if m['Destination'] == '/work')
               for c in inspections]
    result = {
        'scenario': 'retained-volume-overlapping-invocations',
        'same_named_volume': mounted[0] == mounted[1],
        'generation_replaced': before['ownership_index_generation'] != after['ownership_index_generation'],
        'old_process_running': inspections[0]['State']['Running'],
        'old_process_paused': inspections[0]['State']['Paused'],
        'replacement_ready_seconds': ready,
        'replacement_running': inspections[1]['State']['Running'],
        'data_loss_tested': False,
        'peer_only_acknowledgements_tested': False,
    }
    run.results.append(result)
    if ready is None or not all(result[k] for k in ('same_named_volume', 'generation_replaced', 'old_process_running',
                                                   'old_process_paused', 'replacement_running')):
        raise RuntimeError('counterexample was not established')
    # Do not resume concurrent writers: establishing retained access suffices
    # to refute an exclusive-open guarantee. Cleanup forcibly removes only
    # these disposable processes before removing their own volume.


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', type=pathlib.Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    run = Run(args.output.resolve())

    def deadline(_signum, _frame):
        raise TimeoutError('persistent reuse experiment exceeded 240 seconds')

    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(240)
    try:
        experiment(run)
    finally:
        signal.alarm(0)
        try:
            run.save('results', run.results)
        finally:
            run.cleanup()


if __name__ == '__main__':
    main()
