// 日志展开详情与列表摘要的格式
import assert from "node:assert/strict";
import { test } from "node:test";

import type { SystemLog } from "../src/lib/api.ts";
import { detailEntries, formatDuration, getUrls, shortError } from "../src/lib/log-detail.ts";

const call: SystemLog = {
  id: "1",
  time: "2026-11-10 08:00:00",
  type: "call",
  detail: {
    request_text: "很长的提示词",
    urls: ["/a.png", 3],
    account_email: "a@example.com",
    custom_field: "x",
    duration_ms: 3562,
    endpoint: "/v1/images/edits",
    error: "bootstrap failed: status=403, body=<html>…</html>",
    status: "failed",
    nested: { skip: true },
    empty: "",
  },
};

test("列表里的错误去掉上游返回的正文", () => {
  assert.equal(shortError("bootstrap failed: status=403, body=<html>Enable JavaScript</html>"), "bootstrap failed: status=403");
  assert.equal(shortError("ChatGPT 生图超时（已等待 600 秒）"), "ChatGPT 生图超时（已等待 600 秒）");
  assert.equal(shortError(""), "失败");
});

test("详情：常用字段在前并翻译，长字段、对象和空值不进键值列表", () => {
  assert.deepEqual(detailEntries(call), [
    ["结果", "失败"],
    ["错误", "bootstrap failed: status=403, body=<html>…</html>"],
    ["接口", "/v1/images/edits"],
    ["账号", "a@example.com"],
    ["用时", "3.56 s"],
    ["custom_field", "x"],
  ]);
});

test("账号日志的 status 是账号状态", () => {
  const log: SystemLog = { id: "2", time: "", type: "account", detail: { token: "token:abc", status: "正常" } };
  assert.deepEqual(detailEntries(log), [["状态", "正常"], ["账号 token", "token:abc"]]);
});

test("用时和图片地址", () => {
  assert.equal(formatDuration(call), "3.6s");
  assert.equal(formatDuration({ ...call, detail: {} }), "—");
  assert.deepEqual(getUrls(call), ["/a.png"]);
  assert.deepEqual(getUrls(null), []);
});
