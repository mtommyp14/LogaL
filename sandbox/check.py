#!/usr/bin/env python3
"""Black-box HTTP/SSE checks against a running sandbox, without third-party packages."""
import argparse
import base64
import concurrent.futures
import datetime as dt
import json
import socket
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument('--url', default='http://127.0.0.1:18080')
parser.add_argument('--credentials', default='.sandbox/credentials.json')
parser.add_argument('--report', default='.sandbox/http-results.json')
args = parser.parse_args()
credentials = json.loads(Path(args.credentials).read_text())
auth = base64.b64encode((credentials['username'] + ':' + credentials['password']).encode()).decode()
results = []
context = None


def request(path, params=None, authorization=auth, method='GET'):
    if params:
        path += '?' + urllib.parse.urlencode(params)
    headers = {} if authorization is None else {'Authorization': 'Basic ' + authorization}
    req = urllib.request.Request(args.url + path, headers=headers, method=method)
    try:
        return urllib.request.urlopen(req, timeout=25)
    except urllib.error.HTTPError as exc:
        return exc


def status(path, expected, **kwargs):
    with request(path, **kwargs) as response:
        assert response.status == expected, (path, response.status, response.read().decode())


def get(path, **query):
    with request(path, query or None) as response:
        assert response.status == 200, (path, response.status, response.read().decode())
        return json.load(response)


def query(**extra):
    return dict(ctx=context, ns='logal-sandbox', workload='demo-api', kind='Deployment', **extra)


def stream(params, limit=None):
    lines = []
    ended = False
    with request('/api/logs', params) as response:
        assert response.status == 200, (response.status, response.read().decode())
        assert 'text/event-stream' in response.headers.get('Content-Type', '')
        for raw in response:
            text = raw.decode().strip()
            if not text.startswith('data: '):
                continue
            payload = text[6:]
            if payload == '__END__':
                ended = True
                break
            lines.append(json.loads('"' + payload + '"'))
            if limit and len(lines) >= limit:
                break
    return lines, ended


def history(**extra):
    now = dt.datetime.now(dt.timezone.utc)
    params = query(**extra)
    params.update({'from': (now - dt.timedelta(minutes=30)).isoformat(), 'to': now.isoformat()})
    lines, ended = stream(params)
    assert ended, 'custom history did not terminate'
    return lines


def check(name, fn):
    started = time.monotonic()
    try:
        detail = fn()
        results.append(dict(name=name, status='PASS', seconds=round(time.monotonic()-started, 3), detail=detail))
        print('PASS', name, '' if detail is None else detail, flush=True)
    except Exception as exc:
        results.append(dict(name=name, status='FAIL', seconds=round(time.monotonic()-started, 3), error=str(exc)))
        print('FAIL', name, str(exc), flush=True)


def discover():
    global context
    contexts = get('/api/contexts')
    assert contexts
    context = contexts[0]
    return contexts


def discovery():
    assert get('/api/namespaces', ctx=context) == ['logal-sandbox']
    workloads = get('/api/workloads', ctx=context, ns='logal-sandbox')
    for name, kind in [('demo-api', 'Deployment'), ('demo-worker', 'StatefulSet'), ('demo-agent', 'DaemonSet')]:
        assert {'name': name, 'kind': kind} in workloads, workloads
    pods = get('/api/pods', **query())
    assert len(pods) == 3, pods
    for pod in pods:
        flags = dict(zip(pod['containers'], pod['is_sidecar']))
        assert flags == {'app': False, 'istio-proxy': True, 'init-config': True}, flags
    return {'workloads': len(workloads), 'deployment_pods': len(pods)}


def history_check():
    lines = history(containers='app')
    assert any('SANDBOX_API' in line for line in lines), lines[:3]
    history_pods = {line.split(' [', 1)[1].split(']')[0] for line in lines}
    current_pods = {pod['name'] for pod in get('/api/pods', **query())}
    assert len(current_pods) == 3 and current_pods <= history_pods, history_pods
    assert all('][app]' in line for line in lines)
    return {'rows': len(lines), 'history_pods': len(history_pods), 'current_pods': len(current_pods)}


def selected_history():
    pod = get('/api/pods', **query())[0]['name']
    lines = history(pods=pod, containers='app', filter='sandbox_error')
    assert lines and all('['+pod+'][app]' in line and 'SANDBOX_ERROR' in line for line in lines)
    return {'rows': len(lines), 'pod': pod}


def quoted_history():
    lines = history(containers='app', filter='SANDBOX_QUOTED')
    assert lines
    assert all('{"value":"hello","path":"C:\\\\tmp","html":"<tag>"}' in line for line in lines), lines[:1]
    return {'rows': len(lines)}


def live():
    lines, ended = stream(query(since='realtime', tail='1', containers='app', filter='SANDBOX_API'), limit=6)
    assert len(lines) == 6 and not ended
    assert all('SANDBOX_API' in line and '][app]' in line for line in lines)
    return {'events': len(lines)}


def sidecar():
    lines, _ = stream(query(since='realtime', tail='1', containers='istio-proxy'), limit=3)
    assert len(lines) == 3 and all('SANDBOX_SIDECAR' in line for line in lines)


def init_logs():
    pod = get('/api/pods', **query())[0]['name']
    data = get('/api/container-logs', ctx=context, ns='logal-sandbox', pod=pod, container='init-config', init='true')
    assert any('SANDBOX_INIT' in line for line in data['lines']), data


def workload_kinds():
    for workload, kind, marker in [('demo-worker', 'StatefulSet', 'SANDBOX_WORKER'), ('demo-agent', 'DaemonSet', 'SANDBOX_AGENT')]:
        params = dict(ctx=context, ns='logal-sandbox', workload=workload, kind=kind, since='realtime', tail='1')
        lines, _ = stream(params, limit=2)
        assert all(marker in line for line in lines), lines
        age = get('/api/pod-age', ctx=context, ns='logal-sandbox', workload=workload, kind=kind)
        assert age['pod_name'].startswith(workload), age


def namespace_boundary():
    for endpoint in ['workloads', 'pods', 'pod-age', 'history-info', 'logs', 'container-logs']:
        params = dict(ctx=context, ns='logal-sandbox-denied', workload='forbidden-app', pod='forbidden', container='app')
        status('/api/'+endpoint, 403, params=params)


def invalid_requests():
    for extra in [{'from': 'bad'}, {'since': 'bad'}, {'tail': '-1'}, {'from': '2026-01-02T00:00:00Z', 'to': '2026-01-01T00:00:00Z'}]:
        status('/api/logs', 400, params=query(**extra))
    status('/api/pods', 400, params=dict(ctx='does-not-exist', ns='logal-sandbox'))


def concurrent_clients():
    with concurrent.futures.ThreadPoolExecutor(max_workers=3) as executor:
        streams = list(executor.map(lambda _: stream(query(since='realtime', tail='1', containers='app', filter='SANDBOX_API'), limit=3)[0], range(3)))
    assert all(len(lines) == 3 for lines in streams)
    return {'clients': 3, 'events': 9}


check('unauthenticated UI denied', lambda: status('/', 401, authorization=None))
check('unauthenticated API denied', lambda: status('/api/config', 401, authorization=None))
check('wrong password denied', lambda: status('/api/config', 401, authorization=base64.b64encode(b'sandbox:wrong').decode()))
check('public health probe', lambda: status('/healthz', 200, authorization=None))
check('authenticated UI', lambda: status('/', 200))
check('API disallows writes', lambda: status('/api/config', 405, method='POST'))
check('cluster discovery', discover)
check('namespace workload pod and container discovery', discovery)
check('custom history across three pods', history_check)
check('history pod/container/case-insensitive grep selection', selected_history)
check('history preserves quotes backslashes and HTML text', quoted_history)
check('live SSE across multiple pods', live)
check('explicit sidecar stream', sidecar)
check('terminated init-container logs', init_logs)
check('StatefulSet and DaemonSet live logs and age', workload_kinds)
check('namespace access blocked at all APIs', namespace_boundary)
check('invalid inputs rejected', invalid_requests)
check('concurrent SSE clients', concurrent_clients)
check('history metadata available', lambda: (_ for _ in ()).throw(AssertionError('no history')) if not get('/api/history-info', **query())['available'] else None)
Path(args.report).parent.mkdir(parents=True, exist_ok=True)
Path(args.report).write_text(json.dumps(results, indent=2))
failed = sum(row['status'] == 'FAIL' for row in results)
print(f'{len(results)-failed}/{len(results)} checks passed')
raise SystemExit(1 if failed else 0)
