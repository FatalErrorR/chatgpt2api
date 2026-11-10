"use client";

import Link from "next/link";
import { useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import { LoaderCircle } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  fetchAccounts,
  fetchImageOutputCounts,
  fetchImageStorage,
  fetchProxies,
  fetchSystemLogs,
  type Account,
  type ImageOutputCounts,
  type ImageStorageStats,
  type ProxyEntry,
  type SystemLog,
} from "@/lib/api";
import { getValidatedAuthSession } from "@/lib/auth-session";
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

function formatSize(mb: number) {
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`;
}

function isEditCooled(account: Account) {
  const until = account.edit_upload_blocked_until ? Date.parse(account.edit_upload_blocked_until) : NaN;
  return Number.isFinite(until) && until > Date.now();
}

function today() {
  const now = new Date();
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())}`;
}

type OverviewData = {
  accounts: Account[];
  proxies: ProxyEntry[];
  counts: ImageOutputCounts | null;
  storage: ImageStorageStats | null;
  failures: SystemLog[];
};

function Kpi({ label, value, note, tone }: { label: string; value: React.ReactNode; note: string; tone?: "bad" | "good" }) {
  return (
    <div className="rounded-[10px] border border-line bg-surface px-[18px] py-4">
      <div className="text-xs text-ink-3">{label}</div>
      <div className="mt-1 mb-0.5 font-serif text-[30px] leading-tight text-ink">{value}</div>
      <div className={cn("text-xs", tone === "bad" ? "text-bad" : tone === "good" ? "text-ok" : "text-ink-2")}>{note}</div>
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
  const [data, setData] = useState<OverviewData | null>(null);
  const [error, setError] = useState("");

  useEffect(() => {
    let active = true;
    const load = async () => {
      // 各块互不依赖：某个接口失败只影响对应的块
      const [accounts, proxies, counts, storage, logs] = await Promise.allSettled([
        fetchAccounts(),
        fetchProxies(),
        fetchImageOutputCounts(),
        fetchImageStorage(),
        fetchSystemLogs({ type: "call", start_date: today() }),
      ]);
      if (!active) return;
      if (accounts.status === "rejected") {
        setError(accounts.reason instanceof Error ? accounts.reason.message : "加载失败");
      }
      setData({
        accounts: accounts.status === "fulfilled" ? accounts.value.items : [],
        proxies: proxies.status === "fulfilled" ? proxies.value.items : [],
        counts: counts.status === "fulfilled" ? counts.value : null,
        storage: storage.status === "fulfilled" ? storage.value : null,
        failures:
          logs.status === "fulfilled"
            ? logs.value.items.filter((item) => (item.detail as { status?: string } | undefined)?.status === "failed").slice(0, 5)
            : [],
      });
    };
    void load();
    return () => {
      active = false;
    };
  }, []);

  const stats = useMemo(() => {
    const accounts = data?.accounts ?? [];
    const normal = accounts.filter((item) => item.status === "正常");
    return {
      total: accounts.length,
      normal: normal.length,
      abnormal: accounts.filter((item) => item.status === "异常").length,
      cooled: accounts.filter(isEditCooled).length,
      quota: normal.reduce((sum, item) => sum + Math.max(0, item.quota || 0), 0),
      badProxies: (data?.proxies ?? []).filter((proxy) => proxy.health.abnormal),
    };
  }, [data]);

  if (!data) {
    return (
      <div className="grid min-h-[50vh] place-items-center">
        <LoaderCircle className="size-5 animate-spin text-ink-3" />
      </div>
    );
  }

  const counts = data.counts;
  const total = (key: keyof ImageOutputCounts["generations"]) => (counts ? counts.generations[key] + counts.edits[key] : 0);
  const maxTotal = Math.max(1, ...PERIODS.map((period) => total(period.key)));
  const firstBadError = stats.badProxies.find((proxy) => proxy.health.last_error)?.health.last_error ?? "";
  const todos = [
    stats.badProxies.length > 0 && {
      tone: "bad",
      title: `${stats.badProxies.length} 个代理异常`,
      note: firstBadError || "连续请求失败",
      href: "/accounts",
      action: "查看代理",
    },
    stats.abnormal > 0 && { tone: "bad", title: `${stats.abnormal} 个账号异常`, note: "可在号池里尝试恢复", href: "/accounts", action: "查看" },
    stats.cooled > 0 && { tone: "warn", title: `${stats.cooled} 个账号图生图冷却`, note: "冷却结束后自动恢复", href: "/accounts", action: "查看" },
    data.storage && data.storage.disk_free_mb < 2048 && {
      tone: "warn",
      title: "磁盘空间不足",
      note: `剩余 ${formatSize(data.storage.disk_free_mb)}`,
      href: "/image-manager",
      action: "去清理",
    },
  ].filter(Boolean) as Array<{ tone: "bad" | "warn"; title: string; note: string; href: string; action: string }>;

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

      {error ? <div className="rounded-lg bg-bad-soft px-3 py-2.5 text-sm text-bad">加载账号失败：{error}</div> : null}

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
        />
        <Kpi label="今日生图" value={formatNumber(total("today"))} note={`昨日 ${formatNumber(total("yesterday"))}`} />
        <Kpi
          label="代理"
          value={data.proxies.length}
          note={stats.badProxies.length > 0 ? `${stats.badProxies.length} 个异常` : "全部正常"}
          tone={stats.badProxies.length > 0 ? "bad" : "good"}
        />
        <Kpi
          label="图片存储"
          value={data.storage ? formatSize(data.storage.image_size_mb) : "—"}
          note={data.storage ? `${formatNumber(data.storage.image_count)} 张 · 磁盘剩余 ${formatSize(data.storage.disk_free_mb)}` : "暂无数据"}
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
        {data.failures.length === 0 ? (
          <div className="py-2 text-sm text-ink-3">今天还没有失败的调用。</div>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-[13px]">
              <tbody>
                {data.failures.map((item) => {
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
