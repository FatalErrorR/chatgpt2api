"use client";

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { ChevronLeft, ChevronRight, Copy, Download, HardDrive, LoaderCircle, Plus, RefreshCw, Trash2, X } from "lucide-react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/confirm-dialog";
import { DateRangeFilter } from "@/components/date-range-filter";
import { PageHeader } from "@/components/page-header";
import { ImageLightbox } from "@/components/image-lightbox";
import { batchBarClass, batchBarDangerClass } from "@/components/ui/batch-bar";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { compressAllImages, deleteImageTag, deleteManagedImages, deleteToTarget, downloadImages, downloadSingleImage, fetchImageStorage, fetchImageTags, fetchManagedImagePaths, fetchManagedImages, setImageTags, type ImageStorageStats, type ManagedImage } from "@/lib/api";
import { EmptyState, ErrorState, SkeletonLines } from "@/components/ui/states";
import { useAuthGuard } from "@/lib/use-auth-guard";
import { cn } from "@/lib/utils";

const LONG_PRESS_MS = 800;
const PAGE_SIZE = 12;
// 叠在图片上的小按钮：浅底深字，深浅色模式下都看得清
const OVERLAY_BUTTON = "inline-flex size-7 items-center justify-center rounded-[6px] bg-[rgba(252,251,248,.92)] text-[#1F1E1B] shadow-sm transition hover:bg-white";
const TAG_CLASS = "inline-flex h-[22px] items-center gap-1 rounded-[6px] px-2 text-xs";

function formatSize(size: number) {
  return size > 1024 * 1024 ? `${(size / 1024 / 1024).toFixed(2)} MB` : `${Math.ceil(size / 1024)} KB`;
}

function formatMb(mb: number) {
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`;
}

function imageKey(item: ManagedImage) {
  return item.rel || item.url;
}

function ImageManagerContent() {
  const [items, setItems] = useState<ManagedImage[]>([]); // 当前页
  const [total, setTotal] = useState(0);
  const [pageCount, setPageCount] = useState(1);
  const loadSeqRef = useRef(0);
  const [startDate, setStartDate] = useState("");
  const [endDate, setEndDate] = useState("");
  const [lightboxIndex, setLightboxIndex] = useState(0);
  const [lightboxOpen, setLightboxOpen] = useState(false);
  const [page, setPage] = useState(1);
  const [isLoading, setIsLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [deleteStartDate, setDeleteStartDate] = useState("");
  const [deleteTarget, setDeleteTarget] = useState<ManagedImage | null>(null);
  const [isDeleting, setIsDeleting] = useState(false);
  const [allTags, setAllTags] = useState<string[]>([]);
  const [storage, setStorage] = useState<ImageStorageStats | null>(null);
  const [storageLoading, setStorageLoading] = useState(false);
  const [targetFreeMb, setTargetFreeMb] = useState(500);
  const [cleanupOpen, setCleanupOpen] = useState(false);
  const [cleanupBusy, setCleanupBusy] = useState(false);
  const [cleanupPreview, setCleanupPreview] = useState<{ removed: number; freed_mb: number } | null>(null);

  const loadStorage = useCallback(async () => {
    try {
      setStorageLoading(true);
      const data = await fetchImageStorage();
      setStorage(data);
    } catch { /* ignore */ }
    finally { setStorageLoading(false); }
  }, []);

  useEffect(() => { void loadStorage(); }, [loadStorage]);
  const [selectedTags, setSelectedTags] = useState<string[]>([]);
  const [tagEditTarget, setTagEditTarget] = useState<ManagedImage | null>(null);
  const [tagInput, setTagInput] = useState("");
  const [dialogVisible, setDialogVisible] = useState(false);
  const [selectedPaths, setSelectedPaths] = useState<string[]>([]);
  const [deleteMode, setDeleteMode] = useState<"selected" | "filtered" | "byDate" | null>(null);
  const [isDownloading, setIsDownloading] = useState(false);

  const lightboxImages = items.map((item) => ({
    id: item.name,
    src: item.url,
    sizeLabel: formatSize(item.size),
    dimensions: item.width && item.height ? `${item.width} x ${item.height}` : undefined,
  }));
  const safePage = Math.min(page, pageCount);
  const currentRows = items;
  const selectedSet = useMemo(() => new Set(selectedPaths), [selectedPaths]);
  const selectedCount = deleteMode === "filtered" ? total : deleteMode === "byDate" ? 0 : selectedPaths.length;
  const currentPageSelected = currentRows.length > 0 && currentRows.every((item) => selectedSet.has(imageKey(item)));
  const allSelected = total > 0 && selectedPaths.length >= total;

  /** 取当前筛选条件下的这一页；选中状态跨页保留。refresh 让后端立即和磁盘对账。 */
  const loadImages = async (options: { refresh?: boolean } = {}) => {
    const seq = ++loadSeqRef.current;
    setIsLoading(true);
    try {
      const [data, tagsData] = await Promise.all([
        fetchManagedImages({
          start_date: startDate,
          end_date: endDate,
          tags: selectedTags,
          page,
          page_size: PAGE_SIZE,
          refresh: options.refresh,
        }),
        fetchImageTags(),
      ]);
      if (seq !== loadSeqRef.current) return; // 已经有更新的请求，丢弃过期结果
      setItems(data.items);
      setTotal(data.total);
      setPageCount(data.page_count);
      if (data.page !== page) setPage(data.page);
      setAllTags(tagsData.tags);
      setLoadError("");
    } catch (error) {
      if (seq === loadSeqRef.current) setLoadError(error instanceof Error ? error.message : "加载图片失败");
    } finally {
      if (seq === loadSeqRef.current) setIsLoading(false);
    }
  };

  const filterPaths = () => fetchManagedImagePaths({ start_date: startDate, end_date: endDate, tags: selectedTags });

  const toggleAllResults = async (checked: boolean) => {
    if (!checked) {
      setSelectedPaths([]);
      return;
    }
    try {
      const data = await filterPaths();
      setSelectedPaths(data.paths);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "获取图片列表失败");
    }
  };

  const closeDialog = useCallback(() => {
    setDialogVisible(false);
    setTimeout(() => setDeleteTarget(null), 200);
  }, []);

  const openDeleteDialog = useCallback((item: ManagedImage) => {
    setDeleteTarget(item);
    setDialogVisible(true);
  }, []);

  const handleDelete = async () => {
    if (!deleteTarget) return;
    setIsDeleting(true);
    try {
      await deleteManagedImages({ paths: [deleteTarget.rel] });
      setSelectedPaths((prev) => prev.filter((p) => p !== imageKey(deleteTarget)));
      toast.success("图片已删除");
      void loadImages();
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "删除失败");
    } finally {
      setIsDeleting(false);
      closeDialog();
    }
  };

  const handleSetTags = async (item: ManagedImage, tags: string[]) => {
    try {
      const result = await setImageTags(item.rel, tags);
      setItems((prev) => prev.map((i) => i.rel === item.rel ? { ...i, tags: result.tags } : i));
      const tagsData = await fetchImageTags();
      setAllTags(tagsData.tags);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "设置标签失败");
    }
  };

  const handleAddTag = (item: ManagedImage) => {
    const tag = tagInput.trim();
    if (!tag) return;
    const current = item.tags ?? [];
    if (current.includes(tag)) {
      toast.error("标签已存在");
      return;
    }
    void handleSetTags(item, [...current, tag]);
    setTagInput("");
  };

  const handleRemoveTag = (item: ManagedImage, tag: string) => {
    void handleSetTags(item, (item.tags ?? []).filter((t) => t !== tag));
  };

  const toggleFilterTag = (tag: string) => {
    setSelectedTags((prev) => prev.includes(tag) ? prev.filter((t) => t !== tag) : [...prev, tag]);
    setPage(1);
  };

  const [pressingTag, setPressingTag] = useState<string | null>(null);
  const pressTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const [tagDeleteTarget, setTagDeleteTarget] = useState<string | null>(null);

  const handleDeleteTag = async (tag: string) => {
    try {
      const result = await deleteImageTag(tag);
      setAllTags((prev) => prev.filter((t) => t !== tag));
      setSelectedTags((prev) => prev.filter((t) => t !== tag));
      setItems((prev) => prev.map((item) => ({
        ...item,
        tags: (item.tags ?? []).filter((t) => t !== tag),
      })));
      toast.success(`标签"${tag}"已删除，影响 ${result.removed_from} 张图片`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "删除标签失败");
    }
  };

  const startTagPress = useCallback((tag: string) => {
    setPressingTag(tag);
    pressTimerRef.current = setTimeout(() => {
      setPressingTag(null);
      setTagDeleteTarget(tag);
    }, LONG_PRESS_MS);
  }, []);

  const stopTagPress = useCallback(() => {
    setPressingTag(null);
    if (pressTimerRef.current) {
      clearTimeout(pressTimerRef.current);
      pressTimerRef.current = null;
    }
  }, []);

  const clearFilters = () => {
    setStartDate("");
    setEndDate("");
    setSelectedTags([]);
    setPage(1);
  };

  const togglePaths = (paths: string[], checked: boolean) => {
    setSelectedPaths((current) => checked ? Array.from(new Set([...current, ...paths])) : current.filter((path) => !paths.includes(path)));
  };

  const confirmDelete = async () => {
    if (!deleteMode || selectedCount === 0) return;
    setIsDeleting(true);
    try {
      const data = await deleteManagedImages(deleteMode === "filtered" ? { start_date: startDate, end_date: endDate, all_matching: true } : { paths: selectedPaths });
      toast.success(`已删除 ${data.removed} 张图片`);
      setDeleteMode(null);
      setSelectedPaths([]);
      await loadImages();
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "删除图片失败");
    } finally {
      setIsDeleting(false);
    }
  };

  const handleBatchDownload = async () => {
    setIsDownloading(true);
    try {
      const paths = deleteMode === "filtered" ? (await filterPaths()).paths : selectedPaths;
      if (paths.length === 0) return;
      await downloadImages(paths);
      toast.success(`已下载 ${paths.length} 张图片`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "下载失败");
    } finally {
      setIsDownloading(false);
    }
  };

  const handleSingleDownload = async (item: ManagedImage) => {
    await downloadSingleImage(item.rel);
  };

  useEffect(() => {
    void loadImages();
  }, [startDate, endDate, selectedTags, page]);

  return (
    <section className="space-y-5">
      <PageHeader
        title="图片"
        description="生成的图片按时间倒序排列，过期的图片会被定时清理。"
        actions={
          <>
            <Popover>
              <PopoverTrigger asChild>
                <Button variant="outline">
                  <HardDrive className="size-4" />
                  存储
                </Button>
              </PopoverTrigger>
              <PopoverContent align="end" className="w-56 p-1">
                <button
                  type="button"
                  className="flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-left text-[13px] text-ink-2 hover:bg-surface-hover hover:text-ink"
                  onClick={async () => {
                    try {
                      const r = await compressAllImages();
                      toast.success(`已压缩，节省 ${r.saved_mb} MB`);
                      void loadStorage();
                    } catch {
                      toast.error("压缩失败");
                    }
                  }}
                >
                  压缩优化
                </button>
                <button
                  type="button"
                  className="flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-left text-[13px] text-bad hover:bg-bad-soft"
                  onClick={() => setDeleteMode("byDate")}
                >
                  按日期删除…
                </button>
                <button
                  type="button"
                  className="flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-left text-[13px] text-bad hover:bg-bad-soft"
                  onClick={() => {
                    setCleanupPreview(null);
                    setCleanupOpen(true);
                  }}
                >
                  清理到剩余空间…
                </button>
              </PopoverContent>
            </Popover>
            <Button variant="outline" onClick={() => { void loadStorage(); void loadImages({ refresh: true }); }} disabled={isLoading}>
              <RefreshCw className={`size-4 ${isLoading ? "animate-spin" : ""}`} />
              刷新
            </Button>
          </>
        }
      />

      {storage ? (
        <div className="flex flex-wrap items-center gap-7 rounded-[10px] border border-line bg-surface px-[18px] py-3.5">
          <div>
            <div className="text-xs text-ink-3">图片</div>
            <div className="font-serif text-xl text-ink">{storage.image_count.toLocaleString("en-US")}</div>
          </div>
          <div>
            <div className="text-xs text-ink-3">占用</div>
            <div className="font-serif text-xl text-ink">{formatMb(storage.image_size_mb)}</div>
          </div>
          <div className="min-w-[200px] flex-1">
            <div className="text-xs text-ink-3">
              磁盘 {formatMb(storage.disk_used_mb)} / {formatMb(storage.disk_total_mb)} · 剩余{" "}
              <span className={storage.disk_free_mb < 500 ? "text-bad" : undefined}>{formatMb(storage.disk_free_mb)}</span>
            </div>
            <div className="mt-1.5 h-1.5 overflow-hidden rounded-full bg-sunken">
              <i className="block h-full bg-ink" style={{ width: `${Math.min(100, (storage.disk_used_mb / Math.max(1, storage.disk_total_mb)) * 100)}%` }} />
            </div>
          </div>
        </div>
      ) : (
        <div className="rounded-[10px] border border-line bg-surface px-[18px] py-3.5 text-[13px] text-ink-3">
          {storageLoading ? "正在读取存储信息…" : "存储信息加载失败"}
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <DateRangeFilter startDate={startDate} endDate={endDate} onChange={(start, end) => { setStartDate(start); setEndDate(end); setPage(1); }} />
        {startDate || endDate || selectedTags.length > 0 ? (
          <Button variant="ghost" size="sm" onClick={clearFilters}>
            清除筛选
          </Button>
        ) : null}
        {startDate || endDate ? (
          <Button variant="outline" size="sm" className="text-bad hover:text-bad" onClick={() => setDeleteMode("filtered")} disabled={isDeleting || total === 0}>
            <Trash2 className="size-3.5" />
            删除这个日期范围的 {total} 张
          </Button>
        ) : null}
        <span className="flex-1" />
        <label className="flex items-center gap-2 text-xs text-ink-2">
          <Checkbox checked={currentPageSelected} disabled={currentRows.length === 0} onCheckedChange={(checked) => togglePaths(currentRows.map(imageKey), Boolean(checked))} />
          本页全选
        </label>
        <span className="text-xs text-ink-3">
          共 {total.toLocaleString("en-US")} 张 · 第 {safePage} / {pageCount} 页
        </span>
      </div>

      {allTags.length > 0 ? (
        <div className="flex flex-wrap items-center gap-1.5" title="点击按标签筛选，长按删除标签">
          {allTags.map((tag) => {
            const active = selectedTags.includes(tag);
            return (
              <button
                key={tag}
                type="button"
                className={cn(
                  TAG_CLASS,
                  "relative overflow-hidden transition",
                  active ? "bg-clay-soft text-clay-hover" : "bg-sunken text-ink-2 hover:text-ink",
                  pressingTag === tag && "ring-2 ring-bad/40",
                )}
                onClick={() => toggleFilterTag(tag)}
                onMouseDown={() => startTagPress(tag)}
                onMouseUp={stopTagPress}
                onMouseLeave={stopTagPress}
                onTouchStart={() => startTagPress(tag)}
                onTouchEnd={stopTagPress}
              >
                {tag}
                {active ? <X className="size-3" /> : null}
                {pressingTag === tag ? <span className="pointer-events-none absolute inset-0 origin-left animate-[grow_800ms_linear_forwards] bg-bad/15" /> : null}
              </button>
            );
          })}
          {selectedTags.length > 0 ? (
            <button type="button" className={cn(TAG_CLASS, "text-ink-3 hover:text-ink")} onClick={() => { setSelectedTags([]); setPage(1); }}>
              清除标签
            </button>
          ) : null}
        </div>
      ) : null}

      {selectedPaths.length > 0 ? (
        <div className={batchBarClass}>
          <span>已选 {selectedPaths.length.toLocaleString("en-US")} 张</span>
          {!allSelected ? (
            <button type="button" className="rounded-[6px] px-2 py-1 text-page/75 hover:text-page" onClick={() => void toggleAllResults(true)}>
              全选 {total.toLocaleString("en-US")} 张
            </button>
          ) : null}
          <span className="flex-1" />
          <button type="button" className="rounded-[6px] px-2.5 py-1 text-page/75 hover:text-page" onClick={() => setSelectedPaths([])} disabled={isDeleting}>
            取消选择
          </button>
          <button
            type="button"
            className="inline-flex items-center gap-1.5 rounded-[6px] px-2.5 py-1 hover:bg-page/10 disabled:opacity-50"
            onClick={() => void handleBatchDownload()}
            disabled={isDownloading || isDeleting}
          >
            {isDownloading ? <LoaderCircle className="size-3.5 animate-spin" /> : <Download className="size-3.5" />}
            下载
          </button>
          <button
            type="button"
            className={batchBarDangerClass}
            onClick={() => setDeleteMode("selected")}
            disabled={isDeleting}
          >
            <Trash2 className="size-3.5" />
            删除
          </button>
        </div>
      ) : null}

      {loadError && items.length === 0 ? (
        <ErrorState message={`加载图片失败：${loadError}`} onRetry={() => void loadImages({ refresh: true })} />
      ) : isLoading && items.length === 0 ? (
        <div className="grid grid-cols-2 gap-3.5 sm:grid-cols-3 lg:grid-cols-4">
          {Array.from({ length: 8 }, (_, index) => (
            <div key={index} className="overflow-hidden rounded-[10px] border border-line bg-surface">
              <div className="aspect-[4/3] animate-pulse bg-sunken" />
              <SkeletonLines lines={2} className="px-3 py-2" />
            </div>
          ))}
        </div>
      ) : total === 0 ? (
        <div className="rounded-[10px] border border-line bg-surface">
          <EmptyState
            title={startDate || endDate || selectedTags.length > 0 ? "没有匹配的图片" : "还没有图片"}
            description={startDate || endDate || selectedTags.length > 0 ? "换个日期范围或标签，或清除筛选。" : "生成的图片会保存在这里。"}
          />
        </div>
      ) : (
        <>
          {loadError ? <div className="rounded-[7px] bg-bad-soft px-3 py-2 text-[13px] text-bad">刷新失败：{loadError}（下面是上次的结果）</div> : null}
          <div className={cn("grid grid-cols-2 gap-3.5 transition-opacity sm:grid-cols-3 lg:grid-cols-4", isLoading && "opacity-60")}>
            {currentRows.map((item, imageIndex) => {
              const selected = selectedSet.has(imageKey(item));
              return (
                <div
                  key={item.rel}
                  className={cn(
                    "group overflow-hidden rounded-[10px] border bg-surface transition",
                    selected ? "border-clay shadow-[inset_0_0_0_1px_var(--clay)]" : "border-line",
                  )}
                >
                  <div className="relative">
                    <button
                      type="button"
                      className="block aspect-[4/3] w-full cursor-zoom-in overflow-hidden bg-sunken"
                      onClick={() => {
                        setLightboxIndex(imageIndex);
                        setLightboxOpen(true);
                      }}
                      title="查看大图"
                    >
                      <img
                        src={item.thumbnail_url || item.url}
                        alt={item.name}
                        loading="lazy"
                        className="h-full w-full object-cover transition group-hover:scale-[1.02]"
                        onError={(event) => {
                          if (event.currentTarget.src !== item.url) event.currentTarget.src = item.url;
                        }}
                      />
                    </button>
                    <div className="absolute top-2 left-2">
                      <Checkbox
                        aria-label="选择图片"
                        className={cn("bg-surface/90 transition", selected ? "opacity-100" : "opacity-100 sm:opacity-0 sm:group-hover:opacity-100")}
                        checked={selected}
                        onCheckedChange={(checked) => togglePaths([imageKey(item)], Boolean(checked))}
                      />
                    </div>
                    <div className="absolute right-2 bottom-2 flex gap-1 opacity-100 transition sm:opacity-0 sm:group-hover:opacity-100">
                      <button type="button" className={OVERLAY_BUTTON} title="下载图片" onClick={() => void handleSingleDownload(item)}>
                        <Download className="size-3.5" />
                      </button>
                      <button
                        type="button"
                        className={OVERLAY_BUTTON}
                        title="复制图片地址"
                        onClick={() => {
                          void navigator.clipboard.writeText(item.url);
                          toast.success("图片地址已复制");
                        }}
                      >
                        <Copy className="size-3.5" />
                      </button>
                      <button type="button" className={cn(OVERLAY_BUTTON, "hover:bg-bad hover:text-white")} title="删除图片" onClick={() => openDeleteDialog(item)}>
                        <Trash2 className="size-3.5" />
                      </button>
                    </div>
                  </div>
                  <div className="grid gap-1.5 px-3 py-2.5 text-xs text-ink-3">
                    <div className="truncate text-[13px] font-medium text-ink" title={item.created_at}>{String(item.created_at).slice(5, 16)}</div>
                    <div className="truncate">
                      {[item.width && item.height ? `${item.width} × ${item.height}` : "", formatSize(item.size)].filter(Boolean).join(" · ")}
                    </div>
                    <div className="flex flex-wrap items-center gap-1">
                      {(item.tags ?? []).map((tag) => (
                        <span key={tag} className={cn(TAG_CLASS, "h-5 bg-sunken pr-1 text-ink-2")}>
                          {tag}
                          <button type="button" className="rounded-[4px] text-ink-3 hover:text-ink" title={`移除标签 ${tag}`} onClick={() => handleRemoveTag(item, tag)}>
                            <X className="size-3" />
                          </button>
                        </span>
                      ))}
                      <Popover open={tagEditTarget?.rel === item.rel} onOpenChange={(open) => { setTagEditTarget(open ? item : null); setTagInput(""); }}>
                        <PopoverTrigger asChild>
                          <button
                            type="button"
                            className="inline-flex h-5 items-center gap-0.5 rounded-[6px] border border-dashed border-line-strong px-1.5 text-[11px] text-ink-3 hover:border-ink-3 hover:text-ink"
                            title="添加标签"
                          >
                            <Plus className="size-3" />
                            标签
                          </button>
                        </PopoverTrigger>
                        <PopoverContent align="start" className="w-60 p-3">
                          <div className="grid gap-2">
                            <div className="text-xs text-ink-3">添加标签</div>
                            <div className="flex gap-1.5">
                              <Input
                                value={tagInput}
                                onChange={(event) => setTagInput(event.target.value)}
                                placeholder="输入标签名"
                                autoFocus
                                onKeyDown={(event) => {
                                  if (event.key === "Enter") {
                                    event.preventDefault();
                                    handleAddTag(item);
                                  }
                                }}
                              />
                              <Button size="icon" variant="outline" className="size-[34px] shrink-0" onClick={() => handleAddTag(item)} aria-label="添加">
                                <Plus className="size-3.5" />
                              </Button>
                            </div>
                            {allTags.filter((tag) => !(item.tags ?? []).includes(tag)).length > 0 ? (
                              <div className="flex flex-wrap gap-1 border-t border-line pt-2">
                                {allTags.filter((tag) => !(item.tags ?? []).includes(tag)).map((tag) => (
                                  <button
                                    key={tag}
                                    type="button"
                                    className={cn(TAG_CLASS, "bg-sunken text-ink-2 hover:text-ink")}
                                    onClick={() => {
                                      void handleSetTags(item, [...(item.tags ?? []), tag]);
                                      setTagEditTarget(null);
                                    }}
                                  >
                                    {tag}
                                  </button>
                                ))}
                              </div>
                            ) : null}
                          </div>
                        </PopoverContent>
                      </Popover>
                    </div>
                  </div>
                </div>
              );
            })}
          </div>
          {pageCount > 1 ? (
            <div className="flex items-center justify-end gap-2 text-xs text-ink-3">
              <span>
                第 {safePage} / {pageCount} 页
              </span>
              <Button variant="outline" size="icon" className="size-8" aria-label="上一页" disabled={safePage <= 1} onClick={() => setPage((value) => Math.max(1, value - 1))}>
                <ChevronLeft className="size-4" />
              </Button>
              <Button variant="outline" size="icon" className="size-8" aria-label="下一页" disabled={safePage >= pageCount} onClick={() => setPage((value) => Math.min(pageCount, value + 1))}>
                <ChevronRight className="size-4" />
              </Button>
            </div>
          ) : null}
        </>
      )}

      <Dialog open={cleanupOpen} onOpenChange={(open) => (!cleanupBusy ? setCleanupOpen(open) : null)}>
        <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-[480px]">
          <DialogHeader className="gap-1.5">
            <DialogTitle className="font-serif text-[22px] font-medium">清理到剩余空间</DialogTitle>
            <p className="text-[13px] leading-6 text-ink-2">从最旧的图片开始删除，直到磁盘剩余空间达到目标。删除前会先预览影响范围，删除后无法恢复。</p>
          </DialogHeader>
          <div className="flex items-center gap-2 text-[13px] text-ink-2">
            目标剩余
            <Input
              className="h-[34px] w-28 rounded-[7px] text-right"
              type="number"
              min={50}
              value={targetFreeMb}
              onChange={(e) => {
                setTargetFreeMb(Number(e.target.value) || 500);
                setCleanupPreview(null);
              }}
            />
            MB
            {storage ? <span className="text-xs text-ink-3">（当前剩余 {formatMb(storage.disk_free_mb)}）</span> : null}
          </div>
          {cleanupPreview ? (
            <div className={`rounded-[7px] px-3 py-2.5 text-[13px] ${cleanupPreview.removed > 0 ? "bg-bad-soft text-bad" : "bg-sunken text-ink-2"}`}>
              {cleanupPreview.removed > 0
                ? `将删除最旧的 ${cleanupPreview.removed.toLocaleString("en-US")} 张图片，释放约 ${cleanupPreview.freed_mb} MB。`
                : "剩余空间已经达到目标，不会删除任何图片。"}
            </div>
          ) : null}
          <DialogFooter className="pt-2">
            <Button variant="ghost" onClick={() => setCleanupOpen(false)} disabled={cleanupBusy}>取消</Button>
            {!cleanupPreview ? (
              <Button
                disabled={cleanupBusy}
                onClick={async () => {
                  setCleanupBusy(true);
                  try {
                    const r = await deleteToTarget(targetFreeMb, true);
                    setCleanupPreview({ removed: r.removed, freed_mb: r.freed_mb ?? 0 });
                  } catch {
                    toast.error("预览失败");
                  } finally {
                    setCleanupBusy(false);
                  }
                }}
              >
                {cleanupBusy ? <LoaderCircle className="size-4 animate-spin" /> : null}
                预览
              </Button>
            ) : (
              <Button
                variant="destructive"
                disabled={cleanupBusy || cleanupPreview.removed === 0}
                onClick={async () => {
                  setCleanupBusy(true);
                  try {
                    const r = await deleteToTarget(targetFreeMb);
                    toast.success(`已删除 ${r.removed} 张图片，释放 ${r.freed_mb ?? 0} MB`);
                    setCleanupOpen(false);
                    void loadStorage();
                    void loadImages({ refresh: true });
                  } catch {
                    toast.error("清理失败");
                  } finally {
                    setCleanupBusy(false);
                  }
                }}
              >
                {cleanupBusy ? <LoaderCircle className="size-4 animate-spin" /> : null}
                删除 {cleanupPreview.removed.toLocaleString("en-US")} 张
              </Button>
            )}
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={deleteMode === "byDate"} onOpenChange={(open) => (!open && !isDeleting ? setDeleteMode(null) : null)}>
        <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-[480px]">
          <DialogHeader className="gap-1.5">
            <DialogTitle className="font-serif text-[22px] font-medium">按日期删除图片</DialogTitle>
            <DialogDescription className="text-[13px] leading-6 text-ink-2">
              删除所选日期之前的全部图片和缩略图，无法恢复。
            </DialogDescription>
          </DialogHeader>
          <label className="flex items-center gap-2 text-[13px] text-ink-2">
            删除
            <Input className="w-44" type="date" value={deleteStartDate} onChange={(event) => setDeleteStartDate(event.target.value)} />
            之前的图片
          </label>
          <DialogFooter className="pt-2">
            <Button variant="ghost" onClick={() => setDeleteMode(null)} disabled={isDeleting}>取消</Button>
            <Button
              variant="destructive"
              disabled={!deleteStartDate || isDeleting}
              onClick={async () => {
                if (!deleteStartDate) return;
                try {
                  setIsDeleting(true);
                  const r = await deleteManagedImages({ end_date: deleteStartDate, all_matching: true });
                  toast.success(`已删除 ${r.removed} 张图片`);
                  setDeleteMode(null);
                  void loadStorage();
                  void loadImages();
                } catch {
                  toast.error("删除失败");
                } finally {
                  setIsDeleting(false);
                }
              }}
            >
              {isDeleting ? <LoaderCircle className="size-4 animate-spin" /> : null}
              删除
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <ConfirmDialog
        open={dialogVisible}
        onOpenChange={(open) => { if (!open) closeDialog(); }}
        title="删除这张图片？"
        description="图片和缩略图会从磁盘删除，无法恢复。"
        confirmLabel="删除"
        danger
        busy={isDeleting}
        onConfirm={() => void handleDelete()}
      >
        {deleteTarget ? (
          <div className="flex items-center gap-3 overflow-hidden rounded-[7px] border border-line bg-sunken p-2.5">
            <img
              src={deleteTarget.thumbnail_url || deleteTarget.url}
              alt=""
              className="size-14 shrink-0 rounded-[6px] object-cover"
              onError={(event) => { if (event.currentTarget.src !== deleteTarget.url) event.currentTarget.src = deleteTarget.url; }}
            />
            <div className="min-w-0 text-xs text-ink-3">
              <div className="truncate text-[13px] text-ink">{deleteTarget.name}</div>
              <div className="truncate">{deleteTarget.created_at} · {formatSize(deleteTarget.size)}</div>
            </div>
          </div>
        ) : null}
      </ConfirmDialog>

      <ImageLightbox
        images={lightboxImages}
        currentIndex={lightboxIndex}
        open={lightboxOpen}
        onOpenChange={setLightboxOpen}
        onIndexChange={setLightboxIndex}
      />
      <ConfirmDialog
        open={deleteMode === "selected" || deleteMode === "filtered"}
        onOpenChange={(open) => (!open ? setDeleteMode(null) : null)}
        title={`删除 ${selectedCount.toLocaleString("en-US")} 张图片？`}
        description={
          deleteMode === "filtered"
            ? `删除 ${startDate || "最早"} 至 ${endDate || startDate || "今天"} 的全部图片和缩略图，无法恢复。`
            : "所选图片和缩略图会从磁盘删除，无法恢复。"
        }
        confirmLabel="删除"
        danger
        busy={isDeleting}
        onConfirm={() => void confirmDelete()}
      />
      <ConfirmDialog
        open={Boolean(tagDeleteTarget)}
        onOpenChange={(open) => { if (!open) setTagDeleteTarget(null); }}
        title={`删除标签「${tagDeleteTarget ?? ""}」？`}
        description="标签会从所有图片上移除，图片本身不受影响。"
        confirmLabel="删除标签"
        danger
        onConfirm={() => {
          if (tagDeleteTarget) void handleDeleteTag(tagDeleteTarget);
          setTagDeleteTarget(null);
        }}
      />
    </section>
  );
}

export default function ImageManagerPage() {
  const { isCheckingAuth, session } = useAuthGuard(["admin"]);
  if (isCheckingAuth || !session || session.role !== "admin") {
    return <SkeletonLines lines={6} className="pt-6" />;
  }
  return <ImageManagerContent />;
}
