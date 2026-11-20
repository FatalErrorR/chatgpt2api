"use client";

import { ChevronLeft, ChevronRight, Copy, Pencil, RefreshCw, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { RowMenu } from "@/components/row-menu";
import { Checkbox } from "@/components/ui/checkbox";
import { StatusDot } from "@/components/ui/status-dot";
import {
  STATUS_TONE,
  accountStatusError,
  displayAccountSource,
  displayAccountType,
  formatCreatedAt,
  formatQuota,
  formatRestoreAt,
  isEditUploadCooled,
  paginationItems,
  restoreLabel,
} from "@/lib/account-format";
import type { Account, ProxyEntry } from "@/lib/api";
import { proxyForAccount, proxyShortName } from "@/lib/proxy-match";
import { cn } from "@/lib/utils";

import { proxyLabel } from "./account-import-dialog";

export type AccountRowActions = {
  onEdit: (account: Account) => void;
  onRefresh: (token: string) => void;
  onDelete: (token: string) => void;
  isRowBusy: (token: string) => boolean;
  disabled: boolean;
};

function copyToken(account: Account) {
  void navigator.clipboard.writeText(account.access_token);
  toast.success("已复制 token");
}

/** 账号表：窄屏卡片、宽屏表格，加底部分页。 */
export function AccountTable({
  rows,
  isLoading,
  totalAccounts,
  filteredCount,
  proxyByKey,
  selectedIds,
  onSelectedIdsChange,
  page,
  pageCount,
  onPageChange,
  actions,
}: {
  rows: Account[];
  isLoading: boolean;
  totalAccounts: number;
  filteredCount: number;
  proxyByKey: Map<string, ProxyEntry>;
  selectedIds: string[];
  onSelectedIdsChange: (update: (prev: string[]) => string[]) => void;
  page: number;
  pageCount: number;
  onPageChange: (page: number) => void;
  actions: AccountRowActions;
}) {
  const allCurrentSelected = rows.length > 0 && rows.every((row) => selectedIds.includes(row.access_token));
  const toggleOne = (token: string, checked: boolean) =>
    onSelectedIdsChange((prev) => (checked ? Array.from(new Set([...prev, token])) : prev.filter((item) => item !== token)));
  const togglePage = (checked: boolean) =>
    onSelectedIdsChange((prev) =>
      checked
        ? Array.from(new Set([...prev, ...rows.map((row) => row.access_token)]))
        : prev.filter((id) => !rows.some((row) => row.access_token === id)),
    );

  return (
    // 宽屏：卡片撑满父容器（与左侧筛选栏等高），表头和分页固定，中间行在卡片内滚动
    <div className="flex flex-col overflow-hidden rounded-[10px] border border-line bg-surface lg:h-full lg:min-h-0">
      <div className="thin-scrollbar min-h-0 overflow-x-auto lg:flex-1 lg:overflow-y-auto">
      {isLoading && totalAccounts === 0 ? (
        <div className="space-y-3 p-5" aria-busy="true">
          {[80, 95, 60, 88, 72].map((width) => (
            <div key={width} className="h-3 animate-pulse rounded-md bg-sunken" style={{ width: `${width}%` }} />
          ))}
        </div>
      ) : (
        <>
          {/* 手机：卡片列表 */}
          <div className="divide-y divide-line md:hidden">
            {rows.map((account) => {
              const statusError = accountStatusError(account);
              const proxy = proxyForAccount(proxyByKey, account);
              const restore = restoreLabel(account, formatRestoreAt(account.restore_at));
              return (
                <div key={account.access_token} className="flex items-start gap-3 px-3.5 py-3">
                  <Checkbox
                    className="mt-0.5"
                    aria-label="选择账号"
                    checked={selectedIds.includes(account.access_token)}
                    onCheckedChange={(checked) => toggleOne(account.access_token, Boolean(checked))}
                  />
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center justify-between gap-2">
                      <span className="truncate text-[13px] text-ink">{account.email ?? "—"}</span>
                      <StatusDot tone={STATUS_TONE[account.status]}>{account.status}</StatusDot>
                    </div>
                    <div className={cn("mt-0.5 truncate text-xs", statusError ? "text-bad" : "text-ink-3")}>
                      {statusError ||
                        [proxy ? proxyShortName(proxy) : "", displayAccountType(account), `额度 ${formatQuota(account)}`, restore]
                          .filter(Boolean)
                          .join(" · ")}
                    </div>
                  </div>
                  <RowMenu
                    items={[
                      { label: "复制 token", icon: Copy, onSelect: () => copyToken(account) },
                      { label: "编辑", icon: Pencil, onSelect: () => actions.onEdit(account), disabled: actions.disabled },
                      { label: "刷新额度", icon: RefreshCw, onSelect: () => actions.onRefresh(account.access_token), disabled: actions.isRowBusy(account.access_token) },
                      { label: "删除", icon: Trash2, danger: true, onSelect: () => actions.onDelete(account.access_token) },
                    ]}
                  />
                </div>
              );
            })}
          </div>
          <div className="hidden md:block">
            <table className="w-full min-w-[760px] text-left text-[13px]">
              {/* 表头吸顶：用 inset 阴影画底线，滚动时线跟着表头走 */}
              <thead className="text-xs text-ink-3 [&_th]:sticky [&_th]:top-0 [&_th]:z-10 [&_th]:bg-surface [&_th]:shadow-[inset_0_-1px_0_var(--line)]">
                <tr>
                  <th className="w-10 py-2.5 pr-1 pl-3.5 font-normal">
                    <Checkbox aria-label="选择本页" checked={allCurrentSelected} onCheckedChange={(checked) => togglePage(Boolean(checked))} />
                  </th>
                  <th className="px-2.5 py-2.5 font-normal">账号</th>
                  <th className="px-2.5 py-2.5 font-normal">代理</th>
                  <th className="px-2.5 py-2.5 font-normal">状态</th>
                  <th className="px-2.5 py-2.5 text-right font-normal">额度</th>
                  <th className="px-2.5 py-2.5 font-normal">恢复时间</th>
                  <th className="px-2.5 py-2.5 text-right font-normal">成功 / 失败</th>
                  <th className="w-20 px-2.5 py-2.5" />
                </tr>
              </thead>
              <tbody>
                {rows.map((account) => {
                  const statusError = accountStatusError(account);
                  const cooldown = isEditUploadCooled(account) ? formatRestoreAt(account.edit_upload_blocked_until) : null;
                  const restoreAt = formatRestoreAt(account.restore_at);
                  const restore = restoreLabel(account, restoreAt);
                  const url = (account.proxy || "").trim();
                  const proxy = proxyForAccount(proxyByKey, account);
                  const inflight = account.image_inflight ?? 0;
                  const busy = actions.isRowBusy(account.access_token);
                  return (
                    <tr key={account.access_token} className="border-b border-line transition-colors last:border-b-0 hover:bg-surface-hover">
                      <td className="py-3 pr-1 pl-3.5">
                        <Checkbox
                          aria-label="选择账号"
                          checked={selectedIds.includes(account.access_token)}
                          onCheckedChange={(checked) => toggleOne(account.access_token, Boolean(checked))}
                        />
                      </td>
                      <td className="max-w-[280px] px-2.5 py-3">
                        <div className="truncate text-ink" title={account.email ?? undefined}>{account.email ?? "—"}</div>
                        {statusError ? (
                          <div className="truncate text-xs text-bad" title={statusError}>{statusError}</div>
                        ) : cooldown ? (
                          <div className="truncate text-xs text-warn">图生图冷却 · {cooldown.relative.replace("剩余 ", "还剩 ")}</div>
                        ) : (
                          <div className="truncate text-xs text-ink-3">
                            {displayAccountType(account)} · {displayAccountSource(account)} · {formatCreatedAt(account.created_at)}
                          </div>
                        )}
                      </td>
                      <td className="px-2.5 py-3 whitespace-nowrap">
                        {!url ? (
                          <span className="text-ink-3">—</span>
                        ) : (
                          <span
                            className={cn("block max-w-[150px] truncate", proxy?.health.abnormal ? "text-bad" : "text-ink-2")}
                            title={proxy ? `${proxyLabel(proxy)}${proxy.health.abnormal ? `\n代理异常：${proxy.health.last_error || "连续请求失败"}` : ""}` : undefined}
                          >
                            {proxy ? proxyShortName(proxy) : "…"}
                          </span>
                        )}
                      </td>
                      <td className="px-2.5 py-3">
                        <StatusDot tone={STATUS_TONE[account.status]} title={statusError || undefined}>
                          {account.status}
                        </StatusDot>
                      </td>
                      <td className="px-2.5 py-3 text-right tabular-nums text-ink">{formatQuota(account)}</td>
                      <td className="px-2.5 py-3 whitespace-nowrap">
                        {restore ? (
                          <>
                            <div className="text-ink">{restore}</div>
                            <div className="text-xs text-ink-3">{restoreAt.absolute}</div>
                          </>
                        ) : (
                          <span className="text-ink-3">—</span>
                        )}
                      </td>
                      <td className="px-2.5 py-3 text-right font-mono text-xs whitespace-nowrap text-ink-2 tabular-nums">
                        {inflight > 0 ? (
                          <span className="mr-2 text-warn" title="正在生成的图片数；号池空闲时仍大于 0 说明并发槽位泄漏">
                            在途 {inflight}
                          </span>
                        ) : null}
                        {account.success} / {account.fail}
                      </td>
                      <td className="px-2.5 py-3">
                        <div className="flex items-center justify-end gap-0.5">
                          <button
                            type="button"
                            title="复制 token"
                            className="grid size-8 place-items-center rounded-[7px] text-ink-3 transition hover:bg-surface-hover hover:text-ink"
                            onClick={() => copyToken(account)}
                          >
                            <Copy className="size-4" />
                          </button>
                          <RowMenu
                            items={[
                              { label: "编辑", icon: Pencil, onSelect: () => actions.onEdit(account), disabled: actions.disabled },
                              { label: busy ? "刷新中…" : "刷新额度", icon: RefreshCw, onSelect: () => actions.onRefresh(account.access_token), disabled: busy },
                              { label: "删除", icon: Trash2, danger: true, onSelect: () => actions.onDelete(account.access_token), disabled: actions.disabled },
                            ]}
                          />
                        </div>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
          {!isLoading && rows.length === 0 ? (
            <div className="px-6 py-14 text-center text-[13px] text-ink-3">
              <div className="mb-1 font-serif text-[17px] text-ink">{totalAccounts === 0 ? "还没有账号" : "没有匹配的账号"}</div>
              {totalAccounts === 0 ? "导入账号后会显示在这里" : "调整筛选条件或搜索关键字后重试"}
            </div>
          ) : null}
        </>
      )}
      </div>
      <div className="flex shrink-0 flex-wrap items-center justify-between gap-3 border-t border-line px-3.5 py-2.5 text-xs text-ink-3">
        <span>{filteredCount === totalAccounts ? `全部 ${totalAccounts} 个` : `筛选后 ${filteredCount} 个，共 ${totalAccounts} 个`}</span>
        <div className="flex items-center gap-1">
          <button
            type="button"
            aria-label="上一页"
            className="grid size-7 place-items-center rounded-md text-ink-2 hover:bg-surface-hover disabled:opacity-40"
            disabled={page <= 1}
            onClick={() => onPageChange(Math.max(1, page - 1))}
          >
            <ChevronLeft className="size-4" />
          </button>
          {paginationItems(page, pageCount).map((item, index) =>
            item === "..." ? (
              <span key={`ellipsis-${index}`} className="px-1">…</span>
            ) : (
              <button
                key={item}
                type="button"
                onClick={() => onPageChange(item)}
                className={cn("grid h-7 min-w-7 place-items-center rounded-md px-1.5 font-medium tabular-nums", item === page ? "bg-ink text-page" : "text-ink-2 hover:bg-surface-hover")}
              >
                {item}
              </button>
            ),
          )}
          <button
            type="button"
            aria-label="下一页"
            className="grid size-7 place-items-center rounded-md text-ink-2 hover:bg-surface-hover disabled:opacity-40"
            disabled={page >= pageCount}
            onClick={() => onPageChange(Math.min(pageCount, page + 1))}
          >
            <ChevronRight className="size-4" />
          </button>
        </div>
      </div>
    </div>
  );
}
