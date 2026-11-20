"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { Activity, LoaderCircle, Power, PowerOff, RefreshCw, Search, ShieldCheck, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/confirm-dialog";
import { PageHeader } from "@/components/page-header";
import { RowMenu } from "@/components/row-menu";
import { batchBarClass, batchBarDangerClass } from "@/components/ui/batch-bar";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Segmented } from "@/components/ui/segmented";
import { EmptyState, ErrorState, SkeletonLines } from "@/components/ui/states";
import { StatusDot, type StatusTone } from "@/components/ui/status-dot";
import {
  deleteProxy,
  enableProxies,
  fetchProxies,
  fetchSettingsConfig,
  renameProxy,
  testProxies,
  type ProxyEntry,
  type ProxyTestResult,
} from "@/lib/api";
import { proxyShortName } from "@/lib/proxy-match";
import { useAuthGuard } from "@/lib/use-auth-guard";
import { cn } from "@/lib/utils";

import { ProxyBatchAddDialog } from "./components/proxy-batch-add-dialog";
import { ProxyCheckDialog, ProxyRetireDialog } from "./components/proxy-retire-dialogs";

type Filter = "all" | "enabled" | "abnormal" | "disabled";

function healthOf(proxy: ProxyEntry): { tone: StatusTone; text: string } {
  if (proxy.disabled) return { tone: "off", text: "已停用" };
  if (proxy.health.abnormal) return { tone: "bad", text: `异常 · 连续失败 ${proxy.health.consecutive_failures} 次` };
  if (proxy.health.successes + proxy.health.failures === 0) return { tone: "off", text: "暂无请求" };
  return { tone: "ok", text: `正常 · 成功 ${proxy.health.successes} / 失败 ${proxy.health.failures}` };
}

function testText(result: ProxyTestResult) {
  if (result.status >= 200 && result.status < 400) return { ok: true, text: `可用 · ${result.latency_ms} ms` };
  if (result.status === 403) return { ok: false, text: "被 Cloudflare 拦截（403）" };
  return { ok: false, text: result.error || `HTTP ${result.status}` };
}

function Stat({ label, value, note, tone }: { label: string; value: React.ReactNode; note?: React.ReactNode; tone?: "bad" }) {
  return (
    <div className="rounded-[10px] border border-line bg-surface px-4 py-3">
      <div className="text-xs text-ink-3">{label}</div>
      <div className="mt-0.5 font-serif text-[26px] leading-tight text-ink">{value}</div>
      {note ? <div className={cn("truncate text-xs", tone === "bad" ? "text-bad" : "text-ink-2")}>{note}</div> : null}
    </div>
  );
}

function ProxiesContent() {
  const [proxies, setProxies] = useState<ProxyEntry[]>([]);
  const [maxPerProxy, setMaxPerProxy] = useState(4);
  const [isLoading, setIsLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [filter, setFilter] = useState<Filter>("all");
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState<string[]>([]);
  const [names, setNames] = useState<Record<string, string>>({});
  const [results, setResults] = useState<Record<string, ProxyTestResult>>({});
  const [testing, setTesting] = useState<string[]>([]);
  const [busy, setBusy] = useState(false);
  const [retireKeys, setRetireKeys] = useState<string[]>([]);
  const [checkOpen, setCheckOpen] = useState(false);
  const [pendingDelete, setPendingDelete] = useState<ProxyEntry[]>([]);

  const applyItems = useCallback((items: ProxyEntry[]) => {
    setProxies(items);
    setLoadError("");
    setSelected((prev) => prev.filter((key) => items.some((item) => item.key === key)));
  }, []);

  const load = useCallback(async () => {
    setIsLoading(true);
    try {
      const [list, settings] = await Promise.allSettled([fetchProxies(), fetchSettingsConfig()]);
      if (list.status === "fulfilled") applyItems(list.value.items);
      else setLoadError(list.reason instanceof Error ? list.reason.message : "加载代理失败");
      if (settings.status === "fulfilled") setMaxPerProxy(Number(settings.value.config.proxy_max_accounts) || 4);
    } finally {
      setIsLoading(false);
    }
  }, [applyItems]);

  useEffect(() => {
    void load();
  }, [load]);

  const stats = useMemo(() => {
    const enabled = proxies.filter((proxy) => !proxy.disabled);
    return {
      total: proxies.length,
      enabled: enabled.length,
      disabled: proxies.length - enabled.length,
      abnormal: enabled.filter((proxy) => proxy.health.abnormal).length,
      used: enabled.reduce((sum, proxy) => sum + proxy.account_count, 0),
      stranded: proxies.filter((proxy) => proxy.disabled).reduce((sum, proxy) => sum + proxy.account_count, 0),
    };
  }, [proxies]);

  const rows = useMemo(() => {
    const q = query.trim().toLowerCase();
    return proxies.filter((proxy) => {
      const matchFilter =
        filter === "all" ||
        (filter === "enabled" && !proxy.disabled) ||
        (filter === "disabled" && proxy.disabled) ||
        (filter === "abnormal" && !proxy.disabled && proxy.health.abnormal);
      const matchQuery = !q || proxy.name.toLowerCase().includes(q) || proxy.masked_url.toLowerCase().includes(q);
      return matchFilter && matchQuery;
    });
  }, [proxies, filter, query]);

  const byKey = useMemo(() => new Map(proxies.map((proxy) => [proxy.key, proxy])), [proxies]);
  const selectedProxies = selected.map((key) => byKey.get(key)).filter((proxy): proxy is ProxyEntry => Boolean(proxy));
  const allRowsSelected = rows.length > 0 && rows.every((row) => selected.includes(row.key));

  const handleTest = async (targets: ProxyEntry[]) => {
    if (targets.length === 0) return;
    setTesting(targets.map((proxy) => proxy.key));
    try {
      const data = await testProxies(targets.map((proxy) => proxy.url));
      applyItems(data.items);
      setResults((prev) => ({ ...prev, ...data.results }));
      const passed = Object.values(data.results).filter((result) => result.status >= 200 && result.status < 400).length;
      toast.success(`测试完成：${passed}/${Object.keys(data.results).length} 个可用`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "测试代理失败");
    } finally {
      setTesting([]);
    }
  };

  const handleEnable = async (targets: ProxyEntry[]) => {
    setBusy(true);
    try {
      const data = await enableProxies(targets.map((proxy) => proxy.key));
      applyItems(data.items);
      toast.success(`已启用 ${data.enabled} 个代理`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "启用失败");
    } finally {
      setBusy(false);
    }
  };

  const handleRename = async (proxy: ProxyEntry) => {
    const name = (names[proxy.key] ?? proxy.name).trim();
    if (name === proxy.name) return;
    try {
      applyItems((await renameProxy(proxy.url, name)).items);
      toast.success("已保存代理名称");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "保存名称失败");
    }
  };

  const handleDelete = async (targets: ProxyEntry[]) => {
    setBusy(true);
    try {
      let items = proxies;
      for (const proxy of targets) {
        items = (await deleteProxy(proxy.url)).items;
      }
      applyItems(items);
      toast.success(`已删除 ${targets.length} 个代理`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "删除失败");
      void load();
    } finally {
      setBusy(false);
      setPendingDelete([]);
    }
  };

  const filters = [
    ["all", `全部 ${stats.total}`],
    ["enabled", `启用 ${stats.enabled}`],
    ["abnormal", `异常 ${stats.abnormal}`],
    ["disabled", `停用 ${stats.disabled}`],
  ] as const;
  const capacity = stats.enabled * maxPerProxy;
  const deletable = selectedProxies.filter((proxy) => proxy.account_count === 0);

  return (
    <section className="space-y-5">
      <PageHeader
        title="代理"
        description="账号走哪个出口以账号上的代理为准。停用的代理不再分配账号，随时可以启用。"
        actions={
          <>
            <Button variant="ghost" size="icon" onClick={() => void load()} disabled={isLoading} title="重新加载">
              <RefreshCw className={cn("size-4", isLoading && "animate-spin")} />
            </Button>
            <ProxyBatchAddDialog onChange={applyItems} />
            <Button variant="clay" onClick={() => setCheckOpen(true)} disabled={stats.enabled === 0}>
              <ShieldCheck className="size-4" />
              检测并清理
            </Button>
          </>
        }
      />

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <Stat label="代理" value={stats.total} note={`启用 ${stats.enabled} · 停用 ${stats.disabled}`} />
        <Stat label="异常" value={stats.abnormal} note={stats.abnormal ? "连续失败 3 次以上" : "启用的代理都正常"} tone={stats.abnormal ? "bad" : undefined} />
        <Stat
          label="账号名额"
          value={
            <>
              {stats.used} <span className="font-sans text-sm text-ink-3">/ {capacity}</span>
            </>
          }
          note={`每个代理最多 ${maxPerProxy} 个，在设置里改`}
        />
        <Stat
          label="停用代理上的账号"
          value={stats.stranded}
          note={stats.stranded ? "还走着已停用的代理，选中这些代理再停用一次可迁走" : "没有"}
          tone={stats.stranded ? "bad" : undefined}
        />
      </div>

      <div className="flex flex-wrap items-center gap-2">
        <Segmented value={filter} options={filters} onChange={setFilter} />
        <div className="relative min-w-[200px] flex-1">
          <Search className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-ink-3" />
          <Input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索名称或地址" className="pl-9" />
        </div>
      </div>

      {selectedProxies.length > 0 ? (
        <div className={batchBarClass}>
          <span>已选 {selectedProxies.length} 个代理</span>
          <span className="flex-1" />
          <button
            type="button"
            className="inline-flex h-7 items-center gap-1.5 rounded-md border border-page/20 px-2.5 text-xs transition hover:bg-page/10 disabled:opacity-50"
            onClick={() => void handleTest(selectedProxies)}
            disabled={testing.length > 0}
          >
            {testing.length ? <LoaderCircle className="size-3.5 animate-spin" /> : <Activity className="size-3.5" />}
            测试
          </button>
          <button
            type="button"
            className="inline-flex h-7 items-center gap-1.5 rounded-md border border-page/20 px-2.5 text-xs transition hover:bg-page/10 disabled:opacity-50"
            onClick={() => void handleEnable(selectedProxies.filter((proxy) => proxy.disabled))}
            disabled={busy || !selectedProxies.some((proxy) => proxy.disabled)}
          >
            <Power className="size-3.5" />
            启用
          </button>
          <button
            type="button"
            className={cn(batchBarDangerClass, "h-7 text-xs")}
            onClick={() => setRetireKeys(selectedProxies.filter((proxy) => !proxy.disabled || proxy.account_count > 0).map((proxy) => proxy.key))}
            disabled={busy || !selectedProxies.some((proxy) => !proxy.disabled || proxy.account_count > 0)}
          >
            <PowerOff className="size-3.5" />
            迁移账号并停用
          </button>
          <button
            type="button"
            className={cn(batchBarDangerClass, "h-7 text-xs")}
            onClick={() => setPendingDelete(deletable)}
            disabled={busy || deletable.length === 0}
            title={deletable.length < selectedProxies.length ? "只删除没有账号的代理" : undefined}
          >
            <Trash2 className="size-3.5" />
            删除{deletable.length ? ` ${deletable.length}` : ""}
          </button>
          <button type="button" onClick={() => setSelected([])} className="h-7 rounded-md px-2.5 text-xs transition hover:bg-page/10">
            取消
          </button>
        </div>
      ) : null}

      <div className="overflow-hidden rounded-[10px] border border-line bg-surface">
        {loadError && proxies.length === 0 ? (
          <ErrorState className="p-4" message={`加载代理失败：${loadError}`} onRetry={() => void load()} />
        ) : isLoading && proxies.length === 0 ? (
          <SkeletonLines lines={6} className="px-[18px] py-4" />
        ) : rows.length === 0 ? (
          <EmptyState
            title={proxies.length === 0 ? "还没有代理" : "没有匹配的代理"}
            description={proxies.length === 0 ? "点「批量添加」添加代理。" : "换个筛选或搜索词。"}
          />
        ) : (
          <div className="thin-scrollbar overflow-x-auto">
            <table className="w-full min-w-[880px] text-left text-[13px]">
              <thead className="text-xs text-ink-3 [&_th]:border-b [&_th]:border-line">
                <tr>
                  <th className="w-10 py-2.5 pr-1 pl-3.5 font-normal">
                    <Checkbox
                      aria-label="全选"
                      checked={allRowsSelected}
                      onCheckedChange={(checked) =>
                        setSelected((prev) =>
                          checked ? Array.from(new Set([...prev, ...rows.map((row) => row.key)])) : prev.filter((key) => !rows.some((row) => row.key === key)),
                        )
                      }
                    />
                  </th>
                  <th className="px-2.5 py-2.5 font-normal">名称</th>
                  <th className="px-2.5 py-2.5 font-normal">地址</th>
                  <th className="px-2.5 py-2.5 text-right font-normal">账号</th>
                  <th className="px-2.5 py-2.5 font-normal">健康</th>
                  <th className="px-2.5 py-2.5 font-normal">本次测试</th>
                  <th className="w-24 px-2.5 py-2.5" />
                </tr>
              </thead>
              <tbody>
                {rows.map((proxy) => {
                  const health = healthOf(proxy);
                  const test = results[proxy.key] ? testText(results[proxy.key]) : null;
                  const isTesting = testing.includes(proxy.key);
                  const full = !proxy.disabled && proxy.account_count >= maxPerProxy;
                  return (
                    <tr key={proxy.key} className={cn("border-b border-line last:border-b-0 hover:bg-surface-hover", proxy.disabled && "text-ink-3")}>
                      <td className="py-2 pr-1 pl-3.5">
                        <Checkbox
                          aria-label="选择代理"
                          checked={selected.includes(proxy.key)}
                          onCheckedChange={(checked) =>
                            setSelected((prev) => (checked ? Array.from(new Set([...prev, proxy.key])) : prev.filter((key) => key !== proxy.key)))
                          }
                        />
                      </td>
                      <td className="px-2.5 py-2">
                        <Input
                          value={names[proxy.key] ?? proxy.name}
                          onChange={(event) => setNames((prev) => ({ ...prev, [proxy.key]: event.target.value }))}
                          onBlur={() => void handleRename(proxy)}
                          onKeyDown={(event) => {
                            if (event.key === "Enter") event.currentTarget.blur();
                          }}
                          placeholder={proxyShortName(proxy)}
                          aria-label="代理名称"
                          className="h-8 w-36 border-transparent bg-transparent px-1.5 hover:border-line focus-visible:border-clay"
                        />
                      </td>
                      <td className="max-w-[300px] truncate px-2.5 py-2 font-mono text-xs text-ink-2" title={proxy.masked_url}>
                        {proxy.masked_url}
                      </td>
                      <td className={cn("px-2.5 py-2 text-right font-mono text-xs whitespace-nowrap tabular-nums", full ? "text-warn" : "text-ink-2")}>
                        {proxy.account_count}
                        {proxy.disabled ? "" : ` / ${maxPerProxy}`}
                      </td>
                      <td className="max-w-[240px] px-2.5 py-2">
                        <StatusDot tone={health.tone} className="min-w-0 overflow-hidden" title={proxy.health.last_error || undefined}>
                          <span className="truncate">{health.text}</span>
                        </StatusDot>
                      </td>
                      <td className="max-w-[220px] truncate px-2.5 py-2 text-xs" title={test?.text}>
                        {isTesting ? (
                          <span className="inline-flex items-center gap-1.5 text-ink-3">
                            <LoaderCircle className="size-3.5 animate-spin" />
                            测试中
                          </span>
                        ) : test ? (
                          <span className={test.ok ? "text-ok" : "text-bad"}>{test.text}</span>
                        ) : (
                          <span className="text-ink-3">—</span>
                        )}
                      </td>
                      <td className="px-2.5 py-2">
                        <div className="flex items-center justify-end gap-0.5">
                          <button
                            type="button"
                            title="测试这个代理"
                            className="grid size-8 place-items-center rounded-[7px] text-ink-3 transition hover:bg-surface-hover hover:text-ink disabled:opacity-40"
                            disabled={testing.length > 0}
                            onClick={() => void handleTest([proxy])}
                          >
                            {isTesting ? <LoaderCircle className="size-4 animate-spin" /> : <Activity className="size-4" />}
                          </button>
                          <RowMenu
                            items={[
                              proxy.disabled
                                ? { label: "启用", icon: Power, onSelect: () => void handleEnable([proxy]), disabled: busy }
                                : { label: "迁移账号并停用", icon: PowerOff, danger: true, onSelect: () => setRetireKeys([proxy.key]), disabled: busy },
                              ...(proxy.disabled && proxy.account_count > 0
                                ? [{ label: "迁走剩下的账号", icon: PowerOff, danger: true, onSelect: () => setRetireKeys([proxy.key]), disabled: busy }]
                                : []),
                              {
                                label: proxy.account_count > 0 ? `删除（还有 ${proxy.account_count} 个账号）` : "删除",
                                icon: Trash2,
                                danger: true,
                                onSelect: () => setPendingDelete([proxy]),
                                disabled: busy || proxy.account_count > 0,
                              },
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
        )}
      </div>

      <ProxyRetireDialog keys={retireKeys} onClose={() => setRetireKeys([])} onDone={(items) => {
        applyItems(items);
        setSelected([]);
      }} />
      <ProxyCheckDialog
        open={checkOpen}
        onClose={() => setCheckOpen(false)}
        onChecked={(items, checked) => {
          applyItems(items);
          setResults((prev) => ({ ...prev, ...checked }));
        }}
        onDone={applyItems}
      />
      <ConfirmDialog
        open={pendingDelete.length > 0}
        onOpenChange={(open) => (!open ? setPendingDelete([]) : null)}
        title={`删除 ${pendingDelete.length} 个代理？`}
        description="只从代理清单里删除，账号不受影响（只能删没有账号的代理）。想保留以后再用，可以改用停用。"
        confirmLabel="删除"
        danger
        busy={busy}
        onConfirm={() => void handleDelete(pendingDelete)}
      />
    </section>
  );
}

export default function ProxiesPage() {
  const { isCheckingAuth, session } = useAuthGuard(["admin"]);
  if (isCheckingAuth || !session || session.role !== "admin") {
    return <SkeletonLines lines={6} className="pt-6" />;
  }
  return <ProxiesContent />;
}
