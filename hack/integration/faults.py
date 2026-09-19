"""Fault injection in the harness-owned kind cluster.

Call only from run.py --faults after its identity, isolation and drift fixtures
pass. Scenarios, in order:

1. Manager crash after the replica CAS, before the journal records it (scale-out).
2. Manager crash after durable intent, before the replica CAS (contraction).
3. Node loss for a Bucket replica: cordon plus pod deletion, contraction blocked.
4. Node loss for a launcher-managed PersistentFleet replica: retained claim,
   no decrement, same-host return.
5. S3 latency through toxiproxy: contraction still completes, write readable.
6. S3 partition through toxiproxy while a contraction is issued: no further
   decrement, no completion or loss from absent evidence, write readable after
   recovery, and whatever the post-outage outcome is, exactly one effect.

Every assertion is about fail-closed behavior. This module never discovers a
kubeconfig, touches AWS, or changes anything outside the harness cluster.
"""
import json
import time

JOURNAL = 'celld.example.com/lifecycle-journal'
OPERATION = 'celld.example.com/lifecycle-operation'


def exercise(k, apply, get, wait_for, ready, curl, probe, cluster, curl_image, set_operator_fault):
    def journal(fleet):
        values = json.loads(k('get', 'celldstoragereservations', '-o', 'json'))['items']
        value = next(v for v in values if v['spec']['fleetName'] == fleet and v['spec']['fleetNamespace'] == 'fleets')
        return json.loads(value['metadata'].get('annotations', {}).get(JOURNAL, 'null'))

    def workload(fleet):
        kind = 'deployment' if get('celldfleet', fleet)['spec']['profile'] == 'Bucket' else 'statefulset'
        return get(kind, fleet)

    def ready_reason(fleet):
        for c in get('celldfleet', fleet).get('status', {}).get('conditions', []):
            if c['type'] == 'Ready':
                return c['reason']
        return ''

    def operation_id(fleet):
        return get('celldfleet', fleet).get('status', {}).get('lifecycle', {}).get('operationID')

    def settled(fleet, count):
        return workload(fleet)['spec']['replicas'] == count and ready(fleet) and not journal(fleet).get('Operation')

    def completions(fleet, op):
        return [h for h in journal(fleet).get('History', []) if h['ID'] == op]

    def fleet_pods(fleet):
        uid = get('celldfleet', fleet)['metadata']['uid']
        return json.loads(k('-n', 'fleets', 'get', 'pods', '-l', 'celld.example.com/fleet-uid=' + uid, '-o', 'json'))['items']

    def operator_pod():
        pods = json.loads(k('-n', 'celld-system', 'get', 'pods', '-l', 'app.kubernetes.io/name=celld-operator', '-o', 'json'))['items']
        # The harness also labels a probe pod in this namespace; keep only the Deployment's pods.
        live = [p for p in pods if not p['metadata'].get('deletionTimestamp') and p['metadata']['name'].startswith('celld-operator-')]
        assert len(live) == 1, [p['metadata']['name'] for p in live]
        return live[0]

    def operator_restarts():
        return sum(s.get('restartCount', 0) for s in operator_pod()['status'].get('containerStatuses', []))

    def crashed_at(point):
        pod = operator_pod()['metadata']['name']
        try:
            previous = k('-n', 'celld-system', 'logs', pod, '--previous')
        except RuntimeError:
            return False
        return 'injected crash at lifecycle fault point "%s"' % point in previous

    def hold(seconds, invariant, description):
        until = time.monotonic() + seconds
        while time.monotonic() < until:
            assert invariant(), 'violated during hold: ' + description
            time.sleep(2)
        print('PASS:', description, 'held for', seconds, 's', flush=True)

    def write_readable(pod, fleet):
        return json.loads(curl(pod, fleet, '/?cell=integration&id=ack', 8080))['stored'] is True

    # ---- 1. crash after the replica effect, before the journal transition ----
    wait_for(lambda: settled('alpha', 2), 'alpha at baseline before crash injection', timeout=240)
    generation = get('deployment', 'alpha')['metadata']['generation']
    set_operator_fault('after-effect')
    k('-n', 'fleets', 'patch', 'celldfleet', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'replicas': 3}}))
    wait_for(lambda: get('deployment', 'alpha')['spec']['replicas'] == 3 and operator_restarts() >= 1 and crashed_at('after-effect'),
             'manager exited after the replica CAS with the journal still at the prior count', timeout=240)
    issued = get('deployment', 'alpha')['metadata']['annotations'][OPERATION]
    # The restarted manager reconstructs the issued effect from the workload alone.
    wait_for(lambda: settled('alpha', 3) and journal('alpha')['Applied'] == 3, 'restart reconstructs the issued addition', timeout=240)
    assert len(completions('alpha', issued)) == 1, completions('alpha', issued)
    assert get('deployment', 'alpha')['metadata']['generation'] == generation + 1, 'more than one workload spec write'
    set_operator_fault(None)
    print('PASS: crash after effect: one replica write, one completion record', flush=True)

    # ---- 2. crash after durable intent, before the replica effect ----
    generation = get('deployment', 'alpha')['metadata']['generation']
    set_operator_fault('before-effect')
    k('-n', 'fleets', 'patch', 'celldfleet', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'replicas': 2}}))
    wait_for(lambda: operator_restarts() >= 1 and crashed_at('before-effect'), 'manager exited after journaling contraction intent, before the CAS', timeout=420)
    intent = journal('alpha')['Operation']
    assert intent and intent['From'] == 3 and intent['To'] == 2, intent
    assert get('deployment', 'alpha')['spec']['replicas'] == 3
    assert get('deployment', 'alpha')['metadata']['generation'] == generation, 'effect issued despite the crash'
    hold(20, lambda: get('deployment', 'alpha')['spec']['replicas'] == 3 and get('deployment', 'alpha')['metadata']['generation'] == generation,
         'crash-looping manager never issues the effect')
    set_operator_fault(None)
    wait_for(lambda: settled('alpha', 2), 'recovered manager issues the journaled contraction exactly once', timeout=420)
    assert get('deployment', 'alpha')['metadata']['generation'] == generation + 1, 'more than one workload spec write'
    assert len(completions('alpha', intent['ID'])) == 1, completions('alpha', intent['ID'])
    assert write_readable('client', 'alpha')
    print('PASS: crash before effect: intent retained, one decrement after restart, write readable', flush=True)

    # ---- 3. node loss for a Bucket replica ----
    k('-n', 'fleets', 'patch', 'celldfleet', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'replicas': 3}}))
    wait_for(lambda: settled('alpha', 3), 'alpha at three before node loss', timeout=240)
    victim = next(p for p in fleet_pods('alpha') if p['spec']['nodeName'] == cluster + '-worker2')
    k('cordon', cluster + '-worker2')
    try:
        k('-n', 'fleets', 'delete', 'pod', victim['metadata']['name'], '--wait=false')

        def replacement_pending():
            pods = fleet_pods('alpha')
            pending = [p for p in pods if p['status'].get('phase') == 'Pending' and not p['metadata'].get('deletionTimestamp')]
            return len(pods) >= 3 and any(any(c.get('reason') == 'Unschedulable' for c in p['status'].get('conditions', [])) for p in pending)
        wait_for(replacement_pending, 'strict hostname separation leaves the replacement Pending on a cordoned cluster', timeout=120)
        generation = get('deployment', 'alpha')['metadata']['generation']
        k('-n', 'fleets', 'patch', 'celldfleet', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'replicas': 2}}))
        time.sleep(15)
        hold(45, lambda: get('deployment', 'alpha')['spec']['replicas'] == 3 and get('deployment', 'alpha')['metadata']['generation'] == generation,
             'contraction is not issued while a replica is unavailable (reason: %s)' % ready_reason('alpha'))
        assert ready_reason('alpha') not in ('Ready', 'Provisioning'), ready_reason('alpha')
    finally:
        k('uncordon', cluster + '-worker2')
    wait_for(lambda: settled('alpha', 2), 'contraction proceeds once every replica is observable again', timeout=420)
    assert write_readable('client', 'alpha')
    print('PASS: Bucket node loss: no decrement while unavailable, write readable after recovery', flush=True)

    # ---- 4. node loss for a launcher-managed PersistentFleet replica ----
    probe('beta-client', {'celld.example.com/client-of': 'beta'})
    curl('beta-client', 'beta', '/?cell=integration&id=ack', 8080, method='PUT')
    wait_for(lambda: settled('beta', 2), 'beta at baseline before node loss', timeout=240)
    host = get('pod', 'beta-1')['spec']['nodeName']
    claim_uid = get('pvc', 'data-beta-1')['metadata']['uid']
    sts_generation = get('statefulset', 'beta')['metadata']['generation']
    k('cordon', host)
    try:
        k('-n', 'fleets', 'delete', 'pod', 'beta-1', '--wait=true')
        wait_for(lambda: get('pod', 'beta-1')['status'].get('phase') == 'Pending', 'recreated ordinal waits for its retained host', timeout=120)
        hold(30, lambda: get('statefulset', 'beta')['spec']['replicas'] == 2 and get('pvc', 'data-beta-1')['metadata']['uid'] == claim_uid
             and get('statefulset', 'beta')['metadata']['generation'] == sts_generation,
             'no decrement, template change or claim replacement while the ordinal is unschedulable')
        assert not ready('beta')
    finally:
        k('uncordon', host)
    wait_for(lambda: settled('beta', 2) and get('pod', 'beta-1')['spec']['nodeName'] == host, 'ordinal returns on its original host with the same claim', timeout=300)
    assert get('pvc', 'data-beta-1')['metadata']['uid'] == claim_uid
    assert write_readable('beta-client', 'beta')
    print('PASS: PersistentFleet node loss: retained claim, same host, write readable', flush=True)

    # ---- toxiproxy control ----
    def toxic(method, path, body=None):
        args = ['-n', 'celld-test-store', 'exec', 'toxi-ctl', '--', 'curl', '--fail', '--silent', '--show-error', '--max-time', '5', '-X', method, 'http://toxiproxy-api:8474' + path]
        if body is not None:
            args += ['-H', 'Content-Type: application/json', '-d', json.dumps(body)]
        return k(*args)

    proxies = json.loads(toxic('GET', '/proxies'))
    assert proxies['minio']['enabled'] and proxies['minio']['listen'].endswith(':9000'), proxies

    # ---- 5. S3 latency ----
    toxic('POST', '/proxies/minio/toxics', {'name': 'latency', 'type': 'latency', 'stream': 'downstream', 'attributes': {'latency': 250, 'jitter': 50}})
    try:
        started = time.monotonic()
        k('-n', 'fleets', 'patch', 'celldfleet', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'replicas': 3}}))
        wait_for(lambda: settled('alpha', 3), 'growth under 250 ms S3 latency', timeout=300)
        k('-n', 'fleets', 'patch', 'celldfleet', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'replicas': 2}}))
        wait_for(lambda: settled('alpha', 2), 'contraction under 250 ms S3 latency', timeout=420)
        assert write_readable('client', 'alpha')
        print('PASS: S3 latency: shrink/grow completed in %.0f s, write readable' % (time.monotonic() - started), flush=True)
    finally:
        toxic('DELETE', '/proxies/minio/toxics/latency')

    # ---- 6. S3 partition during an issued contraction ----
    k('-n', 'fleets', 'patch', 'celldfleet', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'replicas': 3}}))
    wait_for(lambda: settled('alpha', 3), 'alpha at three before partition', timeout=300)
    applied_before = journal('alpha')['Applied']
    k('-n', 'fleets', 'patch', 'celldfleet', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'replicas': 2}}))
    deadline = time.monotonic() + 420
    caught = None
    while time.monotonic() < deadline:
        deployment = get('deployment', 'alpha')
        op = journal('alpha').get('Operation')
        if deployment['spec']['replicas'] == 2 and op:
            caught = op
            break
        if not op and deployment['spec']['replicas'] == 2:
            break
        time.sleep(0.5)
    toxic('POST', '/proxies/minio/toxics', {'name': 'partition', 'type': 'timeout', 'stream': 'upstream', 'attributes': {'timeout': 0}})
    try:
        if caught:
            print('Partition injected with operation', caught['ID'], 'in phase', caught['Phase'], flush=True)

            def frozen():
                j = journal('alpha')
                op = j.get('Operation')
                return (get('deployment', 'alpha')['spec']['replicas'] == 2 and j['Applied'] == applied_before and j.get('Loss', '') == ''
                        and op is not None and op['ID'] == caught['ID'] and not completions('alpha', caught['ID']))
            hold(45, frozen, 'issued contraction neither completes, decrements again, nor records loss while S3 is unreachable')
        else:
            print('Partition window missed: contraction completed before injection; testing steady-state outage', flush=True)
            hold(45, lambda: get('deployment', 'alpha')['spec']['replicas'] == 2 and journal('alpha').get('Loss', '') == '',
                 'no replica or loss change while S3 is unreachable')
    finally:
        toxic('DELETE', '/proxies/minio/toxics/partition')
    wait_for(lambda: all(p['status'].get('phase') == 'Running' and any(c['type'] == 'Ready' and c['status'] == 'True' for c in p['status'].get('conditions', []))
                         for p in fleet_pods('alpha') if not p['metadata'].get('deletionTimestamp')),
             'runtime pods serve again after the partition (self-fenced processes restarted)', timeout=300)
    assert write_readable('client', 'alpha')
    # Liveness after a partition is not promised: the operator may complete or
    # stay blocked on replaced generations. Safety is: one effect, no false completion.
    outcome_deadline = time.monotonic() + 240
    while time.monotonic() < outcome_deadline and journal('alpha').get('Operation'):
        time.sleep(3)
    final = journal('alpha')
    assert get('deployment', 'alpha')['spec']['replicas'] == 2
    if caught:
        assert len(completions('alpha', caught['ID'])) <= 1
        if final.get('Operation'):
            print('Post-partition outcome: operation %s remains open, reason %s (documented liveness limit)' % (caught['ID'], ready_reason('alpha')), flush=True)
            assert final['Applied'] == applied_before
        else:
            print('Post-partition outcome: operation completed after evidence returned', flush=True)
            assert final['Applied'] == 2 and len(completions('alpha', caught['ID'])) == 1
    print('PASS: S3 partition: fail closed throughout, acknowledged write readable after recovery', flush=True)
    print(k('get', 'celldstoragereservations', '-o', 'json'), flush=True)
