#!/bin/bash
# usage: run-python.sh OUTDIR  — fresh Python instance on :8200 against the fake upstream, then record
set -e
OUT=$1; W=${PY_DIR:-/tmp/c2a-pyfake}
pkill -f "uvicorn main:app --host 127.0.0.1 --port 8200" || true
sleep 1
rm -rf $W/data && mkdir -p $W/data
cd $W && CHATGPT2API_UPSTREAM_BASE_URL=http://127.0.0.1:18100 nohup .venv/bin/python -m uvicorn main:app --host 127.0.0.1 --port 8200 > /tmp/c2a-pyfake.log 2>&1 &
for i in $(seq 1 60); do [ "$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8200/health)" = 200 ] && break; sleep 0.5; done
K=$(python3 -c "import json;print(json.load(open('$W/config.json'))['auth-key'])")
rm -rf $OUT
python3 $(dirname "$0")/contract/contract.py record --base http://127.0.0.1:8200 --admin-key "$K" --out $OUT > $OUT.log 2>&1
echo "recorded $(ls $OUT | wc -l) scenarios into $OUT"
