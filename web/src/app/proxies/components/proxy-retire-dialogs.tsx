"use client";

import { useEffect, useState } from "react";
import { LoaderCircle } from "lucide-react";
import { toast } from "sonner";

import { ProxyMovePlanView } from "@/components/proxy-move-plan";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { SkeletonLines } from "@/components/ui/states";
import { checkProxies, retireProxies, type ProxyCheckResult, type ProxyEntry, type ProxyRetirePlan, type ProxyTestResult } from "@/lib/api";

async function confirmRetire(keys: string[], onDone: (items: ProxyEntry[]) => void) {
  const data = await retireProxies(keys, false);
  onDone(data.items);
  toast.success(`已停用 ${data.disabled ?? keys.length} 个代理，迁移 ${data.moved ?? 0} 个账号`);
}

/** 迁移账号并停用：先预演方案，确认后执行。 */
export function ProxyRetireDialog({
  keys,
  onClose,
  onDone,
}: {
  /** 要停用的代理 key；空数组表示关闭 */
  keys: string[];
  onClose: () => void;
  onDone: (items: ProxyEntry[]) => void;
}) {
  const [plan, setPlan] = useState<ProxyRetirePlan | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const open = keys.length > 0;

  useEffect(() => {
    if (!open) return;
    let active = true;
    setPlan(null);
    setError("");
    retireProxies(keys, true)
      .then((data) => active && setPlan(data))
      .catch((err) => active && setError(err instanceof Error ? err.message : "生成迁移方案失败"));
    return () => {
      active = false;
    };
    // keys 是新数组时才重新预演
  }, [open, keys.join("|")]);

  const accounts = plan ? plan.retiring.reduce((sum, item) => sum + item.account_count, 0) : 0;
  return (
    <Dialog open={open} onOpenChange={(next) => (!next && !busy ? onClose() : null)}>
      <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-[560px]">
        <DialogHeader className="gap-1.5">
          <DialogTitle className="font-serif text-[22px] font-medium">停用 {keys.length} 个代理？</DialogTitle>
          <DialogDescription className="text-[13px] leading-6 text-ink-2">
            先把这些代理上的账号迁到其它可用代理（账号最少的优先），再停用代理。停用的代理留在清单里，随时可以启用。
          </DialogDescription>
        </DialogHeader>
        {error ? (
          <div className="rounded-[7px] bg-bad-soft px-3 py-2.5 text-[13px] text-bad">{error}</div>
        ) : !plan ? (
          <SkeletonLines lines={3} />
        ) : (
          <div className="grid gap-3">
            <div className="text-xs text-ink-3">
              {plan.retiring.map((item) => `${item.label}（${item.account_count} 个账号）`).join("、")}
            </div>
            {accounts > 0 ? <ProxyMovePlanView plan={plan} /> : <div className="text-[13px] text-ink-2">这些代理上没有账号，直接停用。</div>}
          </div>
        )}
        <DialogFooter className="pt-2">
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            取消
          </Button>
          <Button
            variant="destructive"
            disabled={busy || !plan || Boolean(plan.error)}
            onClick={async () => {
              setBusy(true);
              try {
                await confirmRetire(keys, onDone);
                onClose();
              } catch (err) {
                toast.error(err instanceof Error ? err.message : "停用失败");
              } finally {
                setBusy(false);
              }
            }}
          >
            {busy ? <LoaderCircle className="size-4 animate-spin" /> : null}
            {accounts > 0 ? `迁移 ${accounts} 个账号并停用` : "停用"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}

/** 检测并清理：所有启用的代理测一轮，失败的给出停用方案；大面积失败判定为代理商故障，只提示。 */
export function ProxyCheckDialog({
  open,
  onClose,
  onChecked,
  onDone,
}: {
  open: boolean;
  onClose: () => void;
  onChecked: (items: ProxyEntry[], results: Record<string, ProxyTestResult>) => void;
  onDone: (items: ProxyEntry[]) => void;
}) {
  const [result, setResult] = useState<ProxyCheckResult | null>(null);
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!open) return;
    let active = true;
    setResult(null);
    setError("");
    checkProxies()
      .then((data) => {
        if (!active) return;
        setResult(data);
        onChecked(data.items, data.results);
      })
      .catch((err) => active && setError(err instanceof Error ? err.message : "检测失败"));
    return () => {
      active = false;
    };
    // 每次打开检测一次
  }, [open]);

  const failedKeys = result?.failures.map((item) => item.key) ?? [];
  const accounts = result?.failures.reduce((sum, item) => sum + item.account_count, 0) ?? 0;
  const canApply = Boolean(result && !result.outage && result.plan && !result.plan.error && failedKeys.length > 0);

  return (
    <Dialog open={open} onOpenChange={(next) => (!next && !busy ? onClose() : null)}>
      <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-[600px]">
        <DialogHeader className="gap-1.5">
          <DialogTitle className="font-serif text-[22px] font-medium">检测并清理</DialogTitle>
          <DialogDescription className="text-[13px] leading-6 text-ink-2">
            所有启用的代理连一次 chatgpt.com，失败的（连不上、超时、402、被 Cloudflare 拦截）停用，账号迁到其它代理。
          </DialogDescription>
        </DialogHeader>
        {error ? (
          <div className="rounded-[7px] bg-bad-soft px-3 py-2.5 text-[13px] text-bad">{error}</div>
        ) : !result ? (
          <div className="grid gap-2">
            <div className="flex items-center gap-2 text-[13px] text-ink-2">
              <LoaderCircle className="size-4 animate-spin" />
              正在检测，代理多时需要半分钟到一分钟…
            </div>
            <SkeletonLines lines={3} />
          </div>
        ) : (
          <div className="grid gap-3">
            <div className="text-[13px] text-ink-2">
              检测 <b className="font-medium text-ink">{result.tested}</b> 个，通过 <b className="font-medium text-ok">{result.passed}</b> 个，失败{" "}
              <b className={result.failures.length ? "font-medium text-bad" : "font-medium text-ink"}>{result.failures.length}</b> 个。
            </div>
            {result.failures.length === 0 ? (
              <div className="rounded-[7px] bg-ok-soft px-3 py-2.5 text-[13px] text-ok">全部通过，不需要清理。</div>
            ) : (
              <>
                {result.outage ? (
                  <div className="rounded-[7px] bg-warn-soft px-3 py-2.5 text-[13px] leading-6 text-ink">
                    大面积失败，多半是代理商的问题（欠费、流量用完或整体故障），不是个别代理坏了。这时停用代理也没有好代理接手，先检查代理商账户，恢复后再检测。
                  </div>
                ) : null}
                <ul className="thin-scrollbar max-h-36 space-y-1 overflow-y-auto rounded-[7px] bg-sunken px-3 py-2 text-xs">
                  {result.failures.map((item) => (
                    <li key={item.key} className="grid grid-cols-[minmax(0,1fr)_auto] gap-3">
                      <span className="truncate text-ink" title={item.error}>
                        {item.label} <span className="text-bad">· {item.error}</span>
                      </span>
                      <span className="whitespace-nowrap text-ink-3">{item.account_count} 个账号</span>
                    </li>
                  ))}
                </ul>
                {!result.outage && result.plan && accounts > 0 ? <ProxyMovePlanView plan={result.plan} /> : null}
              </>
            )}
          </div>
        )}
        <DialogFooter className="pt-2">
          <Button variant="ghost" onClick={onClose} disabled={busy}>
            {canApply ? "取消" : "关闭"}
          </Button>
          {canApply ? (
            <Button
              variant="destructive"
              disabled={busy}
              onClick={async () => {
                setBusy(true);
                try {
                  await confirmRetire(failedKeys, onDone);
                  onClose();
                } catch (err) {
                  toast.error(err instanceof Error ? err.message : "停用失败");
                } finally {
                  setBusy(false);
                }
              }}
            >
              {busy ? <LoaderCircle className="size-4 animate-spin" /> : null}
              停用 {failedKeys.length} 个{accounts > 0 ? `并迁移 ${accounts} 个账号` : ""}
            </Button>
          ) : null}
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
