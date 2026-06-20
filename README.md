# LogaL — Kubernetes Log Viewer

> **English** | [Bahasa Indonesia](#bahasa-indonesia)

[![CI](https://github.com/mtommyp14/LogaL/actions/workflows/ci.yml/badge.svg)](https://github.com/mtommyp14/LogaL/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-13+-4169E1?logo=postgresql&logoColor=white)](https://postgresql.org)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.25+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io)

**Dark-themed web UI for viewing and searching Kubernetes pod logs with automatic collection and persistent history.**

LogaL runs as a single Go binary, watches every pod in the cluster using `kubectl logs`, stores all logs in PostgreSQL, and serves them through a fast web UI. It uses leader election so only one pod collects logs while standby replicas provide high availability.

> **No `stern` dependency.** LogaL streams logs directly through `kubectl logs --follow`, so the container image stays small and has no extra CLI tools beyond `kubectl`.

---

## 🚀 Quick Start

### 1. Local (with existing `kubectl` access)

```bash
# Clone / download
cd LogaL

# Set database (PostgreSQL required)
export DATABASE_URL=postgres://user:pass@localhost:5432/logal

# Run
./start.sh
```

Then open http://localhost:8080.

### 2. Kubernetes

```bash
# 1. Create database secret
kubectl create secret generic logal-db \
  --from-literal=url="postgres://logal:password@db-host:5432/logal" \
  -n logging

# 2. Deploy
kubectl apply -f deployment.yaml
```

---

## ✨ Features

- **Automatic log collection** — watches all pods in all namespaces and stores every container's logs in PostgreSQL.
- **Leader election** — only one pod collects logs; standby replicas take over automatically during rolling updates or failures.
- **Dark theme** UI optimized for long log sessions.
- **Real-time streaming** via Server-Sent Events (`kubectl logs`).
- **Persistent history** in PostgreSQL with configurable retention (`LOG_RETENTION_DAYS`).
- **Custom time range** picker with live UTC hints and per-row filtering.
- **Multi-pod / multi-container** selector, smart sidecar OFF by default.
- **Grep filter** applied both in real-time and history.
- **Log level highlighting**: ERROR (red), WARN (yellow), INFO (green).
- **Date dividers** and **UTC timestamps**.
- **Copy & download** logs.
- **Auto-reconnect** on pod restart / network blip.
- **Multi-replica** safe — scales horizontally without shared PVC.

---

## 🏗️ Architecture

LogaL uses **leader election** so only one pod actively collects logs, while the other replicas stay on standby and take over automatically if the leader disappears.

```mermaid
graph LR
    A[Browser] -->|HTTP / SSE| B(LogaL Server)
    B -->|leader election| D[(PostgreSQL)]
    B -->|kubectl logs --follow| C[Kubernetes Cluster]
    B -->|INSERT / SELECT| D
    D -->|history stream| B
    B -->|SSE| A
```

### Leader election

```mermaid
sequenceDiagram
    participant P1 as LogaL Pod 1
    participant P2 as LogaL Pod 2
    participant DB as PostgreSQL

    P1->>DB: acquire lease
    DB-->>P1: success
    P2->>DB: acquire lease
    DB-->>P2: failed (held by P1)
    P2->>P2: sleep & retry
    P1-->>P1: watch pods & stream logs

    Note over P1,DB: P1 renews lease every 5s

    P1-xP1: terminated
    P2->>DB: acquire lease
    DB-->>P2: success
    P2-->>P2: become leader, start collector
```

### Log collection mode (auto-collect)

```mermaid
sequenceDiagram
    participant L as LogaL Leader
    participant K as Kubernetes API
    participant D as PostgreSQL

    loop watch pods
        K->>L: pod ADDED / MODIFIED / DELETED
        L->>L: start/stop per-container streams
    end
    loop per running container
        K->>L: log lines
        L->>D: INSERT log
    end
```

### On-demand streaming mode

```mermaid
sequenceDiagram
    participant U as Browser
    participant L as LogaL Server
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
    participant L as LogaL Server
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
Leader pod already collecting all cluster logs
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
- Go 1.22+ (for building from source)

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
| `LOG_RETENTION_DAYS` | `7` | How many days of log history to keep. The UI time-range dropdown is generated from this value. |
| `ALLOWED_NAMESPACES` | *(empty)* | Comma-separated namespace whitelist (optional). Empty = all namespaces. |
| `POD_NAME` | `logal-local` | Pod identity used for leader election (set automatically in Kubernetes via downward API) |

---

## 🚢 Deployment

The included `deployment.yaml` creates:
- `logging` namespace
- ServiceAccount + ClusterRole + ClusterRoleBinding (read pods, logs, namespaces, workloads)
- **Deployment** with 2 replicas, leader election, and rolling update strategy
- **Service** for the UI/API
- Optional **Ingress** template (commented)

### Single Cluster (in-cluster)

```bash
# 1. Create database secret
kubectl create secret generic logal-db \
  --from-literal=url="postgres://logal:password@db-host:5432/logal" \
  -n logging

# 2. Deploy
kubectl apply -f deployment.yaml

# 3. Open port-forward (or expose via Ingress)
kubectl port-forward -n logging svc/logal 8080:80
```

### How it works after deploy

1. Both pods start and try to acquire the leader lease in PostgreSQL.
2. One pod becomes the **leader** and begins watching all pods.
3. The leader starts a `kubectl logs --follow` stream for every container in every running pod and inserts every line into PostgreSQL.
4. The second pod stays **standby** and takes over if the leader fails or is rolled out.
5. Open the UI and pick any workload — the history is already there.

### Multi-Cluster

```bash
# 1. Merge kubeconfigs
KUBECONFIG=./kube-a:./kube-b:./kube-c kubectl config view --flatten > merged

# 2. Store as a Secret
kubectl create secret generic logal-kubeconfig \
  --from-file=config=merged -n logging

# 3. Create database secret
kubectl create secret generic logal-db \
  --from-literal=url="postgres://logal:password@db-host:5432/logal" \
  -n logging

# 4. Deploy
kubectl apply -f deployment.yaml
```

### Add a New Cluster

```bash
# Update secret and restart
kubectl create secret generic logal-kubeconfig \
  --from-file=config=merged \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl rollout restart deployment/logal -n logging
```

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

Please keep the README updated in both English and Indonesian when adding new features.

---

## 📄 License

This project is licensed under the [MIT License](LICENSE).

---

---

# 🇮🇩 Bahasa Indonesia

> [English](#logal-kubernetes-log-viewer) | **Bahasa Indonesia**

[![CI](https://github.com/mtommyp14/LogaL/actions/workflows/ci.yml/badge.svg)](https://github.com/mtommyp14/LogaL/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-13+-4169E1?logo=postgresql&logoColor=white)](https://postgresql.org)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.25+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io)

**Web UI bertema gelap untuk melihat, mencari, dan mengumpulkan log Kubernetes pods secara otomatis dengan riwayat tersimpan.**

LogaL berjalan sebagai binary Go tunggal, memantau setiap pod di cluster menggunakan `kubectl logs`, menyimpan semua log ke PostgreSQL, dan menyajikannya melalui UI web. Menggunakan leader election sehingga hanya satu pod yang mengumpulkan log sementara replica standby memberikan high availability.

> **Tidak ada dependensi `stern`.** LogaL streaming log langsung via `kubectl logs --follow`, jadi image container tetap kecil dan tidak membutuhkan CLI tool tambahan selain `kubectl`.

---

## 🚀 Quick Start

### 1. Lokal (dengan akses `kubectl` yang sudah ada)

```bash
# Clone / download
cd LogaL

# Set database (PostgreSQL wajib)
export DATABASE_URL=postgres://user:pass@localhost:5432/logal

# Jalankan
./start.sh
```

Lalu buka http://localhost:8080.

### 2. Kubernetes

```bash
# 1. Buat secret database
kubectl create secret generic logal-db \
  --from-literal=url="postgres://logal:password@db-host:5432/logal" \
  -n logging

# 2. Deploy
kubectl apply -f deployment.yaml
```

---

## ✨ Fitur

- **Pengumpulan log otomatis** — memantau semua pod di semua namespace dan menyimpan log setiap container ke PostgreSQL.
- **Leader election** — hanya satu pod yang mengumpulkan log; replica standby mengambil alih secara otomatis saat rolling update atau kegagalan.
- **Dark theme** UI yang nyaman untuk sesi log panjang.
- **Streaming real-time** via Server-Sent Events (`kubectl logs`).
- **Riwayat tersimpan** di PostgreSQL dengan retention configurable (`LOG_RETENTION_DAYS`).
- **Custom time range** picker dengan hint UTC live dan filter per baris.
- **Multi-pod / multi-container** selector, sidecar OFF secara default.
- **Filter grep** untuk real-time dan history.
- **Highlight level log**: ERROR (merah), WARN (kuning), INFO (hijau).
- **Pemisah tanggal** dan **timestamp UTC**.
- **Copy & download** log.
- **Auto-reconnect** saat pod restart atau jaringan terputus.
- **Multi-replica** safe — bisa scale horizontal tanpa PVC bersama.

---

## 🏗️ Arsitektur

LogaL menggunakan **leader election** sehingga hanya satu pod yang aktif mengumpulkan log, sementara replica lainnya standby dan mengambil alih secara otomatis jika leader hilang.

```mermaid
graph LR
    A[Browser] -->|HTTP / SSE| B(LogaL Server)
    B -->|leader election| D[(PostgreSQL)]
    B -->|kubectl logs --follow| C[Cluster Kubernetes]
    B -->|INSERT / SELECT| D
    D -->|stream history| B
    B -->|SSE| A
```

### Leader election

```mermaid
sequenceDiagram
    participant P1 as LogaL Pod 1
    participant P2 as LogaL Pod 2
    participant DB as PostgreSQL

    P1->>DB: acquire lease
    DB-->>P1: success
    P2->>DB: acquire lease
    DB-->>P2: failed (held by P1)
    P2->>P2: sleep & retry
    P1-->>P1: watch pods & stream logs

    Note over P1,DB: P1 renews lease every 5s

    P1-xP1: terminated
    P2->>DB: acquire lease
    DB-->>P2: success
    P2-->>P2: become leader, start collector
```

### Mode pengumpulan otomatis (auto-collect)

```mermaid
sequenceDiagram
    participant L as LogaL Leader
    participant K as Kubernetes API
    participant D as PostgreSQL

    loop watch pods
        K->>L: pod ADDED / MODIFIED / DELETED
        L->>L: start/stop per-container streams
    end
    loop per running container
        K->>L: log lines
        L->>D: INSERT log
    end
```

### Mode streaming on-demand

```mermaid
sequenceDiagram
    participant U as Browser
    participant L as LogaL Server
    participant K as Kubernetes API
    participant D as PostgreSQL

    U->>L: GET /api/logs?workload=...
    par history
        L->>D: SELECT log sejak ...
        D->>L: baris
        L->>U: SSE data lines
    and live
        L->>K: kubectl logs --follow per pod/container
        K->>L: baris log
        L->>U: SSE data line
    end
```

### Mode custom range

```mermaid
sequenceDiagram
    participant U as Browser
    participant L as LogaL Server
    participant D as PostgreSQL

    U->>L: GET /api/logs?from=...&to=...
    L->>D: SELECT log ANTARA ...
    D->>L: baris
    L->>U: SSE data lines
    L->>U: __END__
```

### Alur Kerja

```
User membuka LogaL UI
         ↓
Pod leader sudah mengumpulkan semua log cluster
         ↓
User pilih workload + time range
         ↓
LogaL query PostgreSQL history → stream ke browser (history)
LogaL juga start live kubectl logs → stream log baru real-time
         ↓
Browser tampilkan:
  - Scroll ke atas   = log history
  - Scroll ke bawah  = log real-time terbaru

Mode custom range:
  → hanya query history PostgreSQL (tidak ada live streaming)
  → filter setiap baris berdasarkan from/to timestamp (UTC)
  → kirim sinyal __END__ saat selesai
```

---

## 📋 Persyaratan

- Cluster Kubernetes (1.25+) atau akses `kubectl` lokal
- PostgreSQL 13+
- `kubectl` terinstall secara lokal (untuk `start.sh`)
- Go 1.22+ (untuk build dari source)

---

## 📦 Instalasi

### Build dari source

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
# Single platform (lokal)
docker build -t your-registry/logal:latest .
docker push your-registry/logal:latest

# Multi-platform (untuk Mac M-series + cluster Linux EKS/AMD64)
docker buildx build --platform linux/amd64,linux/arm64 \
  -t your-registry/logal:latest --push .
```

---

## ⚙️ Konfigurasi

| Variable | Default | Deskripsi |
|----------|---------|-----------|
| `PORT` | `8080` | Port HTTP server |
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/logal` | **Wajib** PostgreSQL connection string |
| `KUBECONFIG` | `~/.kube/config` | Path ke kubeconfig (opsional untuk multi-cluster) |
| `LOG_RETENTION_DAYS` | `7` | Berapa hari log history disimpan. Dropdown time range UI dibuat dari nilai ini. |
| `ALLOWED_NAMESPACES` | *(kosong)* | Whitelist namespace, dipisah koma (opsional). Kosong = semua namespace. |
| `POD_NAME` | `logal-local` | Identitas pod untuk leader election (otomatis di-set via downward API di Kubernetes) |

---

## 🚢 Deployment

File `deployment.yaml` menyediakan:
- Namespace `logging`
- ServiceAccount, ClusterRole, dan ClusterRoleBinding (read pods, logs, namespaces, workloads)
- Deployment dengan 2 replica, leader election, dan rolling update strategy
- Service untuk UI/API
- Template Ingress (tercomment)

### Single Cluster (in-cluster)

```bash
# 1. Buat secret database
kubectl create secret generic logal-db \
  --from-literal=url="postgres://logal:password@db-host:5432/logal" \
  -n logging

# 2. Deploy
kubectl apply -f deployment.yaml

# 3. Akses via port-forward (atau expose via Ingress)
kubectl port-forward -n logging svc/logal 8080:80
```

### Cara kerja setelah deploy

1. Kedua pod start dan mencoba mengambil leader lease di PostgreSQL.
2. Satu pod menjadi **leader** dan mulai memantau semua pod.
3. Leader menjalankan `kubectl logs --follow` untuk setiap container di setiap pod yang Running dan menyimpan setiap baris ke PostgreSQL.
4. Pod kedua tetap **standby** dan mengambil alih jika leader gagal atau di-rollout.
5. Buka UI dan pilih workload apa saja — history-nya sudah tersedia.

### Multi-Cluster

```bash
# 1. Gabungkan kubeconfigs
KUBECONFIG=./kube-a:./kube-b:./kube-c kubectl config view --flatten > merged

# 2. Simpan sebagai Secret
kubectl create secret generic logal-kubeconfig \
  --from-file=config=merged -n logging

# 3. Buat secret database
kubectl create secret generic logal-db \
  --from-literal=url="postgres://logal:password@db-host:5432/logal" \
  -n logging

# 4. Deploy
kubectl apply -f deployment.yaml
```

### Tambah Cluster Baru

```bash
# Update secret dan restart deployment
kubectl create secret generic logal-kubeconfig \
  --from-file=config=merged \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl rollout restart deployment/logal -n logging
```

---

## 🔄 Migrasi dari Flat Files

Jika kamu memiliki file `/data/logs` dari deployment LogaL/LogT sebelumnya:

```bash
# Jalankan secara lokal atau di dalam pod
export DATABASE_URL=postgres://logal:password@db-host:5432/logal
export LOG_DIR=/data/logs

cd cmd/migrate
go run .
```

Migrasi membaca file seperti `/data/logs/{cluster}/{namespace}/{workload}/YYYY-MM-DD.log` dan memasukkannya ke PostgreSQL.

---

## 🛠️ Development

```bash
# Jalankan lokal (memerlukan PostgreSQL berjalan)
./start.sh

# Atau build + run manual
export DATABASE_URL=postgres://postgres:postgres@localhost:5432/logal
go run .
```

Schema database dibuat otomatis saat startup.

---

## 🤝 Kontribusi

Kontribusi sangat diterima!

1. Fork repository ini.
2. Buat branch fitur: `git checkout -b feat/my-feature`.
3. Commit perubahan kamu.
4. Push branch dan buka Pull Request.

Mohon update README dalam bahasa Inggris dan Indonesia saat menambah fitur baru.

---

## 📄 Lisensi

Proyek ini dilisensikan di bawah [MIT License](LICENSE).
