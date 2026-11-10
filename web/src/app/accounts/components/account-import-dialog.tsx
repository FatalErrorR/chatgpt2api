"use client";

import { useRouter } from "next/navigation";
import { useEffect, useMemo, useRef, useState, type ChangeEvent } from "react";
import {
  ArrowLeft,
  ChevronRight,
  Copy,
  ExternalLink,
  FileJson,
  FileText,
  Files,
  KeyRound,
  LoaderCircle,
  LogIn,
  ServerCog,
  ShieldCheck,
  Upload,
} from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Card, CardContent } from "@/components/ui/card";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Textarea } from "@/components/ui/textarea";
import {
  createAccounts,
  fetchAccounts,
  fetchProtocolLoginProgress,
  finishOAuthLogin,
  startOAuthLogin,
  startProtocolLogin,
  type Account,
  type AccountImportPayload,
  type ImportProxyChoice,
  type OAuthLoginStartResponse,
  type ProtocolLoginProgress,
  type ProxyEntry,
} from "@/lib/api";
import { previewProxyDistribution } from "@/lib/proxy-distribution";
import { cn } from "@/lib/utils";

type ImportMethod = "menu" | "token" | "session" | "codex-auth" | "account-json" | "oauth" | "protocol";

type AccountImportDialogProps = {
  disabled?: boolean;
  proxies?: ProxyEntry[];
  onImported: (items: Account[]) => void;
};

type PendingAccountJsonImport = {
  tokens: string[];
  accounts: AccountImportPayload[];
  parsedAccountCount: number;
  errorCount: number;
};

const sessionUrl = "https://chatgpt.com/api/auth/session";

function splitTokens(value: string) {
  return value
    .split(/\r?\n/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function getSessionAccessToken(value: unknown) {
  const token = (value as { accessToken?: unknown })?.accessToken;
  return typeof token === "string" ? token.trim() : "";
}

function getAccountJsonAccount(value: unknown): AccountImportPayload | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return null;
  }
  const raw = value as Record<string, unknown>;
  const tokenValue = raw.access_token ?? raw.accessToken;
  const token = typeof tokenValue === "string" ? tokenValue.trim() : "";
  if (!token) {
    return null;
  }

  const payload: AccountImportPayload = {
    ...raw,
    access_token: token,
    source_type: "codex",
  };
  delete payload.accessToken;
  if (payload.type === "codex") {
    payload.export_type = "codex";
    delete payload.type;
  }
  return payload;
}

function getAccountJsonAccounts(value: unknown): AccountImportPayload[] {
  if (Array.isArray(value)) {
    return value
      .map((item) => getAccountJsonAccount(item))
      .filter((item): item is AccountImportPayload => Boolean(item));
  }

  const singleAccount = getAccountJsonAccount(value);
  if (singleAccount) {
    return [singleAccount];
  }

  if (value && typeof value === "object") {
    const raw = value as Record<string, unknown>;
    const nested = raw.accounts ?? raw.items;
    if (Array.isArray(nested)) {
      return nested
        .map((item) => getAccountJsonAccount(item))
        .filter((item): item is AccountImportPayload => Boolean(item));
    }
  }

  return [];
}

function getCodexAuthAccount(value: unknown): AccountImportPayload | null {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return null;
  }
  const raw = value as Record<string, unknown>;
  const tokenValue = raw.access_token ?? raw.accessToken;
  const token = typeof tokenValue === "string" ? tokenValue.trim() : "";
  if (!token) {
    return null;
  }

  const payload: AccountImportPayload = {
    ...raw,
    access_token: token,
    export_type: "codex",
    source_type: "codex",
  };
  delete payload.accessToken;
  if (payload.type === "codex") {
    delete payload.type;
  }
  return payload;
}

function readFileAsText(file: File) {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onload = () => resolve(typeof reader.result === "string" ? reader.result : "");
    reader.onerror = () => reject(reader.error ?? new Error(`读取文件失败: ${file.name}`));
    reader.readAsText(file);
  });
}

function MethodCard({
  title,
  description,
  icon: Icon,
  onClick,
  recommended = false,
}: {
  title: string;
  description: string;
  icon: typeof KeyRound;
  onClick: () => void;
  recommended?: boolean;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "grid w-full grid-cols-[20px_1fr_auto] items-center gap-3 rounded-[7px] border px-3.5 py-2.5 text-left transition",
        recommended ? "border-clay bg-clay-soft hover:bg-clay-soft/80" : "border-line bg-surface hover:bg-surface-hover",
      )}
    >
      <Icon className="size-4 text-ink-2" />
      <span className="min-w-0">
        <span className="block text-[13px] font-medium text-ink">
          {title}
          {recommended ? <span className="ml-2 text-xs font-normal text-clay-hover">推荐</span> : null}
        </span>
        <span className="block text-xs leading-5 text-ink-3">{description}</span>
      </span>
      <ChevronRight className="size-4 text-ink-3" />
    </button>
  );
}

function MethodGroup({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="space-y-1.5">
      <div className="pt-1 text-[11px] tracking-[0.06em] text-ink-3">{label}</div>
      {children}
    </div>
  );
}

export function proxyLabel(proxy: ProxyEntry) {
  return proxy.name ? `${proxy.name}（${proxy.masked_url}）` : proxy.masked_url;
}

export function AccountImportDialog({ disabled, proxies = [], onImported }: AccountImportDialogProps) {
  const router = useRouter();
  const [open, setOpen] = useState(false);
  const [method, setMethod] = useState<ImportMethod>("menu");
  const [tokenInput, setTokenInput] = useState("");
  const [sessionInput, setSessionInput] = useState("");
  const [codexAuthInput, setCodexAuthInput] = useState("");
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [pendingAccountJsonImport, setPendingAccountJsonImport] = useState<PendingAccountJsonImport | null>(null);
  const [confirmOpen, setConfirmOpen] = useState(false);
  const [oauthEmailHint, setOauthEmailHint] = useState("");
  const [oauthSession, setOauthSession] = useState<OAuthLoginStartResponse | null>(null);
  const [oauthCallbackInput, setOauthCallbackInput] = useState("");
  const [oauthStarting, setOauthStarting] = useState(false);
  const [proxyChoice, setProxyChoice] = useState("none");
  const [newProxy, setNewProxy] = useState("");
  const [distributeUrls, setDistributeUrls] = useState<string[]>([]);
  const [protocolInput, setProtocolInput] = useState("");
  const [protocolProgress, setProtocolProgress] = useState<ProtocolLoginProgress | null>(null);
  const [protocolRunning, setProtocolRunning] = useState(false);
  const protocolAbortRef = useRef(false);

  // 组件卸载时停止轮询
  useEffect(() => {
    return () => {
      protocolAbortRef.current = true;
    };
  }, []);

  const newAccountCount = useMemo(() => {
    if (method === "token") return splitTokens(tokenInput).length;
    if (method === "protocol") return protocolInput.split(/\r?\n/).map((line) => line.trim()).filter(Boolean).length;
    if (method === "account-json") return pendingAccountJsonImport?.parsedAccountCount ?? 0;
    if (method === "session") {
      try {
        return getSessionAccessToken(JSON.parse(sessionInput)) ? 1 : 0;
      } catch {
        return 0;
      }
    }
    if (method === "codex-auth") {
      try {
        return getCodexAuthAccount(JSON.parse(codexAuthInput)) ? 1 : 0;
      } catch {
        return 0;
      }
    }
    if (method === "oauth") return oauthSession ? 1 : 0;
    return 0;
  }, [method, tokenInput, protocolInput, pendingAccountJsonImport, sessionInput, codexAuthInput, oauthSession]);

  const distributionPreview = useMemo(() => {
    const selected = distributeUrls.flatMap((url) => {
      const proxy = proxies.find((item) => item.url === url);
      return proxy ? [{ url: proxy.url, accountCount: proxy.account_count, abnormal: Boolean(proxy.health.abnormal) }] : [];
    });
    return previewProxyDistribution(selected, newAccountCount);
  }, [distributeUrls, proxies, newAccountCount]);

  const txtInputRef = useRef<HTMLInputElement | null>(null);
  const accountJsonInputRef = useRef<HTMLInputElement | null>(null);

  const resetState = () => {
    setMethod("menu");
    setTokenInput("");
    setSessionInput("");
    setCodexAuthInput("");
    setPendingAccountJsonImport(null);
    setConfirmOpen(false);
    setOauthEmailHint("");
    setOauthSession(null);
    setOauthCallbackInput("");
    setOauthStarting(false);
    setProxyChoice("none");
    setNewProxy("");
    setDistributeUrls([]);
    protocolAbortRef.current = true;
    setProtocolInput("");
    setProtocolProgress(null);
    setProtocolRunning(false);
  };

  const importProxy = (): ImportProxyChoice | null => {
    if (proxyChoice === "none") {
      return { proxy: "", distribute_proxies: [] };
    }
    if (proxyChoice === "distribute") {
      if (distributeUrls.length === 0) {
        toast.error("平均分配至少要选一个代理");
        return null;
      }
      return { proxy: "", distribute_proxies: distributeUrls };
    }
    if (proxyChoice === "new") {
      const url = newProxy.trim();
      if (!url) {
        toast.error("请填写新代理地址");
        return null;
      }
      return { proxy: url, distribute_proxies: [] };
    }
    return { proxy: proxyChoice, distribute_proxies: [] };
  };

  const handleProxyChoiceChange = (value: string) => {
    setProxyChoice(value);
    if (value === "distribute") {
      // 默认勾选所有正常代理
      setDistributeUrls(proxies.filter((proxy) => !proxy.health.abnormal).map((proxy) => proxy.url));
    }
  };

  const toggleDistributeUrl = (url: string, checked: boolean) => {
    setDistributeUrls((prev) => (checked ? [...prev, url] : prev.filter((item) => item !== url)));
  };

  const handleOpenChange = (nextOpen: boolean) => {
    setOpen(nextOpen);
    if (!nextOpen) {
      resetState();
    }
  };

  const submitTokens = async (tokens: string[], successText?: string, accountPayloads: AccountImportPayload[] = []) => {
    const normalizedTokens = tokens.map((item) => item.trim()).filter(Boolean);

    if (normalizedTokens.length === 0) {
      toast.error("请先提供至少一个可用 Token");
      return;
    }

    const proxy = importProxy();
    if (proxy === null) {
      return;
    }

    setIsSubmitting(true);
    try {
      const data = await createAccounts(normalizedTokens, accountPayloads, proxy);
      onImported(data.items);
      setOpen(false);
      resetState();

      if ((data.errors?.length ?? 0) > 0) {
        const firstError = data.errors?.[0]?.error;
        toast.error(
          `${successText ?? "导入完成"}，新增 ${data.added ?? 0} 个，已刷新 ${data.refreshed ?? 0} 个，失败 ${data.errors?.length ?? 0} 个${firstError ? `，首个错误：${firstError}` : ""}`,
        );
      } else {
        toast.success(
          `${successText ?? "导入完成"}，新增 ${data.added ?? 0} 个，跳过 ${data.skipped ?? 0} 个重复项，已自动刷新账号信息`,
        );
      }
    } catch (error) {
      const message = error instanceof Error ? error.message : "导入账户失败";
      toast.error(message);
    } finally {
      setIsSubmitting(false);
    }
  };

  const handleImportTokenText = async () => {
    await submitTokens(splitTokens(tokenInput), "Access Token 导入完成");
  };

  // 起授权：拿 authorize URL，立刻在新窗口打开，方便用户登录
  const handleStartOAuth = async () => {
    setOauthStarting(true);
    try {
      const data = await startOAuthLogin(oauthEmailHint.trim());
      setOauthSession(data);
      setOauthCallbackInput("");
      if (typeof window !== "undefined") {
        window.open(data.authorize_url, "_blank", "noopener,noreferrer");
      }
      toast.success("已打开 OpenAI 授权页面，请在登录后复制 callback URL 回来");
    } catch (error) {
      const message = error instanceof Error ? error.message : "OAuth 起始失败";
      toast.error(message);
    } finally {
      setOauthStarting(false);
    }
  };

  // 用粘贴回来的 callback URL 完成换 token + 落盘
  const handleStartProtocolLogin = async () => {
    if (!protocolInput.trim()) {
      toast.error("请先粘贴账号，每行一个：邮箱----密码----TOTP密钥");
      return;
    }
    const proxy = importProxy();
    if (proxy === null) {
      return;
    }

    protocolAbortRef.current = false;
    setProtocolRunning(true);
    setProtocolProgress(null);
    try {
      const started = await startProtocolLogin(protocolInput, proxy);
      if (started.total === 0) {
        toast.error(started.invalid[0]?.error ?? "没有识别到有效账号");
        setProtocolProgress({
          id: started.progress_id,
          status: "done",
          total: 0,
          completed: 0,
          success: 0,
          failed: 0,
          invalid: started.invalid,
          items: [],
        });
        return;
      }
      let progress = await fetchProtocolLoginProgress(started.progress_id);
      setProtocolProgress(progress);
      while (progress.status !== "done" && !protocolAbortRef.current) {
        await new Promise((resolve) => setTimeout(resolve, 2000));
        if (protocolAbortRef.current) {
          return;
        }
        progress = await fetchProtocolLoginProgress(started.progress_id);
        setProtocolProgress(progress);
      }
      if (protocolAbortRef.current) {
        return;
      }
      const list = await fetchAccounts();
      onImported(list.items);
      const invalidText = progress.invalid.length > 0 ? `，${progress.invalid.length} 行格式无效` : "";
      if (progress.failed > 0) {
        toast.error(`协议登录完成：成功 ${progress.success} 个，失败 ${progress.failed} 个${invalidText}`);
      } else {
        toast.success(`协议登录完成：成功 ${progress.success} 个${invalidText}`);
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "批量协议登录失败");
    } finally {
      setProtocolRunning(false);
    }
  };

  const handleFinishOAuth = async () => {
    if (!oauthSession) {
      toast.error("请先点击\"打开授权页面\"获取 session");
      return;
    }
    const trimmed = oauthCallbackInput.trim();
    if (!trimmed) {
      toast.error("请粘贴 callback URL 或 code");
      return;
    }

    const proxy = importProxy();
    if (proxy === null) {
      return;
    }

    setIsSubmitting(true);
    try {
      const data = await finishOAuthLogin(oauthSession.session_id, trimmed, proxy);
      onImported(data.items);
      setOpen(false);
      resetState();

      if ((data.errors?.length ?? 0) > 0) {
        const firstError = data.errors?.[0]?.error;
        toast.error(
          `OAuth 登录完成，新增 ${data.added ?? 0} 个，已刷新 ${data.refreshed ?? 0} 个，失败 ${data.errors?.length ?? 0} 个${firstError ? `，首个错误：${firstError}` : ""}`,
        );
      } else {
        toast.success(
          `OAuth 登录完成，新增 ${data.added ?? 0} 个，跳过 ${data.skipped ?? 0} 个重复项，已自动刷新账号信息`,
        );
      }
    } catch (error) {
      const message = error instanceof Error ? error.message : "OAuth 换 token 失败";
      toast.error(message);
    } finally {
      setIsSubmitting(false);
    }
  };

  // 复制 authorize URL 到剪贴板（适配浏览器和 fallback）
  const handleCopyAuthorizeUrl = async () => {
    if (!oauthSession) {
      return;
    }
    try {
      if (navigator.clipboard?.writeText) {
        await navigator.clipboard.writeText(oauthSession.authorize_url);
        toast.success("授权 URL 已复制到剪贴板");
      } else {
        toast.error("当前环境不支持自动复制，请手动选择并复制");
      }
    } catch {
      toast.error("复制失败，请手动选择并复制");
    }
  };

  const handleTxtSelected = async (event: ChangeEvent<HTMLInputElement>) => {
    const file = event.target.files?.[0];
    event.target.value = "";

    if (!file) {
      return;
    }

    try {
      const content = await readFileAsText(file);
      const tokens = splitTokens(content);

      if (tokens.length === 0) {
        toast.error("TXT 文件里没有读取到有效 Token");
        return;
      }

      setTokenInput((prev) => {
        const next = [...splitTokens(prev), ...tokens];
        return next.join("\n");
      });
      toast.success(`已从 ${file.name} 读取 ${tokens.length} 个 Token`);
    } catch (error) {
      const message = error instanceof Error ? error.message : "读取 TXT 文件失败";
      toast.error(message);
    }
  };

  const handleImportSessionJson = async () => {
    if (!sessionInput.trim()) {
      toast.error("请先粘贴完整 Session JSON");
      return;
    }

    try {
      const payload = JSON.parse(sessionInput) as unknown;
      const token = getSessionAccessToken(payload);

      if (!token) {
        toast.error("未从 Session JSON 中提取到 accessToken");
        return;
      }

      await submitTokens([token], "Session JSON 导入完成");
    } catch (error) {
      const message = error instanceof Error ? error.message : "Session JSON 解析失败";
      toast.error(message);
    }
  };

  const handleImportCodexAuthJson = async () => {
    if (!codexAuthInput.trim()) {
      toast.error("请先粘贴 Codex 认证 JSON");
      return;
    }

    try {
      const payload = JSON.parse(codexAuthInput) as unknown;
      const account = getCodexAuthAccount(payload);

      if (!account) {
        toast.error("未从 Codex 认证 JSON 中提取到 access_token");
        return;
      }

      await submitTokens([account.access_token], "Codex 认证 JSON 导入完成", [account]);
    } catch (error) {
      const message = error instanceof Error ? error.message : "Codex 认证 JSON 解析失败";
      toast.error(message);
    }
  };

  const handleAccountJsonSelected = async (event: ChangeEvent<HTMLInputElement>) => {
    const files = Array.from(event.target.files ?? []);
    event.target.value = "";

    if (files.length === 0) {
      return;
    }

    try {
      const results = await Promise.all(
        files.map(async (file) => {
          const raw = await readFileAsText(file);
          const parsed = JSON.parse(raw) as unknown;
          const accounts = getAccountJsonAccounts(parsed);
          return {
            accounts,
          };
        }),
      );

      const accounts = results.flatMap((item) => item.accounts);
      const tokens = accounts.map((item) => item.access_token);
      const parsedAccountCount = accounts.length;
      const errorCount = results.filter((item) => item.accounts.length === 0).length;

      if (parsedAccountCount === 0) {
        toast.error("这些账号 JSON 文件里没有读取到可用 access_token");
        return;
      }

      setPendingAccountJsonImport({
        tokens,
        accounts,
        parsedAccountCount,
        errorCount,
      });
      setConfirmOpen(true);
    } catch (error) {
      const message = error instanceof Error ? error.message : "读取账号 JSON 文件失败";
      toast.error(message);
    }
  };

  const renderMethodBody = () => {
    if (method === "token") {
      const tokenCount = splitTokens(tokenInput).length;

      return (
        <div className="space-y-4">
          <div className="flex items-center justify-between">
            <button
              type="button"
              onClick={() => setMethod("menu")}
              className="inline-flex items-center gap-1 text-sm text-ink-3 transition hover:text-ink"
            >
              <ArrowLeft className="size-4" />
              返回导入方式
            </button>
            <span className="text-xs text-ink-3">当前识别 {tokenCount} 个 Token</span>
          </div>
          <div className="space-y-2">
            <label className="text-sm font-medium text-ink-2">Access Token 列表</label>
            <Textarea
              placeholder="每行一个 Access Token..."
              value={tokenInput}
              onChange={(event) => setTokenInput(event.target.value)}
              className="min-h-56 resize-none rounded-[7px] border-line"
            />
          </div>
          <div className="rounded-[10px] border border-dashed border-line bg-sunken p-4">
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <div className="space-y-1">
                <div className="text-sm font-medium text-ink">从 TXT 文件导入</div>
                <div className="text-sm leading-6 text-ink-3">支持 `.txt`，文件内容也是一行一个 Token。</div>
              </div>
              <Button
                type="button"
                variant="outline"
                className="rounded-[7px] border-line bg-surface"
                onClick={() => txtInputRef.current?.click()}
                disabled={isSubmitting}
              >
                <FileText className="size-4" />
                选择 TXT
              </Button>
            </div>
          </div>
          <input
            ref={txtInputRef}
            type="file"
            accept=".txt,text/plain"
            className="hidden"
            onChange={(event) => void handleTxtSelected(event)}
          />
        </div>
      );
    }

    if (method === "session") {
      return (
        <div className="space-y-4">
          <button
            type="button"
            onClick={() => setMethod("menu")}
            className="inline-flex items-center gap-1 text-sm text-ink-3 transition hover:text-ink"
          >
            <ArrowLeft className="size-4" />
            返回导入方式
          </button>
          <div className="rounded-[10px] border border-line bg-sunken p-4 text-sm leading-6 text-ink-2">
            打开
            {" "}
            <a
              href={sessionUrl}
              target="_blank"
              rel="noreferrer"
              className="inline-flex items-center gap-1 font-medium text-ink underline underline-offset-4"
            >
              {sessionUrl}
              <ExternalLink className="size-3.5" />
            </a>
            ，复制页面返回的完整 JSON，系统会自动提取其中的 `accessToken` 导入。
          </div>
          <div className="rounded-[10px] border border-amber-200 bg-amber-50 p-4 text-sm leading-6 text-amber-900">
            <div className="font-medium">风险提示</div>
            <div>
              不要使用自己的大号，尽量使用不常用的小号进行导入，避免出现封号风险。本项目不承担任何封号风险责任。
            </div>
          </div>
          <div className="space-y-2">
            <label className="text-sm font-medium text-ink-2">Session JSON</label>
            <Textarea
              placeholder='粘贴完整 JSON，例如包含 "accessToken" 的对象...'
              value={sessionInput}
              onChange={(event) => setSessionInput(event.target.value)}
              className="min-h-56 resize-none rounded-[7px] border-line font-mono text-xs"
            />
          </div>
        </div>
      );
    }

    if (method === "protocol") {
      const lineCount = protocolInput
        .split(/\r?\n/)
        .filter((line) => line.trim() && !line.trim().startsWith("#")).length;

      return (
        <div className="space-y-4">
          <div className="flex items-center justify-between">
            <button
              type="button"
              onClick={() => setMethod("menu")}
              disabled={protocolRunning}
              className="inline-flex items-center gap-1 text-sm text-ink-3 transition hover:text-ink disabled:opacity-50"
            >
              <ArrowLeft className="size-4" />
              返回导入方式
            </button>
            <span className="text-xs text-ink-3">当前识别 {lineCount} 行</span>
          </div>
          <div className="space-y-2">
            <label className="text-sm font-medium text-ink-2">账号列表</label>
            <Textarea
              placeholder={"邮箱----密码----TOTP密钥\n没开二步验证的账号可省略 TOTP 密钥：邮箱----密码"}
              value={protocolInput}
              onChange={(event) => setProtocolInput(event.target.value)}
              disabled={protocolRunning}
              className="min-h-40 resize-none rounded-[7px] border-line font-mono text-xs"
            />
            <p className="text-xs leading-5 text-ink-3">
              后台直接走授权协议登录，成功后写入号池并自动刷新额度；同邮箱的账号会更新 token，不会重复添加。一次最多 200 个。
            </p>
          </div>
          {protocolProgress ? (
            <div className="space-y-2 rounded-[10px] border border-line bg-sunken p-3">
              <div className="text-sm font-medium text-ink">
                进度 {protocolProgress.completed}/{protocolProgress.total}，成功 {protocolProgress.success}，失败{" "}
                {protocolProgress.failed}
              </div>
              <div className="max-h-44 space-y-1 overflow-y-auto text-xs">
                {protocolProgress.items.map((item) => (
                  <div key={item.email} className="flex items-start justify-between gap-3">
                    <span className="truncate text-ink-2">{item.email}</span>
                    <span
                      className={cn(
                        "shrink-0",
                        item.status === "成功" ? "text-emerald-600" : "",
                        item.status === "失败" ? "text-bad" : "",
                        item.status === "登录中" ? "text-amber-600" : "",
                        item.status === "等待" ? "text-ink-3" : "",
                      )}
                    >
                      {item.status}
                      {item.error ? `：${item.error}` : ""}
                    </span>
                  </div>
                ))}
                {protocolProgress.invalid.map((item) => (
                  <div key={`invalid-${item.line}`} className="flex items-start justify-between gap-3 text-bad">
                    <span className="truncate">第 {item.line} 行{item.email ? ` ${item.email}` : ""}</span>
                    <span className="shrink-0">{item.error}</span>
                  </div>
                ))}
              </div>
            </div>
          ) : null}
        </div>
      );
    }

    if (method === "oauth") {
      return (
        <div className="space-y-4">
          <button
            type="button"
            onClick={() => setMethod("menu")}
            className="inline-flex items-center gap-1 text-sm text-ink-3 transition hover:text-ink"
          >
            <ArrowLeft className="size-4" />
            返回导入方式
          </button>
          <div className="rounded-[10px] border border-line bg-sunken p-4 text-sm leading-6 text-ink-2 space-y-2">
            <div className="font-medium text-ink">操作步骤</div>
            <ol className="list-decimal pl-5 space-y-1">
              <li>（可选）填写你 ChatGPT 账号的邮箱，登录页会预填。</li>
              <li>点击下方"打开授权页面"，在新标签里登录自己的 ChatGPT 账号。</li>
              <li>登录完成后浏览器会跳到 <code className="rounded bg-sunken px-1">platform.openai.com/auth/callback?code=...</code>。立刻从地址栏复制整段 URL（或开 F12 在 Network 里抓到 callback 那一行，右键 Copy → Copy URL）。</li>
              <li>把 callback URL 粘到下面输入框，点"完成导入"。</li>
            </ol>
          </div>
          <div className="space-y-2">
            <label className="text-sm font-medium text-ink-2">邮箱（可选预填）</label>
            <input
              type="email"
              placeholder="you@example.com"
              value={oauthEmailHint}
              onChange={(event) => setOauthEmailHint(event.target.value)}
              disabled={Boolean(oauthSession) || oauthStarting}
              className="w-full rounded-[7px] border border-line bg-surface px-3 py-2 text-sm outline-none focus:border-clay"
            />
          </div>
          {!oauthSession ? (
            <Button
              type="button"
              className="h-10 rounded-[7px] bg-ink text-page hover:bg-ink/85"
              onClick={() => void handleStartOAuth()}
              disabled={oauthStarting}
            >
              {oauthStarting ? <LoaderCircle className="size-4 animate-spin" /> : <ExternalLink className="size-4" />}
              打开授权页面
            </Button>
          ) : (
            <div className="space-y-3">
              <div className="rounded-[10px] border border-line bg-surface p-3 text-xs leading-6 text-ink-2 break-all font-mono">
                {oauthSession.authorize_url}
              </div>
              <div className="flex flex-wrap gap-2">
                <Button
                  type="button"
                  variant="outline"
                  className="rounded-[7px] border-line bg-surface"
                  onClick={() => void handleCopyAuthorizeUrl()}
                >
                  <Copy className="size-4" />
                  复制授权 URL
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  className="rounded-[7px] border-line bg-surface"
                  onClick={() => window.open(oauthSession.authorize_url, "_blank", "noopener,noreferrer")}
                >
                  <ExternalLink className="size-4" />
                  再次打开
                </Button>
                <Button
                  type="button"
                  variant="outline"
                  className="rounded-[7px] border-line bg-surface"
                  onClick={() => {
                    setOauthSession(null);
                    setOauthCallbackInput("");
                  }}
                >
                  重新生成
                </Button>
              </div>
              <div className="space-y-2">
                <label className="text-sm font-medium text-ink-2">粘贴 callback URL（或仅 code）</label>
                <Textarea
                  placeholder={"https://platform.openai.com/auth/callback?code=...&state=..."}
                  value={oauthCallbackInput}
                  onChange={(event) => setOauthCallbackInput(event.target.value)}
                  className="min-h-24 resize-none rounded-[7px] border-line font-mono text-xs"
                />
              </div>
            </div>
          )}
          <div className="rounded-[10px] border border-amber-200 bg-amber-50 p-4 text-sm leading-6 text-amber-900">
            <div className="font-medium">注意</div>
            <div>
              授权码（code）只能使用一次。如果浏览器的 callback 页加载完成、显示了 OpenAI 的错误页，那 code 大概率已经被消耗，
              请点击"重新生成"再走一次。整个流程在 10 分钟内完成即可。
            </div>
          </div>
        </div>
      );
    }

    if (method === "account-json") {
      return (
        <div className="space-y-4">
          <button
            type="button"
            onClick={() => setMethod("menu")}
            className="inline-flex items-center gap-1 text-sm text-ink-3 transition hover:text-ink"
          >
            <ArrowLeft className="size-4" />
            返回导入方式
          </button>
          <div className="rounded-[10px] border border-dashed border-line bg-sunken p-5">
            <div className="space-y-2">
              <div className="text-sm font-medium text-ink">选择本地账号 JSON 文件</div>
              <div className="text-sm leading-6 text-ink-3">
                支持本项目导出的单账号对象或全部账号数组，也兼容每个文件一个账号对象的 CPA JSON。
                系统会自动提取 `access_token` 或 `accessToken`。
              </div>
            </div>
            <Button
              type="button"
              className="mt-4 rounded-[7px] bg-ink text-page hover:bg-ink/85"
              onClick={() => accountJsonInputRef.current?.click()}
              disabled={isSubmitting}
            >
              <Files className="size-4" />
              选择 JSON 文件
            </Button>
          </div>
          <input
            ref={accountJsonInputRef}
            type="file"
            accept=".json,application/json"
            multiple
            className="hidden"
            onChange={(event) => void handleAccountJsonSelected(event)}
          />
          {pendingAccountJsonImport ? (
            <div className="rounded-[10px] border border-line bg-surface p-4 text-sm leading-6 text-ink-2">
              最近一次读取到 {pendingAccountJsonImport.parsedAccountCount} 个 Token
              {pendingAccountJsonImport.errorCount > 0 ? `，另有 ${pendingAccountJsonImport.errorCount} 个文件未提取成功` : ""}。
            </div>
          ) : null}
        </div>
      );
    }

    if (method === "codex-auth") {
      return (
        <div className="space-y-4">
          <button
            type="button"
            onClick={() => setMethod("menu")}
            className="inline-flex items-center gap-1 text-sm text-ink-3 transition hover:text-ink"
          >
            <ArrowLeft className="size-4" />
            返回导入方式
          </button>
          <div className="space-y-2">
            <label className="text-sm font-medium text-ink-2">Codex 认证 JSON</label>
            <Textarea
              placeholder='粘贴包含 "access_token"、"refresh_token"、"id_token" 的 Codex 认证 JSON...'
              value={codexAuthInput}
              onChange={(event) => setCodexAuthInput(event.target.value)}
              className="min-h-64 resize-none rounded-[7px] border-line font-mono text-xs"
            />
          </div>
        </div>
      );
    }

    const toSettings = () => {
      setOpen(false);
      resetState();
      router.push("/settings");
    };
    return (
      <div className="space-y-3">
        <MethodGroup label="登录">
          <MethodCard
            recommended
            title="批量协议登录"
            description="每行「邮箱----密码----TOTP 密钥」，自动登录、入池并续期"
            icon={ShieldCheck}
            onClick={() => setMethod("protocol")}
          />
          <MethodCard
            title="OAuth 登录"
            description="浏览器登录后回填 callback，拿到 refresh_token 自动续期"
            icon={LogIn}
            onClick={() => setMethod("oauth")}
          />
        </MethodGroup>
        <MethodGroup label="粘贴凭证">
          <MethodCard title="Access Token" description="一行一个，或读取 TXT 文件" icon={KeyRound} onClick={() => setMethod("token")} />
          <MethodCard title="Session JSON" description="从 chatgpt.com 的 session 接口复制完整 JSON" icon={FileJson} onClick={() => setMethod("session")} />
          <MethodCard title="Codex 认证 JSON" description="导入后账号来源标记为 codex" icon={FileJson} onClick={() => setMethod("codex-auth")} />
          <MethodCard title="账号 JSON 文件" description="本项目导出的账号 JSON，也兼容 CPA JSON 文件" icon={Files} onClick={() => setMethod("account-json")} />
        </MethodGroup>
        <MethodGroup label="从服务器拉取">
          <MethodCard title="CPA 服务器" description="在设置里配置远程 CPA 服务器后选择账号导入" icon={Files} onClick={toSettings} />
          <MethodCard title="Sub2API 服务器" description="在设置里配置 Sub2API 服务器后选择 OpenAI 账号导入" icon={ServerCog} onClick={toSettings} />
        </MethodGroup>
      </div>
    );
  };

  const footerDisabled = disabled || isSubmitting;

  return (
    <>
      <Dialog open={open} onOpenChange={handleOpenChange}>
        <Button variant="clay" onClick={() => setOpen(true)} disabled={disabled}>
          <Upload className="size-4" />
          导入账号
        </Button>
        <DialogContent showCloseButton={false} className="max-h-[92vh] overflow-y-auto rounded-[14px] p-6 sm:max-w-[560px]">
          <DialogHeader className="gap-1.5">
            <DialogTitle className="font-serif text-[22px] font-medium">
              {method === "menu"
                ? "导入账号"
                : method === "token"
                  ? "导入 Access Token"
                  : method === "session"
                    ? "导入 Session JSON"
                    : method === "codex-auth"
                      ? "导入 Codex 认证 JSON"
                    : method === "oauth"
                      ? "OAuth 登录已有账号"
                      : method === "protocol"
                        ? "批量协议登录"
                        : "导入账号 JSON"}
            </DialogTitle>
            <DialogDescription className="text-[13px] leading-6 text-ink-2">
              {method === "menu"
                ? "选择一种导入方式。导入成功后会自动拉取邮箱、类型和额度。"
                : method === "token"
                  ? "支持手动粘贴或从 TXT 文件导入，一行一个 Token。"
                  : method === "session"
                    ? "粘贴完整 Session JSON，系统会自动提取 accessToken。"
                    : method === "codex-auth"
                      ? "粘贴 Codex 认证 JSON，系统会按 codex 来源导入。"
                    : method === "oauth"
                      ? "用浏览器跑一遍 OpenAI 标准 OAuth，拿回 refresh_token 后系统会自动续期。"
                      : method === "protocol"
                        ? "每行一个账号：邮箱----密码----TOTP密钥，后台自动完成登录并入池。"
                        : "支持读取本项目导出的单账号对象或全部账号数组，并在提交前做数量确认。"}
            </DialogDescription>
          </DialogHeader>


          {renderMethodBody()}

          {method !== "menu" ? (
          <div className="space-y-2">
            <label className="text-[13px] text-ink-2">代理</label>
            <Select value={proxyChoice} onValueChange={handleProxyChoiceChange}>
              <SelectTrigger className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="none">不指定，走全局代理</SelectItem>
                {proxies.length > 1 ? <SelectItem value="distribute">平均分配到多个代理</SelectItem> : null}
                {proxies.map((proxy) => (
                  <SelectItem key={proxy.url} value={proxy.url}>
                    {proxyLabel(proxy)}
                    {proxy.health.abnormal ? "（异常）" : ""}
                  </SelectItem>
                ))}
                <SelectItem value="new">使用新代理</SelectItem>
              </SelectContent>
            </Select>
            {proxyChoice === "distribute" ? (
              <div className="space-y-2 rounded-[7px] border border-line bg-surface p-3">
                {proxies.map((proxy) => (
                  <label key={proxy.url} className="flex items-center gap-2 text-sm text-ink-2">
                    <Checkbox
                      checked={distributeUrls.includes(proxy.url)}
                      onCheckedChange={(checked) => toggleDistributeUrl(proxy.url, checked === true)}
                    />
                    <span className="min-w-0 flex-1 truncate">{proxyLabel(proxy)}</span>
                    <span className={cn("shrink-0 text-xs", proxy.health.abnormal ? "text-bad" : "text-ink-3")}>
                      {proxy.health.abnormal ? "异常 · " : ""}
                      {proxy.account_count} 个账号
                    </span>
                  </label>
                ))}
                <div className="rounded-[7px] border border-line">
                  {distributionPreview.map((item) => {
                    const proxy = proxies.find((entry) => entry.url === item.url);
                    return (
                      <div key={item.url} className="grid grid-cols-[1fr_auto] gap-3 border-t border-line px-3 py-2 text-[13px] first:border-t-0">
                        <span className="truncate text-ink">{proxy?.name || proxy?.masked_url || item.url}</span>
                        <span className="text-ink-3 tabular-nums">
                          {item.abnormal && distributionPreview.some((entry) => !entry.abnormal) ? "跳过" : `现有 ${item.accountCount} · 本次 +${item.assigned}`}
                        </span>
                      </div>
                    );
                  })}
                </div>
                <p className="text-xs leading-5 text-ink-3">
                  {newAccountCount > 0
                    ? `这 ${newAccountCount} 个新账号分给负载最少的代理，平手按上面的顺序。已在号池里的账号保留原代理。`
                    : "填入要导入的账号后，这里会显示每个代理分到几个。"}
                  异常代理不参与，除非选中的代理全部异常。
                </p>
              </div>
            ) : proxyChoice === "new" ? (
              <Input
                value={newProxy}
                onChange={(event) => setNewProxy(event.target.value)}
                placeholder="http://user:pass@host:port"
               
              />
            ) : (
              <p className="text-xs text-ink-3">
                {proxyChoice === "none"
                  ? "不写专属代理，这些账号继续走全局代理。"
                  : "这个地址会写到这次导入的账号上，不改全局代理。"}
              </p>
            )}
          </div>
          ) : null}

          <DialogFooter className="pt-2">
            <Button
              variant="ghost"
             
              onClick={() => setOpen(false)}
              disabled={footerDisabled}
            >
              取消
            </Button>
            {method === "token" ? (
              <Button
               
                onClick={() => void handleImportTokenText()}
                disabled={footerDisabled}
              >
                {isSubmitting ? <LoaderCircle className="size-4 animate-spin" /> : null}
                导入 Token
              </Button>
            ) : null}
            {method === "session" ? (
              <Button
               
                onClick={() => void handleImportSessionJson()}
                disabled={footerDisabled}
              >
                {isSubmitting ? <LoaderCircle className="size-4 animate-spin" /> : null}
                导入 JSON
              </Button>
            ) : null}
            {method === "codex-auth" ? (
              <Button
               
                onClick={() => void handleImportCodexAuthJson()}
                disabled={footerDisabled}
              >
                {isSubmitting ? <LoaderCircle className="size-4 animate-spin" /> : null}
                导入 JSON
              </Button>
            ) : null}
            {method === "oauth" ? (
              <Button
                className={cn(
                  "",
                  !oauthSession ? "hidden" : "",
                )}
                onClick={() => void handleFinishOAuth()}
                disabled={footerDisabled || !oauthSession || !oauthCallbackInput.trim()}
              >
                {isSubmitting ? <LoaderCircle className="size-4 animate-spin" /> : null}
                完成导入
              </Button>
            ) : null}
            {method === "protocol" ? (
              <Button
               
                onClick={() => void handleStartProtocolLogin()}
                disabled={footerDisabled || protocolRunning || !protocolInput.trim()}
              >
                {protocolRunning ? <LoaderCircle className="size-4 animate-spin" /> : null}
                {protocolRunning ? "登录中..." : "开始登录"}
              </Button>
            ) : null}
            {method === "account-json" ? (
              <Button
                className={cn(
                  "",
                  !pendingAccountJsonImport ? "hidden" : "",
                )}
                onClick={() => setConfirmOpen(true)}
                disabled={footerDisabled || !pendingAccountJsonImport}
              >
                查看导入确认
              </Button>
            ) : null}
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <Dialog open={confirmOpen} onOpenChange={setConfirmOpen}>
        <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-[480px]">
          <DialogHeader className="gap-1.5">
            <DialogTitle className="font-serif text-[22px] font-medium">确认导入账号</DialogTitle>
            <DialogDescription className="text-[13px] leading-6 text-ink-2">
              {pendingAccountJsonImport
                ? `确认识别到 ${pendingAccountJsonImport.parsedAccountCount} 个 Token，是否确认导入？`
                : "尚未读取到可导入的 Token。"}
              {pendingAccountJsonImport?.errorCount
                ? `，另有 ${pendingAccountJsonImport.errorCount} 个文件未提取成功。`
                : "。"}
            </DialogDescription>
          </DialogHeader>
          <DialogFooter className="pt-2">
            <Button
              variant="ghost"
             
              onClick={() => setConfirmOpen(false)}
              disabled={isSubmitting}
            >
              返回
            </Button>
            <Button
             
              onClick={() =>
                void submitTokens(
                  pendingAccountJsonImport?.tokens ?? [],
                  "账号 JSON 导入完成",
                  pendingAccountJsonImport?.accounts ?? [],
                )
              }
              disabled={isSubmitting || !pendingAccountJsonImport}
            >
              {isSubmitting ? <LoaderCircle className="size-4 animate-spin" /> : null}
              确认导入
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  );
}
