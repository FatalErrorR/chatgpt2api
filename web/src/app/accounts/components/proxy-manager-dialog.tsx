"use client";

import { useState } from "react";
import { Activity, Globe, LoaderCircle, Plus, Trash2 } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { StatusDot } from "@/components/ui/status-dot";
import { Textarea } from "@/components/ui/textarea";
import {
  addProxies,
  deleteProxy,
  renameProxy,
  testProxies,
  type ProxyBatchAddResult,
  type ProxyEntry,
  type ProxyTestResult,
} from "@/lib/api";
import { cn } from "@/lib/utils";

type ProxyManagerDialogProps = {
  proxies: ProxyEntry[];
  onChange: (items: ProxyEntry[]) => void;
};

function healthText(proxy: ProxyEntry) {
  const { health } = proxy;
  if (health.abnormal) {
    return `异常：连续失败 ${health.consecutive_failures} 次`;
  }
  if (health.successes + health.failures === 0) {
    return "暂无请求";
  }
  return `正常 · 成功 ${health.successes} / 失败 ${health.failures}`;
}

function testText(result: ProxyTestResult) {
  if (result.status >= 200 && result.status < 400) {
    return `测试可用 · ${result.latency_ms} ms`;
  }
  if (result.status === 403) {
    return `测试被 Cloudflare 拦截（HTTP 403）· ${result.latency_ms} ms`;
  }
  return `测试失败：${result.error || `HTTP ${result.status}`}`;
}

export function ProxyManagerDialog({ proxies, onChange }: ProxyManagerDialogProps) {
  const [open, setOpen] = useState(false);
  const [names, setNames] = useState<Record<string, string>>({});
  const [newLines, setNewLines] = useState("");
  const [vendor, setVendor] = useState("");
  const [batchResult, setBatchResult] = useState<ProxyBatchAddResult | null>(null);
  const [busy, setBusy] = useState(false);
  const [testing, setTesting] = useState<string[] | null>(null); // 正在测试的代理 key；null 表示没在测
  const [results, setResults] = useState<Record<string, ProxyTestResult>>({});
  const [adding, setAdding] = useState(false);

  const handleTest = async (targets: ProxyEntry[]) => {
    setTesting(targets.map((proxy) => proxy.key));
    try {
      const data = await testProxies(targets.length === proxies.length ? [] : targets.map((proxy) => proxy.url));
      onChange(data.items);
      setResults((prev) => ({ ...prev, ...data.results }));
      const passed = Object.values(data.results).filter((result) => result.status >= 200 && result.status < 400).length;
      toast.success(`测试完成：${passed}/${Object.keys(data.results).length} 个代理可用`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "测试代理失败");
    } finally {
      setTesting(null);
    }
  };

  const run = async (action: () => Promise<{ items: ProxyEntry[] }>, success: string) => {
    setBusy(true);
    try {
      const data = await action();
      onChange(data.items);
      toast.success(success);
      return true;
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "操作失败");
      return false;
    } finally {
      setBusy(false);
    }
  };

  const handleOpenChange = (nextOpen: boolean) => {
    setOpen(nextOpen);
    if (nextOpen) {
      setNames(Object.fromEntries(proxies.map((proxy) => [proxy.url, proxy.name])));
    }
  };

  const handleRename = (proxy: ProxyEntry) => {
    const name = (names[proxy.url] ?? "").trim();
    if (name === proxy.name) {
      return;
    }
    void run(() => renameProxy(proxy.url, name), "已保存代理名称");
  };

  const lineCount = newLines.split("\n").filter((line) => line.trim()).length;

  const handleAdd = async () => {
    if (lineCount === 0) {
      toast.error("请填写代理，每行一个");
      return;
    }
    setBusy(true);
    try {
      const data = await addProxies(newLines, vendor.trim());
      onChange(data.items);
      setNames((prev) => ({ ...prev, ...Object.fromEntries(data.items.map((proxy) => [proxy.url, prev[proxy.url] ?? proxy.name])) }));
      setBatchResult(data);
      const summary = `新增 ${data.added.length} 个，跳过 ${data.skipped.length} 个，无效 ${data.invalid.length} 个`;
      if (data.added.length > 0) toast.success(summary);
      else toast.error(summary);
      if (data.invalid.length === 0) {
        setNewLines("");
        if (data.skipped.length === 0) setAdding(false);
      } else {
        // 只留下无效的行，改完可以直接再提交
        const invalidLines = new Set(data.invalid.map((item) => item.line));
        setNewLines(newLines.split("\n").filter((line, index) => line.trim() && invalidLines.has(index + 1)).join("\n"));
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "添加代理失败");
    } finally {
      setBusy(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <Button variant="outline" onClick={() => handleOpenChange(true)}>
        <Globe className="size-4" />
        代理
      </Button>
      <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-2xl">
        <DialogHeader className="gap-1.5">
          <DialogTitle className="font-serif text-[22px] font-medium">代理</DialogTitle>
          <DialogDescription className="text-[13px] text-ink-2">
            给代理起名字、查看健康状况。账号走哪个出口仍以账号上的代理地址为准。
          </DialogDescription>
        </DialogHeader>

        <div className="max-h-[50vh] space-y-2 overflow-y-auto">
          {proxies.length === 0 ? (
            <p className="py-8 text-center text-[13px] text-ink-3">还没有代理，在下方添加。</p>
          ) : (
            proxies.map((proxy) => {
              const result = results[proxy.key];
              const passed = result ? result.status >= 200 && result.status < 400 : false;
              const tone = proxy.health.abnormal ? "bad" : proxy.health.successes > 0 ? "ok" : "off";
              return (
                <div key={proxy.url} className="grid grid-cols-[18px_1fr_auto] items-center gap-3 rounded-[7px] border border-line px-3 py-2.5">
                  <StatusDot tone={tone} />
                  <div className="min-w-0">
                    <div className="flex min-w-0 items-center gap-2">
                      <Input
                        value={names[proxy.url] ?? proxy.name}
                        onChange={(event) => setNames((prev) => ({ ...prev, [proxy.url]: event.target.value }))}
                        onBlur={() => handleRename(proxy)}
                        onKeyDown={(event) => {
                          if (event.key === "Enter") handleRename(proxy);
                        }}
                        placeholder="未命名"
                        title="点击修改名称"
                        className="h-7 w-28 shrink-0 rounded-md border-transparent bg-transparent px-1.5 font-medium shadow-none hover:border-line focus-visible:border-line-strong"
                        disabled={busy}
                      />
                      <span className="min-w-0 truncate font-mono text-xs text-ink-3" title={proxy.masked_url}>
                        {proxy.masked_url}
                      </span>
                    </div>
                    <div
                      className={cn(
                        "truncate px-1.5 text-xs",
                        result ? (passed ? "text-ok" : "text-bad") : proxy.health.abnormal ? "text-bad" : "text-ink-3",
                      )}
                      title={result ? testText(result) : proxy.health.last_error || undefined}
                    >
                      {result
                        ? testText(result)
                        : `${healthText(proxy)}${proxy.health.abnormal && proxy.health.last_error ? ` · ${proxy.health.last_error}` : ""}`}
                    </div>
                  </div>
                  <div className="flex items-center gap-1">
                    <span className="mr-1 text-xs whitespace-nowrap text-ink-3">{proxy.account_count} 个账号</span>
                    <button
                      type="button"
                      className="grid size-8 place-items-center rounded-[7px] text-ink-3 transition hover:bg-surface-hover hover:text-ink disabled:opacity-40"
                      disabled={busy || testing !== null}
                      title="测试这个代理"
                      onClick={() => void handleTest([proxy])}
                    >
                      {testing?.includes(proxy.key) ? <LoaderCircle className="size-4 animate-spin" /> : <Activity className="size-4" />}
                    </button>
                    <button
                      type="button"
                      className="grid size-8 place-items-center rounded-[7px] text-ink-3 transition hover:bg-bad-soft hover:text-bad disabled:opacity-40"
                      disabled={busy || proxy.account_count > 0}
                      title={proxy.account_count > 0 ? "还有账号在用这个代理，不能删除" : "删除代理"}
                      onClick={() => void run(() => deleteProxy(proxy.url), "已删除代理")}
                    >
                      <Trash2 className="size-4" />
                    </button>
                  </div>
                </div>
              );
            })
          )}
        </div>

        {adding ? (
          <div className="space-y-2.5 rounded-[10px] border border-line bg-sunken p-3">
            <Textarea
              value={newLines}
              onChange={(event) => setNewLines(event.target.value)}
              placeholder={"每行一个代理，例如：\n194.39.33.194:5903:账号:密码\n1.2.3.4:8080\nsocks5://user:pass@host:1080"}
              className="min-h-28 resize-y rounded-[7px] border-line-strong bg-surface px-3 py-2 font-mono text-xs leading-5 text-ink shadow-none placeholder:text-ink-3 focus-visible:border-ink-3 focus-visible:ring-0"
              disabled={busy}
              autoFocus
            />
            <div className="flex flex-wrap items-center gap-2">
              <Input
                value={vendor}
                onChange={(event) => setVendor(event.target.value)}
                placeholder="厂商名称（可选）"
                className="h-[34px] w-44 rounded-[7px] border-line-strong bg-surface px-3 text-[13px] shadow-none placeholder:text-ink-3 focus-visible:border-ink-3 focus-visible:ring-0"
                disabled={busy}
              />
              <span className="min-w-0 flex-1 text-xs text-ink-3">
                {vendor.trim() ? `依次命名为「${vendor.trim()}-序号」，接着这个厂商已有的最大序号` : "不填厂商则不命名，之后可逐个修改"}
              </span>
              <Button onClick={() => void handleAdd()} disabled={busy || lineCount === 0}>
                {busy ? <LoaderCircle className="size-4 animate-spin" /> : null}
                添加{lineCount > 0 ? ` ${lineCount} 个` : ""}
              </Button>
            </div>
            {batchResult && (batchResult.skipped.length > 0 || batchResult.invalid.length > 0) ? (
              <ul className="max-h-28 space-y-0.5 overflow-y-auto text-xs">
                {[
                  ...batchResult.invalid.map((item) => ({ line: item.line, text: item.text, note: item.error, bad: true })),
                  ...batchResult.skipped.map((item) => ({
                    line: item.line,
                    text: item.text,
                    note: item.name ? `${item.reason}（${item.name}）` : item.reason,
                    bad: false,
                  })),
                ]
                  .sort((a, b) => a.line - b.line)
                  .map((item) => (
                    <li key={item.line} className={item.bad ? "text-bad" : "text-ink-3"}>
                      第 {item.line} 行 <span className="font-mono">{item.text}</span>：{item.note}
                    </li>
                  ))}
              </ul>
            ) : null}
          </div>
        ) : null}
        <p className="text-xs leading-5 text-ink-3">
          健康状况只统计本次启动以来的请求：连续 3 次被 Cloudflare 挑战或连接失败就标记异常，成功一次即恢复。代理续费或更换后点「全部测试」可立即清掉异常标记。
        </p>

        <DialogFooter className="pt-1 sm:justify-between">
          <Button
            variant="ghost"
            onClick={() => {
              setAdding((value) => !value);
              setBatchResult(null);
            }}
          >
            <Plus className="size-4" />
            添加代理
          </Button>
          <div className="flex gap-2">
            <Button variant="outline" disabled={busy || testing !== null || proxies.length === 0} onClick={() => void handleTest(proxies)}>
              {testing !== null && testing.length > 1 ? <LoaderCircle className="size-4 animate-spin" /> : <Activity className="size-4" />}
              全部测试
            </Button>
            <Button onClick={() => setOpen(false)}>完成</Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
