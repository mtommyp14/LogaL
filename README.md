# LogaL — Kubernetes Log Viewer

> **English** | [Bahasa Indonesia](#bahasa-indonesia)

[![CI](https://github.com/yourusername/LogaL/actions/workflows/ci.yml/badge.svg)](https://github.com/yourusername/LogaL/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-13+-4169E1?logo=postgresql&logoColor=white)](https://postgresql.org)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.25+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io)

**Dark-themed web UI for tailing Kubernetes pod logs in real-time with history support.**

Powered by [`stern`](https://github.com/stern/stern) on the backend, a single Go binary, and PostgreSQL storage. Multi-replica ready, no database knowledge required to run it.

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

- **Dark theme** UI optimized for long log sessions.
- **Real-time streaming** via Server-Sent Events (`stern`).
- **History** stored in PostgreSQL with automatic cleanup (default 3 days).
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

```
Developer (Browser)
       │ HTTP / SSE
       ▼
┌─────────────────────────────────────────┐
│             LogaL Pod                   │
│         (namespace: logging)            │
│                                         │
│  ┌──────────┐   ┌────────┐   ┌────────┐ │
│  │Go Server │──▶│ Stern  │   │   DB   │ │
│  │REST + SSE│   │(child  │   │PostgreSQL
│  │Static UI │   │process)│   │        │ │
│  └──────────┘   └────────┘   └────────┘ │
│        │            │             ▲      │
│        │        Log Router       │      │
│        │         ├── SSE ───────┼─────┼──▶ Browser
│        │         └── Write ─────┘      │
└────────┼────────────────────────────────┘
         │ kubectl / stern
    ┌────┴────┬──────────┐
    ▼         ▼          ▼
Cluster A  Cluster B  Cluster C
```

### Flow

```
User selects workload + time range
         ↓
LogaL checks:
  History in PostgreSQL? → query DB → stream to browser (history)
  Stern runs in parallel → new logs keep coming (real-time)
         ↓
Browser displays:
  - Scroll up   = history logs
  - Scroll down = latest real-time logs

Custom range mode:
  → queries PostgreSQL history only (no stern)
  → filters each row by from/to timestamp (UTC)
  → sends __END__ signal when done
```

---

## 📋 Requirements

- Kubernetes cluster (1.25+) or local `kubectl` access
- PostgreSQL 13+
- `kubectl` and `stern` installed locally (for `start.sh`)
- Go 1.22+ (for building from source)

---

## 📦 Installation

### Build from source

```bash
# Clone
git clone https://github.com/yourusername/LogaL.git
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
docker build -t your-registry/logal:latest .
docker push your-registry/logal:latest
```

---

## ⚙️ Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | HTTP server port |
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/logal` | **Required** PostgreSQL connection string |
| `KUBECONFIG` | `~/.kube/config` | Path to kubeconfig (optional for multi-cluster) |
| `LOG_RETENTION_DAYS` | `3` | How many days of log history to keep |
| `ALLOWED_NAMESPACES` | *(empty)* | Comma-separated namespace whitelist (optional) |

---

## 🚢 Deployment

### Single Cluster (in-cluster)

```bash
# 1. Create database secret
kubectl create secret generic logal-db \
  --from-literal=url="postgres://logal:password@db-host:5432/logal" \
  -n logging

# 2. Deploy
kubectl apply -f deployment.yaml
```

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

[![CI](https://github.com/yourusername/LogaL/actions/workflows/ci.yml/badge.svg)](https://github.com/yourusername/LogaL/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![PostgreSQL](https://img.shields.io/badge/PostgreSQL-13+-4169E1?logo=postgresql&logoColor=white)](https://postgresql.org)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-1.25+-326CE5?logo=kubernetes&logoColor=white)](https://kubernetes.io)

**Web UI bertema gelap untuk melihat log Kubernetes pods secara real-time beserta riwayat log.**

Menggunakan [`stern`](https://github.com/stern/stern) di backend, binary Go tunggal, dan penyimpanan PostgreSQL. Siap multi-replica, tidak perlu pengetahuan database untuk menjalankannya.

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

- **Dark theme** UI yang nyaman untuk sesi log panjang.
- **Streaming real-time** via Server-Sent Events (`stern`).
- **History** tersimpan di PostgreSQL dengan auto-cleanup (default 3 hari).
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

```
Developer (Browser)
       │ HTTP / SSE
       ▼
┌─────────────────────────────────────────┐
│             LogaL Pod                   │
│         (namespace: logging)            │
│                                         │
│  ┌──────────┐   ┌────────┐   ┌────────┐ │
│  │Go Server │──▶│ Stern  │   │   DB   │ │
│  │REST + SSE│   │(child  │   │PostgreSQL
│  │Static UI │   │process)│   │        │ │
│  └──────────┘   └────────┘   └────────┘ │
│        │            │             ▲      │
│        │        Log Router       │      │
│        │         ├── SSE ───────┼─────┼──▶ Browser
│        │         └── Write ─────┘      │
└────────┼────────────────────────────────┘
         │ kubectl / stern
    ┌────┴────┬──────────┐
    ▼         ▼          ▼
Cluster A  Cluster B  Cluster C
```

### Alur Kerja

```
User pilih workload + time range
         ↓
LogaL cek:
  Ada history di PostgreSQL? → query DB → stream ke browser (history)
  Stern berjalan paralel → log baru terus masuk (real-time)
         ↓
Browser tampilkan:
  - Scroll ke atas   = log history
  - Scroll ke bawah  = log real-time terbaru

Mode custom range:
  → hanya query history PostgreSQL (stern tidak dijalankan)
  → filter setiap baris berdasarkan from/to timestamp (UTC)
  → kirim sinyal __END__ saat selesai
```

---

## 📋 Persyaratan

- Cluster Kubernetes (1.25+) atau akses `kubectl` lokal
- PostgreSQL 13+
- `kubectl` dan `stern` terinstall secara lokal (untuk `start.sh`)
- Go 1.22+ (untuk build dari source)

---

## 📦 Instalasi

### Build dari source

```bash
# Clone
git clone https://github.com/yourusername/LogaL.git
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
docker build -t your-registry/logal:latest .
docker push your-registry/logal:latest
```

---

## ⚙️ Konfigurasi

| Variable | Default | Deskripsi |
|----------|---------|-----------|
| `PORT` | `8080` | Port HTTP server |
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/logal` | **Wajib** PostgreSQL connection string |
| `KUBECONFIG` | `~/.kube/config` | Path ke kubeconfig (opsional untuk multi-cluster) |
| `LOG_RETENTION_DAYS` | `3` | Berapa hari log history disimpan |
| `ALLOWED_NAMESPACES` | *(kosong)* | Whitelist namespace, dipisah koma (opsional) |

---

## 🚢 Deployment

### Single Cluster (in-cluster)

```bash
# 1. Buat secret database
kubectl create secret generic logal-db \
  --from-literal=url="postgres://logal:password@db-host:5432/logal" \
  -n logging

# 2. Deploy
kubectl apply -f deployment.yaml
```

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
