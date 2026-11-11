# Docker Desktop sandbox verification

Verified on 2026-11-11 against Docker Desktop Kubernetes v1.36.1 using the locally built `logal:sandbox-verified` image. The sandbox remains running.

**37/37 integration checks passed.** Go regression tests, `go vet`, JavaScript syntax checks, and Docker image builds also passed.

| Suite | Result | Coverage |
|---|---|---|
| HTTP/API/SSE | 19/19 | Auth, health, read-only API, cluster/namespace/workload/pod/container discovery, three live pods, history for current and replaced pods, grep, pod/container filters, quoted text, sidecars, init logs, StatefulSet/DaemonSet streams and age, namespace denial, invalid inputs, concurrent clients, history metadata |
| Browser | 11/11 | Browser Basic Auth, pod selection, safe text rendering, live/stop, clipboard, a newly downloaded real `.log` file, custom history completion/highlighting, StatefulSet selection, All types, session restore, no uncaught JS exceptions |
| Lifecycle/database | 4/4 | Forbidden namespace excluded from PostgreSQL, init replay suppressed during reconnect, three new pods collected with old history retained, expired fixture removed while recent fixture/history survives collector restart |
| Context routing | 3/3 | Separate collection/history identities for local-east/local-west, namespace restriction across both contexts |

A bug found during verification was fixed: when Type is All, the UI now infers the selected workload's kind for pod discovery, pod age, and live streaming. Previously a StatefulSet could be treated as a Deployment.

## Access

From Windows, open http://localhost:18081. Local credentials and a port-forward reconnect command are in `.sandbox/OPEN-LOGAL.md`; secrets are excluded from Git and Docker context. The port-forward must remain running and needs reconnecting after the LogaL pod is replaced.

Running workloads: LogaL, PostgreSQL, demo-api (three replicas, each with app/sidecar/init containers), demo-worker (StatefulSet), demo-agent (DaemonSet), and forbidden-app in a separate namespace. The extra context-test Deployment has been scaled to zero.

## Scope and limits

- Both context aliases target the same physical Kubernetes cluster. Independent-cluster networking, cloud auth, and failure isolation are not verified.
- This is a functional sandbox test, not sustained load/performance testing or a security audit.
- PostgreSQL uses `emptyDir`; its data is lost when its pod is replaced. Restart tests replaced LogaL/application pods, not PostgreSQL.
- Replay suppression operates within a collector process. Exactly-once ingestion across process restarts is not guaranteed.
- Per-user accounts/roles, SSO/OIDC, and ingress TLS are not implemented or claimed by these tests.

Raw reports, a browser screenshot, and actual downloaded logs are under `.sandbox/`. Reusable manifests and scripts are in `sandbox/`; see `sandbox/README.md` for setup, reruns, and removal.
