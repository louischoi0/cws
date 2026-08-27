#!/bin/sh
# Starts all three processes: the KDS instance, the API server, and the
# read-only web dashboard.
set -e
ROOT="$(cd "$(dirname "$0")" && pwd)"

PIDS=""
cleanup() { [ -n "$PIDS" ] && kill $PIDS 2>/dev/null; }
trap cleanup EXIT INT TERM

"$ROOT/ckdbs/build-release/kds_server" --config "$ROOT/kds.conf" &
KDS_PID=$!
PIDS="$KDS_PID"

# Give kds_server a moment to open its listening socket before the API
# server's own connect-retry loop starts hammering it.
sleep 0.5

# ...then check it actually survived. `set -e` does not catch a background
# job's failure, so without this a kds_server that died on startup (a port
# already in use is the usual way) would leave the other two running
# against whatever else holds that port — a partial stack that looks like
# a whole one.
if ! kill -0 "$KDS_PID" 2>/dev/null; then
    echo "run.sh: kds_server exited during startup; not starting the rest." >&2
    echo "run.sh: check whether something already holds the port in kds.conf." >&2
    exit 1
fi

(cd "$ROOT/server" && go run .) &
PIDS="$PIDS $!"

# The dashboard retries per request, so it needs no ordering guarantee —
# it renders an error page until the API answers.
(cd "$ROOT/web" && go run .) &
PIDS="$PIDS $!"

wait
