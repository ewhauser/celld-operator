"""Render and inspect Helm's actual output, including RBAC and opt-in monitoring."""
from pathlib import Path
import subprocess
import yaml

ROOT = Path(__file__).resolve().parents[1]
CHART = str(ROOT / 'charts/celld-operator')

def render(*args):
    result = subprocess.run(['helm', 'template', 'example', CHART, '--namespace', 'operator-test', '--include-crds', *args], check=True, text=True, capture_output=True)
    return [doc for doc in yaml.safe_load_all(result.stdout) if doc]

base = render()
deploy = next(doc for doc in base if doc['kind'] == 'Deployment')
assert deploy['spec']['replicas'] == 2
pod = deploy['spec']['template']['spec']
assert pod['containers'][0]['image'] == 'ghcr.io/ewhauser/celld-operator:dev'
assert '--operator-namespace=operator-test' in pod['containers'][0]['args']
assert not any(doc['kind'] in ('ServiceMonitor', 'PrometheusRule') for doc in base)
assert not any(arg.startswith('--ec2-fencing-') for arg in pod['containers'][0]['args'])
assert len([doc for doc in base if doc['kind'] == 'CustomResourceDefinition']) == 2
role = next(doc for doc in base if doc['kind'] == 'ClusterRole')
canonical = list(yaml.safe_load_all((ROOT / 'config/manager/operator.yaml').read_text()))
assert role['rules'] == next(doc['rules'] for doc in canonical if doc['kind'] == 'ClusterRole')
# Namespaced privileges never leak into the cluster role, and the per-namespace
# Role renders exactly the canonical rules into each configured fleet namespace.
namespaced_kinds = {('', 'secrets'), ('', 'pods'), ('apps', 'deployments'), ('apps', 'statefulsets')}
for rule in role['rules']:
    for group in rule['apiGroups']:
        for resource in rule['resources']:
            assert (group, resource) not in namespaced_kinds, f'cluster-wide grant on {group}/{resource}'
assert not any(doc['kind'] == 'Role' and doc['metadata']['name'].endswith('-fleet') for doc in base)
fleet_canonical = list(yaml.safe_load_all((ROOT / 'config/rbac/fleet-namespace.yaml').read_text()))
scoped = render('--set', 'fleetNamespaces={agents-prod,agents-staging}')
fleet_roles = [doc for doc in scoped if doc['kind'] == 'Role' and doc['metadata']['name'].endswith('-fleet')]
assert sorted(doc['metadata']['namespace'] for doc in fleet_roles) == ['agents-prod', 'agents-staging']
for doc in fleet_roles:
    assert doc['rules'] == next(d['rules'] for d in fleet_canonical if d['kind'] == 'Role')
fleet_bindings = [doc for doc in scoped if doc['kind'] == 'RoleBinding' and doc['metadata']['name'].endswith('-fleet')]
assert len(fleet_bindings) == 2 and all(b['subjects'][0]['namespace'] == 'operator-test' for b in fleet_bindings)
for doc in base:
    if doc['kind'].endswith('RoleBinding'):
        assert doc['subjects'][0]['namespace'] == 'operator-test'
        assert doc['subjects'][0]['name'] == pod['serviceAccountName']
full = render('--set', 'metrics.enabled=true,metrics.serviceMonitor.enabled=true,metrics.prometheusRule.enabled=true', '--set', 'image.digest=sha256:' + 'a' * 64, '--set', 'launcherImage=ghcr.io/example/operator@sha256:' + 'a' * 64)
assert any(doc['kind'] == 'ServiceMonitor' for doc in full)
assert any(doc['kind'] == 'PrometheusRule' for doc in full)
fullpod = next(doc for doc in full if doc['kind'] == 'Deployment')['spec']['template']['spec']
assert fullpod['containers'][0]['image'].endswith('@sha256:' + 'a' * 64)
assert any(arg.startswith('--launcher-image=') for arg in fullpod['containers'][0]['args'])
assert not any(doc['kind'] == 'PodDisruptionBudget' for doc in render('--set', 'replicaCount=1'))
fenced = render('--set-string', 'ec2Fencing.account=123456789012,ec2Fencing.region=us-east-1')
fenceargs = next(doc for doc in fenced if doc['kind'] == 'Deployment')['spec']['template']['spec']['containers'][0]['args']
assert '--ec2-fencing-account=123456789012' in fenceargs
assert '--ec2-fencing-region=us-east-1' in fenceargs
for bad in ('fleetNamespaces={Bad_Name}', 'ec2Fencing.region=us-east-1', 'ec2Fencing.account=bad', 'replicaCount=0', 'metrics.serviceMonitor.enabled=true', 'metrics.port=8082', 'launcherImage=mutable:latest', 'image.digest=sha256:bad'):
    result = subprocess.run(['helm', 'template', 'example', CHART, '--set', bad], text=True, capture_output=True)
    assert result.returncode != 0, f'invalid chart values accepted: {bad}'
print('Helm rendering, canonical RBAC, HA, monitoring and invalid-value checks passed.')
