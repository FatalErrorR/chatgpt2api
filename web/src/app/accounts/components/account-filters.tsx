"use client";

import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { StatusDot } from "@/components/ui/status-dot";
import { ACCOUNT_STATUS_OPTIONS, type StatusFilter } from "@/lib/account-format";
import type { ProxyEntry } from "@/lib/api";
import { ABNORMAL_PROXY_FILTER, proxyShortName } from "@/lib/proxy-match";
import { cn } from "@/lib/utils";

import { proxyLabel } from "./account-import-dialog";

export type AccountFilters = { status: StatusFilter; type: string; proxy: string };

type Counts = {
  status: Record<string, number>;
  types: Map<string, number>;
  proxyKeys: Map<string, number>;
  noProxy: number;
  abnormalProxy: number;
};

const STATUS_FACETS = [
  ["all", "全部", null],
  ["正常", "正常", "ok"],
  ["限流", "限流", "warn"],
  ["异常", "异常", "bad"],
  ["禁用", "禁用", "off"],
  ["edit_cooldown", "图生图冷却", "warn"],
] as const;

const facetButton = (active: boolean) =>
  cn(
    "flex w-full items-center justify-between gap-2 rounded-[7px] px-[9px] py-1.5 text-left text-[13px] transition",
    active ? "bg-surface text-ink shadow-[inset_0_0_0_1px_var(--line)]" : "text-ink-2 hover:bg-surface-hover hover:text-ink",
  );
const facetCount = "text-xs text-ink-3 tabular-nums";

/** 宽屏左侧筛选栏：状态 / 类型 / 代理，带数量，可以组合。 */
export function AccountFilterSidebar({
  filters,
  counts,
  total,
  proxies,
  onChange,
}: {
  filters: AccountFilters;
  counts: Counts;
  total: number;
  proxies: ProxyEntry[];
  onChange: (patch: Partial<AccountFilters>) => void;
}) {
  return (
    <aside className="hidden min-w-0 content-start gap-[18px] lg:grid [&>div]:min-w-0">
      <div>
        <h4 className="mb-1.5 text-xs font-medium tracking-[0.04em] text-ink-3">状态</h4>
        {STATUS_FACETS.map(([value, label, tone]) => (
          <button key={value} type="button" className={facetButton(filters.status === value)} onClick={() => onChange({ status: value })}>
            {tone ? <StatusDot tone={tone}>{label}</StatusDot> : <span>{label}</span>}
            <span className={facetCount}>{counts.status[value] ?? 0}</span>
          </button>
        ))}
      </div>
      <div>
        <h4 className="mb-1.5 text-xs font-medium tracking-[0.04em] text-ink-3">类型</h4>
        <button type="button" className={facetButton(filters.type === "all")} onClick={() => onChange({ type: "all" })}>
          <span>全部</span>
          <span className={facetCount}>{total}</span>
        </button>
        {Array.from(counts.types.entries()).map(([type, count]) => (
          <button key={type} type="button" className={facetButton(filters.type === type)} onClick={() => onChange({ type })}>
            <span>{type}</span>
            <span className={facetCount}>{count}</span>
          </button>
        ))}
      </div>
      <div>
        <h4 className="mb-1.5 text-xs font-medium tracking-[0.04em] text-ink-3">代理</h4>
        <button type="button" className={facetButton(filters.proxy === "all")} onClick={() => onChange({ proxy: "all" })}>
          <span>全部</span>
          <span className={facetCount}>{total}</span>
        </button>
        {counts.abnormalProxy > 0 || filters.proxy === ABNORMAL_PROXY_FILTER ? (
          <button type="button" className={facetButton(filters.proxy === ABNORMAL_PROXY_FILTER)} onClick={() => onChange({ proxy: ABNORMAL_PROXY_FILTER })}>
            <StatusDot tone="bad">代理异常</StatusDot>
            <span className={facetCount}>{counts.abnormalProxy}</span>
          </button>
        ) : null}
        {/* 代理多时只露 10 行（每行 h-8），其余滚动；「全部」「未指定」留在外面 */}
        <div className="thin-scrollbar max-h-80 overflow-y-auto pr-1 [&>button]:h-8">
          {proxies.map((proxy) => (
            <button
              key={proxy.key}
              type="button"
              className={facetButton(filters.proxy === proxy.key)}
              onClick={() => onChange({ proxy: proxy.key })}
              title={proxyLabel(proxy)}
            >
              <StatusDot tone={proxy.health.abnormal ? "bad" : proxy.health.successes > 0 ? "ok" : "off"} className="min-w-0 overflow-hidden">
                <span className="truncate">{proxyShortName(proxy)}</span>
              </StatusDot>
              <span className={facetCount}>{counts.proxyKeys.get(proxy.key) ?? 0}</span>
            </button>
          ))}
        </div>
        <button type="button" className={facetButton(filters.proxy === "none")} onClick={() => onChange({ proxy: "none" })}>
          <StatusDot tone="off">未指定</StatusDot>
          <span className={facetCount}>{counts.noProxy}</span>
        </button>
      </div>
    </aside>
  );
}

/** 窄屏没有筛选栏，用下拉代替。 */
export function AccountFilterSelects({
  filters,
  typeOptions,
  proxies,
  onChange,
}: {
  filters: AccountFilters;
  typeOptions: string[];
  proxies: ProxyEntry[];
  onChange: (patch: Partial<AccountFilters>) => void;
}) {
  return (
    <div className="flex flex-wrap gap-2 lg:hidden">
      <Select value={filters.status} onValueChange={(value) => onChange({ status: value as StatusFilter })}>
        <SelectTrigger className="h-[34px] w-[120px] rounded-[7px]"><SelectValue /></SelectTrigger>
        <SelectContent>
          {ACCOUNT_STATUS_OPTIONS.map((option) => (
            <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>
          ))}
        </SelectContent>
      </Select>
      <Select value={filters.type} onValueChange={(value) => onChange({ type: value })}>
        <SelectTrigger className="h-[34px] w-[110px] rounded-[7px]"><SelectValue /></SelectTrigger>
        <SelectContent>
          <SelectItem value="all">全部类型</SelectItem>
          {typeOptions.map((type) => (
            <SelectItem key={type} value={type}>{type}</SelectItem>
          ))}
        </SelectContent>
      </Select>
      <Select value={filters.proxy} onValueChange={(value) => onChange({ proxy: value })}>
        <SelectTrigger className="h-[34px] w-[120px] rounded-[7px]"><SelectValue /></SelectTrigger>
        <SelectContent>
          <SelectItem value="all">全部代理</SelectItem>
          <SelectItem value={ABNORMAL_PROXY_FILTER}>代理异常</SelectItem>
          <SelectItem value="none">未指定代理</SelectItem>
          {proxies.map((proxy) => (
            <SelectItem key={proxy.key} value={proxy.key}>{proxyShortName(proxy)}</SelectItem>
          ))}
        </SelectContent>
      </Select>
    </div>
  );
}
