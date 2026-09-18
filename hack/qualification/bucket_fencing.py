#!/usr/bin/env python3
"""Local Bucket logical fencing experiments; no AWS or Kubernetes access."""
import argparse
import concurrent.futures
import json
import pathlib
import signal
import time
from run import Run, docker, http

class BucketFencing(Run):
    def sequence(self, node, op, delay=0):
        code, raw = http(self.nodes[node][1], f'/?cell=counter&sequence=1&id={op}&delay={delay}', 'PUT')
        if code != 200:
            return {'id': op, 'status': code, 'body': raw[:300]}
        value = json.loads(raw)
        if not isinstance(value.get('sequence'), int):
            raise RuntimeError(f'invalid acknowledgement: {value}')
        self.ledger.append(value)
        self.save('acknowledged-ledger', self.ledger)
        return value

    def verify_sequence(self, node):
        seen = {}
        for op in self.ledger:
            if op['sequence'] in seen and seen[op['sequence']] != op['id']:
                raise RuntimeError('conflicting successful writes: '+json.dumps([seen[op['sequence']], op]))
            seen[op['sequence']] = op['id']
            code, raw = http(self.nodes[node][1], f'/?cell=counter&sequence=1&id={op["id"]}')
            if code != 200 or json.loads(raw) != op:
                raise RuntimeError('acknowledged sequence lost: '+json.dumps([op,code,raw]))
        return {'acknowledged': len(self.ledger), 'distinct_sequences': len(seen), 'missing': []}

    def experiment(self):
        network=self.name+'-storage'
        docker('network','create',network)
        try:
            docker('network','connect',network,self.name+'-store')
            store_ip=json.loads(docker('inspect',self.name+'-store'))[0]['NetworkSettings']['Networks'][network]['IPAddress']
            self._experiment(network,store_ip)
        finally:
            for name in [self.name+'-store', *[n[0] for n in self.nodes.values()]]:
                try: docker('network','disconnect',network,name)
                except RuntimeError: pass
            docker('network','rm',network)

    def _experiment(self, network, store_ip):
        for node in ('a','b','c'):
            self.start(node,'bucket',('-e','S3_ENDPOINT=http://'+store_ip+':9000'))
            docker('network','connect',network,self.nodes[node][0])
            if self.ready(node) is None:
                raise RuntimeError('startup failed: '+node)
        for i in range(10):
            self.sequence('a',f'initial-{i}')
        _, state = self.state('a','before-pause')
        # Locate the actual owner; ingress selection is not evidence of ownership.
        owner = None
        for node in ('a','b','c'):
            _, state = self.state(node,'owner-'+node)
            if state['owned_cells'] > 0:
                if owner is not None:
                    raise RuntimeError('counter has ambiguous owners')
                owner=node
        if owner is None:
            raise RuntimeError('counter owner absent')
        survivor=next(n for n in ('a','b','c') if n!=owner)
        old=self.nodes[owner][0]
        before=self.metadata('before-pause')
        # Pause during a committed application write's delayed response. The
        # timeout is ambiguous; never claim that a missing response means no write.
        with concurrent.futures.ThreadPoolExecutor(max_workers=1) as pool:
            pending=pool.submit(self.sequence,owner,'in-flight-before-pause',2000)
            time.sleep(0.3)
            docker('pause',old)
            try:
                time.sleep(12)
                self.metadata('paused-expired')
                takeover=self.sequence(survivor,'takeover')
                if 'sequence' not in takeover:
                    raise RuntimeError('survivor did not acknowledge takeover: '+json.dumps(takeover))
                _, after=self.state(survivor,'survivor-after-takeover')
                if after['owned_cells'] < 1:
                    raise RuntimeError('survivor did not own counter')
                self.results.append({'scenario':'paused-owner-takeover','old_process_running':docker('inspect','--format','{{.State.Running}}',old)=='true','old_process_paused':True,**self.verify_sequence(survivor)})
            finally:
                docker('unpause',old)
            try:
                pending_result=pending.result(timeout=8)
            except (OSError, TimeoutError) as exc:
                pending_result={'ambiguous_response':type(exc).__name__}
        for i in range(10):
            self.sequence(survivor,f'after-resume-{i}')
        time.sleep(2)
        old_running=docker('inspect','--format','{{.State.Running}}',old)=='true'
        self.results.append({'scenario':'resumed-old-owner','old_process_running':old_running,'in_flight':pending_result,**self.verify_sequence(survivor)})
        self.metadata('after-resume')
        # Selective peer partition with a SECOND network carrying S3 traffic.
        # A live renewing lease must prevent claiming logical removal completion.
        isolated=survivor
        name=self.nodes[isolated][0]
        try:
            start=self.metadata('before-peer-partition')['nodes']['nodes/'+isolated+'.json']['expires_ms']
            docker('network','disconnect',self.name,name)
            time.sleep(15)
            after=self.metadata('peer-partition-s3-live')['nodes']['nodes/'+isolated+'.json']
            alive=docker('inspect','--format','{{.State.Running}}',name)=='true'
            if not alive or after['expires_ms'] <= start or after['expires_ms'] <= int(time.time()*1000):
                raise RuntimeError('partition did not preserve live S3 lease')
            self.nodes[isolated]=(name,self.port(name,8080),self.port(name,8081))
            for i in range(5):
                result=self.sequence(isolated,f'isolated-s3-owner-{i}')
                if 'sequence' not in result: raise RuntimeError('isolated owner did not acknowledge with S3: '+json.dumps(result))
            self.results.append({'scenario':'peer-partition-retains-s3','process_running':alive,'lease_renewed':True,'logical_removal_must_block':True,**self.verify_sequence(isolated)})
            docker('network','connect','--alias',isolated,self.name,name)
            self.nodes[isolated]=(name,self.port(name,8080),self.port(name,8081))
            if self.ready(isolated) is None: raise RuntimeError('reconnected node not ready')
            for i in range(10):
                self.sequence(isolated,f'reconnected-{i}')
            self.results.append({'scenario':'peer-reconnect',**self.verify_sequence(isolated)})
        finally:
            pass


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--output',type=pathlib.Path,required=True)
    args=parser.parse_args()
    args.output.mkdir(parents=True,exist_ok=False)
    run=BucketFencing(args.output.resolve())
    def deadline(*_): raise TimeoutError('Bucket fencing qualification exceeded420 seconds')
    signal.signal(signal.SIGALRM,deadline)
    signal.alarm(420)
    try:
        run.setup()
        run.experiment()
    except Exception as exc:
        run.results.append({'error':str(exc)})
        raise
    finally:
        signal.alarm(0)
        try: run.save('results',run.results)
        finally: run.cleanup()

if __name__=='__main__': main()
