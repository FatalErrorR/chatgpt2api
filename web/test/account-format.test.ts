// 总览里账号表的显示与筛选规则
import assert from "node:assert/strict";
import { test } from "node:test";

import { buildTodos, type OverviewSources } from "../src/lib/overview.ts";
import {
  accountStatusError,
  availableQuota,
  facetCounts,
  formatRestoreAt,
  matchesStatusFilter,
  paginationItems,
  restoreLabel,
} from "../src/lib/account-format.ts";
import type { Account, ProxyEntry } from "../src/lib/api.ts";
import { ABNORMAL_PROXY_FILTER, accountMatchesProxyFilter } from "../src/lib/proxy-match.ts";

const NOW = Date.parse("2026-11-13T00:00:00Z");
const account = (patch: Partial<Account>) =>
  ({ access_token: "t", status: "正常", quota: 5, success: 0, fail: 0, type: "free", ...patch }) as Account;

test("恢复时间只对限流账号或还没到点的显示", () => {
  const past = formatRestoreAt("2026-11-12T00:00:00Z", NOW);
  const future = formatRestoreAt("2026-11-14T05:00:00Z", NOW);
  assert.equal(past.relative, "已到恢复时间");
  assert.equal(restoreLabel({ status: "正常" }, past), "");
  assert.equal(restoreLabel({ status: "异常" }, past), "");
  assert.equal(restoreLabel({ status: "限流" }, past), "已到恢复时间");
  assert.equal(restoreLabel({ status: "正常" }, future), "剩余 1 天 5 小时");
  assert.equal(formatRestoreAt(null).relative, "");
});

test("可用额度只算正常账号，负数按 0", () => {
  assert.equal(availableQuota([account({ quota: 5 }), account({ quota: 7, status: "限流" }), account({ quota: -3 })]), 5);
});

test("筛选数量与图生图冷却", () => {
  const cooled = account({ edit_upload_blocked_until: "2026-11-13T02:00:00Z" });
  const expired = account({ edit_upload_blocked_until: "2026-11-12T02:00:00Z" });
  const counts = facetCounts([cooled, expired, account({ status: "异常", proxy: "http://a", proxy_key: "k1" }), account({ image_inflight: 2 })], NOW);
  assert.equal(counts.status.edit_cooldown, 1);
  assert.equal(counts.status["异常"], 1);
  assert.equal(counts.noProxy, 3);
  assert.equal(counts.proxyKeys.get("k1"), 1);
  assert.equal(counts.inflight, 2);
  assert.equal(matchesStatusFilter(cooled, "edit_cooldown", NOW), true);
  assert.equal(matchesStatusFilter(expired, "edit_cooldown", NOW), false);
});

test("异常原因只在异常账号上给出", () => {
  assert.equal(accountStatusError(account({ status: "异常", last_refresh_error: " token invalidated " })), "token invalidated");
  assert.equal(accountStatusError(account({ status: "正常", last_refresh_error: "old" })), "");
});

test("代理异常筛选只匹配挂在异常代理上的账号", () => {
  const bad = new Set(["k-bad"]);
  assert.equal(accountMatchesProxyFilter({ proxy: "http://x", proxy_key: "k-bad" }, ABNORMAL_PROXY_FILTER, bad), true);
  assert.equal(accountMatchesProxyFilter({ proxy: "http://y", proxy_key: "k-ok" }, ABNORMAL_PROXY_FILTER, bad), false);
  assert.equal(accountMatchesProxyFilter({ proxy: "", proxy_key: "" }, ABNORMAL_PROXY_FILTER, bad), false);
  assert.equal(accountMatchesProxyFilter({ proxy: "http://x", proxy_key: "k-bad" }, ABNORMAL_PROXY_FILTER), false);
});

test("分页按钮", () => {
  assert.deepEqual(paginationItems(1, 1), [1]);
  assert.deepEqual(paginationItems(1, 14), [1, 2, "...", 14]);
  assert.deepEqual(paginationItems(7, 14), [1, "...", 6, 7, 8, "...", 14]);
  assert.deepEqual(paginationItems(14, 14), [1, "...", 13, 14]);
});

test("需要处理：账号和代理的问题直接筛选表格，加载失败可重试", () => {
  const ok = <T,>(value: T) => ({ ok: true as const, value });
  const badProxy = { health: { abnormal: true, last_error: "402" } } as ProxyEntry;
  const sources: OverviewSources = {
    accounts: ok([account({ status: "异常" }), account({ edit_upload_blocked_until: "2999-01-01T00:00:00Z" })]),
    proxies: ok([badProxy]),
    counts: { ok: false, error: "HTTP 502" },
    storage: ok({ disk_total_mb: 1, disk_used_mb: 1, disk_free_mb: 100, image_count: 1, image_size_mb: 1, image_size_bytes: 1 }),
    logs: ok([]),
  };
  const todos = buildTodos(sources);
  assert.deepEqual(
    todos.map((todo) => [todo.title, todo.filter ?? todo.href ?? todo.retry]),
    [
      ["生图统计加载失败", "counts"],
      ["1 个代理异常", { proxy: ABNORMAL_PROXY_FILTER }],
      ["1 个账号异常", { status: "异常" }],
      ["1 个账号图生图冷却", { status: "edit_cooldown" }],
      ["磁盘空间不足", "/image-manager"],
    ],
  );
});
