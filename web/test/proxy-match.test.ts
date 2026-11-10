// 运行：node --test web/test/proxy-match.test.ts（Node 22.18+ 直接执行 TS，无需依赖）
import assert from "node:assert/strict";
import { test } from "node:test";

import type { ProxyEntry } from "../src/lib/api.ts";
import { accountMatchesProxyFilter, proxyForAccount } from "../src/lib/proxy-match.ts";

// 与后端一致：socks5:// 与 socks5h:// 两种写法，后端给出同一个 proxy_key
const KEY = "socks5h://3.3.3.3:1080";
const entry: ProxyEntry = {
  url: "socks5://3.3.3.3:1080",
  key: KEY,
  name: "美西",
  masked_url: "socks5://3.3.3.3:1080",
  account_count: 2,
  health: {
    abnormal: true,
    successes: 0,
    failures: 3,
    consecutive_failures: 3,
    last_error: "Cloudflare challenge (HTTP 403)",
    last_failure_at: 1,
    last_success_at: null,
  },
};
const other: ProxyEntry = { ...entry, url: "http://2.2.2.2:8000", key: "http://2.2.2.2:8000", name: "备用" };
const byKey = new Map([entry, other].map((proxy) => [proxy.key, proxy]));

const viaSocks5 = { proxy: "socks5://3.3.3.3:1080", proxy_key: KEY };
const viaSocks5h = { proxy: "socks5h://3.3.3.3:1080", proxy_key: KEY };
const viaOther = { proxy: "http://2.2.2.2:8000", proxy_key: "http://2.2.2.2:8000" };
const direct = { proxy: "", proxy_key: "" };
const accounts = [viaSocks5, viaSocks5h, viaOther, direct];

test("两种写法的账号都匹配同一条代理的名称和健康", () => {
  for (const account of [viaSocks5, viaSocks5h]) {
    const proxy = proxyForAccount(byKey, account);
    assert.equal(proxy?.name, "美西");
    assert.equal(proxy?.health.abnormal, true);
  }
  assert.equal(proxyForAccount(byKey, viaOther)?.name, "备用");
  assert.equal(proxyForAccount(byKey, direct), undefined);
});

test("选择该代理时两种写法的账号都出现", () => {
  assert.deepEqual(accounts.filter((account) => accountMatchesProxyFilter(account, KEY)), [viaSocks5, viaSocks5h]);
});

test("全部代理与未指定代理保持原行为", () => {
  assert.deepEqual(accounts.filter((account) => accountMatchesProxyFilter(account, "all")), accounts);
  assert.deepEqual(
    accounts.filter((account) => accountMatchesProxyFilter(account, "none")),
    [direct],
  );
  assert.equal(accountMatchesProxyFilter({ proxy: null }, "none"), true);
  assert.equal(accountMatchesProxyFilter({ proxy: "  " }, "none"), true);
});

test("没有 proxy_key 的旧数据不会误匹配", () => {
  assert.equal(accountMatchesProxyFilter({ proxy: "socks5://3.3.3.3:1080" }, KEY), false);
  assert.equal(proxyForAccount(byKey, { proxy: "socks5://3.3.3.3:1080" }), undefined);
});

test("代理短名：没名字时用主机:端口，不用以账号名开头的地址", async () => {
  const { proxyShortName, proxyTitle } = await import("../src/lib/proxy-match.ts");
  const masked = "http://vvuyblcwstaticresidential:***@194.39.33.194:5903";
  assert.equal(proxyShortName({ name: "", masked_url: masked }), "194.39.33.194:5903");
  assert.equal(proxyShortName({ name: " IPRoyal-3 ", masked_url: masked }), "IPRoyal-3");
  assert.equal(proxyShortName({ name: "", masked_url: "socks5://9.9.9.9:1080" }), "9.9.9.9:1080");
  assert.equal(proxyShortName({ name: "", masked_url: "http://1.2.3.4:80/" }), "1.2.3.4:80");
  assert.equal(proxyTitle({ name: "", masked_url: masked }), `194.39.33.194:5903（${masked}）`);
});
