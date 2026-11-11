# Local Kubernetes sandbox

This sandbox uses the existing Docker Desktop Kubernetes cluster. It does not create another cluster. Resources live in `logal-sandbox` and `logal-sandbox-denied`; the read-only ClusterRole/Binding is named `logal-sandbox-reader`.

It runs PostgreSQL with temporary `emptyDir` storage, LogaL with authentication, a three-replica Deployment with app/sidecar/init containers, a StatefulSet, a DaemonSet, and a forbidden-namespace application. Database contents survive LogaL restarts but **do not survive replacement of the PostgreSQL pod**. This is a test environment.

## Setup

Run from the repository root, with a Docker CLI connected to Docker Desktop and Python 3 available:

```bash
python3 sandbox/prepare.py
docker build -t logal:sandbox-verified .
```

Docker Desktop's Kind provisioner uses a separate containerd image store. For the local node named `desktop-control-plane`, import the image using a binary-safe shell redirect:

```bash
docker save -o .sandbox/logal.tar logal:sandbox-verified
docker exec -i desktop-control-plane ctr -n k8s.io images import - < .sandbox/logal.tar
```

In WSL where only the Windows Docker CLI works, substitute its executable path. Run the import with a shell that supports binary stdin redirection (e.g. Bash or `cmd.exe`). Other Kubernetes provisioners may require a different image-loading command.

```bash
kubectl --context docker-desktop create namespace logal-sandbox --dry-run=client -o yaml | kubectl --context docker-desktop apply -f -
kubectl --context docker-desktop apply -f .sandbox/secret.json
kubectl --context docker-desktop apply -f sandbox/manifest.yaml
kubectl --context docker-desktop -n logal-sandbox rollout status deployment/logal
```

For access from a Windows browser, run in PowerShell and leave the terminal open:

```powershell
kubectl --context docker-desktop -n logal-sandbox port-forward svc/logal 18081:8080
```

Open **http://localhost:18081**. Credentials and the reconnect command are in `.sandbox/OPEN-LOGAL.md`. Select namespace `logal-sandbox`, workload `demo-api`, then Start. Port-forward pins one pod and must be restarted after replacing the LogaL pod.

## Verification

`check.py` uses only Python's standard library and tests HTTP auth, namespace access, discovery, history, grep, pod/container selection, quotes/backslashes, live SSE, sidecars, init logs, workload types, concurrent clients, and validation:

```bash
python3 sandbox/check.py --url http://127.0.0.1:18081
```

When Python runs inside WSL, forward a separate WSL port `18080` and use the script's default URL.

`lifecycle.py` tests database isolation, replay suppression, pod replacement, retained history, collector restart, and cleanup of expired fixture rows. `multicontext.py` temporarily starts another collector with two context aliases and scales it to zero afterward. Both scripts currently select the local kubeconfig at `/mnt/c/Users/Hi/.kube/config` explicitly and only modify sandbox workloads/data:

```bash
python3 sandbox/lifecycle.py
python3 sandbox/multicontext.py
```

The context aliases both point to **the same Kubernetes cluster**. This verifies context routing/storage identity, not independent-cluster networking, cloud authentication, or failure isolation.

`browser-check.mjs` uses Node's built-in WebSocket and an isolated Chrome DevTools instance at port 19222. It tests browser auth, live rendering, stop, clipboard, file download, custom history, StatefulSet selection (including All types), session restore, and JavaScript errors. On Windows, launch Chrome headless with an isolated `.sandbox/chrome-profile` and `--remote-debugging-port=19222`, then run with Windows Node:

```powershell
node sandbox/browser-check.mjs
```

Local reports, screenshots, downloaded files, credentials, and browser profile are under `.sandbox/` and are excluded from Git and Docker builds. Do not commit these artifacts.

## Remove the sandbox

This removes its temporary database and collected history:

```bash
kubectl --context docker-desktop delete namespace logal-sandbox logal-sandbox-denied
kubectl --context docker-desktop delete clusterrolebinding logal-sandbox-reader
kubectl --context docker-desktop delete clusterrole logal-sandbox-reader
```
