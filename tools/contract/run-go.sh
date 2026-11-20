#!/bin/bash
# usage: run-go.sh OUTDIR — fresh Go instance on :3100 (fresh Postgres db) against the fake upstream, then record
set -e
OUT=$1; G=$(cd "$(dirname "$0")/../.." && pwd); R=${GO_RUN_DIR:-/tmp/c2a-gorun}
K=$(python3 -c "import json;print(json.load(open('${PY_DIR:-/tmp/c2a-pyfake}/config.json'))['auth-key'])")
pkill -f "$R/server" || true
# 按端口再兜一次，避免旧进程占着数据库连接
lsof -ti tcp:3100 -sTCP:LISTEN | xargs kill 2>/dev/null || true
sleep 1
docker exec c2a-pg psql -q -U c2a -d c2a -c "drop database if exists c2a_contract with (force);" -c "create database c2a_contract;"
rm -rf $R && mkdir -p $R && cd $G && go build -o $R/server . && cp -R web $R/web 2>/dev/null || true
# 两边读同一份 config.json（Python 格式），否则设置类场景比的不是同一个输入
cp ${PY_DIR:-/tmp/c2a-pyfake}/config.json $R/config.json
# 整个子 shell 重定向到日志，后台服务就不会占住调用方的管道
(cd $R && exec env CHATGPT2API_AUTH_KEY="$K" DATABASE_URL="postgres://c2a:c2a@127.0.0.1:55432/c2a_contract?sslmode=disable" PORT=3100 GIN_MODE=release \
  CHATGPT2API_UPSTREAM_BASE_URL=http://127.0.0.1:18100 $R/server) > /tmp/c2a-gorun.log 2>&1 < /dev/null &
for i in $(seq 1 60); do [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:3100/version)" = 200 ] && break; sleep 0.5; done
rm -rf $OUT
python3 $G/tools/contract/contract.py record --base http://127.0.0.1:3100 --admin-key "$K" --out $OUT > $OUT.log 2>&1
echo "recorded $(ls $OUT | wc -l) scenarios into $OUT"
