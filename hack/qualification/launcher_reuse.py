#!/usr/bin/env python3
"""Exercise the inherited lock using the real unmodified runtime, locally only."""
import argparse
import hashlib
import hmac
import json
import os
import pathlib
import signal
import time
import urllib.request

from run import Run, docker


def state(port, key, operation='', generation=''):
    body = {'Nonce': os.urandom(32).hex(), 'Operation': operation, 'Generation': generation, 'NotAfterMS': int(time.time()*1000)+3000}
    data = json.dumps(body, separators=(',', ':')).encode()
    signature = hmac.new(key, b'request\0' + data, hashlib.sha256).hexdigest()
    request = urllib.request.Request(f'http://127.0.0.1:{port}/v1', data=data,
                                     headers={'X-Celld-MAC': signature})
    with urllib.request.urlopen(request, timeout=3) as response:
        result = json.load(response)
    signed = json.dumps({'Nonce': result['Nonce'], 'State': result['State']}, separators=(',', ':')).encode()
    expected = hmac.new(key, b'response\0' + signed, hashlib.sha256).hexdigest()
    if result['Nonce'] != body['Nonce'] or not hmac.compare_digest(expected, result['MAC']):
        raise RuntimeError('invalid launcher response authentication')
    return result['State']


def experiment(run, binary):
    run.setup()
    key = os.urandom(32)
    key_path = run.out / 'launcher-key'
    key_path.write_bytes(key)
    def start(node):
        run.start(node, 'fleet', extra=('--entrypoint', '/celld-launcher',
                  '-v', str(binary) + ':/celld-launcher:ro',
                  '-v', str(key_path) + ':/launcher-key/key:ro',
                  '-e', 'POD_UID=' + os.urandom(16).hex(), '-e', 'NODE_NAME=local-host',
                  '-p', '127.0.0.1::8083'))
        return run.port(run.nodes[node][0], 8083)
    old_port = start('a')
    if run.ready('a', seconds=60) is None:
        raise RuntimeError('original runtime not ready')
    first = state(old_port, key)
    record = json.loads(run.s3.get_object(Bucket='qualification', Key='nodes/a.json')['Body'].read())
    if first['Generation'] != record['ownership_index_generation']:
        raise RuntimeError('launcher generation does not match real runtime')
    original = run.nodes['a'][0]
    inherited = docker('exec', original, '/bin/sh', '-c', f'readlink /proc/{first["PID"]}/fd/3')
    if inherited != '/work/.celld-launcher.lock':
        raise RuntimeError('real celld did not retain inherited lock descriptor')
    old = original + '-old'
    docker('pause', original)
    docker('rename', original, old)
    run.containers[run.containers.index(original)] = old
    time.sleep(12)
    new_port = start('a')
    time.sleep(3)
    replacement = state(new_port, key)
    if replacement['Phase'] != 'WaitingForExclusiveVolume' or replacement['PID'] != 0:
        raise RuntimeError('replacement started while old invocation retained lock')
    docker('rm', '-f', old)
    run.containers.remove(old)
    ready = run.ready('a', seconds=60)
    if ready is None:
        raise RuntimeError('replacement failed after old invocation removed')
    current = state(new_port, key)
    run.results.append({'scenario': 'launcher-paused-owner-lock',
                        'generation_matches_runtime': True,
                        'real_celld_inherited_lock_descriptor': inherited,
                        'replacement_blocked_phase': replacement['Phase'],
                        'replacement_child_pid_while_blocked': replacement['PID'],
                        'replacement_ready_after_removing_old_seconds': ready,
                        'new_generation': current['Generation'] != first['Generation'],
                        'peer_only_acknowledgements_tested': False})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', type=pathlib.Path, required=True)
    parser.add_argument('--launcher-binary', type=pathlib.Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    run = Run(args.output.resolve())
    def deadline(_sig, _frame):
        raise TimeoutError('launcher experiment exceeded 240 seconds')
    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(240)
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
