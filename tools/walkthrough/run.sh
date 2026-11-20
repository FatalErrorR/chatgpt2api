#!/bin/bash
# usage: run.sh — production-shaped data, Python (:8400) and Go (:3400) on the same data, then walk
# every admin page on both and compare (API calls, failures, screenshots). Needs tools/contract/setup.sh
# done once, and the frontend built in ../chatgpt2api/web/out (npm run build). Exits non-zero if any
# flow step fails, a page throws, or the two sides differ (compare.py); every part still runs first.
set -e
G=$(cd "$(dirname "$0")/../.." && pwd); W=/tmp/c2a-walk; PY=${PY_DIR:-/tmp/c2a-pyfake}; WEB=${WEB_OUT:-$G/../chatgpt2api/web/out}
CLEAN="env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY -u http_proxy -u https_proxy -u all_proxy -u NO_PROXY -u no_proxy"
TUNNEL_PORT=${TUNNEL_PORT:-18200}; export TUNNEL_PORT
lsof -ti tcp:8400,3400,$TUNNEL_PORT -sTCP:LISTEN | xargs kill 2>/dev/null || true; sleep 1
$CLEAN python3 $G/tools/walkthrough/tunnel.py $TUNNEL_PORT > /dev/null 2>&1 < /dev/null &
TUNNEL_PID=$!; trap 'kill $TUNNEL_PID 2>/dev/null' EXIT
rm -rf $W && mkdir -p $W/go && python3 $G/tools/walkthrough/mockdata.py $W/data
python3 -c "
import json;c=json.load(open('$PY/config.json'));c['image_retention_days']=30
c['third_party_apps']={'infinite_canvas':{'enabled':True,'url':'https://canvas.example.test/'}}
json.dump(c,open('$W/config.json','w'),ensure_ascii=False,indent=2)"
K=$(python3 -c "import json;print(json.load(open('$W/config.json'))['auth-key'])")
rm -rf $PY/data $PY/web_dist && cp -R $W/data $PY/data && cp -R $WEB $PY/web_dist && cp $W/config.json $PY/config.walk.json
(cd $PY && exec $CLEAN CHATGPT2API_CONFIG_FILE=$PY/config.walk.json TIKTOKEN_CACHE_DIR=$HOME/.cache/c2a-tiktoken CHATGPT2API_UPSTREAM_BASE_URL=http://127.0.0.1:1 .venv/bin/python -m uvicorn main:app --host 127.0.0.1 --port 8400) > $W/python.log 2>&1 < /dev/null &
cp -R $W/data $W/go/data && cp -R $WEB $W/go/web_dist && cp $W/config.json $W/go/config.json && cp $G/VERSION $W/go/VERSION
docker exec c2a-pg psql -q -U c2a -d c2a -c "drop database if exists c2a_walk with (force);" -c "create database c2a_walk;"
DB="postgres://c2a:c2a@127.0.0.1:55432/c2a_walk?sslmode=disable"
(cd $G && go build -o $W/go/server . && go build -o $W/migrate ./cmd/migrate)
$W/migrate -data $W/data -database-url "$DB" -expect-db c2a_walk > $W/migrate.log
(cd $W/go && exec $CLEAN CHATGPT2API_AUTH_KEY="$K" CHATGPT2API_CONFIG_FILE=$W/go/config.json DATABASE_URL="$DB" PORT=3400 GIN_MODE=release CHATGPT2API_UPSTREAM_BASE_URL=http://127.0.0.1:1 ./server) > $W/go.log 2>&1 < /dev/null &
for p in 8400 3400; do for i in $(seq 1 60); do [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:$p/version)" = 200 ] && break; sleep 0.5; done; done
export CHROME_PATH=${CHROME_PATH:-$(ls -d ~/Library/Caches/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-*/chrome-headless-shell 2>/dev/null | head -1)}
export NODE_PATH=${NODE_PATH:-$(ls -d ~/.npm/_npx/*/node_modules/playwright/.. 2>/dev/null | head -1)}
status=0
node $G/tools/walkthrough/walk.js http://127.0.0.1:8400 "$K" $W/py-shots || status=1
node $G/tools/walkthrough/walk.js http://127.0.0.1:3400 "$K" $W/go-shots || status=1
$PY/.venv/bin/python $G/tools/walkthrough/compare.py $W/py-shots $W/go-shots || status=1
# flows change data (reassign, retire, batch add, delete by date, settings): run them last, on both
node $G/tools/walkthrough/flows.js http://127.0.0.1:8400 "$K" $W/py-flows || status=1
node $G/tools/walkthrough/flows.js http://127.0.0.1:3400 "$K" $W/go-flows || status=1
$PY/.venv/bin/python $G/tools/walkthrough/compare.py $W/py-flows $W/go-flows flows || status=1
echo "walkthrough exit $status"
exit $status
