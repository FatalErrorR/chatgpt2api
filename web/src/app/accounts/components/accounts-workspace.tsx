"use client";

import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { Download, Image as ImageIcon, LoaderCircle, LogIn, RefreshCw, Search, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/confirm-dialog";
import { RowMenu } from "@/components/row-menu";
import { batchBarClass, batchBarDangerClass } from "@/components/ui/batch-bar";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import {
  availableQuota,
  displayAccountType,
  downloadTokens,
  facetCounts,
  formatCompact,
  matchesStatusFilter,
  type StatusFilter,
} from "@/lib/account-format";
import {
  deleteAccounts,
  fetchAccounts,
  fetchImageTestProgress,
  fetchProxies,
  fetchReLoginProgress,
  fetchRefreshProgress,
  reLoginAccounts,
  refreshAccounts,
  testAccountImages,
  type Account,
  type AccountRefreshResponse,
  type ProxyEntry,
  type RefreshProgressResponse,
} from "@/lib/api";
import { accountMatchesProxyFilter } from "@/lib/proxy-match";
import { cn } from "@/lib/utils";

import { AccountEditDialog } from "./account-edit-dialog";
import { AccountFilterSelects, AccountFilterSidebar, type AccountFilters } from "./account-filters";
import { AccountImportDialog } from "./account-import-dialog";
import { AccountTable } from "./account-table";
import { ProxyManagerDialog } from "./proxy-manager-dialog";

export type AccountSummary = {
  total: number;
  active: number;
  limited: number;
  abnormal: number;
  disabled: number;
  /** 正常账号的可用额度合计（批量刷新时实时更新） */
  quota: string;
  inflight: number;
};

/** 给工作区上下方插槽用的数据和操作：总览的指标、提醒都从这里取，并能直接筛选账号表。 */
export type AccountsWorkspaceContext = {
  accounts: Account[];
  proxies: ProxyEntry[];
  accountsError: string;
  proxiesError: string;
  isLoading: boolean;
  summary: AccountSummary;
  filters: AccountFilters;
  /** 设置筛选（其余筛选和搜索清空），并滚到账号表 */
  applyFilter: (patch: Partial<AccountFilters>) => void;
  reload: () => void;
};

const EMPTY_FILTERS: AccountFilters = { status: "all", type: "all", proxy: "all" };
const HIDDEN_PROGRESS = { visible: false, current: 0, total: 0, message: "", email: "" };

/** 号池：头部操作、筛选栏、账号表、批量操作。title 下方和账号表上下可以插入别的内容（总览用）。 */
export function AccountsWorkspace({
  title,
  description,
  top,
  bottom,
  onReload,
}: {
  title: string;
  description?: ReactNode;
  top?: (context: AccountsWorkspaceContext) => ReactNode;
  bottom?: (context: AccountsWorkspaceContext) => ReactNode;
  /** 点刷新按钮时一并刷新页面上别的数据 */
  onReload?: () => void;
}) {
  const didLoadRef = useRef(false);
  const tableRef = useRef<HTMLDivElement>(null);
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [proxies, setProxies] = useState<ProxyEntry[]>([]);
  const [accountsError, setAccountsError] = useState("");
  const [proxiesError, setProxiesError] = useState("");
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [query, setQuery] = useState("");
  const [filters, setFilters] = useState<AccountFilters>(EMPTY_FILTERS);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState("10");
  const [editingAccount, setEditingAccount] = useState<Account | null>(null);
  const [isLoading, setIsLoading] = useState(true);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [refreshingTokens, setRefreshingTokens] = useState<Set<string>>(new Set());
  const [isDeleting, setIsDeleting] = useState(false);
  const [isRelogining, setIsRelogining] = useState(false);
  const [isTestingImages, setIsTestingImages] = useState(false);
  const [progress, setProgress] = useState(HIDDEN_PROGRESS);
  const [refreshSummary, setRefreshSummary] = useState<Partial<AccountSummary> | null>(null);
  // 删除前确认：写清删几个、影响什么
  const [pendingDelete, setPendingDelete] = useState<{ tokens: string[]; title: string } | null>(null);

  const keepSelected = (items: Account[]) =>
    setSelectedIds((prev) => prev.filter((id) => items.some((item) => item.access_token === id)));

  const applyAccounts = (items: Account[]) => {
    setAccounts(items);
    setAccountsError("");
    keepSelected(items);
  };

  const loadProxies = useCallback(async () => {
    try {
      const data = await fetchProxies();
      setProxies(data.items);
      setProxiesError("");
    } catch (error) {
      setProxiesError(error instanceof Error ? error.message : "加载代理失败");
    }
  }, []);

  const loadAccounts = useCallback(async () => {
    setIsLoading(true);
    try {
      const data = await fetchAccounts();
      setAccounts(data.items);
      setAccountsError("");
      setSelectedIds((prev) => prev.filter((id) => data.items.some((item) => item.access_token === id)));
    } catch (error) {
      setAccountsError(error instanceof Error ? error.message : "加载账号失败");
    } finally {
      setIsLoading(false);
    }
  }, []);

  const reload = useCallback(() => {
    void loadAccounts();
    void loadProxies();
    onReload?.();
  }, [loadAccounts, loadProxies, onReload]);

  useEffect(() => {
    if (didLoadRef.current) return;
    didLoadRef.current = true;
    void loadAccounts();
    void loadProxies();
  }, [loadAccounts, loadProxies]);

  const abnormalProxyKeys = useMemo(() => new Set(proxies.filter((proxy) => proxy.health.abnormal).map((proxy) => proxy.key)), [proxies]);
  const proxyByKey = useMemo(() => new Map(proxies.map((proxy) => [proxy.key, proxy])), [proxies]);
  const counts = useMemo(() => {
    const base = facetCounts(accounts);
    const abnormalProxy = accounts.filter((account) => accountMatchesProxyFilter(account, "abnormal", abnormalProxyKeys)).length;
    return { ...base, abnormalProxy };
  }, [accounts, abnormalProxyKeys]);

  const filteredAccounts = useMemo(() => {
    const normalizedQuery = query.trim().toLowerCase();
    return accounts.filter(
      (account) =>
        (normalizedQuery.length === 0 || (account.email ?? "").toLowerCase().includes(normalizedQuery)) &&
        (filters.type === "all" || displayAccountType(account) === filters.type) &&
        matchesStatusFilter(account, filters.status) &&
        accountMatchesProxyFilter(account, filters.proxy, abnormalProxyKeys),
    );
  }, [accounts, query, filters, abnormalProxyKeys]);

  const pageCount = Math.max(1, Math.ceil(filteredAccounts.length / Number(pageSize)));
  const safePage = Math.min(page, pageCount);
  const startIndex = (safePage - 1) * Number(pageSize);
  const currentRows = filteredAccounts.slice(startIndex, startIndex + Number(pageSize));

  const summary: AccountSummary = useMemo(
    () => ({
      total: accounts.length,
      active: counts.status["正常"] ?? 0,
      limited: counts.status["限流"] ?? 0,
      abnormal: counts.status["异常"] ?? 0,
      disabled: counts.status["禁用"] ?? 0,
      quota: formatCompact(availableQuota(accounts)),
      inflight: counts.inflight,
      ...refreshSummary,
    }),
    [accounts, counts, refreshSummary],
  );

  const selectedTokens = useMemo(() => {
    const selectedSet = new Set(selectedIds);
    return accounts.filter((item) => selectedSet.has(item.access_token)).map((item) => item.access_token);
  }, [accounts, selectedIds]);
  const abnormalTokens = useMemo(() => accounts.filter((item) => item.status === "异常").map((item) => item.access_token), [accounts]);

  const changeFilters = (patch: Partial<AccountFilters>) => {
    setFilters((prev) => ({ ...prev, ...patch }));
    setPage(1);
  };

  const applyFilter = useCallback((patch: Partial<AccountFilters>) => {
    setFilters({ ...EMPTY_FILTERS, ...patch });
    setQuery("");
    setPage(1);
    tableRef.current?.scrollIntoView({ behavior: "smooth", block: "start" });
  }, []);

  const hideProgressSoon = () => setTimeout(() => setProgress(HIDDEN_PROGRESS), 800);

  const pollUntilDone = <T,>(poll: () => Promise<T & { done?: boolean; error?: string | null }>, onTick: (p: T) => void, intervalMs: number) =>
    new Promise<T>((resolve, reject) => {
      const timer = setInterval(async () => {
        try {
          const p = await poll();
          if (p.done) {
            clearInterval(timer);
            if (p.error) reject(new Error(p.error));
            else resolve(p);
          } else {
            onTick(p);
          }
        } catch (error) {
          clearInterval(timer);
          reject(error);
        }
      }, intervalMs);
    });

  const handleDeleteTokens = async (tokens: string[]) => {
    setIsDeleting(true);
    try {
      const data = await deleteAccounts(tokens);
      applyAccounts(data.items);
      toast.success(`删除 ${data.removed ?? 0} 个账户`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "删除账户失败");
    } finally {
      setIsDeleting(false);
    }
  };

  const handleRefreshAccounts = async (accessTokens: string[]) => {
    if (accessTokens.length === 0) {
      toast.error("没有需要刷新的账户");
      return;
    }

    if (accessTokens.length === 1) {
      const [token] = accessTokens;
      setRefreshingTokens((prev) => new Set([...prev, token]));
      try {
        const { progress_id } = await refreshAccounts(accessTokens);
        const done = await pollUntilDone<RefreshProgressResponse>(() => fetchRefreshProgress(progress_id), () => undefined, 500);
        if (done.result) applyAccounts(done.result.items);
      } catch (error) {
        toast.error(error instanceof Error ? error.message : "刷新账户失败");
      } finally {
        setRefreshingTokens((prev) => {
          const next = new Set(prev);
          next.delete(token);
          return next;
        });
      }
      return;
    }

    setIsRefreshing(true);
    // 没被刷新的账号作为基数，刷新中实时叠加进度里的结果（统计联动用）
    const refreshing = new Set(accessTokens);
    const rest = accounts.filter((account) => !refreshing.has(account.access_token));
    const base = facetCounts(rest).status;
    const baseQuota = availableQuota(rest);
    const total = accessTokens.length;
    setProgress({ visible: true, current: 0, total, message: "正在刷新账号信息...", email: "" });

    try {
      const { progress_id } = await refreshAccounts(accessTokens);
      const done = await pollUntilDone<RefreshProgressResponse>(
        () => fetchRefreshProgress(progress_id),
        (p) => {
          setProgress((prev) => ({ ...prev, current: p.processed }));
          setRefreshSummary({
            active: (base["正常"] ?? 0) + (p.status_counts?.["正常"] ?? 0),
            limited: (base["限流"] ?? 0) + (p.status_counts?.["限流"] ?? 0),
            abnormal: (base["异常"] ?? 0) + (p.status_counts?.["异常"] ?? 0),
            disabled: (base["禁用"] ?? 0) + (p.status_counts?.["禁用"] ?? 0),
            quota: formatCompact(baseQuota + (p.total_quota ?? 0)),
          });
        },
        300,
      );
      setRefreshSummary(null);
      if (!done.result) throw new Error("刷新结果为空");
      const data: AccountRefreshResponse = done.result;
      applyAccounts(data.items);
      setProgress({ visible: true, current: total, total, message: "刷新完成", email: "" });
      hideProgressSoon();

      const relogined = data.relogined ?? 0;
      if ((data.errors ?? []).length > 0) {
        const firstError = data.errors?.[0]?.error;
        toast.error(`刷新成功 ${data.refreshed} 个，失败 ${(data.errors ?? []).length} 个${firstError ? `，首个错误：${firstError}` : ""}`);
      } else {
        toast.success(`刷新成功 ${data.refreshed} 个账户${relogined > 0 ? `，已触发 ${relogined} 个账号重新登录` : ""}`);
      }
    } catch (error) {
      setProgress(HIDDEN_PROGRESS);
      setRefreshSummary(null);
      toast.error(error instanceof Error ? error.message : "刷新账户失败");
    } finally {
      setIsRefreshing(false);
    }
  };

  const handleTestImages = async (accessTokens: string[]) => {
    if (accessTokens.length === 0) {
      toast.error("请先选择要测试的账户");
      return;
    }
    setIsTestingImages(true);
    const total = accessTokens.length;
    setProgress({ visible: true, current: 0, total, message: "正在测试生图...", email: "" });
    const showTick = (p: RefreshProgressResponse) => {
      const last = [...(p.results ?? [])].reverse().find((item) => item.error);
      setProgress((prev) => ({ ...prev, current: p.processed, email: last?.error || `已处理 ${p.processed}/${p.total}` }));
    };
    try {
      const { progress_id } = await testAccountImages(accessTokens);
      const done = await pollUntilDone<RefreshProgressResponse>(() => fetchImageTestProgress(progress_id), showTick, 500);
      const results = done.results ?? [];
      const failed = results.filter((item) => item.status !== "成功");
      setProgress({ visible: true, current: total, total, message: "测试完成", email: "" });
      hideProgressSoon();
      if (failed.length === 0) toast.success(`测试完成：${results.length} 个成功`);
      else toast.error(`测试完成：成功 ${results.length - failed.length}，失败 ${failed.length}`);
    } catch (error) {
      setProgress(HIDDEN_PROGRESS);
      toast.error(error instanceof Error ? error.message : "测试生图失败");
    } finally {
      setIsTestingImages(false);
    }
  };

  const handleReLogin = async (accessTokens: string[]) => {
    if (accessTokens.length === 0) {
      toast.error("请先选择要恢复的账户");
      return;
    }
    // 只处理异常账号
    const targets = accessTokens.filter((token) => accounts.find((account) => account.access_token === token)?.status === "异常");
    if (targets.length === 0) {
      toast.error("选中账号中没有异常账号");
      return;
    }
    if (targets.length < accessTokens.length) {
      toast.info(`已过滤 ${accessTokens.length - targets.length} 个非异常账号`);
    }

    setIsRelogining(true);
    const targetSet = new Set(targets);
    const base = facetCounts(accounts.filter((account) => !targetSet.has(account.access_token))).status;
    const total = targets.length;
    setProgress({ visible: true, current: 0, total, message: "正在尝试恢复异常账号...", email: "" });

    try {
      const { progress_id } = await reLoginAccounts(targets);
      await pollUntilDone<RefreshProgressResponse>(
        () => fetchReLoginProgress(progress_id),
        (p) => {
          const results = p.results ?? [];
          const lastError = [...results].reverse().find((r) => r.error);
          setProgress((prev) => ({
            ...prev,
            current: p.processed,
            email: lastError ? `失败: ${lastError.token} ${lastError.error ?? ""}` : `已处理 ${p.processed}/${p.total}`,
          }));
          // 实时统计：基数 + 已处理的恢复结果（异常、跳过保持异常）
          let active = base["正常"] ?? 0;
          let disabled = base["禁用"] ?? 0;
          let abnormal = (base["异常"] ?? 0) + total;
          for (const r of results) {
            if (r.status === "成功") {
              active += 1;
              abnormal -= 1;
            } else if (r.status === "禁用") {
              disabled += 1;
              abnormal -= 1;
            }
          }
          setRefreshSummary({ active, abnormal, disabled, limited: base["限流"] ?? 0 });
        },
        300,
      );
      setRefreshSummary(null);
      // 等后台线程写完再拉最新数据
      await new Promise<void>((resolve) => setTimeout(resolve, 500));
      try {
        applyAccounts((await fetchAccounts()).items);
      } catch {
        /* 静默失败，列表保持旧数据 */
      }
      setProgress({ visible: true, current: total, total, message: "恢复完成", email: "" });
      hideProgressSoon();
      toast.success("恢复流程已全部完成");
    } catch (error) {
      setProgress(HIDDEN_PROGRESS);
      setRefreshSummary(null);
      toast.error(error instanceof Error ? error.message : "重新登录失败");
    } finally {
      setIsRelogining(false);
    }
  };

  const confirmDelete = (tokens: string[], deleteTitle: string) => {
    if (tokens.length === 0) {
      toast.error("请先选择要删除的账户");
      return;
    }
    setPendingDelete({ tokens, title: deleteTitle });
  };

  const context: AccountsWorkspaceContext = {
    accounts,
    proxies,
    accountsError,
    proxiesError,
    isLoading,
    summary,
    filters,
    applyFilter,
    reload,
  };
  const pendingAccounts = pendingDelete ? accounts.filter((item) => pendingDelete.tokens.includes(item.access_token)) : [];
  const busy = isLoading || isRefreshing || isDeleting;

  return (
    <section className="space-y-5">
      <div className="flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
        <div className="min-w-0">
          <h1 className="text-[30px] font-medium text-ink">{title}</h1>
          {description ? <div className="mt-1 text-sm text-ink-2">{description}</div> : null}
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="ghost" size="icon" onClick={reload} disabled={busy} title="重新加载">
            <RefreshCw className={cn("size-4", isLoading ? "animate-spin" : "")} />
          </Button>
          <ProxyManagerDialog proxies={proxies} onChange={setProxies} />
          <Button variant="outline" onClick={() => void handleRefreshAccounts(accounts.map((item) => item.access_token))} disabled={busy || accounts.length === 0}>
            <RefreshCw className={cn("size-4", isRefreshing ? "animate-spin" : "")} />
            刷新全部额度
          </Button>
          <AccountImportDialog
            disabled={busy}
            proxies={proxies}
            onImported={(items) => {
              applyAccounts(items);
              void loadProxies();
              setSelectedIds([]);
              setPage(1);
            }}
          />
          <RowMenu
            label="更多"
            items={[
              { label: "导出全部 Token", icon: Download, onSelect: () => downloadTokens(accounts), disabled: accounts.length === 0 },
              {
                label: `移除全部异常账号（${abnormalTokens.length}）`,
                icon: Trash2,
                danger: true,
                disabled: abnormalTokens.length === 0 || isDeleting,
                onSelect: () => confirmDelete(abnormalTokens, `移除 ${abnormalTokens.length} 个异常账号？`),
              },
            ]}
          />
        </div>
      </div>

      {top?.(context)}

      {progress.visible ? (
        <div className="rounded-[10px] border border-line bg-surface px-4 py-3">
          <div className="flex items-center justify-between text-[13px]">
            <span className="text-ink-2">
              {progress.message}
              {progress.email ? <span className="ml-1 text-ink">{progress.email}</span> : null}
            </span>
            <span className="font-mono text-xs text-ink tabular-nums">
              {progress.current}/{progress.total}
            </span>
          </div>
          <div className="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-sunken">
            <div
              className="h-full rounded-full bg-ink transition-all duration-300 ease-out"
              style={{ width: `${progress.total > 0 ? (progress.current / progress.total) * 100 : 0}%` }}
            />
          </div>
        </div>
      ) : null}

      <div ref={tableRef} className="grid scroll-mt-4 gap-[22px] lg:grid-cols-[210px_minmax(0,1fr)]">
        <AccountFilterSidebar filters={filters} counts={counts} total={accounts.length} proxies={proxies} onChange={changeFilters} />

        <div className="min-w-0 space-y-3">
          <div className="flex flex-wrap items-center gap-2">
            <div className="relative min-w-[200px] flex-1">
              <Search className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-ink-3" />
              <Input
                value={query}
                onChange={(event) => {
                  setQuery(event.target.value);
                  setPage(1);
                }}
                placeholder="搜索邮箱"
                className="pl-9"
              />
            </div>
            <AccountFilterSelects filters={filters} typeOptions={Array.from(counts.types.keys())} proxies={proxies} onChange={changeFilters} />
            <Select
              value={pageSize}
              onValueChange={(value) => {
                setPageSize(value);
                setPage(1);
              }}
            >
              <SelectTrigger className="w-[96px] text-xs text-ink-2"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="10">每页 10</SelectItem>
                <SelectItem value="20">每页 20</SelectItem>
                <SelectItem value="50">每页 50</SelectItem>
                <SelectItem value="100">每页 100</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {selectedIds.length > 0 ? (
            <div className={batchBarClass}>
              <span>已选 {selectedIds.length} 个账号</span>
              <span className="flex-1" />
              {(
                [
                  ["刷新额度", RefreshCw, isRefreshing, () => void handleRefreshAccounts(selectedTokens)],
                  ["测试生图", ImageIcon, isTestingImages, () => void handleTestImages(selectedTokens)],
                  ["尝试恢复", LogIn, isRelogining, () => void handleReLogin(selectedTokens)],
                  ["导出", Download, false, () => downloadTokens(accounts.filter((item) => selectedTokens.includes(item.access_token)))],
                ] as const
              ).map(([label, Icon, working, action]) => (
                <button
                  key={label}
                  type="button"
                  onClick={action}
                  disabled={working}
                  className="inline-flex h-7 items-center gap-1.5 rounded-md border border-page/20 px-2.5 text-xs transition hover:bg-page/10 disabled:opacity-50"
                >
                  {working ? <LoaderCircle className="size-3.5 animate-spin" /> : <Icon className="size-3.5" />}
                  {label}
                </button>
              ))}
              <button
                type="button"
                onClick={() => confirmDelete(selectedTokens, `删除 ${selectedTokens.length} 个账号？`)}
                disabled={isDeleting}
                className={cn(batchBarDangerClass, "h-7 text-xs")}
              >
                <Trash2 className="size-3.5" />
                删除
              </button>
              <button type="button" onClick={() => setSelectedIds([])} className="h-7 rounded-md px-2.5 text-xs transition hover:bg-page/10">
                取消
              </button>
            </div>
          ) : null}

          {accountsError ? (
            <div className="flex flex-wrap items-center gap-3 rounded-[7px] bg-bad-soft px-3 py-2 text-[13px] text-bad">
              <span className="min-w-0 flex-1">
                加载账号失败：{accountsError}
                {accounts.length > 0 ? "（下面是上次的结果）" : ""}
              </span>
              <Button variant="ghost" size="sm" className="h-7 text-bad hover:bg-bad/10 hover:text-bad" onClick={() => void loadAccounts()}>
                <RefreshCw className="size-3.5" />
                重试
              </Button>
            </div>
          ) : null}

          <AccountTable
            rows={currentRows}
            isLoading={isLoading}
            totalAccounts={accounts.length}
            filteredCount={filteredAccounts.length}
            proxyByKey={proxyByKey}
            selectedIds={selectedIds}
            onSelectedIdsChange={setSelectedIds}
            page={safePage}
            pageCount={pageCount}
            onPageChange={setPage}
            actions={{
              onEdit: setEditingAccount,
              onRefresh: (token) => void handleRefreshAccounts([token]),
              onDelete: (token) => confirmDelete([token], "删除这个账号？"),
              isRowBusy: (token) => isRefreshing || refreshingTokens.has(token),
              disabled: isDeleting,
            }}
          />
        </div>
      </div>

      {bottom?.(context)}

      <AccountEditDialog
        account={editingAccount}
        proxies={proxies}
        onClose={() => setEditingAccount(null)}
        onSaved={(items) => {
          applyAccounts(items);
          setEditingAccount(null);
          void loadProxies();
        }}
        onDelete={(token) => {
          setEditingAccount(null);
          confirmDelete([token], "删除这个账号？");
        }}
      />

      <ConfirmDialog
        open={Boolean(pendingDelete)}
        onOpenChange={(open) => (!open ? setPendingDelete(null) : null)}
        title={pendingDelete?.title ?? ""}
        description="账号和它们的 token 会从号池移除，无法恢复。已生成的图片不受影响。"
        confirmLabel="删除"
        danger
        busy={isDeleting}
        onConfirm={() => {
          if (!pendingDelete) return;
          void handleDeleteTokens(pendingDelete.tokens).finally(() => setPendingDelete(null));
        }}
      >
        {pendingAccounts.length > 0 ? (
          <div className="max-h-40 overflow-y-auto rounded-[7px] bg-sunken px-3 py-2 font-mono text-xs leading-6 text-ink-2">
            {pendingAccounts.slice(0, 20).map((item) => (
              <div key={item.access_token} className="truncate">{item.email || item.access_token.slice(0, 16)}</div>
            ))}
            {pendingAccounts.length > 20 ? <div>…另外 {pendingAccounts.length - 20} 个</div> : null}
          </div>
        ) : null}
      </ConfirmDialog>
    </section>
  );
}
