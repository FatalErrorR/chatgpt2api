// 概览：某块数据加载失败时必须显示成失败，不能落成 0 / 「全部正常」/「一切正常」
import assert from "node:assert/strict";
import { test } from "node:test";

import type { Account, ProxyEntry } from "../src/lib/api.ts";
import { buildTodos, failedSources, settle, type OverviewSources } from "../src/lib/overview.ts";

const ok = <T,>(value: T) => ({ ok: true as const, value });
const fail = (error: string) => ({ ok: false as const, error });

const healthy: OverviewSources = {
  accounts: ok([{ access_token: "t", status: "正常", quota: 5 } as Account]),
  proxies: ok([]),
  counts: ok({ generations: { today: 1, yesterday: 0, d7: 1, d14: 1, d30: 1 }, edits: { today: 0, yesterday: 0, d7: 0, d14: 0, d30: 0 } }),
  storage: ok({ disk_total_mb: 10000, disk_used_mb: 1000, disk_free_mb: 9000, image_count: 3, image_size_mb: 1, image_size_bytes: 1 }),
  logs: ok([]),
};

test("全部加载成功且没有问题时才是「一切正常」", () => {
  assert.deepEqual(buildTodos(healthy), []);
  assert.deepEqual(failedSources(healthy), []);
});

for (const key of ["accounts", "proxies", "counts", "storage", "logs"] as const) {
  test(`${key} 加载失败会进「需要处理」和失败列表`, () => {
    const sources = { ...healthy, [key]: fail("HTTP 502") } as OverviewSources;
    const todos = buildTodos(sources);
    assert.equal(todos.length, 1);
    assert.equal(todos[0].tone, "bad");
    assert.match(todos[0].title, /加载失败$/);
    assert.equal(todos[0].note, "HTTP 502");
    assert.deepEqual(failedSources(sources).map((item) => item.key), [key]);
  });
}

test("失败的块不会再推出基于空数据的结论，成功的块照常报警", () => {
  const badProxy = { health: { abnormal: true, last_error: "Cloudflare challenge" } } as ProxyEntry;
  const sources: OverviewSources = {
    ...healthy,
    accounts: fail("timeout"),
    proxies: ok([badProxy]),
    storage: ok({ ...(healthy.storage as { ok: true; value: never }).value, disk_free_mb: 100 }),
  };
  assert.deepEqual(
    buildTodos(sources).map((todo) => todo.title),
    ["账号加载失败", "1 个代理异常", "磁盘空间不足"],
  );
});

test("settle 把异常变成带原因的失败", async () => {
  const [good, bad, empty] = await Promise.allSettled([Promise.resolve(1), Promise.reject(new Error("HTTP 500")), Promise.reject("")]);
  assert.deepEqual(settle(good), { ok: true, value: 1 });
  assert.deepEqual(settle(bad), { ok: false, error: "HTTP 500" });
  assert.deepEqual(settle(empty), { ok: false, error: "加载失败" });
});
