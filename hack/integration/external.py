"""External capacity mode: a HorizontalPodAutoscaler owns spec.replicas via /scale.

Call only from run.py --external after its fixtures pass. Proves, against a real
HPA controller and Metrics Server:

1. The /scale subresource reports the fleet's pods and selector, and a write
   through it lands in spec.replicas without the operator ever writing it back.
2. An HPA targeting the CelldFleet raises the fleet to its maximum through the
   operator's journaled addition path.
3. A lowered HPA maximum contracts the fleet through the same gated executor
   the disposable fixture uses for automatic contraction; the acknowledged
   write stays readable.
4. Deleting the HPA and switching the policy off returns ownership to the user.
"""
import json
import time


def exercise(k, apply, get, wait_for, ready, curl):
    def journal(fleet):
        values = json.loads(k('get', 'celldstoragereservations', '-o', 'json'))['items']
        value = next(v for v in values if v['spec']['fleetName'] == fleet and v['spec']['fleetNamespace'] == 'fleets')
        return json.loads(value['metadata']['annotations']['celld.example.com/lifecycle-journal'])

    def scale():
        return json.loads(k('get', '--raw', '/apis/celld.example.com/v1alpha1/namespaces/fleets/celldfleets/alpha/scale'))

    def settled(count):
        return get('deployment', 'alpha')['spec']['replicas'] == count and ready('alpha') and not journal('alpha').get('Operation')

    k('-n', 'fleets', 'patch', 'celldfleet', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'capacity': {'mode': 'External'}}}))
    wait_for(lambda: get('celldfleet', 'alpha').get('status', {}).get('capacity', {}).get('reason') == 'ExternalOwner', 'External mode names the /scale writer as owner')
    view = scale()
    assert view['spec']['replicas'] == 2 and view['status']['replicas'] == 2 and view['status']['selector'].startswith('celld.example.com/fleet-uid='), view
    print('PASS: /scale reports spec, observed replicas and the fleet selector', flush=True)

    # Three nodes with strict hostname separation bound the fleet at three replicas.
    hpa = {'apiVersion': 'autoscaling/v2', 'kind': 'HorizontalPodAutoscaler', 'metadata': {'name': 'alpha', 'namespace': 'fleets'}, 'spec': {
        'scaleTargetRef': {'apiVersion': 'celld.example.com/v1alpha1', 'kind': 'CelldFleet', 'name': 'alpha'},
        'minReplicas': 2, 'maxReplicas': 3,
        # Any CPU use exceeds 1% of the 250m request, so the HPA drives to max.
        'metrics': [{'type': 'Resource', 'resource': {'name': 'cpu', 'target': {'type': 'Utilization', 'averageUtilization': 1}}}],
        'behavior': {'scaleDown': {'stabilizationWindowSeconds': 0, 'policies': [{'type': 'Pods', 'value': 1, 'periodSeconds': 15}]},
                     'scaleUp': {'stabilizationWindowSeconds': 0}}}}
    apply(hpa)
    wait_for(lambda: get('hpa', 'alpha').get('status', {}).get('currentReplicas', 0) >= 2, 'HPA reads the fleet through /scale', timeout=240)
    wait_for(lambda: get('celldfleet', 'alpha')['spec']['replicas'] == 3, 'HPA raises spec.replicas to its maximum', timeout=300)
    wait_for(lambda: settled(3), 'operator applies the HPA addition through the journal', timeout=300)
    generation = get('celldfleet', 'alpha')['metadata']['generation']
    time.sleep(20)
    assert get('celldfleet', 'alpha')['metadata']['generation'] == generation, 'operator or HPA kept rewriting spec.replicas'
    print('PASS: External mode never fights the HPA; desired 3 applied 3', flush=True)

    # Lower the ceiling: the HPA requests contraction; the fixture executor runs it.
    k('-n', 'fleets', 'patch', 'hpa', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'minReplicas': 2, 'maxReplicas': 2}}))
    wait_for(lambda: get('celldfleet', 'alpha')['spec']['replicas'] == 2, 'HPA lowers spec.replicas', timeout=300)
    wait_for(lambda: settled(2), 'HPA-requested contraction executes through the gated Bucket executor (local fixture)', timeout=600)
    assert json.loads(curl('client', 'alpha', '/?cell=integration&id=ack', 8080))['stored'] is True
    view = scale()
    assert view['spec']['replicas'] == 2 and view['status']['replicas'] == 2, view
    print('PASS: acknowledged write readable after HPA-driven shrink; /scale consistent', flush=True)

    # kubectl scale is an ordinary /scale writer once the HPA is gone.
    k('-n', 'fleets', 'delete', 'hpa', 'alpha', '--wait=true')
    k('-n', 'fleets', 'scale', 'celldfleet/alpha', '--replicas=3')
    wait_for(lambda: settled(3), 'kubectl scale through /scale adds a replica via the journal', timeout=300)

    # Return ownership: drop the policy; spec.replicas is a manual field again.
    k('-n', 'fleets', 'patch', 'celldfleet', 'alpha', '--type=merge', '-p', json.dumps({'spec': {'capacity': None, 'replicas': 2}}))
    wait_for(lambda: settled(2), 'manual ownership restored after External mode', timeout=600)
    print(k('get', 'celldstoragereservations', '-o', 'json'), flush=True)
