#!/bin/bash
# Rollback rehearsal (acceptance G2) on this machine, synthetic data only: Python data -> migrate ->
# the Go version takes writes (accounts, 3 generated images, logs, counts) while Python takes its
# own -> stop Go -> chatgpt2api-migrate rollback -> lay the files over Python's data -> restart
# Python and check through its API that both sides' writes are there. Exits non-zero on a failure.
# Needs tools/contract/setup.sh done once (PostgreSQL c2a-pg, Python reference in /tmp/c2a-pyfake).
set -euo pipefail
G=$(cd "$(dirname "$0")/../.." && pwd); R=${REHEARSE_DIR:-/tmp/c2a-rollback}; PY=${PY_DIR:-/tmp/c2a-pyfake}
CLEAN="env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY -u http_proxy -u https_proxy -u all_proxy -u NO_PROXY -u no_proxy"
pass() { echo "  通过  $*"; }
fail() { echo "  失败  $*"; exit 1; }
lsof -ti tcp:8401,3401,18300 -sTCP:LISTEN | xargs kill 2>/dev/null || true; sleep 1
rm -rf "$R" && mkdir -p "$R/go"
python3 "$G/tools/walkthrough/mockdata.py" "$R/snapshot" | tail -1
KEY=$(python3 -c "import json;print(json.load(open('$PY/config.json'))['auth-key'])")
python3 -c "
import json;c=json.load(open('$PY/config.json'));c['image_retention_days']=3650
json.dump(c,open('$R/config.json','w'),ensure_ascii=False,indent=2)"
cp "$R/config.json" "$R/go/config.json"
rm -rf "$PY/data" && cp -R "$R/snapshot" "$PY/data"      # Python's data directory
cp -R "$R/snapshot/images" "$R/go/data-images-copy" && mkdir -p "$R/go/data" && mv "$R/go/data-images-copy" "$R/go/data/images"
docker exec c2a-pg psql -q -U c2a -d c2a -c "drop database if exists c2a_rollback with (force);" -c "create database c2a_rollback;"
DB="postgres://c2a:c2a@127.0.0.1:55432/c2a_rollback?sslmode=disable"
(cd "$G" && go build -o "$R/server" . && go build -o "$R/migrate" ./cmd/migrate && go build -o "$R/fake" ./tools/fakeupstream)
"$R/migrate" -data "$R/snapshot" -database-url "$DB" -expect-db c2a_rollback | tail -2
"$R/fake" -addr 127.0.0.1:18300 -delay 1s -unique-images > "$R/fake.log" 2>&1 & FAKE=$!
start_python() { (cd "$PY" && exec $CLEAN CHATGPT2API_CONFIG_FILE="$R/config.json" TIKTOKEN_CACHE_DIR=$HOME/.cache/c2a-tiktoken CHATGPT2API_UPSTREAM_BASE_URL=http://127.0.0.1:1 \
  .venv/bin/python -m uvicorn main:app --host 127.0.0.1 --port 8401) >> "$R/python.log" 2>&1 < /dev/null & PYPID=$!; }
wait_up() { for _ in $(seq 1 60); do [ "$(curl -s -o /dev/null -w '%{http_code}' "$1/version")" = 200 ] && return 0; sleep 0.5; done; fail "$1 没有起来"; }
api() { curl -s -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' "$@"; }
start_python
(cd "$R/go" && exec $CLEAN CHATGPT2API_AUTH_KEY="$KEY" CHATGPT2API_CONFIG_FILE="$R/go/config.json" DATABASE_URL="$DB" PORT=3401 GIN_MODE=release \
  CHATGPT2API_BACKGROUND_JOBS=off CHATGPT2API_UPSTREAM_BASE_URL=http://127.0.0.1:18300 "$R/server") > "$R/go.log" 2>&1 < /dev/null & GOPID=$!
trap 'kill $FAKE $GOPID $PYPID 2>/dev/null' EXIT
wait_up http://127.0.0.1:8401; wait_up http://127.0.0.1:3401

echo "== 并行期间：Go 加 2 个账号并生成 3 张图，Python 加 1 个账号"
api -o /dev/null -d '{"tokens":["fake-ok-go1","fake-ok-go2"]}' http://127.0.0.1:3401/api/accounts
api -o /dev/null -X POST -d '{"tokens":["py-only-token"]}' http://127.0.0.1:8401/api/accounts
# the 130 synthetic accounts point at closed proxies; keep the generation on the two fake ones
api -o /dev/null -d '{"access_tokens":[]}' http://127.0.0.1:3401/api/accounts/refresh || true
urls=()
for i in 1 2 3; do
  u=$(api -d "{\"prompt\":\"rehearsal $i\",\"response_format\":\"url\"}" http://127.0.0.1:3401/v1/images/generations | python3 -c "import json,sys;print(json.load(sys.stdin)['data'][0]['url'])") || fail "Go 生图 $i 失败"
  urls+=("${u#*/images/}")
done
pass "Go 生成 3 张：${urls[*]}"
kill -TERM $GOPID; wait $GOPID 2>/dev/null || true; pass "Go 已停止（等待写入完成）"

echo "== 回切"
"$R/migrate" rollback -snapshot "$R/snapshot" -python "$PY/data" -go-data "$R/go/data" -out "$R/out" -database-url "$DB" -expect-db c2a_rollback || fail "回切文件生成失败"
kill $PYPID; wait $PYPID 2>/dev/null || true
cp -R "$PY/data" "$R/python-before-apply"                 # G3: the backup taken before applying
rsync -a "$R/out/" "$PY/data/"
start_python; wait_up http://127.0.0.1:8401

echo "== 回切后的 Python"
accounts=$(api http://127.0.0.1:8401/api/accounts)
for token in fake-ok-go1 fake-ok-go2 py-only-token; do
  echo "$accounts" | python3 -c "import json,sys;sys.exit(0 if any(a.get('access_token')=='$token' for a in json.load(sys.stdin)['items']) else 1)" && pass "账号 $token 在" || fail "账号 $token 不在"
done
api "http://127.0.0.1:8401/api/logs?type=call" | python3 -c "
import json,sys;items=json.load(sys.stdin)['items']
n=sum(1 for i in items if i.get('summary')=='文生图调用完成' and i['detail'].get('status')=='success')
sys.exit(0 if n>=3 else 1)" && pass "Python 的日志页看到 Go 期间的 3 条文生图调用" || fail "日志里没有 Go 的调用"
gen=$(api http://127.0.0.1:8401/api/logs/image-counts | python3 -c "import json,sys;print(json.load(sys.stdin)['generations']['today'])")
[ "$gen" -ge 3 ] && pass "今日文生图计数 ${gen}（含 Go 的 3 张）" || fail "计数 ${gen}"
for rel in "${urls[@]}"; do
  [ "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8401/images/$rel")" = 200 ] && pass "图片 $rel 可访问" || fail "图片 $rel 不可访问"
done
api "http://127.0.0.1:8401/api/images?refresh=true" | python3 -c "
import json,sys;d=json.load(sys.stdin);rels=json.dumps(d)
sys.exit(0 if all(r in rels for r in sys.argv[1:]) else 1)" "${urls[@]}" && pass "图片管理列表里有这 3 张" || fail "图片列表里缺"
echo "回切演练通过"
