"use client";

import { Fragment, useEffect, useMemo, useState } from "react";
import { ChevronDown, ChevronLeft, ChevronRight, RefreshCw, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/confirm-dialog";
import { DateRangeFilter } from "@/components/date-range-filter";
import { ImageLightbox } from "@/components/image-lightbox";
import { ImageThumbnail, getImageThumbnailUrl } from "@/components/image-thumbnail";
import { PageHeader } from "@/components/page-header";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Segmented } from "@/components/ui/segmented";
import { EmptyState, ErrorState, SkeletonLines } from "@/components/ui/states";
import { StatusDot } from "@/components/ui/status-dot";
import { deleteSystemLogs, fetchSystemLogs, type SystemLog } from "@/lib/api";
import { detailEntries, formatDuration, getUrls, isFailed, shortError, text } from "@/lib/log-detail";
import { useAuthGuard } from "@/lib/use-auth-guard";
import { cn } from "@/lib/utils";

const LogType = {
  Call: "call",
  Account: "account",
} as const;
type LogTypeValue = (typeof LogType)[keyof typeof LogType];

const TYPE_OPTIONS = [
  [LogType.Call, "调用"],
  [LogType.Account, "账号"],
] as const;

const PAGE_SIZE = 20;

function LogDetail({ item, onOpenImage, onDelete }: { item: SystemLog; onOpenImage: (index: number) => void; onDelete: () => void }) {
  const urls = getUrls(item);
  const requestText = text(item, "request_text");
  return (
    <div className="grid gap-3 px-1 py-1">
      <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1.5 text-[12px] sm:gap-x-6">
        {detailEntries(item).map(([label, value]) => (
          <Fragment key={label}>
            <dt className="text-ink-3">{label}</dt>
            <dd className={cn("min-w-0 font-mono break-all whitespace-pre-wrap", label === "错误" ? "text-bad" : "text-ink-2")}>{value}</dd>
          </Fragment>
        ))}
      </dl>
      {urls.length ? (
        <div className="flex flex-wrap gap-2">
          {urls.map((url, index) => (
            <button
              key={`${url}-${index}`}
              type="button"
              className="size-20 overflow-hidden rounded-[7px] border border-line bg-sunken"
              onClick={() => onOpenImage(index)}
              title="预览图片"
            >
              <ImageThumbnail src={url} thumbnailSrc={getImageThumbnailUrl(url)} className="h-full w-full" />
            </button>
          ))}
        </div>
      ) : null}
      {requestText ? (
        <details className="group">
          <summary className="cursor-pointer text-[12px] text-ink-3 hover:text-ink">提示词（{requestText.length} 字）</summary>
          <pre className="mt-2 max-h-64 overflow-auto rounded-[7px] border border-line bg-sunken p-3 font-mono text-[12px] leading-6 whitespace-pre-wrap text-ink-2">
            {requestText}
          </pre>
        </details>
      ) : null}
      <details>
        <summary className="cursor-pointer text-[12px] text-ink-3 hover:text-ink">原始 JSON</summary>
        <pre className="mt-2 max-h-72 overflow-auto rounded-[7px] border border-line bg-sunken p-3 font-mono text-[12px] leading-6 text-ink-2">
          {JSON.stringify(item.detail ?? {}, null, 2)}
        </pre>
      </details>
      <div>
        <Button variant="outline" size="sm" className="text-bad hover:text-bad" onClick={onDelete}>
          <Trash2 className="size-3.5" />
          删除这条
        </Button>
      </div>
    </div>
  );
}

function LogsContent() {
  const [items, setItems] = useState<SystemLog[]>([]);
  const [type, setType] = useState<LogTypeValue>(LogType.Call);
  const [startDate, setStartDate] = useState("");
  const [endDate, setEndDate] = useState("");
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [lightbox, setLightbox] = useState<{ item: SystemLog; index: number } | null>(null);
  const [page, setPage] = useState(1);
  const [isLoading, setIsLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [isDeleting, setIsDeleting] = useState(false);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [deletingItems, setDeletingItems] = useState<SystemLog[]>([]);
  const isCallLog = type === LogType.Call;
  const pageCount = Math.max(1, Math.ceil(items.length / PAGE_SIZE));
  const safePage = Math.min(page, pageCount);
  const currentRows = items.slice((safePage - 1) * PAGE_SIZE, safePage * PAGE_SIZE);
  const selectedSet = useMemo(() => new Set(selectedIds), [selectedIds]);
  const currentPageSelected = currentRows.length > 0 && currentRows.every((item) => selectedSet.has(item.id));
  const columnCount = isCallLog ? 9 : 5;

  const loadLogs = async () => {
    setIsLoading(true);
    try {
      const data = await fetchSystemLogs({ type, start_date: startDate, end_date: endDate });
      setItems(data.items);
      setLoadError("");
      setSelectedIds((current) => current.filter((id) => data.items.some((item) => item.id === id)));
      setPage(1);
    } catch (error) {
      setLoadError(error instanceof Error ? error.message : "加载日志失败");
    } finally {
      setIsLoading(false);
    }
  };

  const toggleIds = (ids: string[], checked: boolean) => {
    setSelectedIds((current) => (checked ? Array.from(new Set([...current, ...ids])) : current.filter((id) => !ids.includes(id))));
  };

  const confirmDelete = async () => {
    const ids = deletingItems.map((item) => item.id);
    if (ids.length === 0) return;
    setIsDeleting(true);
    try {
      const data = await deleteSystemLogs(ids);
      toast.success(`已删除 ${data.removed} 条日志`);
      setDeletingItems([]);
      setSelectedIds((current) => current.filter((id) => !ids.includes(id)));
      if (expandedId && ids.includes(expandedId)) setExpandedId(null);
      await loadLogs();
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "删除日志失败");
    } finally {
      setIsDeleting(false);
    }
  };

  useEffect(() => {
    setExpandedId(null);
    void loadLogs();
  }, [type, startDate, endDate]);

  const hasFilter = Boolean(startDate || endDate);

  return (
    <section className="space-y-5">
      <PageHeader
        title="日志"
        description="调用记录与账号操作记录。点一行展开完整详情。"
        actions={
          <Button variant="outline" onClick={() => void loadLogs()} disabled={isLoading}>
            <RefreshCw className={cn("size-4", isLoading && "animate-spin")} />
            刷新
          </Button>
        }
      />

      <div className="flex flex-wrap items-center gap-2">
        <Segmented value={type} options={TYPE_OPTIONS} onChange={setType} />
        <DateRangeFilter startDate={startDate} endDate={endDate} onChange={(start, end) => { setStartDate(start); setEndDate(end); }} />
        {hasFilter ? (
          <Button variant="ghost" size="sm" onClick={() => { setStartDate(""); setEndDate(""); }}>
            清除日期
          </Button>
        ) : null}
        <span className="flex-1" />
        <span className="text-xs text-ink-3">
          共 {items.length} 条 · 第 {safePage} / {pageCount} 页
        </span>
      </div>

      {selectedIds.length > 0 ? (
        <div className="flex flex-wrap items-center gap-1.5 rounded-[10px] bg-ink py-2 pr-2.5 pl-4 text-[13px] text-page">
          <span>已选 {selectedIds.length} 条</span>
          {selectedIds.length < items.length ? (
            <button type="button" className="rounded-[6px] px-2 py-1 text-page/75 hover:text-page" onClick={() => toggleIds(items.map((item) => item.id), true)}>
              全选 {items.length} 条
            </button>
          ) : null}
          <span className="flex-1" />
          <button type="button" className="rounded-[6px] px-2.5 py-1 text-page/75 hover:text-page" onClick={() => setSelectedIds([])} disabled={isDeleting}>
            取消选择
          </button>
          <button
            type="button"
            className="inline-flex items-center gap-1.5 rounded-[6px] bg-page/10 px-2.5 py-1 text-[#F1B4A8] hover:bg-page/15"
            onClick={() => setDeletingItems(items.filter((item) => selectedSet.has(item.id)))}
            disabled={isDeleting}
          >
            <Trash2 className="size-3.5" />
            删除
          </button>
        </div>
      ) : null}

      <div className="overflow-hidden rounded-[10px] border border-line bg-surface">
        {loadError && items.length === 0 ? (
          <ErrorState className="p-4" message={`加载日志失败：${loadError}`} onRetry={() => void loadLogs()} />
        ) : isLoading && items.length === 0 ? (
          <SkeletonLines lines={6} className="px-[18px] py-4" />
        ) : items.length === 0 ? (
          <EmptyState
            title="没有日志"
            description={hasFilter ? "这个日期范围里没有记录，换个范围或清除日期。" : isCallLog ? "还没有调用记录。" : "还没有账号操作记录。"}
          />
        ) : (
          <>
            {loadError ? <div className="border-b border-line bg-bad-soft px-4 py-2 text-[13px] text-bad">刷新失败：{loadError}（下面是上次的结果）</div> : null}
            {/* 窄屏：卡片列表，展开详情不用横向拖 */}
            <div className="md:hidden">
              {currentRows.map((item) => {
                const expanded = expandedId === item.id;
                const failed = isFailed(item);
                return (
                  <div
                    key={item.id}
                    className={cn("border-b border-line px-4 py-3 last:border-b-0", failed && "shadow-[inset_2px_0_0_var(--bad)]")}
                  >
                    <div className="flex items-start gap-3">
                      <Checkbox
                        aria-label="选择这条"
                        className="mt-0.5"
                        checked={selectedSet.has(item.id)}
                        onCheckedChange={(checked) => toggleIds([item.id], Boolean(checked))}
                      />
                      <button type="button" className="grid min-w-0 flex-1 gap-1 text-left" onClick={() => setExpandedId(expanded ? null : item.id)}>
                        <div className="flex items-center gap-2">
                          <span className="shrink-0 font-mono text-xs whitespace-nowrap text-ink-2">{String(item.time).slice(5)}</span>
                          <span className="flex-1" />
                          {isCallLog ? (
                            failed ? (
                              <StatusDot tone="bad" className="min-w-0 overflow-hidden">
                                <span className="truncate">{shortError(text(item, "error"))}</span>
                              </StatusDot>
                            ) : item.detail?.status === "success" ? (
                              <StatusDot tone="ok">成功</StatusDot>
                            ) : null
                          ) : null}
                          <ChevronDown className={cn("size-4 shrink-0 text-ink-3 transition-transform", !expanded && "-rotate-90")} />
                        </div>
                        <div className="truncate text-[13px] text-ink">
                          {isCallLog ? [text(item, "endpoint"), text(item, "model")].filter(Boolean).join(" · ") || "—" : item.summary || "—"}
                        </div>
                        {isCallLog ? (
                          <div className="truncate text-xs text-ink-3">
                            {[text(item, "account_email"), formatDuration(item)].filter((value) => value && value !== "—").join(" · ")}
                          </div>
                        ) : null}
                      </button>
                    </div>
                    {expanded ? (
                      <div className="mt-3 border-t border-line pt-3">
                        <LogDetail item={item} onOpenImage={(index) => setLightbox({ item, index })} onDelete={() => setDeletingItems([item])} />
                      </div>
                    ) : null}
                  </div>
                );
              })}
            </div>
            <div className="hidden overflow-x-auto md:block">
              <table className={cn("w-full text-left text-[13px]", isCallLog ? "min-w-[920px]" : "min-w-[640px]")}>
                <thead>
                  <tr className="border-b border-line text-xs text-ink-3">
                    <th className="w-10 py-2.5 pl-4">
                      <Checkbox
                        aria-label="选择本页"
                        checked={currentPageSelected}
                        onCheckedChange={(checked) => toggleIds(currentRows.map((item) => item.id), Boolean(checked))}
                      />
                    </th>
                    <th className="px-2.5 py-2.5 font-medium">时间</th>
                    {isCallLog ? (
                      <>
                        <th className="px-2.5 py-2.5 font-medium">接口</th>
                        <th className="px-2.5 py-2.5 font-medium">模型</th>
                        <th className="px-2.5 py-2.5 font-medium">账号</th>
                        <th className="px-2.5 py-2.5 font-medium">结果</th>
                        <th className="px-2.5 py-2.5 text-right font-medium">用时</th>
                        <th className="px-2.5 py-2.5 font-medium">图片</th>
                      </>
                    ) : (
                      <>
                        <th className="px-2.5 py-2.5 font-medium">操作</th>
                        <th className="px-2.5 py-2.5 font-medium">内容</th>
                      </>
                    )}
                    <th className="w-10 py-2.5 pr-3" />
                  </tr>
                </thead>
                <tbody>
                  {currentRows.map((item) => {
                    const expanded = expandedId === item.id;
                    const urls = getUrls(item);
                    const failed = isFailed(item);
                    const toggle = () => setExpandedId(expanded ? null : item.id);
                    return (
                      <Fragment key={item.id}>
                        <tr
                          className={cn("cursor-pointer border-b border-line transition-colors hover:bg-surface-hover/60", expanded && "bg-surface-hover/60")}
                          onClick={toggle}
                        >
                          <td className={cn("w-10 py-3 pl-4", failed && "shadow-[inset_2px_0_0_var(--bad)]")} onClick={(event) => event.stopPropagation()}>
                            <Checkbox aria-label="选择这条" checked={selectedSet.has(item.id)} onCheckedChange={(checked) => toggleIds([item.id], Boolean(checked))} />
                          </td>
                          <td className="px-2.5 py-3 font-mono text-xs whitespace-nowrap text-ink-2">{String(item.time).slice(5)}</td>
                          {isCallLog ? (
                            <>
                              <td className="px-2.5 py-3 whitespace-nowrap text-ink">{text(item, "endpoint") || "—"}</td>
                              <td className="px-2.5 py-3">
                                {text(item, "model") ? <span className="inline-flex h-[22px] items-center rounded-[6px] bg-sunken px-2 text-xs text-ink-2">{text(item, "model")}</span> : <span className="text-ink-3">—</span>}
                              </td>
                              <td className="max-w-[220px] truncate px-2.5 py-3 text-ink-2" title={text(item, "account_email")}>{text(item, "account_email") || <span className="text-ink-3">—</span>}</td>
                              <td className="px-2.5 py-3">
                                {failed ? (
                                  <StatusDot tone="bad" className="flex w-[240px] min-w-0" title={text(item, "error")}>
                                    <span className="truncate">{shortError(text(item, "error"))}</span>
                                  </StatusDot>
                                ) : item.detail?.status === "success" ? (
                                  <StatusDot tone="ok">成功</StatusDot>
                                ) : (
                                  <span className="text-ink-3">—</span>
                                )}
                              </td>
                              <td className="px-2.5 py-3 text-right font-mono text-xs whitespace-nowrap text-ink-2">{formatDuration(item)}</td>
                              <td className="px-2.5 py-3" onClick={(event) => event.stopPropagation()}>
                                {urls.length ? (
                                  <div className="flex items-center gap-1">
                                    {urls.slice(0, 3).map((url, index) => (
                                      <button
                                        key={`${url}-${index}`}
                                        type="button"
                                        className="size-8 overflow-hidden rounded-[6px] border border-line bg-sunken"
                                        onClick={() => setLightbox({ item, index })}
                                        title="预览图片"
                                      >
                                        <ImageThumbnail src={url} thumbnailSrc={getImageThumbnailUrl(url)} className="h-full w-full" />
                                      </button>
                                    ))}
                                    {urls.length > 3 ? <span className="text-xs text-ink-3">+{urls.length - 3}</span> : null}
                                  </div>
                                ) : (
                                  <span className="text-ink-3">—</span>
                                )}
                              </td>
                            </>
                          ) : (
                            <>
                              <td className="px-2.5 py-3 whitespace-nowrap text-ink">{item.summary || "—"}</td>
                              <td className="max-w-[360px] truncate px-2.5 py-3 font-mono text-xs text-ink-3">
                                {detailEntries(item).map(([label, value]) => `${label} ${value}`).join(" · ") || "—"}
                              </td>
                            </>
                          )}
                          <td className="py-3 pr-3 text-ink-3">
                            <ChevronDown className={cn("size-4 transition-transform", !expanded && "-rotate-90")} />
                          </td>
                        </tr>
                        {expanded ? (
                          <tr className="border-b border-line bg-page">
                            <td colSpan={columnCount} className="px-4 py-3">
                              <LogDetail item={item} onOpenImage={(index) => setLightbox({ item, index })} onDelete={() => setDeletingItems([item])} />
                            </td>
                          </tr>
                        ) : null}
                      </Fragment>
                    );
                  })}
                </tbody>
              </table>
            </div>
            {pageCount > 1 ? (
              <div className="flex items-center justify-end gap-2 border-t border-line px-4 py-2.5 text-xs text-ink-3">
                <span>
                  第 {safePage} / {pageCount} 页
                </span>
                <Button variant="outline" size="icon" className="size-8" aria-label="上一页" disabled={safePage <= 1} onClick={() => setPage(safePage - 1)}>
                  <ChevronLeft className="size-4" />
                </Button>
                <Button variant="outline" size="icon" className="size-8" aria-label="下一页" disabled={safePage >= pageCount} onClick={() => setPage(safePage + 1)}>
                  <ChevronRight className="size-4" />
                </Button>
              </div>
            ) : null}
          </>
        )}
      </div>

      <ImageLightbox
        images={getUrls(lightbox?.item ?? null).map((url, index) => ({ id: `${index}`, src: url }))}
        currentIndex={lightbox?.index ?? 0}
        open={lightbox !== null}
        onOpenChange={(open) => (!open ? setLightbox(null) : null)}
        onIndexChange={(index) => setLightbox((current) => (current ? { ...current, index } : current))}
      />
      <ConfirmDialog
        open={deletingItems.length > 0}
        onOpenChange={(open) => (!open ? setDeletingItems([]) : null)}
        title={deletingItems.length === 1 ? "删除这条日志？" : `删除 ${deletingItems.length} 条日志？`}
        description="日志删除后无法恢复。已生成的图片和账号不受影响。"
        confirmLabel="删除"
        danger
        busy={isDeleting}
        onConfirm={() => void confirmDelete()}
      />
    </section>
  );
}

export default function LogsPage() {
  const { isCheckingAuth, session } = useAuthGuard(["admin"]);
  if (isCheckingAuth || !session || session.role !== "admin") {
    return <SkeletonLines lines={6} className="pt-6" />;
  }
  return <LogsContent />;
}
