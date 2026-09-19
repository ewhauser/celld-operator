"""Ordered Bucket live qualification on an explicitly owned disposable cluster."""
import json


def exercise(k, apply, get, wait_for, restart_operator, fleet, curl_image):
    alpha = fleet('alpha', 'bucket-alpha')
    alpha['spec'].update({'bucketWorkload': 'Ordered', 'replicas': 3,
                          'placement': {'azCount': 2, 'zones': ['us-east-1a', 'us-east-1b']}})
    apply(alpha)
    def pods():
        uid = get('celldfleet', 'alpha')['metadata']['uid']
        return json.loads(k('-n', 'fleets', 'get', 'pods', '-l', 'celld.example.com/fleet-uid=' + uid, '-o', 'json'))['items']
    def settled(count):
        current = get('celldfleet', 'alpha')
        try:
            sts = get('statefulset', 'alpha')
        except RuntimeError as error:
            if '(NotFound)' in str(error):
                return False
            raise
        return (sts['spec']['replicas'] == count and sts.get('status', {}).get('readyReplicas') == count
                and any(c['type'] == 'Ready' and c['status'] == 'True' for c in current.get('status', {}).get('conditions', []))
                and not current.get('status', {}).get('lifecycle', {}).get('operationID'))
    def placement(count):
        current = pods()
        assert len(current) == count
        assert {p['metadata']['name'] for p in current} == {'alpha-' + str(i) for i in range(count)}
        assert len({p['spec']['nodeName'] for p in current}) == count
        for pod in current:
            ordinal = int(pod['metadata']['name'].rsplit('-', 1)[1])
            zone = ['us-east-1a', 'us-east-1b'][ordinal % 2]
            node = get('node', pod['spec']['nodeName'], '')
            assert node['metadata']['labels']['topology.kubernetes.io/zone'] == zone
            assert pod['spec']['nodeSelector']['topology.kubernetes.io/zone'] == zone
            assert not pod['spec'].get('schedulingGates')
        assert not json.loads(k('-n', 'fleets', 'get', 'pvc', '-o', 'json'))['items']
        print('PASS: exact ordered membership, AZ mapping, hostname separation, and emptyDir', flush=True)
    def scale(count):
        k('-n', 'fleets', 'patch', 'celldfleet', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'replicas': count}}))
        wait_for(lambda: settled(count), 'Ordered Bucket settled at ' + str(count), timeout=360)
        placement(count)
    wait_for(lambda: settled(3), 'Ordered Bucket gated pods start across two AZs', timeout=360)
    placement(3)
    apply({'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': 'ordered-client', 'namespace': 'fleets', 'labels': {'celld.example.com/client-of': 'alpha'}}, 'spec': {'containers': [{'name': 'client', 'image': curl_image, 'command': ['/bin/sh', '-c', 'sleep 3600']}]}})
    k('-n', 'fleets', 'wait', '--for=condition=Ready', 'pod/ordered-client', '--timeout=90s')
    def request(method, path):
        return json.loads(k('-n', 'fleets', 'exec', 'ordered-client', '--', 'curl', '--fail', '--silent', '--show-error', '--max-time', '10', '-X', method, 'http://alpha:8080' + path))
    paths = ['/?cell=ordered&id=ack-' + str(i) for i in range(12)]
    for path in paths:
        assert request('PUT', path)['stored'] is True
    original = {p['metadata']['name']: p['metadata']['uid'] for p in pods()}
    scale(2)
    assert {p['metadata']['name']: p['metadata']['uid'] for p in pods()} == {key: value for key, value in original.items() if key != 'alpha-2'}
    for path in paths:
        assert request('GET', path)['stored'] is True
    print('PASS: only highest ordinal removed; all 12 acknowledged writes survived', flush=True)
    # This node was admitted by completed removal. Restart its unchanged pinned
    # runtime in place, then require positive generation supersession on the next removal.
    before = get('pod', 'alpha-0')
    old_container = before['status']['containerStatuses'][0]['containerID']
    k('-n', 'fleets', 'exec', 'alpha-0', '--', '/bin/sh', '-c', 'kill -TERM 1')
    wait_for(lambda: get('pod', 'alpha-0')['status']['containerStatuses'][0].get('containerID') != old_container
             and get('pod', 'alpha-0')['status']['containerStatuses'][0].get('ready'), 'same Pod UID acquires a new ready runtime invocation', timeout=180)
    assert get('pod', 'alpha-0')['metadata']['uid'] == before['metadata']['uid']
    scale(3)
    assert get('pod', 'alpha-2')['metadata']['uid'] != original['alpha-2']
    restart_operator()
    scale(2)
    for path in paths:
        assert request('GET', path)['stored'] is True
    reservations = json.loads(k('get', 'celldstoragereservations', '-o', 'json'))['items']
    journal = json.loads(next(r for r in reservations if r['spec']['fleetUID'] == get('celldfleet', 'alpha')['metadata']['uid'])['metadata']['annotations']['celld.example.com/lifecycle-journal'])
    assert any(s.get('SupersededBy') for s in journal['BucketHistory'])
    print('PASS: positive successor history survives manager restart and repeated contraction; 12/12 ledger intact', flush=True)
    denied = False
    try:
        k('-n', 'fleets', 'exec', 'ordered-client', '--', 'curl', '--fail', '--silent', '--max-time', '3', 'http://' + get('pod', 'alpha-0')['status']['podIP'] + ':8081/state')
    except RuntimeError:
        denied = True
    assert denied, 'application client reached private state endpoint'
    print('PASS: private peer/state endpoint remains inaccessible to application clients', flush=True)
