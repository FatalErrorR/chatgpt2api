// 号池左侧代理筛选：代理上百个时只露 10 行，其余在框内滚动，不把整页撑长
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

const page = readFileSync(new URL("../src/app/accounts/page.tsx", import.meta.url), "utf8");

test("代理筛选列表限高 10 行并可滚动", () => {
  const list = page.match(/<div className="([^"]*)">\s*\{proxies\.map\(/);
  assert.ok(list, "代理按钮外面没有限高的滚动容器");
  const classes = list[1].split(/\s+/);
  for (const name of ["max-h-80", "overflow-y-auto", "[&>button]:h-8"]) {
    assert.ok(classes.includes(name), `滚动容器缺少 ${name}（10 行 × h-8 = max-h-80）`);
  }
});
