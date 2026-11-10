import type { SystemLog } from "./api";

// 展开详情里优先显示、并按这个顺序排的字段；其余字段排在后面
const DETAIL_LABELS: Record<string, string> = {
  status: "结果",
  error: "错误",
  endpoint: "接口",
  model: "模型",
  size: "尺寸",
  account_email: "账号",
  key_name: "密钥",
  role: "角色",
  duration_ms: "用时",
  started_at: "开始",
  ended_at: "结束",
  token: "账号 token",
  source: "来源",
  added: "新增",
  skipped: "跳过",
  removed: "删除",
};
// 太长、单独展示的字段
const LONG_FIELDS = new Set(["request_text", "urls"]);

export function text(item: SystemLog, key: string) {
  const value = item.detail?.[key];
  return typeof value === "string" || typeof value === "number" ? String(value) : "";
}

export function formatDuration(item: SystemLog) {
  const value = item.detail?.duration_ms;
  return typeof value === "number" ? `${(value / 1000).toFixed(1)}s` : "—";
}

export function getUrls(item: SystemLog | null) {
  const urls = item?.detail?.urls;
  return Array.isArray(urls) ? urls.filter((url): url is string => typeof url === "string") : [];
}

/** 列表里只放错误的开头：上游返回的 HTML 正文等放进展开详情。 */
export function shortError(error: string) {
  return error.split(/,\s*body=/)[0].trim() || "失败";
}

export function isFailed(item: SystemLog) {
  return item.detail?.status === "failed";
}

const STATUS_TEXT: Record<string, string> = { success: "成功", failed: "失败" };

export function formatDetailValue(key: string, value: unknown) {
  if (key === "duration_ms" && typeof value === "number") return `${(value / 1000).toFixed(2)} s`;
  if (key === "status" && typeof value === "string") return STATUS_TEXT[value] ?? value;
  return String(value);
}

export function detailEntries(item: SystemLog) {
  const entries = Object.entries(item.detail ?? {}).filter(
    ([key, value]) => !LONG_FIELDS.has(key) && value !== null && value !== "" && typeof value !== "object",
  );
  const order = Object.keys(DETAIL_LABELS);
  const rank = (key: string) => (order.includes(key) ? order.indexOf(key) : order.length);
  return entries
    .sort(([a], [b]) => rank(a) - rank(b))
    // 账号日志的 status 是账号状态（正常 / 限流），不是调用结果
    .map(([key, value]) => [key === "status" && !(String(value) in STATUS_TEXT) ? "状态" : DETAIL_LABELS[key] ?? key, formatDetailValue(key, value)] as const);
}
