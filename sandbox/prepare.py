#!/usr/bin/env python3
"""Generate local-only sandbox credentials without printing them."""
import json
import secrets
from pathlib import Path

folder=Path('.sandbox')
folder.mkdir(exist_ok=True)
path=folder/'credentials.json'
if not path.exists():
    path.write_text(json.dumps(dict(username='sandbox',password=secrets.token_urlsafe(20),database_password=secrets.token_urlsafe(20)),indent=2))
    path.chmod(0o600)
credentials=json.loads(path.read_text())
data={'username':credentials['username'],'password':credentials['password'],'database-password':credentials['database_password'],'database-url':'postgres://logal:'+credentials['database_password']+'@postgres:5432/logal?sslmode=disable'}
(folder/'secret.json').write_text(json.dumps({'apiVersion':'v1','kind':'Secret','metadata':{'name':'sandbox-credentials','namespace':'logal-sandbox'},'stringData':data}))
(folder/'OPEN-LOGAL.md').write_text('Open http://localhost:18081 in your Windows browser.\n\nUsername: '+credentials['username']+'\n\nPassword: '+credentials['password']+'\n\nIf the connection stops, run in PowerShell:\n\n```powershell\nkubectl --context docker-desktop -n logal-sandbox port-forward svc/logal 18081:8080\n```\n')
print('Local credentials and secret manifest prepared in .sandbox (excluded from Git and Docker context).')
