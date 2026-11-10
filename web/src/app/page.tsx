"use client";

import Link from "next/link";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import { LoaderCircle, RefreshCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  fetchAccounts,
  fetchImageOutputCounts,
  fetchImageStorage,
  fetchProxies,
  fetchSystemLogs,
  type ImageOutputCounts,
} from "@/lib/api";
import { getValidatedAuthSession } from "@/lib/auth-session";
import { buildTodos, failedSources, formatSize, settle, type OverviewSources } from "@/lib/overview";
import { cn } from "@/lib/utils";
import { getDefaultRouteForRole } from "@/store/auth";

const PERIODS: Array<{ key: keyof ImageOutputCounts["generations"]; label: string }> = [
  { key: "today", label: "今日" },
  { key: "yesterday", label: "昨日" },
  { key: "d7", label: "近 7 天" },
  { key: "d30", label: "近 30 天" },
];

function formatNumber(value: number) {
  return value.toLocaleString("en-US");
}

function today() {
  const now = new Date();
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
}

function Kpi({
  label,
  value,
  note,
  tone,
  error,
}: {
  label: string;
  value: React.ReactNode;
  note: string;
  tone?: "bad" | "good";
  /** 这块数据加载失败：不显示数值，免得 0 / 「全部正常」被当成真的 */
  error?: string;
}) {
  return (
    <div className="rounded-[10px] border border-line bg-surface px-[18px] py-4">
      <div className="text-xs text-ink-3">{label}</div>
      <div className={cn("mt-1 mb-0.5 font-serif text-[30px] leading-tight", error ? "text-ink-3" : "text-ink")}>{error ? "—" : value}</div>
      <div
        className={cn("truncate text-xs", error || tone === "bad" ? "text-bad" : tone === "good" ? "text-ok" : "text-ink-2")}
        title={error || undefined}
      >
        {error ? "加载失败" : note}
      </div>
    </div>
  );
}

function LoadError({ label, error }: { label: string; error: string }) {
  return (
    <div className="flex items-center gap-3 py-2 text-sm text-bad" title={error}>
      <i className="size-[7px] flex-none rounded-full bg-bad" />
      <span className="min-w-0 truncate">{label}加载失败：{error}</span>
    </div>
  );
}

function Panel({ title, action, children, className }: { title: string; action?: React.ReactNode; children: React.ReactNode; className?: string }) {
  return (
    <section className={cn("rounded-[10px] border border-line bg-surface", className)}>
      <div className="flex items-center justify-between px-[18px] pt-3.5 pb-2.5">
        <h2 className="text-[17px] font-medium text-ink">{title}</h2>
        {action}
      </div>
      <div className="px-[18px] pb-4">{children}</div>
    </section>
  );
}

function Overview() {
  const [data, setData] = useState<OverviewSources | null>(null);
  const [reloading, setReloading] = useState(false);

  const load = useCallback(async () => {
    // 各块互不依赖：某个接口失败只影响对应的块，并且要显示成失败，不能当成 0 / 正常
    const [accounts, proxies, counts, storage, logs] = await Promise.allSettled([
      fetchAccounts().then((result) => result.items),
      fetchProxies().then((result) => result.items),
      fetchImageOutputCounts(),
      fetchImageStorage(),
      fetchSystemLogs({ type: "call", start_date: today() }).then((result) => result.items),
    ]);
    return {
      accounts: settle(accounts),
      proxies: settle(proxies),
      counts: settle(counts),
      storage: settle(storage),
      logs: settle(logs),
    };
  }, []);

  useEffect(() => {
    let active = true;
    void load().then((sources) => {
      if (active) setData(sources);
    });
    return () => {
      active = false;
    };
  }, [load]);

  const reload = async () => {
    setReloading(true);
    try {
      setData(await load());
    } finally {
      setReloading(false);
    }
  };

  const stats = useMemo(() => {
    const accounts = data?.accounts.ok ? data.accounts.value : [];
    const normal = accounts.filter((item) => item.status === "正常");
    return {
      total: accounts.length,
      normal: normal.length,
      abnormal: accounts.filter((item) => item.status === "异常").length,
      badProxies: data?.proxies.ok ? data.proxies.value.filter((proxy) => proxy.health.abnormal) : [],
    };
  }, [data]);

  if (!data) {
    return (
      <div className="grid min-h-[50vh] place-items-center">
        <LoaderCircle className="size-5 animate-spin text-ink-3" />
      </div>
    );
  }

  const counts = data.counts.ok ? data.counts.value : null;
  const storage = data.storage.ok ? data.storage.value : null;
  const total = (key: keyof ImageOutputCounts["generations"]) => (counts ? counts.generations[key] + counts.edits[key] : 0);
  const maxTotal = Math.max(1, ...PERIODS.map((period) => total(period.key)));
  const todos = buildTodos(data);
  const failed = failedSources(data);
  const failures = data.logs.ok
    ? data.logs.value.filter((item) => (item.detail as { status?: string } | undefined)?.status === "failed").slice(0, 5)
    : [];

  return (
    <section className="space-y-[18px]">
      <div className="flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 className="text-[30px] font-medium text-ink">概览</h1>
          <p className="mt-1 text-sm text-ink-2">
            {new Date().toLocaleDateString("zh-CN", { month: "long", day: "numeric", weekday: "long" })}
          </p>
        </div>
        <div className="flex gap-2">
          <Button variant="outline" asChild>
            <Link href="/accounts">号池</Link>
          </Button>
          <Button variant="clay" asChild>
            <Link href="/image">去生图</Link>
          </Button>
        </div>
      </div>

      {failed.length > 0 ? (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1.5 rounded-lg bg-bad-soft px-3 py-2 text-sm text-bad">
          <span className="min-w-0 flex-1">
            部分数据加载失败：{failed.map((item) => item.label).join("、")}。下面对应的数字不可信。
          </span>
          <Button variant="ghost" size="sm" className="h-7 text-bad hover:bg-bad/10 hover:text-bad" disabled={reloading} onClick={() => void reload()}>
            <RefreshCw className={cn("size-3.5", reloading && "animate-spin")} />
            重试
          </Button>
        </div>
      ) : null}

      <div className="grid grid-cols-2 gap-3.5 lg:grid-cols-4">
        <Kpi
          label="可用账号"
          value={
            <>
              {stats.normal} <span className="font-sans text-sm text-ink-3">/ {stats.total}</span>
            </>
          }
          note={stats.abnormal > 0 ? `${stats.abnormal} 个异常` : "全部正常"}
          tone={stats.abnormal > 0 ? "bad" : "good"}
          error={data.accounts.ok ? undefined : data.accounts.error}
        />
        <Kpi
          label="今日生图"
          value={formatNumber(total("today"))}
          note={`昨日 ${formatNumber(total("yesterday"))}`}
          error={data.counts.ok ? undefined : data.counts.error}
        />
        <Kpi
          label="代理"
          value={data.proxies.ok ? data.proxies.value.length : 0}
          note={stats.badProxies.length > 0 ? `${stats.badProxies.length} 个异常` : "全部正常"}
          tone={stats.badProxies.length > 0 ? "bad" : "good"}
          error={data.proxies.ok ? undefined : data.proxies.error}
        />
        <Kpi
          label="图片存储"
          value={storage ? formatSize(storage.image_size_mb) : "—"}
          note={storage ? `${formatNumber(storage.image_count)} 张 · 磁盘剩余 ${formatSize(storage.disk_free_mb)}` : ""}
          error={data.storage.ok ? undefined : data.storage.error}
        />
      </div>

      <div className="grid gap-3.5 lg:grid-cols-[1.35fr_1fr]">
        <Panel
          title="生图量"
          action={
            <div className="flex gap-3.5 text-xs text-ink-3">
              <span className="inline-flex items-center gap-1.5"><i className="size-2 rounded-[2px] bg-ink" />文生图</span>
              <span className="inline-flex items-center gap-1.5"><i className="size-2 rounded-[2px] bg-clay" />图生图</span>
            </div>
          }
        >
          {!data.counts.ok ? (
            <LoadError label="生图统计" error={data.counts.error} />
          ) : (
            <div className="grid gap-3">
              {PERIODS.map((period) => {
                const generations = counts?.generations[period.key] ?? 0;
                const edits = counts?.edits[period.key] ?? 0;
                return (
                  <div key={period.key} className="grid grid-cols-[56px_1fr_70px] items-center gap-3 text-xs text-ink-2">
                    <span>{period.label}</span>
                    <div className="flex h-2.5 overflow-hidden rounded-full bg-sunken">
                      <i className="h-full bg-ink" style={{ width: `${(generations / maxTotal) * 100}%` }} />
                      <i className="h-full bg-clay" style={{ width: `${(edits / maxTotal) * 100}%` }} />
                    </div>
                    <span className="text-right font-mono tabular-nums">{formatNumber(generations + edits)}</span>
                  </div>
                );
              })}
            </div>
          )}
        </Panel>

        <Panel title="需要处理">
          {todos.length === 0 ? (
            <div className="flex items-center gap-3 py-2 text-sm text-ink-2">
              <i className="size-[7px] rounded-full bg-ok" />
              一切正常
            </div>
          ) : (
            <div>
              {todos.map((todo) => (
                <div key={todo.title} className="grid grid-cols-[8px_1fr_auto] items-center gap-3 border-t border-line py-2.5 text-sm first:border-t-0">
                  <i className={cn("size-[7px] rounded-full", todo.tone === "bad" ? "bg-bad" : "bg-warn")} />
                  <div className="min-w-0">
                    <div className="text-ink">{todo.title}</div>
                    <div className="truncate text-xs text-ink-3" title={todo.note}>{todo.note}</div>
                  </div>
                  <Button variant="outline" size="sm" asChild>
                    <Link href={todo.href}>{todo.action}</Link>
                  </Button>
                </div>
              ))}
            </div>
          )}
        </Panel>
      </div>

      <Panel
        title="今天失败的调用"
        action={
          <Button variant="ghost" size="sm" asChild>
            <Link href="/logs">全部日志 →</Link>
          </Button>
        }
      >
        {!data.logs.ok ? (
          <LoadError label="调用日志" error={data.logs.error} />
        ) : failures.length === 0 ? (
          <div className="py-2 text-sm text-ink-3">今天还没有失败的调用。</div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-[13px]">
              <tbody>
                {failures.map((item) => {
                  const detail = (item.detail ?? {}) as { endpoint?: string; error?: string; account_email?: string };
                  return (
                    <tr key={item.id} className="border-t border-line first:border-t-0">
                      <td className="py-2.5 pr-4 font-mono text-xs whitespace-nowrap text-ink-2 shadow-[inset_2px_0_0_var(--bad)] pl-3">{String(item.time).slice(11)}</td>
                      <td className="py-2.5 pr-4 whitespace-nowrap text-ink">{detail.endpoint || "—"}</td>
                      <td className="py-2.5 pr-4 whitespace-nowrap text-ink-3">{detail.account_email || "—"}</td>
                      <td className="max-w-[420px] truncate py-2.5 text-ink-3" title={detail.error}>{detail.error || item.summary}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </Panel>
    </section>
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

  return isAdmin ? <Overview /> : null;
}
