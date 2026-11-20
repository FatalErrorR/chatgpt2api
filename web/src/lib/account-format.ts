/** 号池（总览）里账号的显示与筛选规则。纯函数，方便测试。 */
import type { StatusTone } from "@/components/ui/status-dot";

import type { Account, AccountStatus } from "./api";

export type StatusFilter = AccountStatus | "all" | "edit_cooldown";

export const ACCOUNT_STATUS_OPTIONS: { label: string; value: StatusFilter }[] = [
  { label: "全部状态", value: "all" },
  { label: "正常", value: "正常" },
  { label: "限流", value: "限流" },
  { label: "异常", value: "异常" },
  { label: "禁用", value: "禁用" },
  { label: "图生图冷却中", value: "edit_cooldown" },
];

export const STATUS_TONE: Record<AccountStatus, StatusTone> = {
  正常: "ok",
  限流: "warn",
  异常: "bad",
  禁用: "off",
};

export function formatCompact(value: number) {
  return value >= 1000 ? `${(value / 1000).toFixed(1)}k` : String(value);
}

export function formatQuota(account: Account) {
  return String(Math.max(0, account.quota));
}

/** 正常账号的剩余额度合计。 */
export function availableQuota(accounts: Account[]) {
  return accounts.filter((account) => account.status === "正常").reduce((sum, account) => sum + Math.max(0, account.quota), 0);
}

export function formatRestoreAt(value?: string | null, now = Date.now()) {
  if (!value) return { absolute: "—", relative: "" };
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return { absolute: value, relative: "" };

  const diffMs = Math.max(0, date.getTime() - now);
  const totalHours = Math.ceil(diffMs / (1000 * 60 * 60));
  const days = Math.floor(totalHours / 24);
  const hours = totalHours % 24;
  const relative =
    diffMs <= 0 ? "已到恢复时间" : days > 0 ? `剩余 ${days} 天${hours ? ` ${hours} 小时` : ""}` : `剩余 ${Math.max(1, hours)} 小时`;
  const pad = (num: number) => String(num).padStart(2, "0");
  const absolute = `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
  return { absolute, relative };
}

/**
 * 恢复时间列：额度恢复只对限流账号、或恢复时间还没到的账号有意义。
 * 正常且有额度、或已异常的账号，过去的恢复时间只是噪音。
 */
export function restoreLabel(account: Pick<Account, "status">, restore: { relative: string }) {
  if (!restore.relative) return "";
  if (restore.relative === "已到恢复时间" && account.status !== "限流") return "";
  return restore.relative;
}

export function formatCreatedAt(raw?: string | null) {
  if (!raw) return "—";
  const date = new Date(`${raw}Z`);
  if (Number.isNaN(date.getTime())) return String(raw).slice(0, 10);
  const pad = (num: number) => String(num).padStart(2, "0");
  return `${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

export function isEditUploadCooled(account: { edit_upload_blocked_until?: string | null }, now = Date.now()) {
  const raw = account.edit_upload_blocked_until;
  if (!raw) return false;
  const date = new Date(raw);
  return !Number.isNaN(date.getTime()) && date.getTime() > now;
}

export function displayAccountType(account: Pick<Account, "type">) {
  return account.type || "Free";
}

export function displayAccountSource(account: Pick<Account, "source_type">) {
  const source = String(account.source_type || "").trim().toLowerCase();
  return source || "web";
}

/** 异常账号的原因（刷新或换 token 时记下的错误）；不是异常就返回空。 */
export function accountStatusError(account: Pick<Account, "status" | "last_refresh_error" | "last_token_refresh_error">) {
  return account.status === "异常" ? (account.last_refresh_error || account.last_token_refresh_error || "").trim() : "";
}

export function matchesStatusFilter(account: Account, filter: StatusFilter, now = Date.now()) {
  if (filter === "all") return true;
  if (filter === "edit_cooldown") return isEditUploadCooled(account, now);
  return account.status === filter;
}

/** 筛选栏各项的数量，基于全部账号，不受当前筛选影响。 */
export function facetCounts(accounts: Account[], now = Date.now()) {
  const status: Record<string, number> = { all: accounts.length, 正常: 0, 限流: 0, 异常: 0, 禁用: 0, edit_cooldown: 0 };
  const types = new Map<string, number>();
  const proxyKeys = new Map<string, number>();
  let noProxy = 0;
  let inflight = 0;
  for (const account of accounts) {
    status[account.status] = (status[account.status] ?? 0) + 1;
    if (isEditUploadCooled(account, now)) status.edit_cooldown += 1;
    const type = displayAccountType(account);
    types.set(type, (types.get(type) ?? 0) + 1);
    if ((account.proxy || "").trim() && account.proxy_key) {
      proxyKeys.set(account.proxy_key, (proxyKeys.get(account.proxy_key) ?? 0) + 1);
    } else {
      noProxy += 1;
    }
    inflight += account.image_inflight ?? 0;
  }
  return { status, types, proxyKeys, noProxy, inflight };
}

export function downloadTokens(accounts: Account[]) {
  const content = `${accounts.map((account) => account.access_token).join("\n")}\n`;
  const blob = new Blob([content], { type: "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = `accounts-${Date.now()}.txt`;
  link.click();
  URL.revokeObjectURL(url);
}

/** 分页按钮：首页、当前页前后各一页、末页，中间用省略号。 */
export function paginationItems(page: number, pageCount: number): (number | "...")[] {
  const items: (number | "...")[] = [];
  const start = Math.max(1, page - 1);
  const end = Math.min(pageCount, page + 1);
  if (start > 1) items.push(1);
  if (start > 2) items.push("...");
  for (let current = start; current <= end; current += 1) items.push(current);
  if (end < pageCount - 1) items.push("...");
  if (end < pageCount) items.push(pageCount);
  return items;
}
