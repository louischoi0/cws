#!/bin/sh
# Starts the KDS instance and the issues HTTP server together.
set -e
ROOT="$(cd "$(dirname "$0")" && pwd)"

"$ROOT/ckdbs/build-release/kds_server" --config "$ROOT/kds.conf" &
KDS_PID=$!
trap 'kill "$KDS_PID" 2>/dev/null' EXIT

# Give kds_server a moment to open its listening socket before the Go
# server's own connect-retry loop starts hammering it.
sleep 0.3

cd "$ROOT/server"
exec go run .
