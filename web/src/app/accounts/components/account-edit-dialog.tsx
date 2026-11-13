"use client";

import { useEffect, useState } from "react";
import { Link2, LoaderCircle } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { ACCOUNT_STATUS_OPTIONS, accountStatusError } from "@/lib/account-format";
import { testProxy, updateAccount, type Account, type AccountStatus, type ProxyEntry } from "@/lib/api";
import { proxyForAccount } from "@/lib/proxy-match";

import { proxyLabel } from "./account-import-dialog";

const NO_PROXY = "__none__";
const NEW_PROXY = "__new__";

/** 编辑账号：状态、代理（清单里选 / 手填新代理 / 走全局）、测试代理、删除。 */
export function AccountEditDialog({
  account,
  proxies,
  onClose,
  onSaved,
  onDelete,
}: {
  account: Account | null;
  proxies: ProxyEntry[];
  onClose: () => void;
  onSaved: (items: Account[]) => void;
  onDelete: (token: string) => void;
}) {
  const [status, setStatus] = useState<AccountStatus>("正常");
  // NO_PROXY 走全局代理，NEW_PROXY 手填，其余为代理清单的 key
  const [proxyChoice, setProxyChoice] = useState(NO_PROXY);
  const [customProxy, setCustomProxy] = useState("");
  const [isTesting, setIsTesting] = useState(false);
  const [isSaving, setIsSaving] = useState(false);

  useEffect(() => {
    if (!account) return;
    const byKey = new Map(proxies.map((proxy) => [proxy.key, proxy]));
    const proxy = (account.proxy ?? "").trim();
    const listed = proxyForAccount(byKey, account);
    setStatus(account.status);
    setProxyChoice(!proxy ? NO_PROXY : listed ? listed.key : NEW_PROXY);
    setCustomProxy(listed ? "" : proxy);
    // 只在打开另一个账号时重置表单；代理清单刷新不应清掉正在编辑的内容
  }, [account?.access_token]);

  /** 最终要写到账号上的代理；选的还是账号原来那条时保留原写法。 */
  const resolveProxy = () => {
    if (proxyChoice === NO_PROXY) return "";
    if (proxyChoice === NEW_PROXY) return customProxy.trim();
    if (account?.proxy_key === proxyChoice) return (account.proxy ?? "").trim();
    return proxies.find((proxy) => proxy.key === proxyChoice)?.url ?? "";
  };

  const handleTest = async () => {
    const candidate = resolveProxy();
    if (!candidate) {
      toast.error("请先填写代理地址");
      return;
    }
    setIsTesting(true);
    try {
      const data = await testProxy(candidate);
      if (data.result.ok) {
        toast.success(`代理可用（${data.result.latency_ms} ms，HTTP ${data.result.status}）`);
      } else {
        toast.error(`代理不可用：${data.result.error ?? "未知错误"}`);
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "测试代理失败");
    } finally {
      setIsTesting(false);
    }
  };

  const handleSave = async () => {
    if (!account) return;
    setIsSaving(true);
    try {
      const data = await updateAccount(account.access_token, { status, proxy: resolveProxy() });
      onSaved(data.items);
      toast.success("账号信息已更新");
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "更新账号失败");
    } finally {
      setIsSaving(false);
    }
  };

  const reason = account ? accountStatusError(account) : "";

  return (
    <Dialog open={Boolean(account)} onOpenChange={(open) => (!open && !isSaving ? onClose() : null)}>
      <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-[520px]">
        <DialogHeader className="gap-1">
          <DialogTitle className="font-serif text-[22px] font-medium">编辑账号</DialogTitle>
          <DialogDescription className="truncate font-mono text-xs text-ink-2">{account?.email || "—"}</DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <label className="grid gap-1.5 text-[13px] text-ink-2">
            状态
            <Select value={status} onValueChange={(value) => setStatus(value as AccountStatus)}>
              <SelectTrigger className="h-[38px] rounded-[7px]"><SelectValue /></SelectTrigger>
              <SelectContent>
                {ACCOUNT_STATUS_OPTIONS.filter((option) => option.value !== "all" && option.value !== "edit_cooldown").map((option) => (
                  <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>
                ))}
              </SelectContent>
            </Select>
          </label>
          <div className="grid gap-1.5 text-[13px] text-ink-2">
            代理
            <Select value={proxyChoice} onValueChange={setProxyChoice}>
              <SelectTrigger className="h-[38px] w-full min-w-0 rounded-[7px]"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value={NO_PROXY}>不指定，走全局代理</SelectItem>
                {proxies.map((proxy) => (
                  <SelectItem key={proxy.key} value={proxy.key}>
                    {proxyLabel(proxy)} · {proxy.account_count} 个账号
                    {proxy.health.abnormal ? "（异常）" : ""}
                  </SelectItem>
                ))}
                <SelectItem value={NEW_PROXY}>使用新代理</SelectItem>
              </SelectContent>
            </Select>
            {proxyChoice === NEW_PROXY ? (
              <Input value={customProxy} onChange={(event) => setCustomProxy(event.target.value)} placeholder="http://user:pass@host:port" className="h-[38px] rounded-[7px]" />
            ) : null}
          </div>
          <div className="flex items-center justify-between gap-3">
            <span className="min-w-0 truncate text-xs text-ink-3" title={reason || undefined}>
              {reason ? `异常原因：${reason}` : "测试会经所选代理访问 chatgpt.com"}
            </span>
            <Button variant="outline" size="sm" onClick={() => void handleTest()} disabled={isTesting}>
              {isTesting ? <LoaderCircle className="size-4 animate-spin" /> : <Link2 className="size-4" />}
              测试代理
            </Button>
          </div>
        </div>
        <DialogFooter className="pt-2 sm:justify-between">
          <Button variant="outline" className="text-bad" onClick={() => account && onDelete(account.access_token)} disabled={isSaving}>
            删除账号
          </Button>
          <div className="flex gap-2">
            <Button variant="ghost" onClick={onClose} disabled={isSaving}>取消</Button>
            <Button onClick={() => void handleSave()} disabled={isSaving}>
              {isSaving ? <LoaderCircle className="size-4 animate-spin" /> : null}
              保存
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
