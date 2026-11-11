#!/usr/bin/env python3
"""Exercise two context identities against one local cluster; not a two-cluster test."""
import json
import subprocess
import time
from pathlib import Path

prefix=['kubectl','--kubeconfig','/mnt/c/Users/Hi/.kube/config','--context','docker-desktop']
def kube(*args):
    p=subprocess.run(prefix+list(args),text=True,capture_output=True,timeout=100)
    if p.returncode:raise RuntimeError(p.stderr[-2000:])
    return p.stdout.strip()
def sql(q):return kube('exec','-n','logal-sandbox','deployment/postgres','--','psql','-U','logal','-d','logal','-Atc',q)

config='''apiVersion: v1
kind: Config
clusters:
- name: desktop
  cluster:
    server: https://kubernetes.default.svc
    certificate-authority: /var/run/secrets/kubernetes.io/serviceaccount/ca.crt
users:
- name: sandbox-reader
  user:
    tokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
contexts:
- name: local-east
  context: {cluster: desktop, user: sandbox-reader}
- name: local-west
  context: {cluster: desktop, user: sandbox-reader}
current-context: local-east
'''
cm={'apiVersion':'v1','kind':'ConfigMap','metadata':{'name':'logal-sandbox-contexts','namespace':'logal-sandbox'},'data':{'config':config}}
source=json.loads(kube('get','deployment','logal','-n','logal-sandbox','-o','json'))
name='logal-contexts'
source['metadata']={'name':name,'namespace':'logal-sandbox'}
source.pop('status',None)
source['spec']['selector']['matchLabels']={'app':name}
source['spec']['template']['metadata']={'labels':{'app':name}}
pod=source['spec']['template']['spec']
pod['volumes']=[{'name':'contexts','configMap':{'name':'logal-sandbox-contexts'}}]
container=pod['containers'][0]
container['env'].append({'name':'KUBECONFIG','value':'/contexts/config'})
container['volumeMounts']=[{'name':'contexts','mountPath':'/contexts','readOnly':True}]
container['resources']['limits']['memory']='1Gi'
Path('.sandbox/multicontext.json').write_text(json.dumps({'apiVersion':'v1','kind':'List','items':[cm,source]}))
results=[]
try:
    kube('apply','-f','.sandbox/multicontext.json')
    kube('rollout','status','deployment/'+name,'-n','logal-sandbox','--timeout=60s')
    for cluster in ['local-east','local-west']:
        until=time.monotonic()+60
        while int(sql("SELECT count(*) FROM logs WHERE cluster='"+cluster+"' AND workload='demo-api'"))==0:
            if time.monotonic()>until:raise AssertionError(cluster+' has no collected logs')
            time.sleep(2)
        cmd='curl -fsS -u "$AUTH_USERNAME:$AUTH_PASSWORD" "http://127.0.0.1:8080/api/contexts"'
        contexts=json.loads(kube('exec','-n','logal-sandbox','deployment/'+name,'--','sh','-c',cmd))
        assert contexts==['local-east','local-west'],contexts
        cmd='curl -fsS -u "$AUTH_USERNAME:$AUTH_PASSWORD" "http://127.0.0.1:8080/api/history-info?ctx='+cluster+'&ns=logal-sandbox&workload=demo-api"'
        history=json.loads(kube('exec','-n','logal-sandbox','deployment/'+name,'--','sh','-c',cmd))
        assert history['available'],history
        results.append({'name':cluster+' collection and history routing','status':'PASS'})
        print('PASS',cluster,'collection and history routing',flush=True)
    assert sql("SELECT count(*) FROM logs WHERE cluster IN ('local-east','local-west') AND namespace <> 'logal-sandbox'")=='0'
    results.append({'name':'namespace isolation across both contexts','status':'PASS'})
    print('PASS namespace isolation across both contexts',flush=True)
except Exception as e:
    results.append({'name':'multi-context integration','status':'FAIL','error':str(e)})
    print('FAIL multi-context integration',e,flush=True)
finally:
    # Stop the extra collectors; leave the main UI untouched and keep fixture logs.
    kube('scale','deployment/'+name,'-n','logal-sandbox','--replicas=0')
    Path('.sandbox/multicontext-results.json').write_text(json.dumps({'scope':'two aliases on the same Docker Desktop cluster, not independent clusters','results':results},indent=2))
raise SystemExit(any(r['status']=='FAIL' for r in results))
