// 深色主题回归：用 inset 阴影画的选中框/状态线不能被全局去阴影规则清掉；深色按钮不能写死 stone-950 + 白字
import assert from "node:assert/strict";
import { readdirSync, readFileSync, statSync } from "node:fs";
import { join } from "node:path";
import { test } from "node:test";

const SRC = new URL("../src/", import.meta.url).pathname;
const css = readFileSync(join(SRC, "app/globals.css"), "utf8");

function sourceFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name);
    return statSync(path).isDirectory() ? sourceFiles(path) : path.endsWith(".tsx") ? [path] : [];
  });
}

test("深色去阴影规则跳过 inset 阴影", () => {
  const rule = css.match(/([^{}]*)\{\s*box-shadow:\s*none !important;?\s*\}/);
  assert.ok(rule, "找不到深色去阴影规则");
  const selectors = rule[1].split(",").map((selector) => selector.trim()).filter(Boolean);
  for (const selector of selectors) {
    if (selector.includes('[class*="shadow-"]')) {
      assert.match(selector, /:not\(\[class\*="shadow-\[inset"\]\)/, `${selector} 会清掉 inset 选中框`);
    }
  }
});

test("没有写死 stone-950/900 底 + 白字的按钮（深色下会和页面底色融在一起）", () => {
  const offenders = sourceFiles(SRC).flatMap((file) =>
    readFileSync(file, "utf8")
      .split("\n")
      .map((line, index) => ({ line, index }))
      .filter(({ line }) => /(?<![\w:-])bg-stone-9(?:50|00)\b/.test(line) && /\btext-white\b/.test(line))
      .map(({ index }) => `${file.slice(SRC.length)}:${index + 1}`),
  );
  assert.deepEqual(offenders, []);
});

test("号池窄屏下拉里有类型筛选", () => {
  const filters = readFileSync(join(SRC, "app/accounts/components/account-filters.tsx"), "utf8");
  const narrow = filters.slice(filters.indexOf("lg:hidden"));
  assert.match(narrow.slice(0, 2000), /value=\{filters\.type\}[\s\S]*typeOptions\.map/);
});

test("批量操作条（bg-ink）上的按钮不写死浅色，深色下条是浅底", () => {
  const offenders = sourceFiles(SRC).flatMap((file) =>
    readFileSync(file, "utf8")
      .split("\n")
      .map((line, index) => ({ line, index }))
      // 以前操作条上写死的浅红字（灯箱、调试代码块本身是深底，白边框不算）
      .filter(({ line }) => /text-\[#F[01]B[34]A[08]\]/.test(line) && !file.endsWith("batch-bar.ts"))
      .map(({ index }) => `${file.slice(SRC.length)}:${index + 1}`),
  );
  const shared = readFileSync(join(SRC, "components/ui/batch-bar.ts"), "utf8");
  assert.match(shared, /dark:text-/);
  assert.deepEqual(offenders, []);
});

test("页面不写死调色盘颜色：深色兜底已删除，只用设计变量", () => {
  // 灯箱是固定黑底，白色半透明按钮是对的
  const allowed = new Set(["components/image-lightbox.tsx"]);
  const pattern = /(?<![\w:-])(?:bg|text|border)-(?:stone|emerald|amber|sky|slate|zinc|gray|red|green|blue|rose)-\d{2,3}\b|(?<![\w:-])bg-white(?![\w-])/;
  const offenders = sourceFiles(SRC).flatMap((file) => {
    const rel = file.slice(SRC.length);
    if (allowed.has(rel)) return [];
    return readFileSync(file, "utf8")
      .split("\n")
      .map((line, index) => ({ line, index }))
      .filter(({ line }) => pattern.test(line))
      .map(({ index }) => `${rel}:${index + 1}`);
  });
  assert.deepEqual(offenders, []);
});

test("选中的复选框在深色下也有底色", () => {
  const checkbox = readFileSync(join(SRC, "components/ui/checkbox.tsx"), "utf8");
  assert.match(checkbox, /dark:data-\[state=checked\]:bg-primary/);
});
