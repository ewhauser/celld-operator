#!/usr/bin/env python3
"""Paused-store peer-only acknowledgement plus donor retirement and owner failure."""
import argparse
import json
import os
import pathlib
import signal
import time

import boto3
from botocore.config import Config

from run import Run, docker, http
from launcher_reuse import state


def experiment(run, binary):
    run.setup()
    key = os.urandom(32)
    key_path = run.out / 'launcher-key'
    key_path.write_bytes(key)
    ports = {}
    for node in ('a', 'b', 'c'):
        run.start(node, 'fleet', extra=('--entrypoint', '/celld-launcher',
                  '-v', str(binary) + ':/celld-launcher:ro',
                  '-v', str(key_path) + ':/launcher-key/key:ro',
                  '-e', 'POD_UID=' + os.urandom(16).hex(), '-e', 'NODE_NAME=local-host',
                  '-p', '127.0.0.1::8083'))
        ports[node] = run.port(run.nodes[node][0], 8083)
        if run.ready(node, seconds=60) is None:
            raise RuntimeError('runtime not ready: ' + node)
    cell = 'peer-only-cell'
    leaders = []
    for i in range(30):
        code, raw = http(run.nodes['c'][1], '/?cell=' + cell + '&id=warm-' + str(i), 'PUT')
        if code != 200:
            raise RuntimeError('warm write failed: ' + raw)
        snapshot = run.metadata('before-peer-only')
        leaders = [r for r in snapshot['nodes'].values()
                   if r.get('log', {}).get('active') and len(r.get('log', {}).get('ensemble', [])) == 2]
        if len(leaders) == 1:
            break
        time.sleep(1)
    if len(leaders) != 1:
        raise RuntimeError('could not establish one active leader with two witnesses')
    owner = leaders[0]['node']
    donor, survivor = sorted(leaders[0]['log']['ensemble'])
    generation = leaders[0]['ownership_index_generation']
    store = run.name + '-store'
    paused = []
    try:
        docker('pause', store)
        paused.append(store)
        code, raw = http(run.nodes[owner][1], '/?cell=' + cell + '&id=peer-only', 'PUT')
        if code != 200 or json.loads(raw).get('stored') is not True:
            raise RuntimeError('peer-only write did not acknowledge: ' + raw)
        # This acknowledgement occurred while every MinIO process was stopped:
        # the new operation cannot already have been durably handled by S3.
        run.ledger.append({'id': 'peer-only', 'cell': cell})
        run.save('acknowledged-ledger', run.ledger)
        docker('pause', run.nodes[survivor][0])
        paused.append(run.nodes[survivor][0])
        docker('kill', '--signal', 'KILL', run.nodes[owner][0])
        donor_state = state(ports[donor], key)
        state(ports[donor], key, 'peer-only-retirement', donor_state['Generation'])
        # Kill the paused server before restarting it: buffered old-leader PUTs
        # cannot subsequently win when the server resumes. Its writable layer
        # retains the warm objects; no object payloads are read by this harness.
        docker('kill', '--signal', 'KILL', store)
        paused.remove(store)
        docker('start', store)
        run.s3 = boto3.client('s3', endpoint_url=f'http://127.0.0.1:{run.port(store, 9000)}',
                             aws_access_key_id='qualification', aws_secret_access_key='qualification-only',
                             region_name='us-east-1', config=Config(connect_timeout=3, read_timeout=5,
                             retries={'max_attempts': 1}, s3={'addressing_style': 'path'}))
        for _ in range(35):
            if state(ports[donor], key)['Phase'] == 'Stopped':
                break
            time.sleep(1)
        else:
            raise RuntimeError('donor did not produce live stopped evidence')
        docker('unpause', run.nodes[survivor][0])
        paused.remove(run.nodes[survivor][0])
        recovered = run.recover(owner, generation, 'peer-only-recovered', seconds=90)
        if not recovered['candidate_complete']:
            raise RuntimeError('peer-only recovery not positively sealed without loss')
        result = run.verify(survivor)
        run.results.append({'scenario': 'peer-only-owner-loss-and-donor-retirement',
                            'owner': owner, 'donor': donor, 'survivor': survivor,
                            'acknowledged_while_store_paused': True,
                            'owner_killed_before_store_restart': True,
                            'donor_launcher_stopped': True,
                            **recovered, **result})
    finally:
        for name in paused:
            docker('unpause', name)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', type=pathlib.Path, required=True)
    parser.add_argument('--launcher-binary', type=pathlib.Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    run = Run(args.output.resolve())
    def deadline(_sig, _frame):
        raise TimeoutError('peer-only experiment exceeded 420 seconds')
    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(420)
    try:
        experiment(run, args.launcher_binary.resolve())
    finally:
        signal.alarm(0)
        try:
            run.save('results', run.results)
        finally:
            run.cleanup()


if __name__ == '__main__':
    main()
