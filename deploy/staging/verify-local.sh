#!/bin/bash
# Local check of the step 12 layout (deploy/staging/docker-compose.yml) with synthetic data only:
#   1. each migration runs inside its own service, prints the database it reached, refuses a wrong
#      -expect-db, and each database holds only what it should (3 borrowed accounts / the full copy)
#   2. config.json in a mounted directory: a save from the settings page survives recreating the
#      container; the old single-file mount fails with "Resource busy" (shown for contrast)
#   3. the rehearsal instance has no way out: no route, no outside DNS, a manual account refresh
#      that goes to the fixed auth.openai.com URL fails at the network; its own PostgreSQL and the
#      admin port through the gate work; the staging app on the normal network can get out (control)
# Exits non-zero on the first failed check. Needs the image: docker build -t chatgpt2api-go:staging .
set -euo pipefail
G=$(cd "$(dirname "$0")/../.." && pwd); S=${STAGING_DIR:-/tmp/c2a-staging-local}
PROJECT=c2a-staging-local
compose() { docker compose -p $PROJECT --project-directory "$S" -f "$S/docker-compose.yml" "$@"; }
pass() { echo "  通过  $*"; }
fail() { echo "  失败  $*"; exit 1; }
wait_http() { for _ in $(seq 1 90); do [ "$(curl -s -o /dev/null -w '%{http_code}' "$1/version")" = 200 ] && return 0; sleep 1; done; fail "$1 没有起来"; }
CLEAN="env -u HTTP_PROXY -u HTTPS_PROXY -u ALL_PROXY -u http_proxy -u https_proxy -u all_proxy"

[ -d "$S" ] && compose --profile rehearsal down -v --remove-orphans >/dev/null 2>&1 || true
rm -rf "$S" && mkdir -p "$S"/{conf,rehearsal-conf,data,rehearsal-data,source}
cp "$G/deploy/staging/docker-compose.yml" "$S/"
KEY=$(openssl rand -hex 16); RKEY=$(openssl rand -hex 16)
printf 'PG_PASSWORD=%s\nSTAGING_PORT=3401\nREHEARSAL_PORT=3402\nSENTINEL_SOLVER_DIR=/nonexistent\n' "$(openssl rand -hex 12)" > "$S/.env"

echo "== 合成数据"
python3 "$G/tools/walkthrough/mockdata.py" "$S/source/full" | tail -1
python3 - "$S" "$KEY" "$RKEY" <<'EOF'
import base64, json, shutil, sys
from pathlib import Path
s, key, rkey = Path(sys.argv[1]), sys.argv[2], sys.argv[3]
full = s / "source/full"
b64 = lambda d: base64.urlsafe_b64encode(json.dumps(d).encode()).decode().rstrip("=")
accounts = json.loads((full / "accounts.json").read_text())
for i in range(3):  # the "borrowed" accounts: an expired JWT and a refresh token, so a refresh goes to OAuth
    accounts[i].update(access_token=f"{b64({'alg': 'none'})}.{b64({'exp': 1700000000, 'sub': f'synthetic-{i}'})}.sig",
                       refresh_token=f"rt-synthetic-{i}", email=f"borrowed{i}@synthetic.test", status="正常", proxy="")  # direct: only the network can stop it
(full / "accounts.json").write_text(json.dumps(accounts, ensure_ascii=False, indent=2))
shutil.move(str(full / "images"), str(s / "rehearsal-data/images"))
shutil.copytree(s / "rehearsal-data/images", s / "data/images")
shutil.copytree(full, s / "source/staging")
(s / "source/staging/accounts.json").write_text(json.dumps(accounts[:3], ensure_ascii=False, indent=2))
for folder, auth in (("conf", key), ("rehearsal-conf", rkey)):
    (s / folder / "config.json").write_text(json.dumps({"auth-key": auth, "backup": {"enabled": False}, "image_storage": {"mode": "local"},
        "auto_remove_invalid_accounts": False, "image_poll_timeout_secs": 300, "image_retention_days": 30}, indent=2))
EOF

echo "== 1. 迁移在对应服务的容器里执行，只导入该库的数据"
compose up -d --wait pg >/dev/null 2>&1
compose --profile rehearsal up -d --wait pg-rehearsal >/dev/null 2>&1
out=$(compose run --rm --no-deps app ./chatgpt2api-migrate -data /source -expect-db c2a_rehearsal 2>&1) && fail "staging 服务对错库名也执行了" || true
echo "$out" | grep -q '目标库是 "c2a_staging"，不是 -expect-db 指定的 "c2a_rehearsal"' && pass "staging 服务拒绝 -expect-db c2a_rehearsal，打印实际库 c2a_staging" || fail "$out"
out=$(compose run --rm --no-deps app ./chatgpt2api-migrate -data /source -expect-db c2a_staging 2>&1) || fail "$out"
echo "$out" | grep -E "^目标库|现有账号"
echo "$out" | grep -q "c2a_staging（.*现有账号 3 个：borrowed0@synthetic.test、borrowed1@synthetic.test、borrowed2@synthetic.test" && pass "c2a_staging 只有 3 个借用账号" || fail "staging 账号不对"
out=$(compose --profile rehearsal run --rm --no-deps rehearsal ./chatgpt2api-migrate -data /source -expect-db c2a_rehearsal 2>&1) || fail "$out"
echo "$out" | grep -E "^目标库|现有账号"
echo "$out" | grep -q "c2a_rehearsal（.*现有账号 130 个" && pass "c2a_rehearsal 有完整副本的 130 个账号" || fail "rehearsal 账号不对"
[ "$(compose exec -T pg psql -U c2a -d c2a_staging -tAc 'select count(*) from compat_accounts')" = 3 ] && pass "直接查 pg：c2a_staging 3 个账号" || fail "pg 计数"
[ "$(compose exec -T pg psql -U c2a -d postgres -tAc "select count(*) from pg_database where datname='c2a_rehearsal'")" = 0 ] && pass "staging 的 PostgreSQL 里没有演练库" || fail "staging pg 里有演练库"
[ "$(compose exec -T pg-rehearsal psql -U c2a -d c2a_rehearsal -tAc 'select count(*) from compat_accounts')" = 130 ] && pass "直接查 pg-rehearsal：130 个账号" || fail "pg-rehearsal 计数"

echo "== 2. config.json 放在挂载的目录里：设置页保存后重建容器仍在"
compose up -d --no-deps app >/dev/null 2>&1; wait_http http://127.0.0.1:3401
export CHROME_PATH=${CHROME_PATH:-$(ls -d ~/Library/Caches/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-*/chrome-headless-shell 2>/dev/null | head -1)}
export NODE_PATH=${NODE_PATH:-$(ls -d ~/.npm/_npx/*/node_modules/playwright/.. 2>/dev/null | head -1)}
$CLEAN node "$G/deploy/staging/save-setting.js" http://127.0.0.1:3401 "$KEY" 77 || fail "设置页保存失败"
python3 -c "import json,sys;c=json.load(open('$S/conf/config.json'));sys.exit(0 if c.get('image_failover_enabled') is True and int(c.get('image_failover_secs'))==77 else 1)" && pass "宿主机 conf/config.json 已写入 77 秒" || fail "宿主机文件没变"
compose up -d --no-deps --force-recreate app >/dev/null 2>&1; wait_http http://127.0.0.1:3401
curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:3401/api/settings | python3 -c "import json,sys;c=json.load(sys.stdin)['config'];sys.exit(0 if c['image_failover_enabled'] is True and int(c['image_failover_secs'])==77 else 1)" && pass "重建容器后 /api/settings 读到 77 秒" || fail "重建后设置丢失"
compose logs app 2>&1 | grep -qi "resource busy" && fail "日志里有 Resource busy" || pass "日志没有 Resource busy"
# contrast: the single-file mount the earlier plan used
mkdir -p "$S/single" && cp "$S/conf/config.json" "$S/single/config.json"
compose run -d --rm --no-deps --name c2a-single-file -p 127.0.0.1:3403:80 -v "$S/single/config.json:/app/config.json" \
  -e CHATGPT2API_CONFIG_FILE=/app/config.json -e CHATGPT2API_BACKGROUND_JOBS=off app >/dev/null
wait_http http://127.0.0.1:3403
body=$(curl -s -H "Authorization: Bearer $KEY" http://127.0.0.1:3403/api/settings | python3 -c "import json,sys;c=json.load(sys.stdin)['config'];c['image_failover_secs']=88;print(json.dumps(c))")
code=$(curl -s -o "$S/single/resp" -w '%{http_code}' -H "Authorization: Bearer $KEY" -H 'Content-Type: application/json' -d "$body" http://127.0.0.1:3403/api/settings)
echo "  单文件挂载的保存响应：$(head -c 200 "$S/single/resp")"
docker rm -f c2a-single-file >/dev/null
{ [ "$code" != 200 ] || grep -qi busy "$S/single/resp"; } && pass "对照：单文件挂载保存失败（HTTP ${code}），确认旧方案的问题" || echo "  注意  对照：单文件挂载这次保存成功（HTTP ${code}）"

echo "== 3. 演练实例没有外网出口"
compose --profile rehearsal up -d rehearsal-gate >/dev/null 2>&1; wait_http http://127.0.0.1:3402
compose exec -T rehearsal sh -c 'ip route' | grep -q '^default' && fail "演练容器有默认路由" || pass "演练容器没有默认路由"
compose exec -T rehearsal sh -c 'getent hosts auth.openai.com' >/dev/null 2>&1 && fail "演练容器能解析 auth.openai.com" || pass "演练容器解析不了 auth.openai.com"
compose exec -T rehearsal sh -c 'nc -z -w 5 1.1.1.1 443' >/dev/null 2>&1 && fail "演练容器能连 1.1.1.1:443" || pass "演练容器连不上外部 IP（1.1.1.1:443）"
compose exec -T rehearsal sh -c 'nc -z -w 5 pg-rehearsal 5432' && pass "演练容器能连自己的 PostgreSQL" || fail "连不上 pg-rehearsal"
compose exec -T rehearsal sh -c 'nc -z -w 3 pg 5432' >/dev/null 2>&1 && fail "演练容器能连 staging 的 PostgreSQL" || pass "演练容器连不到 staging 的 PostgreSQL"
compose exec -T app sh -c 'nc -z -w 8 auth.openai.com 443' && pass "对照：staging 容器能连 auth.openai.com:443（检测方法有效）" || fail "对照失败：staging 也出不去，检测无效"
n=$(curl -s -H "Authorization: Bearer $RKEY" http://127.0.0.1:3402/api/accounts | python3 -c "import json,sys;print(len(json.load(sys.stdin)['items']))")
[ "$n" = 130 ] && pass "经 gate 访问演练管理接口：130 个账号" || fail "演练管理接口 $n"
token=$(python3 -c "import json;print(json.load(open('$S/source/staging/accounts.json'))[0]['access_token'])")
curl -s -H "Authorization: Bearer $RKEY" -H 'Content-Type: application/json' -d "{\"access_tokens\":[\"$token\"]}" http://127.0.0.1:3402/api/accounts/refresh > "$S/refresh.json"
for _ in $(seq 1 30); do
  err=$(curl -s -H "Authorization: Bearer $RKEY" http://127.0.0.1:3402/api/accounts | python3 -c "
import json,sys
a=[x for x in json.load(sys.stdin)['items'] if x.get('email')=='borrowed0@synthetic.test'][0]
print((a.get('last_token_refresh_error') or '')[:200], '|', a['access_token']=='$token')")
  [ -n "${err%%|*}" ] && [ "${err%% |*}" != "" ] && break; sleep 1
done
echo "  手动刷新的结果：$err"
echo "$err" | grep -q "auth.openai.com" && echo "$err" | grep -qiE "lookup auth.openai.com|no such host|network is unreachable|server misbehaving" && ! echo "$err" | grep -q "dial tcp 127.0.0.1:" \
  && pass "手动刷新直连 auth.openai.com，在网络层（DNS/路由）失败" || fail "没有看到 OAuth 被网络挡住：$err"
echo "$err" | grep -q "| True" && pass "账号 token 未变" || fail "token 变了"
echo "全部通过"
