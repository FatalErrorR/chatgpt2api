"use client";

import { useEffect, useMemo, useState } from "react";
import { LoaderCircle, Search } from "lucide-react";
import { toast } from "sonner";

import { ProxyMovePlanView } from "@/components/proxy-move-plan";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Segmented } from "@/components/ui/segmented";
import { SkeletonLines } from "@/components/ui/states";
import { StatusDot } from "@/components/ui/status-dot";
import { reassignAccountProxies, type Account, type ProxyEntry, type ProxyMovePlan } from "@/lib/api";
import { proxyShortName } from "@/lib/proxy-match";

type Mode = "all" | "chosen";
const MODES = [
  ["all", "所有可用代理"],
  ["chosen", "指定代理"],
] as const;

/** 重新分配选中账号的代理：先预览方案，确认后执行。 */
export function AccountReassignProxyDialog({
  tokens,
  proxies,
  onClose,
  onDone,
}: {
  /** 要重新分配的账号；空数组表示关闭 */
  tokens: string[];
  proxies: ProxyEntry[];
  onClose: () => void;
  onDone: (items: Account[]) => void;
}) {
  const open = tokens.length > 0;
  const [mode, setMode] = useState<Mode>("all");
  const [chosen, setChosen] = useState<string[]>([]);
  const [query, setQuery] = useState("");
  const [plan, setPlan] = useState<ProxyMovePlan | null>(null);
  const [previewing, setPreviewing] = useState(false);
  const [applying, setApplying] = useState(false);

  const enabled = useMemo(() => proxies.filter((proxy) => !proxy.disabled), [proxies]);
  const visible = useMemo(() => {
    const q = query.trim().toLowerCase();
    return enabled.filter((proxy) => !q || proxyShortName(proxy).toLowerCase().includes(q) || proxy.masked_url.toLowerCase().includes(q));
  }, [enabled, query]);
  const targetUrls = mode === "all" ? [] : chosen;

  useEffect(() => {
    if (!open) return;
    setMode("all");
    setChosen([]);
    setQuery("");
  }, [open]);

  // 目标一变就重新预览
  useEffect(() => {
    if (!open || (mode === "chosen" && chosen.length === 0)) {
      setPlan(null);
      return;
    }
    let active = true;
    setPreviewing(true);
    reassignAccountProxies(tokens, targetUrls, true)
      .then((data) => active && setPlan(data))
      .catch((error) => active && setPlan({ moves: [], unchanged: 0, targets: [], max_per_proxy: 0, error: error instanceof Error ? error.message : "生成方案失败" }))
      .finally(() => active && setPreviewing(false));
    return () => {
      active = false;
    };
  }, [open, mode, chosen.join("|"), tokens.join("|")]);

  const apply = async () => {
    setApplying(true);
    try {
      const data = await reassignAccountProxies(tokens, targetUrls, false);
      if (data.items) onDone(data.items);
      toast.success(`已重新分配 ${data.moved ?? 0} 个账号的代理`);
      onClose();
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "重新分配失败");
    } finally {
      setApplying(false);
    }
  };

  return (
    <Dialog open={open} onOpenChange={(next) => (!next && !applying ? onClose() : null)}>
      <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-[600px]">
        <DialogHeader className="gap-1.5">
          <DialogTitle className="font-serif text-[22px] font-medium">重新分配 {tokens.length} 个账号的代理</DialogTitle>
          <DialogDescription className="text-[13px] leading-6 text-ink-2">
            每个账号给账号最少的代理，平手按清单顺序。「所有可用代理」会跳过停用和异常的代理。
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <Segmented value={mode} options={MODES} onChange={setMode} />
          {mode === "chosen" ? (
            <div className="grid gap-2">
              <div className="relative">
                <Search className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-ink-3" />
                <Input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="搜索代理" className="pl-9" />
              </div>
              <div className="thin-scrollbar max-h-44 overflow-y-auto rounded-[7px] border border-line">
                {visible.map((proxy) => (
                  <label key={proxy.key} className="flex items-center gap-2.5 border-t border-line px-3 py-1.5 text-[13px] first:border-t-0 hover:bg-surface-hover">
                    <Checkbox
                      checked={chosen.includes(proxy.url)}
                      onCheckedChange={(checked) =>
                        setChosen((prev) => (checked ? [...prev, proxy.url] : prev.filter((url) => url !== proxy.url)))
                      }
                    />
                    <StatusDot tone={proxy.health.abnormal ? "bad" : proxy.health.successes > 0 ? "ok" : "off"} className="min-w-0 flex-1 overflow-hidden">
                      <span className="truncate">{proxyShortName(proxy)}</span>
                    </StatusDot>
                    <span className="text-xs text-ink-3 tabular-nums">{proxy.account_count} 个账号</span>
                  </label>
                ))}
                {visible.length === 0 ? <div className="px-3 py-3 text-[13px] text-ink-3">没有匹配的代理</div> : null}
              </div>
              <span className="text-xs text-ink-3">已选 {chosen.length} 个</span>
            </div>
          ) : null}
          {mode === "chosen" && chosen.length === 0 ? (
            <div className="text-[13px] text-ink-3">先选要分到哪些代理。</div>
          ) : previewing && !plan ? (
            <SkeletonLines lines={3} />
          ) : plan ? (
            <div className={previewing ? "opacity-60" : undefined}>
              <ProxyMovePlanView plan={plan} />
            </div>
          ) : null}
        </div>
        <DialogFooter className="pt-2">
          <Button variant="ghost" onClick={onClose} disabled={applying}>
            取消
          </Button>
          <Button onClick={() => void apply()} disabled={applying || previewing || !plan || Boolean(plan.error) || plan.moves.length === 0}>
            {applying ? <LoaderCircle className="size-4 animate-spin" /> : null}
            {plan && !plan.error && plan.moves.length > 0 ? `迁移 ${plan.moves.length} 个账号` : "重新分配"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
