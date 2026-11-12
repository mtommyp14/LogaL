# LogaL — Kubernetes Log Viewer

[![CI](https://github.com/mtommyp14/LogaL/actions/workflows/ci.yml/badge.svg)](https://github.com/mtommyp14/LogaL/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-13+-4169E1?logo=postgresql&logoColor=white)](https://postgresql.org)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.25+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io)

**Dark-themed web UI for viewing and searching Kubernetes pod logs with automatic collection and persistent history.**

LogaL runs as a single Go binary. It is deployed as a **Kubernetes DaemonSet** so that one pod runs on every node and collects logs only from pods scheduled on that node. All logs are stored in a central PostgreSQL database and served through a fast web UI.

> **No `stern` dependency.** LogaL streams logs directly through `kubectl logs --follow`, so the container image stays small and only needs `kubectl`.

---

## 🚀 Quick Start

### 1. Local (with existing `kubectl` access)

```bash
# Clone / download
cd LogaL

# Set database (PostgreSQL required)
export DATABASE_URL=postgres://user:pass@localhost:5432/logal

# Protect UI and API (browser prompts for credentials)
export AUTH_USERNAME=admin
export AUTH_PASSWORD=change-this-password

# Run
./start.sh
```

Then open http://localhost:8080.

### 2. Kubernetes

```bash
kubectl create namespace logging
kubectl create secret generic logal-auth -n logging \
  --from-literal=username=admin --from-literal=password=change-this-password

# 1. Create database secret
kubectl create secret generic logal-db \
  --from-literal=url="postgres://logal:password@db-host:5432/logal" \
  -n logging

# 2. Deploy
kubectl apply -f daemonset.yaml

# 3. Access the UI
kubectl port-forward -n logging svc/logal 8080:80
```

---

## ✨ Features

- **Automatic log collection** — one LogaL pod per node collects every container's logs from the same node.
- **Distributed load** — each pod only watches a small number of local pods, so it scales with the cluster.
- **Dark theme** UI optimized for long log sessions.
- **Real-time streaming** via Server-Sent Events (`kubectl logs`).
- **Persistent history** in PostgreSQL with configurable retention (1–365 days).
- **Collection Settings** in the UI: persist all workloads or select exact cluster/namespace/workload identities; choose retention duration. Settings are stored in PostgreSQL and shared across collector pods.
- **Custom time range** picker with live UTC hints and per-row filtering.
- **Multi-pod / multi-container** selector, smart sidecar OFF by default.
- **Grep filter** applied both in real-time and history.
- **Log level highlighting**: ERROR (red), WARN (yellow), INFO (green).
- **Date dividers** and **UTC timestamps**.
- **Copy & download** logs.
- **Auto-reconnect** on pod restart / network blip.

## Screenshots

### Home

![LogaL home screen](docs/screenshots/01-home.png)

### Live Kubernetes logs

![Streaming demo-api logs](docs/screenshots/02-demo-api-logs.png)

### Collection settings — save all workloads

![Collection settings with all workloads selected](docs/screenshots/03-collection-settings.png)

### Collection settings — save selected workload

![Collection settings selecting demo-api in logal-sandbox](docs/screenshots/04-specific-workload-selection.png)

---

## 🏗️ Architecture

LogaL is deployed as a **DaemonSet**. Each pod is bound to its node via `NODE_NAME` and only collects logs from pods running on that node.

```mermaid
graph LR
    A[Browser] -->|HTTP / SSE| B(LogaL Pod)
    B -->|kubectl logs --follow| C[Pods on same node]
    B -->|INSERT / SELECT| D[(PostgreSQL)]
    D -->|history stream| B
    B -->|SSE| A
```

### Log collection mode

```mermaid
sequenceDiagram
    participant L as LogaL Pod on Node N
    participant K as Kubernetes API
    participant D as PostgreSQL

    loop watch pods on node N
        K->>L: pod ADDED / MODIFIED / DELETED
        L->>L: start/stop per-container streams
    end
    loop per running container on node N
        K->>L: log lines
        L->>D: INSERT log
    end
```

### On-demand streaming mode

```mermaid
sequenceDiagram
    participant U as Browser
    participant L as LogaL Pod
    participant K as Kubernetes API
    participant D as PostgreSQL

    U->>L: GET /api/logs?workload=...
    par history
        L->>D: SELECT logs since ...
        D->>L: rows
        L->>U: SSE data lines
    and live
        L->>K: kubectl logs --follow per pod/container
        K->>L: log lines
        L->>U: SSE data line
    end
```

### Custom range mode

```mermaid
sequenceDiagram
    participant U as Browser
    participant L as LogaL Pod
    participant D as PostgreSQL

    U->>L: GET /api/logs?from=...&to=...
    L->>D: SELECT logs BETWEEN ...
    D->>L: rows
    L->>U: SSE data lines
    L->>U: __END__
```

### Flow

```
User opens LogaL UI
         ↓
Each node already collects its local pod logs into PostgreSQL
         ↓
User selects workload + time range
         ↓
LogaL queries PostgreSQL history → stream to browser (history)
LogaL also starts live kubectl logs → stream new logs in real-time
         ↓
Browser displays:
  - Scroll up   = history logs
  - Scroll down = latest real-time logs

Custom range mode:
  → queries PostgreSQL history only (no live streaming)
  → filters each row by from/to timestamp (UTC)
  → sends __END__ signal when done
```

---

## 📋 Requirements

- Kubernetes cluster (1.25+) or local `kubectl` access
- PostgreSQL 13+
- `kubectl` installed locally (for `start.sh`)
- Go 1.25+ (for building from source)

---

## 📦 Installation

### Build from source

```bash
# Clone
git clone https://github.com/mtommyp14/LogaL.git
cd LogaL

# Download dependencies
go mod tidy

# Build
go build -o logal .

# Build migration helper
go build -o logal-migrate ./cmd/migrate
```

### Build Docker image

```bash
# Single platform (local)
docker build -t your-registry/logal:latest .
docker push your-registry/logal:latest

# Multi-platform (for mixed local M-series Mac and Linux EKS/AMD64 clusters)
docker buildx build --platform linux/amd64,linux/arm64 \
  -t your-registry/logal:latest --push .
```

---

## ⚙️ Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP server port |
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/logal` | **Required** PostgreSQL connection string |
| `KUBECONFIG` | `~/.kube/config` | Path to kubeconfig (optional for multi-cluster) |
| `AUTH_USERNAME` / `AUTH_PASSWORD` | *(required)* | HTTP Basic credentials for UI and all APIs. Use HTTPS at the ingress. |
| `AUTH_DISABLED` | `false` | Explicitly set `true` only for local development without authentication. |
| `NODE_NAME` | `''` | Node name for local pod filtering (set via downward API in DaemonSet) |
| `LOG_RETENTION_DAYS` | `7` | How many days of log history to keep. The UI time-range dropdown is generated from this value. |
| `ALLOWED_NAMESPACES` | *(empty)* | Comma-separated namespace whitelist (optional). Empty = all namespaces. |

---

## 🚢 Deployment

The included `daemonset.yaml` creates:
- `logging` namespace
- ServiceAccount + ClusterRole + ClusterRoleBinding (read pods, logs, namespaces, workloads)
- **DaemonSet** that runs one LogaL pod per node
- **Service** for the UI/API
- Optional **Ingress** template (commented)

### Single Cluster (in-cluster)

```bash
kubectl create namespace logging
kubectl create secret generic logal-auth -n logging \
  --from-literal=username=admin --from-literal=password=change-this-password

# 1. Create database secret
kubectl create secret generic logal-db \
  --from-literal=url="postgres://logal:password@db-host:5432/logal" \
  -n logging

# 2. Deploy
kubectl apply -f daemonset.yaml

# 3. Open port-forward (or expose via Ingress)
kubectl port-forward -n logging svc/logal 8080:80
```

### How it works after deploy

1. A LogaL pod is created on every node.
2. Each pod uses the `NODE_NAME` downward API to only watch pods on its own node.
3. Every pod starts a `kubectl logs --follow` stream for each local container and inserts every line into PostgreSQL.
4. Open the UI and pick any workload — the history is already there.

### Multi-Cluster

Use the central Deployment with **one replica** and no `NODE_NAME`. Each kubeconfig context gets its own collector and database cluster identity. Context names must remain stable. Do not run the DaemonSet against the same clusters/database simultaneously. A DaemonSet is for a single in-cluster installation; its stored identity is `in-cluster` (use a separate database for independent installations).

```bash
kubectl create namespace logging
KUBECONFIG=./kube-a:./kube-b kubectl config view --flatten > merged
kubectl create secret generic logal-kubeconfig --from-file=config=merged -n logging
kubectl create secret generic logal-db --from-literal=url="postgres://logal:password@db-host:5432/logal" -n logging
kubectl create secret generic logal-auth -n logging \
  --from-literal=username=admin --from-literal=password=change-this-password
kubectl apply -f deployment-multicluster.yaml
```

Kubeconfig credentials need read access to pods, pod logs, and workloads in each target cluster. External credential helper binaries must be available in the image if the kubeconfig uses them. The runtime image does not bundle cloud provider login tools.

To add a cluster, merge its kubeconfig, then update the secret and restart:

```bash
kubectl create secret generic logal-kubeconfig -n logging \
  --from-file=config=merged --dry-run=client -o yaml | kubectl apply -f -
kubectl rollout restart deployment/logal -n logging
```

Authentication applies to UI, APIs, and SSE streams. `/healthz` is unauthenticated for Kubernetes probes. Basic Auth requires HTTPS when exposed outside a local port-forward. `ALLOWED_NAMESPACES` is enforced on both collection and direct API requests; credentials share the same namespace access.

Older collector records stored under `kubeconfig` or `namespace/pod` cannot be safely assigned to a cluster/workload automatically. They remain in PostgreSQL until retention cleanup; new collection uses the corrected identities. No existing rows are rewritten. Collection is best effort; process restarts may replay recent lines and there is no exactly-once guarantee.

---

## 🔄 Migration from Flat Files

If you have existing `/data/logs` flat files from a previous LogaL/LogT deployment:

```bash
# Run locally or inside the pod
export DATABASE_URL=postgres://logal:password@db-host:5432/logal
export LOG_DIR=/data/logs

cd cmd/migrate
go run .
```

The migration reads files like `/data/logs/{cluster}/{namespace}/{workload}/YYYY-MM-DD.log` and inserts them into PostgreSQL.

---

## 🛠️ Development

```bash
# Run locally (requires PostgreSQL running)
./start.sh

# Or build + run manually
export DATABASE_URL=postgres://postgres:postgres@localhost:5432/logal
export AUTH_USERNAME=admin AUTH_PASSWORD=change-this-password
go run .
```

The schema is auto-created on startup.

---

## 🤝 Contributing

Contributions are welcome!

1. Fork the repository.
2. Create a feature branch: `git checkout -b feat/my-feature`.
3. Commit your changes.
4. Push to the branch and open a Pull Request.

---

## 📄 License

This project is licensed under the [MIT License](LICENSE).
