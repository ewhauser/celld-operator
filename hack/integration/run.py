#!/usr/bin/env python3
"""Disposable kind + Calico + MinIO test. Never reads the default kubeconfig.
All created infrastructure is local and removed in finally, even on assertion failure.
"""
import argparse
import base64
import io
import tarfile
import copy
import hashlib
import json
import os
import signal
import pathlib
import subprocess
import tempfile
import time
import urllib.request
import uuid

ROOT = pathlib.Path(__file__).resolve().parents[2]
IMAGE = 'ghcr.io/denoland/celld@sha256:df8e74bb9a059df5779644368984933eba76acd6a2d196672732f4368f760fc8'
CALICO_URL = 'https://raw.githubusercontent.com/projectcalico/calico/v3.29.3/manifests/calico.yaml'
CALICO_SHA = '9a575859428b822a224dedafc4238555b6b0f910f2abf12983f20f871860914e'
MINIO = 'quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z'
CURL = 'curlimages/curl:8.12.1'
MC = 'quay.io/minio/mc:RELEASE.2025-08-13T08-35-41Z'
TOXIPROXY = 'ghcr.io/shopify/toxiproxy:2.12.0'
METRICS_SERVER = 'registry.k8s.io/metrics-server/metrics-server:v0.8.0'


def run(args, **kwargs):
    result = subprocess.run(args, text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, **kwargs)
    if result.returncode:
        raise RuntimeError(f'{args}: {result.stdout}')
    return result.stdout


def main():
    parser=argparse.ArgumentParser()
    parser.add_argument('--bucket-lifecycle',action='store_true')
    parser.add_argument('--ordered-bucket',action='store_true')
    parser.add_argument('--persistent-lifecycle',action='store_true')
    parser.add_argument('--maintenance',action='store_true')
    parser.add_argument('--faults',action='store_true',help='fault injection: manager crash points, node loss, S3 latency/partition via toxiproxy')
    args=parser.parse_args()
    persistent_lifecycle=args.persistent_lifecycle or args.maintenance or args.faults
    bucket_lifecycle=args.bucket_lifecycle or persistent_lifecycle or args.ordered_bucket
    name = 'celld-step2-' + uuid.uuid4().hex[:8]
    process = None
    created = False
    launcher_image = None
    with tempfile.TemporaryDirectory(prefix=name) as tmp:
        tmp = pathlib.Path(tmp)
        config = tmp / 'kubeconfig'
        env = {**os.environ, 'KUBECONFIG': str(config)}
        def k(*args, **kwargs):
            return run(['kubectl', '--kubeconfig', str(config), '--context', 'kind-' + name, *args], timeout=300, **kwargs)
        def apply(obj):
            return k('apply', '-f', '-', input=json.dumps(obj))
        def get(kind, resource, ns='fleets'):
            return json.loads(k('-n', ns, 'get', kind, resource, '-o', 'json'))
        def succeeded(pod, ns):
            phase=get('pod',pod,ns)['status'].get('phase')
            if phase=='Failed':
                raise RuntimeError('Test Pod failed: '+pod+'\n'+k('-n',ns,'logs',pod))
            return phase=='Succeeded'
        def wait_for(check, description, timeout=180):
            deadline = time.monotonic() + timeout
            while time.monotonic() < deadline:
                if check():
                    print('PASS:', description, flush=True)
                    return
                time.sleep(2)
            raise RuntimeError('Timed out: ' + description)
        try:
            (tmp / 'kind.yaml').write_text('''kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: 192.168.0.0/16
nodes:
- role: control-plane
  labels:
    topology.kubernetes.io/zone: us-east-1a
- role: worker
  labels:
    topology.kubernetes.io/zone: us-east-1a
- role: worker
  labels:
    topology.kubernetes.io/zone: us-east-1a
''')
            if args.ordered_bucket:
                config_text=(tmp/'kind.yaml').read_text()
                before,last=config_text.rsplit('us-east-1a',1)
                (tmp/'kind.yaml').write_text(before+'us-east-1b'+last)
            print('Creating isolated cluster', name, flush=True)
            # Name is unique; cleanup is authorized only for this invocation's cluster.
            created = True
            run(['kind', 'create', 'cluster', '--name', name, '--image', 'kindest/node:v1.31.4', '--config', str(tmp / 'kind.yaml'), '--kubeconfig', str(config)], env=env, timeout=300)
            with urllib.request.urlopen(CALICO_URL, timeout=60) as response:
                calico = response.read()
            assert hashlib.sha256(calico).hexdigest() == CALICO_SHA
            (tmp / 'calico.yaml').write_bytes(calico)
            k('apply', '-f', str(tmp / 'calico.yaml'))
            k('wait', '--for=condition=Ready', 'nodes', '--all', '--timeout=240s')
            k('taint', 'nodes', name+'-control-plane', 'node-role.kubernetes.io/control-plane:NoSchedule-')
            k('-n', 'kube-system', 'rollout', 'status', 'daemonset/calico-node', '--timeout=240s')
            k('apply', '-f', str(ROOT / 'config/crd'))
            k('wait', '--for=condition=Established', 'crd/celldfleets.celld.example.com', '--timeout=60s')
            # Manager manifest is validated, but run the native binary under the exact SA RBAC below.
            k('apply', '-f', str(ROOT / 'config/manager/operator.yaml'))
            k('-n', 'celld-system', 'scale', 'deployment/celld-operator', '--replicas=0')
            for ns in ('fleets', 'other', 'celld-test-store', 'unbound'):
                k('create', 'namespace', ns)
                k('-n', ns, 'create', 'serviceaccount', 'runtime')
            # Fleet-namespace privileges are granted per namespace; `unbound`
            # deliberately receives none.
            for ns in ('fleets', 'other'):
                k('-n', ns, 'apply', '-f', str(ROOT / 'config/rbac/fleet-namespace.yaml'))
            node_arch=json.loads(k('get','node',name+'-control-plane','-o','json'))['status']['nodeInfo']['architecture']
            nodes=(name+'-control-plane', name+'-worker', name+'-worker2')
            for index,image in enumerate((IMAGE, MINIO, MC, CURL)+((METRICS_SERVER,) if bucket_lifecycle else ())+((TOXIPROXY,) if args.faults else ())):
                # Docker's containerd store may have only the local platform of
                # a multiarch image. Export that platform explicitly; ordinary
                # kind load docker-image can fail on absent sibling manifests.
                cached=subprocess.run(['docker','image','inspect',image],capture_output=True,text=True,timeout=30)
                archive=tmp/('cached-image-'+str(index)+'.tar')
                if cached.returncode == 0 and '@sha256:' not in image:
                    run(['docker','image','save','--platform','linux/'+node_arch,'-o',str(archive),image],timeout=180)
                    run(['kind','load','image-archive','--name',name,str(archive)],timeout=180)
                    archive.unlink()
                elif cached.returncode == 0:
                    # A digest reference has no tag for kind to import. Save the
                    # full index (its digest is the pinned one) and name it in
                    # containerd directly; the kubelet then finds the exact pin
                    # without a registry round trip.
                    run(['docker','image','save','-o',str(archive),image],timeout=180)
                    for node in nodes:
                        run(['docker','cp',str(archive),node+':/celld-cached-image.tar'],timeout=120)
                        run(['docker','exec',node,'ctr','-n','k8s.io','images','import','--index-name',image,'--platform','linux/'+node_arch,'/celld-cached-image.tar'],timeout=300)
                        run(['docker','exec',node,'rm','-f','/celld-cached-image.tar'],timeout=30)
                    archive.unlink()
                    print('Loaded pinned image from local cache:', image, flush=True)
                    continue
                print('Pulling into disposable node:', image, flush=True)
                for node in nodes:
                    # Registry throughput varies; a slow pull must not abort a long run.
                    for attempt in range(3):
                        try:
                            run(['docker', 'exec', node, 'crictl', 'pull', image], timeout=600)
                            break
                        except (RuntimeError, subprocess.TimeoutExpired) as err:
                            if attempt == 2: raise
                            print('Retrying image pull on', node, 'after:', str(err)[:200], flush=True)
            # Runtime egress admits store-namespace pods labeled app=minio. In
            # faults mode the fixed `minio` name resolves to toxiproxy, which
            # forwards to the real server under `minio-backend`.
            apply({'apiVersion':'v1','kind':'Pod','metadata':{'name':'minio','namespace':'celld-test-store','labels':{'app':'minio','role':'backend'}},'spec':{'containers':[{'name':'minio','image':MINIO,'args':['server','/data'],'env':[{'name':'MINIO_ROOT_USER','value':'qualification'},{'name':'MINIO_ROOT_PASSWORD','value':'qualification-only'}]}]}})
            def service(svc,role,ports):
                apply({'apiVersion':'v1','kind':'Service','metadata':{'name':svc,'namespace':'celld-test-store'},'spec':{'selector':{'app':'minio','role':role},'ports':[{'name':'p'+str(p),'port':p,'targetPort':p} for p in ports]}})
            k('-n','celld-test-store','wait','--for=condition=Ready','pod/minio','--timeout=120s')
            if args.faults:
                service('minio-backend','backend',[9000])
                apply({'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'toxiproxy','namespace':'celld-test-store'},'data':{'toxiproxy.json':json.dumps([{'name':'minio','listen':'0.0.0.0:9000','upstream':'minio-backend.celld-test-store.svc:9000','enabled':True}])}})
                apply({'apiVersion':'v1','kind':'Pod','metadata':{'name':'toxiproxy','namespace':'celld-test-store','labels':{'app':'minio','role':'proxy'}},'spec':{'containers':[{'name':'toxiproxy','image':TOXIPROXY,'args':['-config','/config/toxiproxy.json','-host','0.0.0.0'],'volumeMounts':[{'name':'config','mountPath':'/config','readOnly':True}]}],'volumes':[{'name':'config','configMap':{'name':'toxiproxy'}}]}})
                service('minio','proxy',[9000])
                service('toxiproxy-api','proxy',[8474])
                apply({'apiVersion':'v1','kind':'Pod','metadata':{'name':'toxi-ctl','namespace':'celld-test-store'},'spec':{'containers':[{'name':'ctl','image':CURL,'command':['/bin/sh','-c','sleep 7200']}]}})
                k('-n','celld-test-store','wait','--for=condition=Ready','pod/toxiproxy','pod/toxi-ctl','--timeout=120s')
            else:
                service('minio','backend',[9000])
            k('-n','celld-test-store','run','seed','--restart=Never','--image='+MC,'--command','--','/bin/sh','-c','attempt=0; until mc alias set local http://minio:9000 qualification qualification-only; do attempt=$((attempt+1)); test "$attempt" -lt 30 || exit 1; sleep 2; done; mc mb local/bucket-alpha local/bucket-beta')
            wait_for(lambda: succeeded('seed','celld-test-store'),'create two isolated test buckets')
            arch=json.loads(k('get','node',name+'-control-plane','-o','json'))['status']['nodeInfo']['architecture']
            package,integrity={
                'arm64':('arm64','8bwX7a8FghIgrupcxb4aUmYDLp8pX06rGh5HqDT7bB+8Rdells6mHvrFHHW2JAOPZUbnjUpKTLg6ECyzvas2AQ=='),
                'amd64':('x64','uqZMTLr/zR/ed4jIGnwSLkaHmPjOjJvnm6TVVitAa08SLS9Z0VM8wIRx7gWbJB5/J54YuIMInDquWyYvQLZkgw=='),
            }[arch]
            with urllib.request.urlopen(f'https://registry.npmjs.org/@esbuild/linux-{package}/-/linux-{package}-0.25.12.tgz',timeout=30) as response:
                archive=response.read()
            assert base64.b64encode(hashlib.sha512(archive).digest()).decode()==integrity
            with tarfile.open(fileobj=io.BytesIO(archive),mode='r:gz') as tar:
                (tmp/'esbuild').write_bytes(tar.extractfile('package/bin/esbuild').read())
            (tmp/'esbuild').chmod(0o755)
            run(['docker','cp',str(tmp/'esbuild'),name+'-control-plane:/opt/celld-test-esbuild'],timeout=30)
            run(['docker','cp',str(ROOT/'hack/qualification/app'),name+'-control-plane:/opt/celld-test-app'],timeout=30)
            for bucket in ('bucket-alpha','bucket-beta'):
                deploy_name='deploy-'+bucket
                deploy_env={'CELLD_BUCKET':'s3://'+bucket,'AWS_REGION':'us-east-1','AWS_ALLOW_HTTP':'true','S3_ENDPOINT':'http://minio:9000','AWS_ACCESS_KEY_ID':'qualification','AWS_SECRET_ACCESS_KEY':'qualification-only','CELLD_ESBUILD':'/esbuild'}
                apply({'apiVersion':'v1','kind':'Pod','metadata':{'name':deploy_name,'namespace':'celld-test-store'},'spec':{'nodeName':name+'-control-plane','restartPolicy':'Never','containers':[{'name':'deploy','image':IMAGE,'args':['deploy','/app'],'env':[{'name':key,'value':value} for key,value in deploy_env.items()],'volumeMounts':[{'name':'app','mountPath':'/app','readOnly':True},{'name':'esbuild','mountPath':'/esbuild','readOnly':True}]}],'volumes':[{'name':'app','hostPath':{'path':'/opt/celld-test-app'}},{'name':'esbuild','hostPath':{'path':'/opt/celld-test-esbuild'}}]}})
                wait_for(lambda:succeeded(deploy_name,'celld-test-store'),'qualification app deployed to '+bucket,timeout=120)
            apply({'apiVersion':'storage.k8s.io/v1','kind':'StorageClass','metadata':{'name':'retained'},'provisioner':'rancher.io/local-path','reclaimPolicy':'Retain','volumeBindingMode':'WaitForFirstConsumer'})
            token = k('-n','celld-system','create','token','celld-operator','--duration=1h').strip()
            operator_config = json.loads(k('config','view','--raw','-o','json'))
            operator_config['users'] = [{'name':'operator','user':{'token':token}}]
            operator_config['contexts'][0]['context']['user']='operator'
            operator_path=tmp/'operator-kubeconfig'
            operator_path.write_text(json.dumps(operator_config))
            log = open(tmp/'operator.log','w')
            if bucket_lifecycle:
                # Run the actual manager under its in-cluster ServiceAccount so
                # direct Pod IP state collection and Metrics Server are real.
                run(['go','build','-o',str(tmp/'operator'),'./cmd/celld-operator'],cwd=ROOT,env={**env,'GOOS':'linux','GOARCH':arch,'CGO_ENABLED':'0'},timeout=300)
                (tmp/'operator').chmod(0o755)
                run(['docker','cp',str(tmp/'operator'),name+'-control-plane:/opt/celld-test-operator'],timeout=30)
                patch={'spec':{'replicas':1,'template':{'spec':{'nodeName':name+'-control-plane','securityContext':{'runAsUser':65532},'containers':[{'name':'operator','image':IMAGE,'command':['/operator'],'args':['--operator-namespace=celld-system','--network-policy-enforced','--local-test','--local-evidence'],'volumeMounts':[{'name':'operator-binary','mountPath':'/operator','readOnly':True}]}],'volumes':[{'name':'operator-binary','hostPath':{'path':'/opt/celld-test-operator','type':'File'}}]}}}}
                if persistent_lifecycle:
                    run(['go','build','-o',str(tmp/'celld-launcher'),'./cmd/celld-launcher'],cwd=ROOT,env={**env,'GOOS':'linux','GOARCH':arch,'CGO_ENABLED':'0'},timeout=300)
                    (tmp/'celld-launcher').chmod(0o755)
                    launcher_image='celld-launcher-test:'+name
                    (tmp/'Dockerfile').write_text('FROM '+IMAGE+'\nCOPY celld-launcher /celld-launcher\n')
                    run(['docker','build','-t',launcher_image,str(tmp)],timeout=180)
                    run(['kind','load','docker-image','--name',name,launcher_image],timeout=180)
                    patch['spec']['template']['spec']['containers'][0]['args'].append('--launcher-image='+launcher_image)
                k('-n','celld-system','patch','deployment','celld-operator','--type=strategic','-p',json.dumps(patch))
                k('-n','celld-system','rollout','status','deployment/celld-operator','--timeout=120s')
                operator_args=list(patch['spec']['template']['spec']['containers'][0]['args'])
                with urllib.request.urlopen('https://github.com/kubernetes-sigs/metrics-server/releases/download/v0.8.0/components.yaml',timeout=60) as response:
                    metrics=response.read()
                assert hashlib.sha256(metrics).hexdigest()=='ff64d1a13b9ac3b0635f0dd985815fb44c23eed4706c04e5db1daadf6bc0a83b'
                (tmp/'metrics.yaml').write_bytes(metrics)
                k('apply','-f',str(tmp/'metrics.yaml'))
                k('-n','kube-system','patch','deployment','metrics-server','--type=json','-p',json.dumps([{'op':'add','path':'/spec/template/spec/containers/0/args/-','value':'--kubelet-insecure-tls'}]))
                k('-n','kube-system','rollout','status','deployment/metrics-server','--timeout=300s')
            else:
                process=subprocess.Popen([str(ROOT/'bin/celld-operator'),'--network-policy-enforced','--local-test'],env={**env,'KUBECONFIG':str(operator_path)},stdout=log,stderr=subprocess.STDOUT)
            def set_operator_fault(point):
                # Faults are explicit --local-test manager arguments; the rollout
                # replaces the crash-looping pod when the fault is withdrawn.
                fault=operator_args+(['--local-fault-point='+point] if point else [])
                k('-n','celld-system','patch','deployment','celld-operator','--type=json','-p',json.dumps([{'op':'replace','path':'/spec/template/spec/containers/0/args','value':fault}]))
                k('-n','celld-system','rollout','status','deployment/celld-operator','--timeout=180s')
            def restart_operator():
                nonlocal process
                if bucket_lifecycle:
                    k('-n','celld-system','rollout','restart','deployment/celld-operator')
                    k('-n','celld-system','rollout','status','deployment/celld-operator','--timeout=120s')
                else:
                    process.terminate();process.wait(timeout=20)
                    process=subprocess.Popen([str(ROOT/'bin/celld-operator'),'--network-policy-enforced','--local-test'],env={**env,'KUBECONFIG':str(operator_path)},stdout=log,stderr=subprocess.STDOUT)
            def fleet(name,bucket,profile='Bucket',namespace='fleets'):
                storage={'bucket':bucket,'region':'us-east-1','sizeGiB':1}
                if profile=='PersistentFleet': storage['storageClassName']='retained'
                return {'apiVersion':'celld.example.com/v1alpha1','kind':'CelldFleet','metadata':{'name':name,'namespace':namespace},'spec':{'qualification':'Experimental','profile':profile,'replicas':2,'serviceAccountName':'runtime','storage':storage,'placement':{'azCount':1,'zones':['us-east-1a']}}}
            if args.ordered_bucket:
                from ordered_bucket import exercise
                exercise(k,apply,get,wait_for,restart_operator,fleet,CURL)
                return
            alpha=fleet('alpha','bucket-alpha')
            beta=fleet('beta','bucket-beta','PersistentFleet')
            # An old deterministic claim must never be adopted into a new bucket.
            apply({'apiVersion':'v1','kind':'PersistentVolumeClaim','metadata':{'name':'data-collision-0','namespace':'fleets','labels':{'celld.example.com/fleet-uid':'previous-fleet'}},'spec':{'storageClassName':'retained','accessModes':['ReadWriteOnce'],'resources':{'requests':{'storage':'1Gi'}}}})
            old_claim_uid=get('pvc','data-collision-0')['metadata']['uid']
            apply(fleet('collision','new-bucket','PersistentFleet'))
            wait_for(lambda:any(c['reason']=='StorageIdentityConflict' for c in get('celldfleet','collision').get('status',{}).get('conditions',[])),'pre-existing PVC blocks initial StatefulSet creation')
            assert not k('-n','fleets','get','statefulset','collision','--ignore-not-found','-o','name').strip()
            assert get('pvc','data-collision-0')['metadata']['uid']==old_claim_uid
            apply(alpha);apply(beta)
            def ready(n):
                current=get('celldfleet',n)
                kind='deployment' if current['spec']['profile']=='Bucket' else 'statefulset'
                observed=get(kind,n)
                return (any(c['type']=='Ready' and c['status']=='True' for c in current.get('status',{}).get('conditions',[]))
                        and observed.get('status',{}).get('observedGeneration',0)>=observed['metadata'].get('generation',1)
                        and observed.get('status',{}).get('readyReplicas',0)==observed['spec']['replicas'])
            wait_for(lambda: ready('alpha') and ready('beta'),'both runtime profiles ready through operator reconciliation',timeout=300)
            pods=json.loads(k('-n','fleets','get','pods','-o','json'))['items']
            addresses={}
            for n in ('alpha','beta'):
                uid=get('celldfleet',n)['metadata']['uid']
                selected=[p for p in pods if p['metadata']['labels'].get('celld.example.com/fleet-uid')==uid]
                assert len(selected)==2
                assert len({p['spec']['nodeName'] for p in selected})==2
                addresses[n]=selected[0]['status']['podIP']
            print('PASS: strict replicas use distinct nodes within the configured zone',flush=True)
            unavailable=fleet('unavailable','bucket-unavailable')
            unavailable['spec']['placement']={'azCount':2,'zones':['us-east-1a','us-east-1b']}
            apply(unavailable)
            def zone_blocked():
                uid=get('celldfleet','unavailable')['metadata']['uid']
                pending=json.loads(k('-n','fleets','get','pods','-l','celld.example.com/fleet-uid='+uid,'-o','json'))['items']
                return any(any(c.get('reason')=='Unschedulable' and 'topology spread' in c.get('message','') for c in p.get('status',{}).get('conditions',[])) for p in pending)
            wait_for(zone_blocked,'strict placement blocks a missing eligible zone')
            def probe(name,labels,ns='fleets'):
                metadata={'name':name,'namespace':ns,'labels':labels}
                if 'celld.example.com/fleet-uid' in labels:
                    # Prevent ReplicaSet adoption and exclude the probe from ready Service endpoints.
                    metadata['ownerReferences']=[{'apiVersion':'celld.example.com/v1alpha1','kind':'CelldFleet','name':'alpha','uid':labels['celld.example.com/fleet-uid'],'controller':True}]
                apply({'apiVersion':'v1','kind':'Pod','metadata':metadata,'spec':{'readinessGates':[{'conditionType':'integration.celld.example.com/NotServing'}],'containers':[{'name':'probe','image':CURL,'command':['/bin/sh','-c','sleep 3600']}]}})
                wait_for(lambda:get('pod',name,ns)['status'].get('phase')=='Running','probe '+name+' running',timeout=90)
            def curl(pod,address,path='/state',port=8081,ns='fleets',allowed=True,method='GET'):
                result=subprocess.run(['kubectl','--kubeconfig',str(config),'--context','kind-'+name,'-n',ns,'exec',pod,'--','curl','--fail','--silent','--show-error','--max-time','3','-X',method,f'http://{address}:{port}{path}'],capture_output=True,text=True,timeout=15)
                assert (result.returncode==0)==allowed,(pod,address,allowed,result.stdout,result.stderr)
                if allowed and path=='/state': assert 'node_load' in json.loads(result.stdout)
                return result.stdout
            alpha_uid=get('celldfleet','alpha')['metadata']['uid']
            probe('same-fleet',{'celld.example.com/fleet-uid':alpha_uid})
            probe('client',{'celld.example.com/client-of':'alpha'})
            probe('client-beta',{'celld.example.com/client-of':'beta'})
            probe('untrusted',{})
            probe('operator',{'app.kubernetes.io/name':'celld-operator'},'celld-system')
            curl('same-fleet',addresses['alpha']);curl('same-fleet',addresses['beta'],allowed=False)
            curl('untrusted',addresses['alpha'],allowed=False)
            curl('client',addresses['alpha'],allowed=False)
            curl('operator',addresses['alpha'],ns='celld-system')
            curl('client','alpha','/.well-known/celld/health',8080)
            curl('client','beta','/.well-known/celld/health',8080,allowed=False)
            assert json.loads(curl('client','alpha','/?cell=integration&id=ack',8080,method='PUT'))['stored'] is True
            assert json.loads(curl('client','alpha','/?cell=integration&id=ack',8080))['stored'] is True
            assert json.loads(curl('client-beta','beta','/?cell=integration&id=ack',8080))['stored'] is False
            print('PASS: application writes readable only in their own fleet storage scope',flush=True)
            print('PASS: same-fleet/operator peer access, cross-fleet/untrusted/client peer denial, ClusterIP health routing',flush=True)
            k('-n','fleets','delete','pod','same-fleet','--wait=true')
            apply(fleet('conflict','bucket-alpha',namespace='other'))
            wait_for(lambda:any(c['reason']=='StorageScopeConflict' for c in get('celldfleet','conflict','other').get('status',{}).get('conditions',[])),'cross-namespace storage conflict blocked')
            apply(fleet('denied','bucket-denied',namespace='unbound'))
            wait_for(lambda:any(c['reason']=='NamespaceAccessDenied' for c in get('celldfleet','denied','unbound').get('status',{}).get('conditions',[])),'fleet in a namespace without the fleet Role is reported, not reconciled')
            assert not k('-n','unbound','get','deployment,service,networkpolicy','-o','name').strip()
            for mutate in ('upgrade','invalid-az','invalid-storage'):
                bad=copy.deepcopy(alpha)
                if mutate=='scale-in':bad['spec']['replicas']=1
                if mutate=='upgrade':bad['spec']['storage']['sizeGiB']=2
                if mutate=='invalid-az':bad['metadata']['name']='bad-az';bad['spec']['placement']['azCount']=3
                if mutate=='invalid-storage':bad['metadata']['name']='bad-storage';bad['spec']['profile']='PersistentFleet'
                try: apply(bad)
                except RuntimeError as err:
                    assert 'invalid' in str(err).lower(),str(err)
                else: raise AssertionError('admission accepted '+mutate)
            print('PASS: schema rejects invalid specs and lifecycle mutations',flush=True)
            # OnDelete keeps runtime Pods unchanged while testing drift detection.
            original_template=get('statefulset','beta')['spec']['template']
            beta_pod_uids=[get('pod','beta-'+str(i))['metadata']['uid'] for i in range(2)]
            drift=[{'op':'add','path':'/spec/template/spec/containers/0/livenessProbe','value':{'exec':{'command':['false']}}}, {'op':'add','path':'/spec/template/spec/containers/0/env/-','value':{'name':'CELLD_BUCKET','value':'s3://other-fleet'}}]
            k('-n','fleets','patch','statefulset','beta','--type=json','-p',json.dumps(drift))
            wait_for(lambda:any(c['type']=='Ready' and c['reason']=='LifecycleBlocked' for c in get('celldfleet','beta')['status']['conditions']),'additional probe and bucket override are reported as unsafe drift')
            assert [get('pod','beta-'+str(i))['metadata']['uid'] for i in range(2)]==beta_pod_uids
            k('-n','fleets','patch','statefulset','beta','--type=json','-p',json.dumps([{'op':'replace','path':'/spec/template','value':original_template}]))
            wait_for(lambda:ready('beta'),'restored original template matches normalized API defaults')
            if args.maintenance:
                import maintenance
                maintenance.exercise(k, get, wait_for, restart_operator, ready, curl)
                return
            if args.faults:
                import faults
                faults.exercise(k, apply, get, wait_for, ready, curl, probe, name, CURL, set_operator_fault)
                return
            if persistent_lifecycle:
                # Local-path RWO is deliberately local-test-only: test same-host
                # locks and scheduling, not CSI/RWOP/EBS attachment guarantees.
                probe('beta-client',{'celld.example.com/client-of':'beta'})
                curl('beta-client','beta','/?cell=integration&id=ack',8080,method='PUT')
                def settled(count):
                    return get('statefulset','beta')['spec']['replicas']==count and ready('beta') and not get('celldfleet','beta').get('status',{}).get('lifecycle',{}).get('operationID')
                for cycle in range(2):
                    k('-n','fleets','patch','celldfleet','beta','--type=merge','-p',json.dumps({'spec':{'replicas':3}}))
                    wait_for(lambda:settled(3),'PersistentFleet growth '+str(cycle),timeout=300)
                    retained_uid=get('pvc','data-beta-2')['metadata']['uid']
                    retained_host=get('pod','beta-2')['spec']['nodeName']
                    k('-n','fleets','patch','celldfleet','beta','--type=merge','-p',json.dumps({'spec':{'replicas':2}}))
                    restart_operator()
                    wait_for(lambda:settled(2),'PersistentFleet graceful retirement '+str(cycle),timeout=300)
                    assert get('pvc','data-beta-2')['metadata']['uid']==retained_uid
                    assert json.loads(curl('beta-client','beta','/?cell=integration&id=ack',8080))['stored'] is True
                    k('-n','fleets','patch','celldfleet','beta','--type=merge','-p',json.dumps({'spec':{'replicas':3}}))
                    wait_for(lambda:settled(3),'PersistentFleet same-host reactivation '+str(cycle),timeout=300)
                    assert get('pvc','data-beta-2')['metadata']['uid']==retained_uid
                    assert get('pod','beta-2')['spec']['nodeName']==retained_host
                k('-n','fleets','patch','celldfleet','beta','--type=merge','-p',json.dumps({'spec':{'capacity':{'mode':'Automatic','minReplicas':2,'maxReplicas':3,'minSamples':2,'sampleIntervalSeconds':30,'scaleInStabilizationSeconds':60,'scaleInCooldownSeconds':60,'cpuLowMillicores':200,'cpuHighMillicores':500,'memoryLowMiB':700,'memoryHighMiB':1000}}}))
                wait_for(lambda:settled(2),'PersistentFleet live automatic contraction',timeout=420)
                assert json.loads(curl('beta-client','beta','/?cell=integration&id=ack',8080))['stored'] is True
                print(k('get','celldstoragereservations','-o','json'),flush=True)
                return
            # Manual additive capacity uses the durable journal, across a leader restart.
            for fleet_name, kind in (('alpha','deployment'),('beta','statefulset')):
                before=get(kind,fleet_name)
                before_template=before['spec']['template']
                k('-n','fleets','patch','celldfleet',fleet_name,'--type=merge','-p',json.dumps({'spec':{'replicas':3}}))
                restart_operator()
                wait_for(lambda: get(kind,fleet_name)['spec']['replicas']==3 and ready(fleet_name),'journaled scale-out after controller restart: '+fleet_name,timeout=300)
                after=get(kind,fleet_name)
                uid=get('celldfleet',fleet_name)['metadata']['uid']
                scaled=json.loads(k('-n','fleets','get','pods','-l','celld.example.com/fleet-uid='+uid,'-o','json'))['items']
                assert len({pod['spec']['nodeName'] for pod in scaled})==3
                assert after['metadata']['uid']==before['metadata']['uid']
                assert after['spec']['template']==before_template
                k('-n','fleets','patch','celldfleet',fleet_name,'--type=merge','-p',json.dumps({'spec':{'replicas':1}}))
                if bucket_lifecycle and fleet_name=='alpha':
                    wait_for(lambda:get(kind,fleet_name)['spec']['replicas']==1 and ready(fleet_name) and not get('celldfleet',fleet_name).get('status',{}).get('lifecycle',{}).get('operationID'),'two actual Bucket decrements completed',timeout=300)
                    assert json.loads(curl('client','alpha','/?cell=integration&id=ack',8080))['stored'] is True
                    k('-n','fleets','patch','celldfleet',fleet_name,'--type=merge','-p',json.dumps({'spec':{'replicas':3}}))
                    wait_for(lambda:get(kind,fleet_name)['spec']['replicas']==3 and ready(fleet_name),'Bucket shrink/grow with retained generation history',timeout=240)
                    continue
                why='BucketCompletionUnqualified' if fleet_name=='alpha' else 'FencingUnqualified'
                reasons={why}
                if bucket_lifecycle and fleet_name=='beta':
                    reasons.update({'SessionBindingUnqualified','HistoricalSessionUnresolved','CapacityUncertain','RecoveryInventoryUnavailable'})
                wait_for(lambda:any(c['reason'] in reasons for c in get('celldfleet',fleet_name)['status']['conditions']),'unqualified contraction blocked: '+fleet_name)
                assert get(kind,fleet_name)['spec']['replicas']==3
                k('-n','fleets','patch','celldfleet',fleet_name,'--type=merge','-p',json.dumps({'spec':{'replicas':3}}))
                wait_for(lambda:ready(fleet_name),'restored desired capacity: '+fleet_name)
            print('PASS: both profiles scale out without template changes; contraction gates persist',flush=True)
            if bucket_lifecycle:
                # A paused unissued removal cannot change replicas. Its later
                # completion uses the same recorded membership checks.
                k('-n','fleets','patch','celldfleet','alpha','--type=merge','-p',json.dumps({'spec':{'maintenance':{'paused':True},'replicas':2}}))
                wait_for(lambda:get('deployment','alpha')['metadata'].get('annotations',{}).get('celld.example.com/maintenance-fence')=='paused','Bucket pause fence installed')
                time.sleep(12)
                assert get('deployment','alpha')['spec']['replicas']==3
                k('-n','fleets','patch','celldfleet','alpha','--type=merge','-p',json.dumps({'spec':{'maintenance':None}}))
                restart_operator()
                wait_for(lambda:get('deployment','alpha')['spec']['replicas']==2 and ready('alpha') and not get('celldfleet','alpha').get('status',{}).get('lifecycle',{}).get('operationID'),'Bucket resume and manager restart completes removal',timeout=240)
                k('-n','fleets','patch','celldfleet','alpha','--type=merge','-p',json.dumps({'spec':{'replicas':3}}))
                wait_for(lambda:get('deployment','alpha')['spec']['replicas']==3 and ready('alpha'),'second Bucket growth',timeout=240)
                k('-n','fleets','patch','celldfleet','alpha','--type=merge','-p',json.dumps({'spec':{'capacity':{'mode':'Automatic','minReplicas':1,'maxReplicas':3,'minSamples':2,'sampleIntervalSeconds':30,'scaleInStabilizationSeconds':60,'scaleInCooldownSeconds':60,'cpuLowMillicores':200,'cpuHighMillicores':500,'memoryLowMiB':700,'memoryHighMiB':1000}}}))
                wait_for(lambda:get('deployment','alpha')['spec']['replicas']==1 and ready('alpha') and not get('celldfleet','alpha').get('status',{}).get('lifecycle',{}).get('operationID'),'live Metrics Server automatic Bucket 3-to-1 contraction',timeout=420)
                assert json.loads(curl('client','alpha','/?cell=integration&id=ack',8080))['stored'] is True
                retired=get('celldfleet','alpha')['status']['lifecycle']['retiredBucketSessions']
                assert retired>=5
                print('PASS: acknowledged write preserved through repeated manual/automatic Bucket shrink-grow; retired sessions report unknown physical liveness:',retired,flush=True)
                print(k('get','celldstoragereservations','-o','json'),flush=True)
                return
            # No Metrics Server is installed in this isolated cluster. Verify the
            # real API defaults/validation, exact metrics RBAC, and conservative
            # missing-data path in both profiles. Native HTTP fixture tests cover
            # successful /state + Metrics Server transport and pod incarnation races.
            for fleet_name, kind in (('alpha','deployment'),('beta','statefulset')):
                before=get(kind,fleet_name)
                k('-n','fleets','patch','celldfleet',fleet_name,'--type=merge','-p',json.dumps({'spec':{'capacity':{}}}))
                wait_for(lambda:get('celldfleet',fleet_name).get('status',{}).get('capacity',{}).get('mode')=='Shadow','capacity defaults to shadow: '+fleet_name)
                assert get(kind,fleet_name)['spec']==before['spec']
                assert get('celldfleet',fleet_name)['spec']['capacity']['maxReplicas']==10
                try:
                    k('-n','fleets','patch','celldfleet',fleet_name,'--type=merge','-p',json.dumps({'spec':{'capacity':{'minReplicas':10,'maxReplicas':3}}}))
                except RuntimeError as err:
                    assert 'invalid' in str(err).lower(),str(err)
                else: raise AssertionError('admission accepted inverted capacity bounds')
                k('-n','fleets','patch','celldfleet',fleet_name,'--type=merge','-p',json.dumps({'spec':{'capacity':{'mode':'ScaleOut'}}}))
                wait_for(lambda:get('celldfleet',fleet_name).get('status',{}).get('capacity',{}).get('reason') in ('PendingCapacity','IncompleteMetrics'),'missing metrics block automatic capacity: '+fleet_name)
                assert get(kind,fleet_name)['spec']==before['spec']
            print('PASS: shadow and opt-in automatic modes preserve capacity with missing metrics; API policy edits/defaults validated',flush=True)
            for fleet_name, kind in (('alpha','deployment'),('beta','statefulset')):
                before=get(kind,fleet_name)
                k('-n','fleets','patch','celldfleet',fleet_name,'--type=merge','-p',json.dumps({'spec':{'maintenance':{'paused':True}}}))
                wait_for(lambda:get(kind,fleet_name)['metadata'].get('annotations',{}).get('celld.example.com/maintenance-fence')=='paused' and get('celldfleet',fleet_name).get('status',{}).get('readyReplicas')==3 and ready(fleet_name),'pause preserves serving readiness: '+fleet_name)
                assert get(kind,fleet_name)['spec']==before['spec']
                k('-n','fleets','patch','celldfleet',fleet_name,'--type=merge','-p',json.dumps({'spec':{'maintenance':{'paused':False,'restartToken':'integration-restart'}}}))
                wait_for(lambda:any(c['reason']=='DisruptionUnqualified' for c in get('celldfleet',fleet_name).get('status',{}).get('conditions',[])),'restart request blocked: '+fleet_name)
                assert get(kind,fleet_name)['spec']==before['spec']
                k('-n','fleets','patch','celldfleet',fleet_name,'--type=merge','-p',json.dumps({'spec':{'runtimeImage':'ghcr.io/denoland/celld@sha256:'+'a'*64}}))
                wait_for(lambda:any(c['reason']=='UnsupportedTransition' for c in get('celldfleet',fleet_name).get('status',{}).get('conditions',[])),'unqualified image transition blocked: '+fleet_name)
                assert get(kind,fleet_name)['spec']==before['spec']
                k('-n','fleets','patch','celldfleet',fleet_name,'--type=merge','-p',json.dumps({'spec':{'runtimeImage':None,'maintenance':None}}))
            sts=get('statefulset','beta')
            assert sts['spec']['persistentVolumeClaimRetentionPolicy']=={'whenDeleted':'Retain','whenScaled':'Retain'}
            assert sts['spec']['updateStrategy']['type']=='OnDelete'
            k('-n','fleets','delete','celldfleet','beta','--wait=false')
            wait_for(lambda:any(c['reason']=='DeletionBlocked' for c in get('celldfleet','beta').get('status',{}).get('conditions',[])),'deletion blocked without touching StatefulSet or PVCs')
            assert get('statefulset','beta')['metadata']['uid']==sts['metadata']['uid']
            assert len(json.loads(k('-n','fleets','get','pvc','-l','celld.example.com/fleet-uid='+get('celldfleet','beta')['metadata']['uid'],'-o','json'))['items'])==3
            # Controller restart must preserve reservation/journal and workload UIDs.
            dep_uid=get('deployment','alpha')['metadata']['uid']
            process.terminate();process.wait(timeout=20)
            k('-n','fleets','patch','celldfleet','alpha','--subresource=status','--type=merge','-p',json.dumps({'status':{'conditions':[],'readyReplicas':0}}))
            process=subprocess.Popen([str(ROOT/'bin/celld-operator'),'--network-policy-enforced','--local-test'],env={**env,'KUBECONFIG':str(operator_path)},stdout=log,stderr=subprocess.STDOUT)
            wait_for(lambda:ready('alpha'),'restarted controller reconciles readiness from durable reservation',timeout=90)
            assert get('deployment','alpha')['metadata']['uid']==dep_uid
            assert process.poll() is None
            # Reservation baseline survives a replica edit before any workload exists.
            apply({'apiVersion':'networking.k8s.io/v1','kind':'NetworkPolicy','metadata':{'name':'delayed','namespace':'fleets'},'spec':{'podSelector':{'matchLabels':{'integration.example.com/unused':'true'}},'policyTypes':['Ingress']}})
            delayed=fleet('delayed','bucket-delayed')
            delayed['spec']['replicas']=1
            apply(delayed)
            wait_for(lambda:any(c['reason']=='InfrastructureBlocked' for c in get('celldfleet','delayed').get('status',{}).get('conditions',[])),'reservation persists before blocked initial provisioning')
            k('-n','fleets','patch','celldfleet','delayed','--type=merge','-p',json.dumps({'spec':{'replicas':2}}))
            # This policy was created by this invocation and selects no runtime Pods.
            k('-n','fleets','delete','networkpolicy','delayed')
            def baseline_recovered():
                reservations=json.loads(k('get','celldstoragereservations','-o','json'))['items']
                reservation=next(x for x in reservations if x['spec']['fleetName']=='delayed')
                journal=json.loads(reservation['metadata'].get('annotations',{}).get('celld.example.com/lifecycle-journal','null'))
                return reservation['spec']['initialReplicas']==1 and journal is not None and journal['Initial']==1 and journal['Applied']==2
            wait_for(baseline_recovered,'replica edit before workload creation preserves reservation baseline')
            assert get('deployment','delayed')['spec']['replicas']==2
            print('PASS: controller restart preserves initial workload; all integration assertions passed',flush=True)
        except BaseException as failure:
            if config.exists():
                for args in [('get','pods','-A','-o','wide'),('-n','fleets','get','celldfleets','-o','yaml'),('get','celldstoragereservations','-o','json'),('-n','fleets','get','events','--sort-by=.lastTimestamp')]:
                    try: print(k(*args),flush=True)
                    except RuntimeError as err: print(err,flush=True)
            if (tmp/'operator.log').exists():print((tmp/'operator.log').read_text(),flush=True)
            if bucket_lifecycle and config.exists():
                try: print(k('-n','celld-system','logs','deployment/celld-operator'),flush=True)
                except RuntimeError: pass
            # Optional bounded diagnostic window for an explicitly owned local
            # cluster. Normal runs still clean immediately, including interrupts.
            hold=min(600,max(0,int(os.environ.get('CELLD_TEST_DIAGNOSTIC_HOLD_SECONDS','0'))))
            if hold and not isinstance(failure,KeyboardInterrupt):
                marker=tmp/'continue-cleanup'
                print('Diagnostic hold:',config,'context=kind-'+name,'touch',marker,'to clean early',flush=True)
                until=time.monotonic()+hold
                while time.monotonic()<until and not marker.exists(): time.sleep(2)
            raise
        finally:
            try:
                if process is not None and process.poll() is None:
                    process.terminate()
                    try:
                        process.wait(timeout=20)
                    except subprocess.TimeoutExpired:
                        process.kill();process.wait(timeout=10)
            finally:
                if created:
                    print('Cleaning up only cluster',name,flush=True)
                    run(['kind','delete','cluster','--name',name,'--kubeconfig',str(config)],env=env,timeout=180)
                if launcher_image:
                    run(['docker','image','rm',launcher_image],timeout=60)

if __name__=='__main__':
    def interrupted(signum, frame):
        raise KeyboardInterrupt('integration interrupted')
    signal.signal(signal.SIGTERM, interrupted)
    main()
