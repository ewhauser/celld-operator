"""Live same-pin restart and RetainData deletion in the harness-owned cluster.

Call only from run.py after its identity/network/storage fixtures pass. This
module never discovers a kubeconfig, starts infrastructure, or changes AWS.
"""
import json
import time


def exercise(k, get, wait_for, restart_operator, ready, curl):
    def reservation(fleet):
        values = json.loads(k('get', 'celldstoragereservations', '-o', 'json'))['items']
        return next(value for value in values if value['spec']['fleetName'] == fleet
                    and value['spec']['fleetNamespace'] == 'fleets')

    def journal(fleet):
        value = reservation(fleet)
        return json.loads(value['metadata']['annotations']['celld.example.com/lifecycle-journal'])

    def pods(uid):
        return json.loads(k('-n', 'fleets', 'get', 'pods', '-l',
                            'celld.example.com/fleet-uid=' + uid, '-o', 'json'))['items']

    for fleet, kind, probe in [('alpha', 'deployment', 'client'),
                               ('beta', 'statefulset', 'client-beta')]:
        uid = get('celldfleet', fleet)['metadata']['uid']
        k('-n', 'fleets', 'patch', 'celldfleet', fleet, '--type=merge', '-p',
          json.dumps({'spec': {'capacity': None, 'replicas': 3, 'maintenance': None}}))
        wait_for(lambda: get(kind, fleet)['spec']['replicas'] == 3 and ready(fleet)
                 and not journal(fleet).get('Operation'),
                 'maintenance fixture has three ready replicas: ' + fleet, timeout=360)
        assert json.loads(curl(probe, fleet, '/?cell=maintenance&id=ack', 8080,
                               method='PUT'))['stored'] is True
        before = {pod['metadata']['uid'] for pod in pods(uid)}
        claims = {claim['metadata']['name']: claim['metadata']['uid'] for claim in
                  json.loads(k('-n', 'fleets', 'get', 'pvc', '-l',
                               'celld.example.com/fleet-uid=' + uid, '-o', 'json'))['items']}
        token = 'live-maintenance-' + fleet
        k('-n', 'fleets', 'patch', 'celldfleet', fleet, '--type=merge', '-p',
          json.dumps({'spec': {'maintenance': {'restartToken': token}}}))
        wait_for(lambda: (journal(fleet).get('Maintenance') or {}).get('Phase') in
                 ('Stopping', 'Authorized', 'Recovering'),
                 'restart irreversible authority persisted: ' + fleet, timeout=180)
        restart_operator()
        wait_for(lambda: token in journal(fleet).get('CompletedRestarts', [])
                 and ready(fleet), 'restart recovers across operator replacement: ' + fleet,
                 timeout=600)
        after = {pod['metadata']['uid'] for pod in pods(uid)}
        assert len(after) == 3 and not before.intersection(after), (before, after)
        assert json.loads(curl(probe, fleet, '/?cell=maintenance&id=ack', 8080))['stored'] is True
        for name, claim_uid in claims.items():
            assert get('pvc', name)['metadata']['uid'] == claim_uid
        restart_operator()
        time.sleep(12)
        assert {pod['metadata']['uid'] for pod in pods(uid)} == after, 'completed token replayed'
        print('PASS: exact-UID rolling restart, acknowledged data, retained claims, and token replay: '
              + fleet, flush=True)

        retained = reservation(fleet)
        k('-n', 'fleets', 'delete', 'celldfleet', fleet, '--wait=false')
        def finalized():
            values = json.loads(k('-n', 'fleets', 'get', 'celldfleets', '-o', 'json'))['items']
            return all(value['metadata']['uid'] != uid for value in values)
        wait_for(finalized, 'RetainData finalizer completes with proven shutdown: ' + fleet,
                 timeout=600)
        assert not pods(uid)
        assert reservation(fleet)['metadata']['uid'] == retained['metadata']['uid']
        assert journal(fleet)['Maintenance']['Phase'] == 'Cleanup'
        for name, claim_uid in claims.items():
            assert get('pvc', name)['metadata']['uid'] == claim_uid
        print('PASS: final shutdown removes compute and preserves reservation/PVC identities: '
              + fleet, flush=True)
