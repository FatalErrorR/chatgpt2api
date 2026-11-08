"use client";

import { useState } from "react";
import { Globe, LoaderCircle, Plus, Trash2 } from "lucide-react";
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
import { addProxy, deleteProxy, renameProxy, type ProxyEntry } from "@/lib/api";
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

export function ProxyManagerDialog({ proxies, onChange }: ProxyManagerDialogProps) {
  const [open, setOpen] = useState(false);
  const [names, setNames] = useState<Record<string, string>>({});
  const [newUrl, setNewUrl] = useState("");
  const [newName, setNewName] = useState("");
  const [busy, setBusy] = useState(false);

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

  const handleAdd = async () => {
    if (!newUrl.trim()) {
      toast.error("请填写代理地址");
      return;
    }
    const ok = await run(() => addProxy(newUrl.trim(), newName.trim()), "已添加代理");
    if (ok) {
      setNames((prev) => ({ ...prev, [newUrl.trim()]: newName.trim() }));
      setNewUrl("");
      setNewName("");
    }
  };

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <Button
        variant="outline"
        className="h-10 rounded-xl border-stone-200 bg-white/80 px-4 text-stone-700 hover:bg-white"
        onClick={() => handleOpenChange(true)}
      >
        <Globe className="size-4" />
        代理
      </Button>
      <DialogContent showCloseButton={false} className="rounded-2xl p-6 sm:max-w-2xl">
        <DialogHeader className="gap-2">
          <DialogTitle>代理</DialogTitle>
          <DialogDescription>
            给账号用的代理起名字，查看健康状况。名字只用于展示，账号走哪个出口仍以账号上的代理地址为准。
          </DialogDescription>
        </DialogHeader>

        <div className="max-h-[50vh] space-y-2 overflow-y-auto">
          {proxies.length === 0 ? (
            <p className="py-6 text-center text-sm text-stone-500">还没有代理</p>
          ) : (
            proxies.map((proxy) => (
              <div key={proxy.url} className="rounded-xl border border-stone-200 bg-white p-3">
                <div className="flex items-center gap-2">
                  <Input
                    value={names[proxy.url] ?? proxy.name}
                    onChange={(event) => setNames((prev) => ({ ...prev, [proxy.url]: event.target.value }))}
                    onBlur={() => handleRename(proxy)}
                    onKeyDown={(event) => {
                      if (event.key === "Enter") {
                        handleRename(proxy);
                      }
                    }}
                    placeholder="名称，例如：美西-1"
                    className="h-9 w-40 rounded-lg border-stone-200"
                    disabled={busy}
                  />
                  <span className="min-w-0 flex-1 truncate font-mono text-xs text-stone-600" title={proxy.masked_url}>
                    {proxy.masked_url}
                  </span>
                  <span className="shrink-0 text-xs text-stone-500">{proxy.account_count} 个账号</span>
                  <Button
                    variant="ghost"
                    size="icon"
                    className="size-8 shrink-0 text-stone-500 hover:text-rose-600"
                    disabled={busy || proxy.account_count > 0}
                    title={proxy.account_count > 0 ? "还有账号在用这个代理，不能删除" : "删除代理"}
                    onClick={() => void run(() => deleteProxy(proxy.url), "已删除代理")}
                  >
                    <Trash2 className="size-4" />
                  </Button>
                </div>
                <p
                  className={cn("mt-1.5 text-xs", proxy.health.abnormal ? "text-rose-500" : "text-stone-400")}
                  title={proxy.health.last_error || undefined}
                >
                  {healthText(proxy)}
                  {proxy.health.abnormal && proxy.health.last_error ? ` · ${proxy.health.last_error}` : ""}
                </p>
              </div>
            ))
          )}
        </div>

        <div className="flex items-center gap-2">
          <Input
            value={newName}
            onChange={(event) => setNewName(event.target.value)}
            placeholder="名称"
            className="h-10 w-40 rounded-xl border-stone-200"
            disabled={busy}
          />
          <Input
            value={newUrl}
            onChange={(event) => setNewUrl(event.target.value)}
            placeholder="http://user:pass@host:port"
            className="h-10 flex-1 rounded-xl border-stone-200"
            disabled={busy}
          />
          <Button className="h-10 rounded-xl" onClick={() => void handleAdd()} disabled={busy}>
            {busy ? <LoaderCircle className="size-4 animate-spin" /> : <Plus className="size-4" />}
            添加
          </Button>
        </div>
        <p className="text-xs text-stone-500">
          健康状况只统计本次启动以来的请求：连续 3 次被 Cloudflare 挑战或连接失败就标记异常，成功一次即恢复。
        </p>

        <DialogFooter className="pt-2">
          <Button
            variant="secondary"
            className="h-10 rounded-xl bg-stone-100 px-5 text-stone-700 hover:bg-stone-200"
            onClick={() => setOpen(false)}
          >
            关闭
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
