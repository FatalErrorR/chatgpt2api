"use client";

import { Import, LoaderCircle, Pencil, Plus, Trash2 } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { EmptyState, ErrorState, SkeletonLines } from "@/components/ui/states";

import { useSettingsStore } from "../store";
import { SettingGroup } from "./setting-group";

export function CPAPoolsCard() {
  const pools = useSettingsStore((state) => state.pools);
  const isLoadingPools = useSettingsStore((state) => state.isLoadingPools);
  const poolsLoadError = useSettingsStore((state) => state.poolsLoadError);
  const loadPools = useSettingsStore((state) => state.loadPools);
  const deletingId = useSettingsStore((state) => state.deletingId);
  const loadingFilesId = useSettingsStore((state) => state.loadingFilesId);
  const openAddDialog = useSettingsStore((state) => state.openAddDialog);
  const openEditDialog = useSettingsStore((state) => state.openEditDialog);
  const deletePool = useSettingsStore((state) => state.deletePool);
  const browseFiles = useSettingsStore((state) => state.browseFiles);

  return (
    <SettingGroup title="CPA 连接" description="先配置连接，再按需查询远程账号并选择导入到本地号池。">
        <div className="flex items-center justify-end gap-2">
          {pools.length > 0 ? <Badge className="rounded-md px-2.5 py-1">{pools.length} 个连接</Badge> : null}
          <Button onClick={openAddDialog}>
            <Plus className="size-4" />
            添加连接
          </Button>
        </div>

        {isLoadingPools && pools.length === 0 ? (
          <SkeletonLines lines={4} />
        ) : poolsLoadError && pools.length === 0 ? (
          <ErrorState message={poolsLoadError} onRetry={() => void loadPools()} />
        ) : pools.length === 0 ? (
          <EmptyState title="还没有 CPA 连接" description="添加连接后，可以查询远程账号并导入本地号池。" action={<Button onClick={openAddDialog}>添加连接</Button>} />
        ) : (
          <div className="space-y-3">
            {pools.map((pool) => {
              const isBusy = deletingId === pool.id || loadingFilesId === pool.id;
              const importJob = pool.import_job ?? null;
              const progress = importJob?.total
                ? Math.round((importJob.completed / importJob.total) * 100)
                : 0;

              return (
                <div key={pool.id} className="flex flex-col gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3">
                  <div className="flex items-center justify-between gap-3">
                    <div className="min-w-0">
                      <div className="text-sm font-medium text-ink">{pool.name || pool.base_url}</div>
                      <div className="truncate text-xs text-ink-3">{pool.base_url}</div>
                    </div>
                    <div className="flex items-center gap-1">
                      <button
                        type="button"
                        className="rounded-lg p-2 text-ink-3 transition hover:bg-surface-hover hover:text-ink"
                        onClick={() => openEditDialog(pool)}
                        disabled={isBusy}
                        title="编辑"
                      >
                        <Pencil className="size-4" />
                      </button>
                      <button
                        type="button"
                        className="rounded-lg p-2 text-ink-3 transition hover:bg-bad-soft hover:text-bad"
                        onClick={() => void deletePool(pool)}
                        disabled={isBusy}
                        title="删除"
                      >
                        {deletingId === pool.id ? (
                          <LoaderCircle className="size-4 animate-spin" />
                        ) : (
                          <Trash2 className="size-4" />
                        )}
                      </button>
                    </div>
                  </div>

                  <div className="flex items-center gap-2">
                    <Button
                      variant="outline"
                      className="h-8 rounded-lg border-line bg-surface px-3 text-xs text-ink-2"
                      onClick={() => void browseFiles(pool)}
                      disabled={isBusy}
                    >
                      {loadingFilesId === pool.id ? (
                        <LoaderCircle className="size-3.5 animate-spin" />
                      ) : (
                        <Import className="size-3.5" />
                      )}
                      同步
                    </Button>
                  </div>

                  {importJob ? (
                    <div className="space-y-2 rounded-[7px] bg-sunken px-3 py-3">
                      <div className="text-xs font-medium tracking-[0.16em] text-ink-3 uppercase">导入任务</div>
                      <div className="rounded-lg border border-line bg-surface px-3 py-3">
                        <div className="flex items-center justify-between gap-3">
                          <div className="min-w-0">
                            <div className="text-sm font-medium text-ink-2">
                              状态 {importJob.status}，已处理 {importJob.completed}/{importJob.total}
                            </div>
                            <div className="truncate text-xs text-ink-3">
                              任务 {importJob.job_id.slice(0, 8)} · {importJob.created_at}
                            </div>
                          </div>
                          <Badge
                            variant={
                              importJob.status === "completed"
                                ? "success"
                                : importJob.status === "failed"
                                  ? "danger"
                                  : "info"
                            }
                            className="rounded-md"
                          >
                            {progress}%
                          </Badge>
                        </div>
                        <div className="mt-3 h-2 overflow-hidden rounded-full bg-surface-hover">
                          <div className="h-full rounded-full bg-ink transition-all" style={{ width: `${progress}%` }} />
                        </div>
                        <div className="mt-2 flex flex-wrap gap-2 text-xs text-ink-3">
                          <span>新增 {importJob.added}</span>
                          <span>跳过 {importJob.skipped}</span>
                          <span>刷新 {importJob.refreshed}</span>
                          <span>失败 {importJob.failed}</span>
                        </div>
                      </div>
                    </div>
                  ) : null}
                </div>
              );
            })}
          </div>
        )}

        <div className="rounded-[7px] bg-sunken px-4 py-3 text-[13px] leading-6 text-ink-3">
          点「同步」读取远程账号，确认后由后端下载 token 并导入本地号池。页面只轮询进度。
        </div>
    </SettingGroup>
  );
}
