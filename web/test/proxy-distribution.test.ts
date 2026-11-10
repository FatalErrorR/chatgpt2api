import assert from "node:assert/strict";
import { test } from "node:test";

import { previewProxyDistribution, type DistributionCandidate } from "../src/lib/proxy-distribution.ts";

const proxies: DistributionCandidate[] = [
  { url: "http://a", accountCount: 2, abnormal: false },
  { url: "http://b", accountCount: 2, abnormal: false },
  { url: "http://c", accountCount: 0, abnormal: true },
];

test("平手按清单顺序，先分给前面的代理", () => {
  const result = previewProxyDistribution(proxies, 3);
  assert.deepEqual(result.map((item) => item.assigned), [2, 1, 0]);
});

test("负载更少的代理先拿账号", () => {
  const result = previewProxyDistribution(
    [
      { url: "http://a", accountCount: 5, abnormal: false },
      { url: "http://b", accountCount: 1, abnormal: false },
    ],
    2,
  );
  assert.deepEqual(result.map((item) => item.assigned), [0, 2]);
});

test("跳过异常代理，除非全部都异常", () => {
  const allBad = previewProxyDistribution(
    [
      { url: "http://a", accountCount: 1, abnormal: true },
      { url: "http://b", accountCount: 4, abnormal: true },
    ],
    2,
  );
  assert.deepEqual(allBad.map((item) => item.assigned), [2, 0]);
  const mixed = previewProxyDistribution(proxies, 1);
  assert.equal(mixed.find((item) => item.url === "http://c")?.assigned, 0);
});
