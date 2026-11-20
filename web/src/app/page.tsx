"use client";

import Link from "next/link";
import { useCallback, useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { ChevronDown } from "lucide-react";

import { SkeletonLines } from "@/components/ui/states";
import { Button } from "@/components/ui/button";
import {
  fetchImageOutputCounts,
  fetchImageStorage,
  fetchSystemLogs,
  type ImageCountPeriod,
  type ImageOutputCounts,
  type ImageStorageStats,
  type SystemLog,
} from "@/lib/api";
import { getValidatedAuthSession } from "@/lib/auth-session";
import { shortError } from "@/lib/log-detail";
import { buildTodos, formatSize, settle, type Loaded, type OverviewSources, type Todo } from "@/lib/overview";
import { ABNORMAL_PROXY_FILTER } from "@/lib/proxy-match";
import { cn } from "@/lib/utils";
import { getDefaultRouteForRole } from "@/store/auth";

import { AccountsWorkspace, type AccountsWorkspaceContext } from "./accounts/components/accounts-workspace";

// 今日生图：左栏短期，右栏长期；每栏按本栏最大值画条，免得「全部」把今日压成一条线
const PERIOD_COLUMNS: Array<Array<{ key: ImageCountPeriod; label: string }>> = [
  [
    { key: "today", label: "今日" },
    { key: "yesterday", label: "昨日" },
    { key: "d7", label: "近 7 天" },
  ],
  [
    { key: "d15", label: "近 15 天" },
    { key: "d30", label: "近 30 天" },
    { key: "all", label: "全部" },
  ],
];

// 「今日生图」「今天失败的调用」两块各自是否收起，按浏览器记住
const GENERATION_PANEL_KEY = "chatgpt2api-overview-generation-collapsed";
const FAILED_PANEL_KEY = "chatgpt2api-overview-failed-collapsed";
const FAILED_ROWS = 10;

function formatNumber(value: number) {
  return value.toLocaleString("en-US");
}

function today() {
  const now = new Date();
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
}

type ExtraSources = Pick<OverviewSources, "counts" | "storage" | "logs">;
const PENDING = { ok: false as const, error: "" };

/** 一个指标：可以点（筛选账号表或跳页面）。加载失败时不显示数值，免得 0 /「全部正常」被当真。 */
function Kpi({
  className: extraClassName,
  label,
  value,
  note,
  tone,
  error,
  loading,
  onClick,
  href,
  hint,
}: {
  label: string;
  value: React.ReactNode;
  note: React.ReactNode;
  tone?: "bad" | "good";
  error?: string;
  loading?: boolean;
  onClick?: () => void;
  href?: string;
  hint?: string;
  className?: string;
}) {
  const body = (
    <>
      <div className="text-xs text-ink-3">{label}</div>
      <div className={cn("mt-0.5 font-serif text-[26px] leading-tight", error || loading ? "text-ink-3" : "text-ink")}>
        {loading ? "…" : error ? "—" : value}
      </div>
      <div
        className={cn("truncate text-xs", error || tone === "bad" ? "text-bad" : tone === "good" ? "text-ok" : "text-ink-2")}
        title={error || undefined}
      >
        {loading ? " " : error ? "加载失败" : note}
      </div>
    </>
  );
  const className = cn("block min-w-0 rounded-[10px] border border-line bg-surface px-4 py-3 text-left", extraClassName);
  if (href) {
    return (
      <Link href={href} title={hint} className={cn(className, "transition hover:bg-surface-hover")}>
        {body}
      </Link>
    );
  }
  if (onClick) {
    return (
      <button type="button" title={hint} onClick={onClick} className={cn(className, "transition hover:bg-surface-hover")}>
        {body}
      </button>
    );
  }
  return <div className={className}>{body}</div>;
}

function useExtraSources() {
  const [sources, setSources] = useState<ExtraSources>({ counts: PENDING, storage: PENDING, logs: PENDING });
  const [loaded, setLoaded] = useState(false);
  const load = useCallback(async () => {
    const [counts, storage, logs] = await Promise.allSettled([
      fetchImageOutputCounts(),
      fetchImageStorage(),
      fetchSystemLogs({ type: "call", start_date: today() }).then((result) => result.items),
    ]);
    setSources({ counts: settle(counts), storage: settle(storage), logs: settle(logs) });
    setLoaded(true);
  }, []);
  useEffect(() => {
    void load();
  }, [load]);
  return { sources, loaded, reload: load };
}

function OverviewTop({ ctx, extra, extraLoaded, reloadExtra }: { ctx: AccountsWorkspaceContext; extra: ExtraSources; extraLoaded: boolean; reloadExtra: () => void }) {
  const accountsLoading = ctx.isLoading && ctx.accounts.length === 0 && !ctx.accountsError;
  const accountsSource: Loaded<typeof ctx.accounts> = ctx.accountsError ? { ok: false, error: ctx.accountsError } : { ok: true, value: ctx.accounts };
  const proxiesSource: Loaded<typeof ctx.proxies> = ctx.proxiesError ? { ok: false, error: ctx.proxiesError } : { ok: true, value: ctx.proxies };
  const { summary } = ctx;
  const badProxies = ctx.proxies.filter((proxy) => proxy.health.abnormal).length;
  const counts = extra.counts.ok ? extra.counts.value : null;
  const storage: ImageStorageStats | null = extra.storage.ok ? extra.storage.value : null;
  const todayTotal = counts ? counts.generations.today + counts.edits.today : 0;
  const yesterdayTotal = counts ? counts.generations.yesterday + counts.edits.yesterday : 0;
  const todos =
    accountsLoading || !extraLoaded
      ? []
      : buildTodos({ accounts: accountsSource, proxies: proxiesSource, ...extra });

  const runTodo = (todo: Todo) => {
    if (todo.filter) ctx.applyFilter(todo.filter);
    else if (todo.retry === "accounts" || todo.retry === "proxies") ctx.reload();
    else if (todo.retry) reloadExtra();
  };

  return (
    <>
      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-5">
        <Kpi
          label="可用账号"
          loading={accountsLoading}
          error={ctx.accountsError || undefined}
          value={
            <>
              {summary.active} <span className="font-sans text-sm text-ink-3">/ {summary.total}</span>
            </>
          }
          note={summary.abnormal > 0 ? `${summary.abnormal} 个异常` : "全部正常"}
          tone={summary.abnormal > 0 ? "bad" : "good"}
          onClick={() => ctx.applyFilter(summary.abnormal > 0 ? { status: "异常" } : { status: "正常" })}
          hint={summary.abnormal > 0 ? "筛选异常账号" : "筛选正常账号"}
        />
        <Kpi
          label="可用额度"
          loading={accountsLoading}
          error={ctx.accountsError || undefined}
          value={summary.quota}
          note={`在途 ${summary.inflight} 张`}
          hint="正常账号剩余额度合计；在途是正在生成、尚未结束的图片数"
        />
        <Kpi
          label="今日生图"
          loading={!extraLoaded}
          error={extra.counts.ok ? undefined : extra.counts.error || undefined}
          value={formatNumber(todayTotal)}
          note={`昨日 ${formatNumber(yesterdayTotal)}`}
        />
        <Kpi
          label="代理"
          loading={ctx.isLoading && ctx.proxies.length === 0 && !ctx.proxiesError}
          error={ctx.proxiesError || undefined}
          value={ctx.proxies.length}
          note={badProxies > 0 ? `${badProxies} 个异常` : "全部正常"}
          tone={badProxies > 0 ? "bad" : "good"}
          onClick={badProxies > 0 ? () => ctx.applyFilter({ proxy: ABNORMAL_PROXY_FILTER }) : undefined}
          hint={badProxies > 0 ? "筛选异常代理上的账号" : undefined}
        />
        <Kpi
          label="图片存储"
          loading={!extraLoaded}
          error={extra.storage.ok ? undefined : extra.storage.error || undefined}
          value={storage ? formatSize(storage.image_size_mb) : "—"}
          note={storage ? `${formatNumber(storage.image_count)} 张 · 剩余 ${formatSize(storage.disk_free_mb)}` : ""}
          tone={storage && storage.disk_free_mb < 2048 ? "bad" : undefined}
          href="/image-manager"
          hint="打开图片管理"
          className="col-span-2 sm:col-span-1"
        />
      </div>

      <GenerationPanel extra={extra} extraLoaded={extraLoaded} />

      {todos.length > 0 ? (
        <div className="flex flex-wrap gap-2" role="list" aria-label="需要处理">
          {todos.map((todo) => {
            const content = (
              <>
                <i className={cn("size-[7px] flex-none rounded-full", todo.tone === "bad" ? "bg-bad" : "bg-warn")} />
                <span className="text-ink">{todo.title}</span>
                <span className="hidden max-w-[260px] truncate text-ink-3 sm:inline" title={todo.note}>
                  {todo.note}
                </span>
                <span className="text-clay-hover">{todo.action}</span>
              </>
            );
            const className =
              "inline-flex min-w-0 max-w-full items-center gap-2 rounded-[7px] border border-line bg-surface px-3 py-1.5 text-[13px] transition hover:bg-surface-hover";
            return todo.href ? (
              <Link key={todo.title} href={todo.href} className={className} role="listitem">
                {content}
              </Link>
            ) : (
              <button key={todo.title} type="button" className={className} onClick={() => runTodo(todo)} role="listitem">
                {content}
              </button>
            );
          })}
        </div>
      ) : null}
    </>
  );
}

/** 可折叠的面板，收起状态按浏览器记住。 */
function CollapsiblePanel({
  storageKey,
  title,
  action,
  children,
}: {
  storageKey: string;
  title: string;
  action?: React.ReactNode;
  children: React.ReactNode;
}) {
  const [collapsed, setCollapsed] = useState(false);
  useEffect(() => {
    try {
      setCollapsed(localStorage.getItem(storageKey) === "1");
    } catch {
      /* 读不到就默认展开 */
    }
  }, [storageKey]);
  const toggle = () => {
    setCollapsed((value) => {
      try {
        localStorage.setItem(storageKey, value ? "0" : "1");
      } catch {
        /* 记不住也没关系 */
      }
      return !value;
    });
  };
  return (
    <section className="rounded-[10px] border border-line bg-surface">
      <div className="flex items-center gap-2 pr-3">
        <button type="button" className="flex min-w-0 flex-1 items-center gap-2 px-[18px] py-3 text-left" onClick={toggle} aria-expanded={!collapsed}>
          <ChevronDown className={cn("size-4 shrink-0 text-ink-3 transition-transform", collapsed && "-rotate-90")} />
          <h2 className="text-[17px] font-medium text-ink">{title}</h2>
        </button>
        {action}
      </div>
      {collapsed ? null : <div className="border-t border-line px-[18px] py-4">{children}</div>}
    </section>
  );
}

/** 今日生图：今日 / 昨日 / 近 7 天 / 近 30 天，文生图和图生图分色。 */
function GenerationPanel({ extra, extraLoaded }: { extra: ExtraSources; extraLoaded: boolean }) {
  const counts = extra.counts.ok ? extra.counts.value : null;
  const total = (key: ImageCountPeriod) => (counts ? (counts.generations[key] ?? 0) + (counts.edits[key] ?? 0) : 0);
  return (
    <CollapsiblePanel
      storageKey={GENERATION_PANEL_KEY}
      title="今日生图"
      action={
        <div className="flex gap-3.5 text-xs text-ink-3">
          <span className="inline-flex items-center gap-1.5"><i className="size-2 rounded-[2px] bg-ink" />文生图</span>
          <span className="inline-flex items-center gap-1.5"><i className="size-2 rounded-[2px] bg-clay" />图生图</span>
        </div>
      }
    >
      {!extraLoaded ? (
        <SkeletonLines lines={3} />
      ) : !extra.counts.ok ? (
        <div className="text-[13px] text-bad">生图统计加载失败：{extra.counts.error}</div>
      ) : (
        <div className="grid gap-x-10 gap-y-3 lg:grid-cols-2">
          {PERIOD_COLUMNS.map((column, columnIndex) => {
            const columnMax = Math.max(1, ...column.map((period) => total(period.key)));
            return (
              <div key={columnIndex} className="grid content-start gap-3">
                {column.map((period) => {
                  const generations = counts?.generations[period.key] ?? 0;
                  const edits = counts?.edits[period.key] ?? 0;
                  return (
                    <div key={period.key} className="grid grid-cols-[56px_1fr_72px] items-center gap-3 text-xs text-ink-2">
                      <span>{period.label}</span>
                      <div
                        className="flex h-2.5 overflow-hidden rounded-full bg-sunken"
                        title={`文生图 ${formatNumber(generations)} · 图生图 ${formatNumber(edits)}`}
                      >
                        <i className="h-full bg-ink" style={{ width: `${(generations / columnMax) * 100}%` }} />
                        <i className="h-full bg-clay" style={{ width: `${(edits / columnMax) * 100}%` }} />
                      </div>
                      <span className="text-right font-mono tabular-nums">{formatNumber(generations + edits)}</span>
                    </div>
                  );
                })}
              </div>
            );
          })}
        </div>
      )}
    </CollapsiblePanel>
  );
}

/** 今天失败的调用：单独放在最下面，最近 FAILED_ROWS 条。 */
function FailedCallsPanel({ extra, extraLoaded }: { extra: ExtraSources; extraLoaded: boolean }) {
  const failed: SystemLog[] = extra.logs.ok
    ? extra.logs.value.filter((item) => (item.detail as { status?: string } | undefined)?.status === "failed")
    : [];
  return (
    <CollapsiblePanel
      storageKey={FAILED_PANEL_KEY}
      title={extra.logs.ok && extraLoaded ? `今天失败的调用（${failed.length}）` : "今天失败的调用"}
      action={
        <Button variant="ghost" size="sm" asChild>
          <Link href="/logs">全部日志 →</Link>
        </Button>
      }
    >
      {!extraLoaded ? (
        <SkeletonLines lines={3} />
      ) : !extra.logs.ok ? (
        <div className="text-[13px] text-bad">调用日志加载失败：{extra.logs.error}</div>
      ) : failed.length === 0 ? (
        <div className="text-[13px] text-ink-3">今天还没有失败的调用。</div>
      ) : (
        <div className="overflow-x-auto">
          <table className="w-full text-left text-[13px]">
            <tbody>
              {failed.slice(0, FAILED_ROWS).map((item) => {
                const detail = (item.detail ?? {}) as { endpoint?: string; error?: string; account_email?: string; model?: string };
                return (
                  <tr key={item.id} className="border-t border-line first:border-t-0">
                    <td className="py-2 pr-4 pl-3 font-mono text-xs whitespace-nowrap text-ink-2 shadow-[inset_2px_0_0_var(--bad)]">{String(item.time).slice(11)}</td>
                    <td className="py-2 pr-4 whitespace-nowrap text-ink">{detail.endpoint || "—"}</td>
                    <td className="hidden max-w-[240px] truncate py-2 pr-4 text-ink-3 md:table-cell" title={detail.account_email}>{detail.account_email || "—"}</td>
                    <td className="max-w-[420px] truncate py-2 text-ink-3" title={detail.error}>
                      {detail.error ? shortError(detail.error) : item.summary}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
          {failed.length > FAILED_ROWS ? (
            <div className="pt-2 text-xs text-ink-3">另外还有 {failed.length - FAILED_ROWS} 条，在日志里查看。</div>
          ) : null}
        </div>
      )}
    </CollapsiblePanel>
  );
}

function Overview() {
  const { sources: extra, loaded: extraLoaded, reload: reloadExtra } = useExtraSources();
  return (
    <AccountsWorkspace
      title="总览"
      description={new Date().toLocaleDateString("zh-CN", { month: "long", day: "numeric", weekday: "long" })}
      onReload={() => void reloadExtra()}
      top={(ctx) => <OverviewTop ctx={ctx} extra={extra} extraLoaded={extraLoaded} reloadExtra={() => void reloadExtra()} />}
      bottom={() => <FailedCallsPanel extra={extra} extraLoaded={extraLoaded} />}
    />
  );
}

export default function HomePage() {
  const router = useRouter();
  const [isAdmin, setIsAdmin] = useState(false);

  useEffect(() => {
    let active = true;
    const check = async () => {
      const session = await getValidatedAuthSession();
      if (!active) return;
      if (!session) {
        router.replace("/login");
      } else if (session.role !== "admin") {
        router.replace(getDefaultRouteForRole(session.role));
      } else {
        setIsAdmin(true);
      }
    };
    void check();
    return () => {
      active = false;
    };
  }, [router]);

  return isAdmin ? <Overview /> : <SkeletonLines lines={6} className="pt-6" />;
}
