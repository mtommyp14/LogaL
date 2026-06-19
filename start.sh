#!/bin/bash
# LogaL — Quick local run script
# Requires: Go, stern, kubectl, and PostgreSQL installed locally

set -e

PORT="${PORT:-8080}"
DATABASE_URL="${DATABASE_URL:-postgres://postgres:postgres@localhost:5432/logal}"
LOG_RETENTION_DAYS="${LOG_RETENTION_DAYS:-3}"
KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"

echo "╔══════════════════════════════════════╗"
echo "║      [ LogaL ] - Log Viewer          ║"
echo "╚══════════════════════════════════════╝"
echo ""

# Check dependencies
check_cmd() {
  if ! command -v "$1" &>/dev/null; then
    echo "✗ $1 not found. Please install it first."
    exit 1
  else
    echo "✓ $1 found"
  fi
}

echo "Checking dependencies..."
check_cmd go
check_cmd stern
check_cmd kubectl
echo ""

# Check kubeconfig
if [ -z "$KUBECONFIG" ] || [ ! -f "$KUBECONFIG" ]; then
  echo "⚠ KUBECONFIG not set or file not found: $KUBECONFIG"
  echo "  Set KUBECONFIG env or use: export KUBECONFIG=/path/to/kubeconfig"
  exit 1
fi
echo "✓ kubeconfig: $KUBECONFIG"

# Check DATABASE_URL
echo "✓ database: $DATABASE_URL"
echo ""

echo "Starting LogaL on http://localhost:$PORT ..."
echo "Press Ctrl+C to stop."
echo ""

export PORT="$PORT"
export DATABASE_URL="$DATABASE_URL"
export LOG_RETENTION_DAYS="$LOG_RETENTION_DAYS"
export KUBECONFIG="$KUBECONFIG"

cd "$(dirname "$0")"
go run .
