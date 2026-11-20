#!/bin/bash
# Rebuilds the contract test environment (after a reboot /tmp is empty):
#   - PostgreSQL 16 in docker container c2a-pg on 127.0.0.1:55432 (user/password/db c2a)
#   - the fake upstream on 127.0.0.1:18100
#   - a Python reference instance: detached worktree of $PY_REF (default main) in $PY_DIR (default
#     /tmp/c2a-pyfake), sharing the main checkout's .venv, with a config.json made from the main
#     checkout's config.json plus test overrides (no proxy, short polls). It stays in /tmp; never commit it.
# usage: PY_REPO=../chatgpt2api tools/contract/setup.sh
set -e
G=$(cd "$(dirname "$0")/../.." && pwd)
PY_REPO=${PY_REPO:-$G/../chatgpt2api}; PY_REPO=$(cd "$PY_REPO" && pwd)
W=${PY_DIR:-/tmp/c2a-pyfake}; REF=${PY_REF:-main}

if ! docker ps -a --format '{{.Names}}' | grep -qx c2a-pg; then
  docker run -d --name c2a-pg -e POSTGRES_PASSWORD=c2a -e POSTGRES_USER=c2a -e POSTGRES_DB=c2a -p 127.0.0.1:55432:5432 postgres:16-alpine >/dev/null
fi
docker start c2a-pg >/dev/null
for i in $(seq 1 60); do docker exec c2a-pg pg_isready -U c2a >/dev/null 2>&1 && break; sleep 1; done

pkill -f "/tmp/c2a-fake -addr" || true
(cd "$G" && go build -o /tmp/c2a-fake ./tools/fakeupstream)
(exec /tmp/c2a-fake -addr 127.0.0.1:18100 -delay 2s) > /tmp/c2a-fake.log 2>&1 < /dev/null &

if [ ! -d "$W/.git" ] && [ ! -f "$W/.git" ]; then
  rm -rf "$W"; (cd "$PY_REPO" && git worktree prune && git worktree add --detach -q "$W" "$REF")
fi
[ -e "$W/.venv" ] || ln -s "$PY_REPO/.venv" "$W/.venv"
python3 - "$PY_REPO/config.json" "$W/config.json" <<'PY'
import json, secrets, sys
try:
    config = json.load(open(sys.argv[1]))
except FileNotFoundError:
    config = {"auth-key": "c2a-" + secrets.token_hex(16)}
config.update({"image_retention_days": 15, "proxy": "", "sentinel_solver_url": "", "image_poll_initial_wait_secs": 1, "image_poll_interval_secs": 1})
json.dump(config, open(sys.argv[2], "w"), ensure_ascii=False, indent=2)
PY
mkdir -p "$W/data"
# tiktoken keeps its tables in /tmp by default and downloads them on first use (slow through a proxy,
# and the first image call then times out). Keep them in a persistent cache and fetch them now.
export TIKTOKEN_CACHE_DIR=${TIKTOKEN_CACHE_DIR:-$HOME/.cache/c2a-tiktoken}
mkdir -p "$TIKTOKEN_CACHE_DIR"
"$W/.venv/bin/python" -c "import tiktoken; [tiktoken.get_encoding(n) for n in ('o200k_base', 'cl100k_base')]"
for i in $(seq 1 30); do curl -s -o /dev/null http://127.0.0.1:18100/ && break; sleep 0.5; done
echo "ready: postgres c2a-pg, fake upstream :18100, python worktree $W ($REF)"
