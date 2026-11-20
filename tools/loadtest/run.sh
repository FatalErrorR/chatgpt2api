#!/bin/bash
# Acceptance F1–F3 on this machine: the Go server on PostgreSQL (docker c2a-pg) against the fake
# upstream with 30 s per image, 120 fake accounts at 3 concurrent images each.
# usage: tools/loadtest/run.sh            F1/F2: a baseline request, then 300 at once
#        tools/loadtest/run.sh 2h         F3: 300 workers sending back to back for 2 hours
# Exits non-zero if any request fails.
set -euo pipefail
G=$(cd "$(dirname "$0")/../.." && pwd); L=${LOAD_DIR:-/tmp/c2a-load}; DURATION=${1:-0}
CLEAN="env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY -u http_proxy -u https_proxy -u all_proxy"
ulimit -n 10240
lsof -ti tcp:3500,18200 -sTCP:LISTEN | xargs kill 2>/dev/null || true; sleep 1
rm -rf "$L" && mkdir -p "$L"
docker exec c2a-pg psql -q -U c2a -d c2a -c "drop database if exists c2a_load with (force);" -c "create database c2a_load;"
(cd "$G" && go build -o "$L/server" . && go build -o "$L/fake" ./tools/fakeupstream && go build -o "$L/loadtest" ./tools/loadtest)
KEY=load-$(openssl rand -hex 8)
printf '{"auth-key": "%s", "image_account_concurrency": 3, "image_poll_timeout_secs": 300}\n' "$KEY" > "$L/config.json"
"$L/fake" -addr 127.0.0.1:18200 -delay 30s -log -unique-images > "$L/fake.log" 2>&1 &
FAKE=$!
(cd "$L" && exec $CLEAN CHATGPT2API_CONFIG_FILE="$L/config.json" DATABASE_URL="postgres://c2a:c2a@127.0.0.1:55432/c2a_load?sslmode=disable" \
  PORT=3500 GIN_MODE=release CHATGPT2API_UPSTREAM_BASE_URL=http://127.0.0.1:18200 ./server) > "$L/server.log" 2>&1 &
SERVER=$!
trap 'kill $SERVER $FAKE 2>/dev/null' EXIT
for _ in $(seq 1 60); do [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:3500/version)" = 200 ] && break; sleep 0.5; done
tokens=$(python3 -c "import json;print(json.dumps({'tokens':[f'fake-ok-load{i:03d}' for i in range(120)]}))")
curl -s -o /dev/null -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d "$tokens" http://127.0.0.1:3500/api/accounts
curl -s "http://127.0.0.1:3500/health?format=json" | python3 -c "import json,sys;print('账号', json.load(sys.stdin)['accounts'])"
echo "== 基线：单个请求"
$CLEAN "$L/loadtest" -base http://127.0.0.1:3500 -key "$KEY" -concurrency 1 | head -2
echo "== 300 并发（时长 ${DURATION}）"
LOADTEST_RSS_CSV="$L/rss.csv" $CLEAN "$L/loadtest" -base http://127.0.0.1:3500 -key "$KEY" -pid $SERVER -concurrency 300 -duration "$DURATION"
status=$?
curl -s "http://127.0.0.1:3500/health?format=json" | python3 -c "import json,sys;a=json.load(sys.stdin)['accounts'];print('结束时账号', a)"
exit $status
