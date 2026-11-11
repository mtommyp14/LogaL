#!/usr/bin/env python3
"""Destructive only to dedicated sandbox workloads/fixture rows; no production resources."""
import json
import subprocess
import time
from pathlib import Path

prefix=['kubectl','--kubeconfig','/mnt/c/Users/Hi/.kube/config','--context','docker-desktop']
results=[]

def kube(*args):
    p=subprocess.run(prefix+list(args),text=True,capture_output=True,timeout=100)
    if p.returncode:raise RuntimeError(p.stderr[-1500:])
    return p.stdout.strip()

def sql(query):
    return kube('exec','-n','logal-sandbox','deployment/postgres','--','psql','-U','logal','-d','logal','-Atc',query)

def poll(fn):
    until=time.monotonic()+60
    while time.monotonic()<until:
        if fn():return
        time.sleep(2)
    raise AssertionError('condition did not become true in 60 seconds')

def check(name,fn):
    try:
        detail=fn();results.append(dict(name=name,status='PASS',detail=detail));print('PASS',name,detail or '',flush=True)
    except Exception as e:
        results.append(dict(name=name,status='FAIL',error=str(e)));print('FAIL',name,str(e),flush=True)

def isolation():
    assert sql("SELECT count(*) FROM logs WHERE namespace <> 'logal-sandbox'")=='0'
    assert sql("SELECT count(*) FROM logs WHERE message LIKE '%SANDBOX_FORBIDDEN%'")=='0'

def init_dedup():
    duplicate=sql("SELECT count(*) FROM (SELECT cluster,pod,container,ts,message,count(*) n FROM logs WHERE container='init-config' GROUP BY 1,2,3,4,5 HAVING count(*)>1) d")
    assert duplicate=='0',duplicate
    return {'init_rows':int(sql("SELECT count(*) FROM logs WHERE container='init-config'"))}

def restart_workload():
    pods=json.loads(kube('get','pods','-n','logal-sandbox','-l','test-workload=api','-o','json'))
    old=[p['metadata']['name'] for p in pods['items']]
    kube('rollout','restart','deployment/demo-api','-n','logal-sandbox')
    kube('rollout','status','deployment/demo-api','-n','logal-sandbox','--timeout=60s')
    pods=json.loads(kube('get','pods','-n','logal-sandbox','-l','test-workload=api','-o','json'))
    new=[p['metadata']['name'] for p in pods['items'] if 'deletionTimestamp' not in p['metadata']]
    assert len(new)==3 and set(new).isdisjoint(old),(old,new)
    for pod in new:poll(lambda:int(sql("SELECT count(*) FROM logs WHERE pod='"+pod+"' AND container='app'"))>0)
    for pod in old:assert int(sql("SELECT count(*) FROM logs WHERE pod='"+pod+"'"))>0
    return {'old_pods_retained':len(old),'new_pods_collected':len(new)}

def retention_and_restart():
    sql("INSERT INTO logs(cluster,namespace,workload,pod,container,ts,message) VALUES ('sandbox-fixture','logal-sandbox','fixture','fixture','app',now()-interval '4 days','SANDBOX_RETENTION_OLD'),('sandbox-fixture','logal-sandbox','fixture','fixture','app',now()-interval '1 hour','SANDBOX_RETENTION_KEEP')")
    before=int(sql("SELECT count(*) FROM logs WHERE workload='demo-api'"))
    kube('rollout','restart','deployment/logal','-n','logal-sandbox')
    kube('rollout','status','deployment/logal','-n','logal-sandbox','--timeout=60s')
    poll(lambda:sql("SELECT count(*) FROM logs WHERE message='SANDBOX_RETENTION_OLD'")=='0')
    assert sql("SELECT count(*) FROM logs WHERE message='SANDBOX_RETENTION_KEEP'")=='1'
    assert int(sql("SELECT count(*) FROM logs WHERE workload='demo-api'"))>=before
    sql("DELETE FROM logs WHERE cluster='sandbox-fixture'")
    return {'expired_rows_removed':True,'recent_rows_preserved':True,'history_survives_collector_restart':True}

check('database excludes forbidden namespace',isolation)
check('init-container reconnect does not duplicate records',init_dedup)
check('pod replacement collects new pods and retains old history',restart_workload)
check('retention cleanup and collector restart preserve recent history',retention_and_restart)
Path('.sandbox/lifecycle-results.json').write_text(json.dumps(results,indent=2))
raise SystemExit(any(r['status']=='FAIL' for r in results))
