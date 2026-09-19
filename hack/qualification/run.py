#!/usr/bin/env python3
"""Bounded, local-only runtime experiments. Never uses a Kubernetes/AWS context."""
import argparse
import base64
import hashlib
import io
import tarfile
import json
import pathlib
import signal
import subprocess
import time
import threading
import urllib.request
import urllib.error
import uuid

import boto3
from botocore.config import Config

ROOT = pathlib.Path(__file__).resolve().parents[2]
APP = ROOT / 'hack/qualification/app'
COMMIT = '12d5b6333fe52717325addcfe1e99e9fd4f77bcd'
IMAGE = 'ghcr.io/denoland/celld@sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8'
MINIO = 'quay.io/minio/minio@sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e'


def docker(*args, timeout=90):
    try:
        return subprocess.check_output(['docker', *args], text=True, stderr=subprocess.STDOUT, timeout=timeout).strip()
    except subprocess.CalledProcessError as e:
        raise RuntimeError(e.output) from e


def http(port, path, method='GET'):
    try:
        with urllib.request.urlopen(urllib.request.Request(f'http://127.0.0.1:{port}{path}', method=method), timeout=5) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()


class Run:
    def __init__(self, out):
        self.out = out
        self.name = 'celld-q-' + uuid.uuid4().hex[:10]
        self.containers = []
        self.volumes = []
        self.results = []
        self.nodes = {}
        self.ledger = []
        self.health_checks = 0
        self.env = ['-e', 'AWS_ACCESS_KEY_ID=qualification', '-e', 'AWS_SECRET_ACCESS_KEY=qualification-only',
                    '-e', 'AWS_REGION=us-east-1', '-e', 'AWS_ALLOW_HTTP=true',
                    '-e', 'S3_ENDPOINT=http://store:9000', '-e', 'CELLD_BUCKET=s3://qualification']

    def save(self, name, value):
        (self.out / (name + '.json')).write_text(json.dumps(value, indent=2) + '\n')

    def port(self, name, port):
        for _ in range(30):
            try:
                return int(docker('port', name, str(port)).split(':')[-1])
            except (RuntimeError, ValueError):
                time.sleep(0.2)
        raise RuntimeError('port was not published: ' + name)

    def setup(self):
        self.save('harness', {'sha256': hashlib.sha256(pathlib.Path(__file__).read_bytes()).hexdigest()})
        docker('pull', IMAGE, timeout=180)
        (self.out / 'manifest.txt').write_text(docker('buildx', 'imagetools', 'inspect', IMAGE))
        docker('network', 'create', self.name)
        name = self.name + '-store'
        self.containers.append(name)
        docker('run', '-d', '--name', name, '--network', self.name, '--network-alias', 'store',
               '-p', '127.0.0.1::9000', '-e', 'MINIO_ROOT_USER=qualification',
               '-e', 'MINIO_ROOT_PASSWORD=qualification-only', MINIO, 'server', '/data')
        self.s3 = boto3.client('s3', endpoint_url=f'http://127.0.0.1:{self.port(name, 9000)}',
                              aws_access_key_id='qualification', aws_secret_access_key='qualification-only',
                              region_name='us-east-1', config=Config(connect_timeout=3, read_timeout=5,
                              retries={'max_attempts': 1}, s3={'addressing_style': 'path'}))
        for _ in range(30):
            try:
                self.s3.create_bucket(Bucket='qualification')
                break
            except Exception:
                time.sleep(1)
        else:
            raise RuntimeError('local store did not start')
        image = json.loads(docker('image', 'inspect', IMAGE))
        if image[0]['Config']['Labels'].get('org.opencontainers.image.revision') != COMMIT:
            raise RuntimeError('image revision mismatch')
        self.save('image', image)
        self.save('version', {'version': docker('run', '--rm', IMAGE, '--version')})
        arch = json.loads(docker('image', 'inspect', IMAGE))[0]['Architecture']
        packages = {
            'arm64': ('arm64', '8bwX7a8FghIgrupcxb4aUmYDLp8pX06rGh5HqDT7bB+8Rdells6mHvrFHHW2JAOPZUbnjUpKTLg6ECyzvas2AQ=='),
            'amd64': ('x64', 'uqZMTLr/zR/ed4jIGnwSLkaHmPjOjJvnm6TVVitAa08SLS9Z0VM8wIRx7gWbJB5/J54YuIMInDquWyYvQLZkgw=='),
        }
        package, integrity = packages[arch]
        with urllib.request.urlopen(f'https://registry.npmjs.org/@esbuild/linux-{package}/-/linux-{package}-0.25.12.tgz', timeout=30) as response:
            archive = response.read()
        if base64.b64encode(hashlib.sha512(archive).digest()).decode() != integrity:
            raise RuntimeError('esbuild integrity mismatch')
        with tarfile.open(fileobj=io.BytesIO(archive), mode='r:gz') as tar:
            executable = tar.extractfile('package/bin/esbuild').read()
        esbuild = self.out / 'esbuild'
        esbuild.write_bytes(executable)
        esbuild.chmod(0o755)
        deploy = self.name + '-deploy'
        self.containers.append(deploy)
        docker('create', '--name', deploy, '--network', self.name, *self.env,
               '-e', 'CELLD_ESBUILD=/esbuild', IMAGE, 'deploy', '/app')
        docker('cp', str(APP) + '/.', deploy + ':/app')
        docker('cp', str(esbuild), deploy + ':/esbuild')
        output = docker('start', '--attach', deploy)
        (self.out / 'deploy.log').write_text(output)
        docker('rm', deploy)
        self.containers.remove(deploy)

    def start(self, node, mode, extra=()):
        name = self.name + '-' + node
        volume = name + '-disk'
        if volume not in self.volumes:
            docker('volume', 'create', volume)
            self.volumes.append(volume)
        self.containers.append(name)
        docker('run', '-d', '--name', name, '--network', self.name, '--network-alias', node,
               '--memory', '768m', '--shm-size', '128m', '-p', '127.0.0.1::8080', '-p', '127.0.0.1::8081',
               *self.env, '-e', 'CELLD_NODE=' + node, '-e', 'CELLD_WATCH=/work',
               '-e', 'CELLD_ADDR=0.0.0.0:8080', '-e', 'CELLD_INTERNAL_ADDR=0.0.0.0:8081',
               '-e', 'CELLD_ADVERTISE=' + node + ':8081', '-e', 'CELLD_DURABILITY=' + mode,
               '-e', 'CELLD_TOKIO_THREADS=2', '-e', 'CELLD_SHUTDOWN_TOTAL_MS=5000',
               *extra, '-v', volume + ':/work', IMAGE)
        self.nodes[node] = (name, self.port(name, 8080), self.port(name, 8081))

    def ready(self, node, seconds=45):
        started = time.monotonic()
        self.health_checks += 1
        label = node + '-health-' + str(self.health_checks)
        samples = []
        while time.monotonic() - started < seconds:
            sample = {'seconds': round(time.monotonic() - started, 3)}
            try:
                sample['status'] = http(self.nodes[node][1], '/.well-known/celld/health')[0]
            except (OSError, TimeoutError) as error:
                sample['error'] = type(error).__name__
            samples.append(sample)
            self.save(label, samples)
            if sample.get('status') == 200:
                return round(time.monotonic() - started, 3)
            time.sleep(1)
        return None

    def metadata(self, label):
        # GET only nodes; list log names, never loss bodies or application objects.
        records, keys = {}, []
        seen = set()
        pages = 0
        for prefix in ('nodes/', 'log/'):
            complete = False
            continuations = set()
            for page in self.s3.get_paginator('list_objects_v2').paginate(Bucket='qualification', Prefix=prefix):
                if complete:
                    raise RuntimeError('page received after final page')
                pages += 1
                truncated = page.get('IsTruncated')
                if pages > 1000 or not isinstance(truncated, bool):
                    raise RuntimeError('incomplete or over-budget listing')
                if truncated != bool(page.get('NextContinuationToken')):
                    raise RuntimeError('inconsistent continuation evidence')
                token = page.get('NextContinuationToken')
                if token:
                    if token in continuations:
                        raise RuntimeError('repeated continuation token')
                    continuations.add(token)
                complete = not truncated
                for obj in page.get('Contents', []):
                    key = obj['Key']
                    if not key.startswith(prefix) or key in seen or len(seen) >= 10000:
                        raise RuntimeError('invalid, duplicate, or over-budget key inventory')
                    seen.add(key)
                    keys.append(key)
                    if prefix == 'nodes/':
                        body = self.s3.get_object(Bucket='qualification', Key=key)['Body']
                        try:
                            raw = body.read(1048577)
                            if len(raw) > 1048576:
                                raise RuntimeError('node metadata exceeds budget')
                            records[key] = json.loads(raw)
                        finally:
                            body.close()
            if not complete:
                raise RuntimeError('listing ended without a final page')
        value = {'observed_unix_ms': int(time.time()*1000), 'complete': True,
                 'nodes': records, 'log_keys': [k for k in keys if k.startswith('log/')]}
        self.save(label + '-metadata', value)
        return value

    def state(self, node, label):
        code, raw = http(self.nodes[node][2], '/state')
        if code != 200:
            raise RuntimeError('HTTP state unavailable: ' + str(code))
        value = json.loads(raw)
        self.save(label + '-state', value)
        return code, value

    def writes(self, node, count=10):
        cell = node + '-' + str(len(self.ledger))
        for _ in range(count):
            op = 'op-' + str(len(self.ledger))
            code, raw = http(self.nodes[node][1], '/?id=' + op + '&cell=' + cell, 'PUT')
            if code != 200 or json.loads(raw) != {'id': op, 'stored': True}:
                raise RuntimeError(f'write failed: {code} {raw}')
            self.ledger.append({'id': op, 'cell': cell})
            self.save('acknowledged-ledger', self.ledger)

    def verify(self, node):
        missing = []
        for op in self.ledger:
            code, raw = http(self.nodes[node][1], '/?id=' + op['id'] + '&cell=' + op['cell'])
            if code != 200 or json.loads(raw).get('stored') is not True:
                missing.append(op)
        if missing:
            raise RuntimeError('acknowledged operations missing: ' + json.dumps(missing))
        return {'acknowledged': len(self.ledger), 'missing': missing}

    def stop(self, node, signal):
        name = self.nodes[node][0]
        start = time.monotonic()
        docker('kill', '--signal', signal, name)
        code = docker('wait', name, timeout=25)
        return {'exit_code': int(code), 'stop_seconds': round(time.monotonic()-start, 3)}

    def recover(self, node, generation, label, seconds=45):
        start = time.monotonic()
        while True:
            evidence = self.metadata(label)
            record = evidence['nodes'].get('nodes/' + node + '.json', {})
            matches = record.get('ownership_index_generation', record.get('probe_public_key')) == generation
            loss = [k for k in evidence['log_keys'] if k.endswith('.loss.json')]
            if matches and record.get('log', {}).get('state') == 'sealed':
                return {'candidate_complete': not loss, 'loss': loss, 'recovery_seconds': round(time.monotonic()-start, 3)}
            if time.monotonic()-start >= seconds:
                return {'candidate_complete': False, 'loss': loss, 'recovery_seconds': round(time.monotonic()-start, 3), 'reason': 'no matching sealed record'}
            time.sleep(2)

    def scenarios(self, mode, second_signal='KILL'):
        for n in ('a', 'b', 'c'):
            self.start(n, mode)
            ready = self.ready(n)
            self.results.append({'scenario': 'startup-' + n, 'ready_seconds': ready})
            if ready is None:
                raise RuntimeError('startup not ready: ' + n)
        self.writes('a', 30)
        self.state('a', 'before-graceful')
        before = self.metadata('before-graceful')
        generation = before['nodes']['nodes/a.json']['ownership_index_generation']
        stopped = self.stop('a', 'TERM')
        self.metadata('immediate-graceful')
        recovered = self.recover('a', generation, 'after-graceful')
        self.results.append({'scenario': 'graceful', **stopped, **recovered, **self.verify('b')})
        self.results.append({'scenario': 'next-controlled-removal-gate', 'blocked': not recovered['candidate_complete']})
        # A normal second removal must block; abrupt failure injection is independent.
        if second_signal == 'TERM' and not recovered['candidate_complete']:
            return
        self.writes('b', 30)
        before = self.metadata('before-abrupt')
        generation = before['nodes']['nodes/b.json']['ownership_index_generation']
        stopped = self.stop('b', second_signal)
        self.metadata('immediate-second-removal')
        recovered = self.recover('b', generation, 'after-abrupt')
        self.results.append({'scenario': 'abrupt-process-loss' if second_signal == 'KILL' else 'second-graceful', **stopped, **recovered, **self.verify('c')})
        if second_signal == 'TERM':
            return
        # A full default TTL after confirmed exit; reuse exact named volume and node ID.
        time.sleep(10)
        (self.out / 'b-before-restart.log').write_text(docker('logs', self.nodes['b'][0]))
        docker('rm', self.nodes['b'][0])
        self.containers.remove(self.nodes['b'][0])
        self.start('b', mode, ('-e', 'CELLD_SHUTDOWN_TOTAL_MS=1'))
        ready = self.ready('b')
        self.results.append({'scenario': 'retained-disk-restart', 'ready_seconds': ready, **self.verify('b')})
        self.writes('b', 10)
        before = self.metadata('before-deadline')
        generation = before['nodes']['nodes/b.json']['ownership_index_generation']
        # Pause only this isolated emulator, cutting off uploads during shutdown.
        docker('pause', self.name + '-store')
        try:
            stopped = self.stop('b', 'TERM')
        finally:
            docker('unpause', self.name + '-store')
        self.metadata('immediate-deadline')
        recovered = self.recover('b', generation, 'after-deadline')
        self.results.append({'scenario': 'deadline-cut-storage-stall', **stopped, **recovered, **self.verify('c')})

    def partition(self):
        for node in ('a', 'b', 'c'):
            self.start(node, 'fleet')
            if self.ready(node) is None:
                raise RuntimeError('startup failed before partition')
        self.writes('a', 30)
        before = self.metadata('before-partition')
        generation = before['nodes']['nodes/a.json']['ownership_index_generation']
        started = time.monotonic()
        docker('network', 'disconnect', self.name, self.nodes['a'][0])
        running = True
        while time.monotonic() - started < 25:
            running = docker('inspect', '--format', '{{.State.Running}}', self.nodes['a'][0]) == 'true'
            if not running:
                break
            time.sleep(1)
        result = {'scenario': 'network-partition', 'process_still_running': running,
                  'observation_seconds': round(time.monotonic()-started, 3)}
        if running:
            result['forced_stop'] = self.stop('a', 'KILL')
        else:
            result['exit_code'] = int(docker('wait', self.nodes['a'][0]))
        result.update(self.recover('a', generation, 'after-partition'))
        result.update(self.verify('b'))
        self.results.append(result)

    def pressure(self):
        self.start('a', 'fleet', ('-e', 'CELLD_MAX_RSS_MB=64'))
        ready = self.ready('a')
        self.results.append({'scenario': 'incumbent-startup', 'ready_seconds': ready})
        if ready is None:
            raise RuntimeError('incumbent never became ready before pressure')
        self.writes('a', 5)
        # A tmpfs allocation charges this container's cgroup without editing celld.
        docker('exec', self.nodes['a'][0], 'sh', '-c', 'dd if=/dev/zero of=/dev/shm/pressure bs=1048576 count=96')
        stop = threading.Event()
        def demand():
            while not stop.is_set():
                try:
                    # Cold reads generate admission demand, without untracked write acks.
                    http(self.nodes['a'][1], '/?id=unwritten&cell=cold-demand')
                except (OSError, TimeoutError):
                    pass
                stop.wait(0.1)
        worker = threading.Thread(target=demand, daemon=True)
        worker.start()
        try:
            time.sleep(5)
            _, state = self.state('a', 'pressured-incumbent')
            if state['node_load']['pressured'] is not True:
                raise RuntimeError('pressure injection did not establish pressured=true')
            self.start('b', 'fleet', ('-e', 'CELLD_READY_FLEET_GATE_MS=3000'))
            ready = self.ready('b', seconds=20)
            self.state('b', 'joining')
            self.state('a', 'incumbent-after-join')
            self.results.append({'scenario': 'pressured-startup', 'ready_seconds': ready, 'observation_window_seconds': 20})
            self.metadata('pressure')
        finally:
            stop.set()
            worker.join(timeout=6)
            docker('exec', self.nodes['a'][0], 'rm', '/dev/shm/pressure')
        self.results.append({'scenario': 'pressure-cleared', 'ready_seconds': self.ready('b', seconds=30), **self.verify('b')})

    def cleanup(self):
        failures = []
        for name in self.containers:
            try:
                (self.out / (name.rsplit('-', 1)[-1] + '.log')).write_text(docker('logs', name))
            except Exception as e:
                print('log capture:', e)
            try:
                docker('rm', '-f', name)
            except Exception as e:
                failures.append(f'{name}: {e}')
        for volume in self.volumes:
            try:
                docker('volume', 'rm', volume)
            except Exception as e:
                failures.append(f'{volume}: {e}')
        try:
            docker('network', 'rm', self.name)
        except Exception as e:
            failures.append(f'{self.name}: {e}')
        if failures:
            raise RuntimeError('cleanup incomplete: ' + '; '.join(failures))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--scenario', choices=['bucket', 'fleet', 'pressure', 'contraction', 'partition'], required=True)
    parser.add_argument('--output', type=pathlib.Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    run = Run(args.output.resolve())
    def deadline(_signal, _frame):
        raise TimeoutError('qualification exceeded 420 seconds')
    signal.signal(signal.SIGALRM, deadline)
    signal.alarm(420)
    try:
        run.setup()
        if args.scenario == 'partition':
            run.partition()
        elif args.scenario == 'contraction':
            run.scenarios('fleet', second_signal='TERM')
        elif args.scenario == 'pressure':
            run.pressure()
        else:
            run.scenarios(args.scenario)
    except Exception as e:
        run.results.append({'error': str(e)})
        raise
    finally:
        signal.alarm(0)
        try:
            run.save('results', run.results)
        finally:
            run.cleanup()


if __name__ == '__main__':
    main()
