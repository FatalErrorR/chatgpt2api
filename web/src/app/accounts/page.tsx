"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import {
  ChevronLeft,
  ChevronRight,
  Copy,
  Download,
  Link2,
  Image as ImageIcon,
  LoaderCircle,
  LogIn,
  Pencil,
  RefreshCw,
  Search,
  Trash2,
} from "lucide-react";
import { toast } from "sonner";

import { batchBarDangerClass } from "@/components/ui/batch-bar";
import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { ConfirmDialog } from "@/components/confirm-dialog";
import { RowMenu } from "@/components/row-menu";
import { SkeletonLines } from "@/components/ui/states";
import { StatusDot, type StatusTone } from "@/components/ui/status-dot";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import {
  deleteAccounts,
  fetchAccounts,
  fetchImageOutputCounts,
  fetchProxies,
  fetchRefreshProgress,
  fetchImageTestProgress,
  fetchReLoginProgress,
  reLoginAccounts,
  testAccountImages,
  refreshAccounts,
  testProxy,
  updateAccount,
  type Account,
  type AccountRefreshResponse,
  type AccountStatus,
  type ImageOutputCounts,
  type ProxyEntry,
  type RefreshProgressResponse,
} from "@/lib/api";
import { accountMatchesProxyFilter, proxyForAccount, proxyShortName } from "@/lib/proxy-match";
import { useAuthGuard } from "@/lib/use-auth-guard";
import { cn } from "@/lib/utils";

import { AccountImportDialog, proxyLabel } from "./components/account-import-dialog";
import { ProxyManagerDialog } from "./components/proxy-manager-dialog";

type StatusFilter = AccountStatus | "all" | "edit_cooldown";

const NO_PROXY = "__none__";
const NEW_PROXY = "__new__";

const accountStatusOptions: { label: string; value: StatusFilter }[] = [
  { label: "全部状态", value: "all" },
  { label: "正常", value: "正常" },
  { label: "限流", value: "限流" },
  { label: "异常", value: "异常" },
  { label: "禁用", value: "禁用" },
  { label: "图生图冷却中", value: "edit_cooldown" },
];

const STATUS_TONE: Record<AccountStatus, StatusTone> = {
  正常: "ok",
  限流: "warn",
  异常: "bad",
  禁用: "off",
};

function formatCompact(value: number) {
  if (value >= 1000) {
    return `${(value / 1000).toFixed(1)}k`;
  }
  return String(value);
}

function formatQuota(account: Account) {
  return String(Math.max(0, account.quota));
}

function formatRestoreAt(value?: string | null) {
  if (!value) {
    return { absolute: "—", relative: "" };
  }

  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return { absolute: value, relative: "" };
  }

  const diffMs = Math.max(0, date.getTime() - Date.now());
  const totalHours = Math.ceil(diffMs / (1000 * 60 * 60));
  const days = Math.floor(totalHours / 24);
  const hours = totalHours % 24;
  const relative =
    diffMs <= 0 ? "已到恢复时间" : days > 0 ? `剩余 ${days} 天${hours ? ` ${hours} 小时` : ""}` : `剩余 ${Math.max(1, hours)} 小时`;

  const pad = (num: number) => String(num).padStart(2, "0");
  const absolute = `${pad(date.getMonth() + 1)}-${pad(date.getDate())} ${pad(date.getHours())}:${pad(date.getMinutes())}`;

  return { absolute, relative };
}

/**
 * 恢复时间列：额度恢复只对限流账号、或恢复时间还没到的账号有意义。
 * 正常且有额度、或已异常的账号，过去的恢复时间只是噪音。
 */
function restoreLabel(account: Account, restore: { relative: string }) {
  if (!restore.relative) return "";
  if (restore.relative === "已到恢复时间" && account.status !== "限流") return "";
  return restore.relative;
}

function formatCreatedAt(raw?: string | null) {
  if (!raw) return "—";
  const date = new Date(`${raw}Z`);
  if (Number.isNaN(date.getTime())) return String(raw).slice(0, 10);
  const pad = (num: number) => String(num).padStart(2, "0");
  return `${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

function isEditUploadCooled(account: { edit_upload_blocked_until?: string | null }) {
  const raw = account.edit_upload_blocked_until;
  if (!raw) {
    return false;
  }
  const date = new Date(raw);
  return !Number.isNaN(date.getTime()) && date.getTime() > Date.now();
}

function formatQuotaSummary(accounts: Account[]) {
  const availableAccounts = accounts.filter((account) => account.status === "正常");
  return formatCompact(availableAccounts.reduce((sum, account) => sum + Math.max(0, account.quota), 0));
}

function downloadTokens(accounts: Account[]) {
  const content = `${accounts.map((account) => account.access_token).join("\n")}\n`;
  const blob = new Blob([content], { type: "text/plain;charset=utf-8" });
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = `accounts-${Date.now()}.txt`;
  link.click();
  URL.revokeObjectURL(url);
}

function displayAccountType(account: Account) {
  return account.type || "Free";
}

function displayAccountSource(account: Account) {
  const source = String(account.source_type || "").trim().toLowerCase();
  if (!source) {
    return "web";
  }
  if (source === "web") {
    return "web";
  }
  return source;
}

function AccountsPageContent() {
  const didLoadRef = useRef(false);
  const [accounts, setAccounts] = useState<Account[]>([]);
  const [imageCounts, setImageCounts] = useState<ImageOutputCounts | null>(null);
  const [selectedIds, setSelectedIds] = useState<string[]>([]);
  const [query, setQuery] = useState("");
  const [typeFilter, setTypeFilter] = useState("all");
  const [statusFilter, setStatusFilter] = useState<StatusFilter>("all");
  const [proxyFilter, setProxyFilter] = useState("all");
  const [proxies, setProxies] = useState<ProxyEntry[]>([]);
  const [page, setPage] = useState(1);
  const [pageSize, setPageSize] = useState("10");
  const [editingAccount, setEditingAccount] = useState<Account | null>(null);
  const [editStatus, setEditStatus] = useState<AccountStatus>("正常");
  const [editProxy, setEditProxy] = useState("");
  // 编辑代理：NO_PROXY 走全局代理，NEW_PROXY 手填，其余为代理清单的 key
  const [editProxyChoice, setEditProxyChoice] = useState(NO_PROXY);
  const [isTestingProxy, setIsTestingProxy] = useState(false);
  const [isLoading, setIsLoading] = useState(true);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [refreshingTokens, setRefreshingTokens] = useState<Set<string>>(new Set());
  const [isDeleting, setIsDeleting] = useState(false);
  const [isUpdating, setIsUpdating] = useState(false);
  const [isRelogining, setIsRelogining] = useState(false);
  const [isTestingImages, setIsTestingImages] = useState(false);
  const [progress, setProgress] = useState<{
    visible: boolean;
    current: number;
    total: number;
    message: string;
    email: string;
  }>({
    visible: false,
    current: 0,
    total: 0,
    message: "",
    email: "",
  });
  const progressRef = useRef<ReturnType<typeof setInterval> | null>(null);
  const [refreshSummary, setRefreshSummary] = useState<Record<string, number | string> | null>(null);

  const loadProxies = async () => {
    try {
      const data = await fetchProxies();
      setProxies(data.items);
    } catch (error) {
      const message = error instanceof Error ? error.message : "加载代理失败";
      toast.error(message);
    }
  };

  const loadAccounts = async (silent = false) => {
    if (!silent) {
      setIsLoading(true);
    }
    try {
      const data = await fetchAccounts();
      setAccounts(data.items);
      void loadProxies();
      setSelectedIds((prev) => prev.filter((id) => data.items.some((item) => item.access_token === id)));
    } catch (error) {
      const message = error instanceof Error ? error.message : "加载账户失败";
      toast.error(message);
    } finally {
      if (!silent) {
        setIsLoading(false);
      }
    }
  };

  const loadImageCounts = async () => {
    try {
      setImageCounts(await fetchImageOutputCounts());
    } catch (error) {
      const message = error instanceof Error ? error.message : "加载生图张数失败";
      toast.error(message);
    }
  };

  useEffect(() => {
    if (didLoadRef.current) {
      return;
    }
    didLoadRef.current = true;
    void loadAccounts();
    void loadImageCounts();

    // 清理进度条定时器
    return () => {
      if (progressRef.current) clearInterval(progressRef.current);
    };
  }, []);

  const filteredAccounts = useMemo(() => {
    const normalizedQuery = query.trim().toLowerCase();
    return accounts.filter((account) => {
      const searchMatched =
        normalizedQuery.length === 0 || (account.email ?? "").toLowerCase().includes(normalizedQuery);
      const typeMatched = typeFilter === "all" || displayAccountType(account) === typeFilter;
      const statusMatched =
        statusFilter === "all" ||
        (statusFilter === "edit_cooldown" ? isEditUploadCooled(account) : account.status === statusFilter);
      const proxyMatched = accountMatchesProxyFilter(account, proxyFilter);
      return searchMatched && typeMatched && statusMatched && proxyMatched;
    });
  }, [accounts, query, statusFilter, typeFilter, proxyFilter]);

  const proxyByKey = useMemo(() => new Map(proxies.map((proxy) => [proxy.key, proxy])), [proxies]);

  const pageCount = Math.max(1, Math.ceil(filteredAccounts.length / Number(pageSize)));
  const safePage = Math.min(page, pageCount);
  const startIndex = (safePage - 1) * Number(pageSize);
  const currentRows = filteredAccounts.slice(startIndex, startIndex + Number(pageSize));
  const allCurrentSelected =
    currentRows.length > 0 && currentRows.every((row) => selectedIds.includes(row.access_token));

  const summary = useMemo(() => {
    const total = accounts.length;
    const active = accounts.filter((item) => item.status === "正常").length;
    const limited = accounts.filter((item) => item.status === "限流").length;
    const abnormal = accounts.filter((item) => item.status === "异常").length;
    const disabled = accounts.filter((item) => item.status === "禁用").length;
    const quota = formatQuotaSummary(accounts);

    return { total, active, limited, abnormal, disabled, quota };
  }, [accounts]);

  const accountTypeOptions = useMemo(
    () => [
      { label: "全部类型", value: "all" },
      ...Array.from(new Set(accounts.map(displayAccountType))).map((type) => ({ label: type, value: type })),
    ],
    [accounts],
  );

  const selectedTokens = useMemo(() => {
    const selectedSet = new Set(selectedIds);
    return accounts.filter((item) => selectedSet.has(item.access_token)).map((item) => item.access_token);
  }, [accounts, selectedIds]);

  const abnormalTokens = useMemo(() => {
    return accounts.filter((item) => item.status === "异常").map((item) => item.access_token);
  }, [accounts]);

  const paginationItems = useMemo(() => {
    const items: (number | "...")[] = [];
    const start = Math.max(1, safePage - 1);
    const end = Math.min(pageCount, safePage + 1);

    if (start > 1) items.push(1);
    if (start > 2) items.push("...");
    for (let current = start; current <= end; current += 1) items.push(current);
    if (end < pageCount - 1) items.push("...");
    if (end < pageCount) items.push(pageCount);

    return items;
  }, [pageCount, safePage]);

  const handleDeleteTokens = async (tokens: string[]) => {
    if (tokens.length === 0) {
      toast.error("请先选择要删除的账户");
      return;
    }

    setIsDeleting(true);
    try {
      const data = await deleteAccounts(tokens);
      setAccounts(data.items);
      setSelectedIds((prev) => prev.filter((id) => data.items.some((item) => item.access_token === id)));
      toast.success(`删除 ${data.removed ?? 0} 个账户`);
    } catch (error) {
      const message = error instanceof Error ? error.message : "删除账户失败";
      toast.error(message);
    } finally {
      setIsDeleting(false);
    }
  };

  const handleRefreshAccounts = async (accessTokens: string[]) => {
    if (accessTokens.length === 0) {
      toast.error("没有需要刷新的账户");
      return;
    }

    if (accessTokens.length === 1) {
      setRefreshingTokens((prev) => new Set([...prev, accessTokens[0]]));
      try {
        const { progress_id } = await refreshAccounts(accessTokens);
        // 单账号：轮询等待完成
        await pollRefreshProgress(progress_id, (progress) => {
          if (progress.done && progress.result) {
            setAccounts(progress.result.items);
            setSelectedIds((prev) => prev.filter((id) => progress.result!.items.some((item) => item.access_token === id)));
          }
        });
      } catch (error) {
        const message = error instanceof Error ? error.message : "刷新账户失败";
        toast.error(message);
      } finally {
        setRefreshingTokens((prev) => {
          const next = new Set(prev);
          next.delete(accessTokens[0]);
          return next;
        });
      }
      return;
    }

    setIsRefreshing(true);

    // 计算非选中账号的基数（统计卡片联动用）
    const selectedTokenSet = new Set(accessTokens);
    const baseAccountsList = accounts.filter((a) => !selectedTokenSet.has(a.access_token));
    const baseActive = baseAccountsList.filter((a) => a.status === "正常").length;
    const baseLimited = baseAccountsList.filter((a) => a.status === "限流").length;
    const baseAbnormal = baseAccountsList.filter((a) => a.status === "异常").length;
    const baseDisabled = baseAccountsList.filter((a) => a.status === "禁用").length;
    const baseNormalAccounts = baseAccountsList.filter((a) => a.status === "正常");
    const baseQuotaNum = baseNormalAccounts.reduce((s, a) => s + Math.max(0, a.quota), 0);

    // 显示进度条（只显示当前任务，不含分类统计）
    const total = accessTokens.length;
    setProgress({
      visible: true,
      current: 0,
      total,
      message: "正在刷新账号信息...",
      email: "",
    });

    try {
      const { progress_id } = await refreshAccounts(accessTokens);

      // 轮询进度到完成
      const data = await new Promise<AccountRefreshResponse>((resolve, reject) => {
        const pollTimer = setInterval(async () => {
          try {
            const p = await fetchRefreshProgress(progress_id);
            if (p.done) {
              clearInterval(pollTimer);
              if (p.error) {
                reject(new Error(p.error));
                return;
              }
              if (!p.result) {
                reject(new Error("刷新结果为空"));
                return;
              }
              // 更新最终进度显示
              setProgress((prev) => ({
                ...prev,
                current: prev.total,
                message: "刷新完成",
              }));
              // 清除联动统计
              setRefreshSummary(null);
              resolve(p.result);
            } else {
              // 实时更新进度
              setProgress((prev) => ({
                ...prev,
                current: p.processed,
              }));
              // 实时更新统计卡片：基数 + 已刷新的累加结果
              const runningActive = baseActive + ((p.status_counts?.["正常"]) ?? 0);
              const runningLimited = baseLimited + ((p.status_counts?.["限流"]) ?? 0);
              const runningAbnormal = baseAbnormal + ((p.status_counts?.["异常"]) ?? 0);
              const runningDisabled = baseDisabled + ((p.status_counts?.["禁用"]) ?? 0);
              setRefreshSummary({
                total: accounts.length,
                active: runningActive,
                limited: runningLimited,
                abnormal: runningAbnormal,
                disabled: runningDisabled,
                quota: formatCompact(baseQuotaNum + (p.total_quota ?? 0)),
              });
            }
          } catch (err) {
            clearInterval(pollTimer);
            reject(err);
          }
        }, 300);
      });

      // 刷新完成，更新数据
      setAccounts(data.items);
      setSelectedIds((prev) => prev.filter((id) => data.items.some((item) => item.access_token === id)));

      const relogined = data.relogined ?? 0;

      // 显示重新登录进度
      if (relogined > 0) {
        setProgress({
          visible: true,
          current: 0,
          total: relogined,
          message: `正在尝试对 ${relogined} 个账号进行移除异常状态`,
          email: "",
        });
        // 模拟重新登录进度
        let reCount = 0;
        await new Promise<void>((resolve) => {
          const timer = setInterval(() => {
            reCount += 1;
            if (reCount >= relogined) {
              clearInterval(timer);
              setProgress({
                visible: true,
                current: relogined,
                total: relogined,
                message: "移除异常状态完成",
                email: "",
              });
              setTimeout(() => setProgress({ visible: false, current: 0, total: 0, message: "", email: "" }), 800);
              resolve();
            } else {
              setProgress((prev) => ({ ...prev, current: reCount }));
            }
          }, 150);
          setTimeout(resolve, 2000);
        });
      } else {
        setProgress({
          visible: true,
          current: total,
          total,
          message: "刷新完成",
          email: "",
        });
        setTimeout(() => setProgress({ visible: false, current: 0, total: 0, message: "", email: "" }), 800);
      }

      if ((data.errors ?? []).length > 0) {
        const firstError = data.errors?.[0]?.error;
        toast.error(
          `刷新成功 ${data.refreshed} 个，失败 ${(data.errors ?? []).length} 个${firstError ? `，首个错误：${firstError}` : ""}`,
        );
      } else {
        toast.success(`刷新成功 ${data.refreshed} 个账户${relogined > 0 ? `，已触发 ${relogined} 个账号重新登录` : ""}`);
      }
    } catch (error) {
      setProgress({ visible: false, current: 0, total: 0, message: "", email: "" });
      setRefreshSummary(null);
      const message = error instanceof Error ? error.message : "刷新账户失败";
      toast.error(message);
    } finally {
      setIsRefreshing(false);
    }
  };

  const pollRefreshProgress = async (
    progressId: string,
    onUpdate: (p: RefreshProgressResponse) => void,
  ): Promise<void> => {
    return new Promise<void>((resolve, reject) => {
      const timer = setInterval(async () => {
        try {
          const p = await fetchRefreshProgress(progressId);
          if (p.done) {
            clearInterval(timer);
            if (p.error) {
              reject(new Error(p.error));
            } else {
              onUpdate(p);
              resolve();
            }
          }
        } catch (err) {
          clearInterval(timer);
          reject(err);
        }
      }, 500);
    });
  };

  const handleTestImages = async (accessTokens: string[]) => {
    if (accessTokens.length === 0) {
      toast.error("请先选择要测试的账户");
      return;
    }
    setIsTestingImages(true);
    const total = accessTokens.length;
    setProgress({ visible: true, current: 0, total, message: "正在测试生图...", email: "" });
    try {
      const { progress_id } = await testAccountImages(accessTokens);
      const results = await new Promise<RefreshProgressResponse["results"]>((resolve, reject) => {
        const pollTimer = setInterval(async () => {
          try {
            const p = await fetchImageTestProgress(progress_id);
            const doneResults = p.results ?? [];
            const last = [...doneResults].reverse().find((item) => item.error);
            setProgress((prev) => ({
              ...prev,
              current: p.processed,
              email: last?.error || `已处理 ${p.processed}/${p.total}`,
              message: "正在测试生图...",
            }));
            if (p.done) {
              clearInterval(pollTimer);
              if (p.error) {
                reject(new Error(p.error));
                return;
              }
              resolve(doneResults);
            }
          } catch (err) {
            clearInterval(pollTimer);
            reject(err);
          }
        }, 500);
      });
      const failed = (results ?? []).filter((item) => item.status !== "成功");
      const succeeded = (results ?? []).length - failed.length;
      setProgress({ visible: true, current: total, total, message: "测试完成", email: "" });
      setTimeout(() => setProgress({ visible: false, current: 0, total: 0, message: "", email: "" }), 800);
      if (failed.length === 0) {
        toast.success(`测试完成：${succeeded} 个成功`);
      } else {
        toast.error(`测试完成：成功 ${succeeded}，失败 ${failed.length}`);
      }
    } catch (error) {
      setProgress({ visible: false, current: 0, total: 0, message: "", email: "" });
      toast.error(error instanceof Error ? error.message : "测试生图失败");
    } finally {
      setIsTestingImages(false);
    }
  };

  const handleReLogin = async (accessTokens: string[]) => {
    if (accessTokens.length === 0) {
      toast.error("请先选择要恢复的账户");
      return;
    }

    // 只处理异常账号，过滤非异常账号
    const abnormalTokens = accessTokens.filter((token) => {
      const account = accounts.find((a) => a.access_token === token);
      return account?.status === "异常";
    });

    if (abnormalTokens.length === 0) {
      toast.error("选中账号中没有异常账号");
      return;
    }

    if (abnormalTokens.length < accessTokens.length) {
      toast.info(`已过滤 ${accessTokens.length - abnormalTokens.length} 个非异常账号`);
    }

    setIsRelogining(true);

    // 计算非选中账号的基数（统计卡片联动用）
    const selectedTokenSet = new Set(abnormalTokens);
    const baseAccountsList = accounts.filter((a) => !selectedTokenSet.has(a.access_token));
    const baseActive = baseAccountsList.filter((a) => a.status === "正常").length;
    const baseLimited = baseAccountsList.filter((a) => a.status === "限流").length;
    const baseAbnormal = baseAccountsList.filter((a) => a.status === "异常").length;
    const baseDisabled = baseAccountsList.filter((a) => a.status === "禁用").length;

    // 显示进度条（真实进度）
    const total = abnormalTokens.length;
    setProgress({ visible: true, current: 0, total, message: "正在尝试恢复异常账号...", email: "" });

    try {
      const { progress_id } = await reLoginAccounts(abnormalTokens);

      // 轮询进度到完成
      await new Promise<void>((resolve, reject) => {
        const pollTimer = setInterval(async () => {
          try {
            const p = await fetchReLoginProgress(progress_id);
            if (p.done) {
              clearInterval(pollTimer);
              if (p.error) {
                reject(new Error(p.error));
                return;
              }
              setProgress((prev) => ({ ...prev, current: prev.total, message: "恢复流程已完成" }));
              setRefreshSummary(null);
              resolve();
            } else {
              // 实时更新进度
              const results = p.results ?? [];
              // 找到最新一条有错误的结果
              const lastErrorResult = [...results].reverse().find((r) => r.error);
              const emailHint = lastErrorResult
                ? `失败: ${lastErrorResult.token} ${lastErrorResult.error ?? ""}`
                : `已处理 ${p.processed}/${p.total}`;
              setProgress((prev) => ({
                ...prev,
                current: p.processed,
                email: emailHint,
                message: "正在尝试恢复异常账号...",
              }));

              // 实时更新统计卡片：基数 + 已处理的恢复结果
              let runningActive = baseActive;
              let runningAbnormal = baseAbnormal;
              let runningDisabled = baseDisabled;
              for (const r of results) {
                if (r.status === "成功") {
                  runningActive += 1;
                  runningAbnormal -= 1;
                } else if (r.status === "禁用") {
                  runningDisabled += 1;
                  runningAbnormal -= 1;
                }
                // "异常"或"跳过"：保持异常状态不变
              }
              setRefreshSummary({
                total: accounts.length,
                active: runningActive,
                limited: baseLimited,
                abnormal: runningAbnormal,
                disabled: runningDisabled,
                quota: summary.quota,
              });
            }
          } catch (err) {
            clearInterval(pollTimer);
            reject(err);
          }
        }, 300);
      });

      // 等待后台线程完成，再拉取最新数据
      await new Promise<void>((resolve) => setTimeout(resolve, 500));
      try {
        const freshData = await fetchAccounts();
        setAccounts(freshData.items);
        setSelectedIds((prev) => prev.filter((id) => freshData.items.some((item) => item.access_token === id)));
      } catch { /* 静默失败 */ }

      setProgress({
        visible: true,
        current: total,
        total,
        message: "恢复完成",
        email: "",
      });
      setTimeout(() => setProgress({ visible: false, current: 0, total: 0, message: "", email: "" }), 800);

      toast.success(`恢复流程已全部完成`);
    } catch (error) {
      setProgress({ visible: false, current: 0, total: 0, message: "", email: "" });
      setRefreshSummary(null);
      const message = error instanceof Error ? error.message : "重新登录失败";
      toast.error(message);
    } finally {
      setIsRelogining(false);
    }
  };

  const openEditDialog = (account: Account) => {
    setEditingAccount(account);
    setEditStatus(account.status);
    const proxy = (account.proxy ?? "").trim();
    const listed = proxyForAccount(proxyByKey, account);
    setEditProxyChoice(!proxy ? NO_PROXY : listed ? listed.key : NEW_PROXY);
    setEditProxy(listed ? "" : proxy);
  };

  /** 编辑框最终要写到账号上的代理；选的还是账号原来那条时保留原写法。 */
  const resolveEditProxy = () => {
    if (editProxyChoice === NO_PROXY) return "";
    if (editProxyChoice === NEW_PROXY) return editProxy.trim();
    if (editingAccount?.proxy_key === editProxyChoice) return (editingAccount.proxy ?? "").trim();
    return proxyByKey.get(editProxyChoice)?.url ?? "";
  };

  const handleTestAccountProxy = async () => {
    const candidate = resolveEditProxy();
    if (!candidate) {
      toast.error("请先填写代理地址");
      return;
    }
    setIsTestingProxy(true);
    try {
      const data = await testProxy(candidate);
      data.result.ok
        ? toast.success(`代理可用（${data.result.latency_ms} ms，HTTP ${data.result.status}）`)
        : toast.error(`代理不可用：${data.result.error ?? "未知错误"}`);
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "测试代理失败");
    } finally {
      setIsTestingProxy(false);
    }
  };

  const handleUpdateAccount = async () => {
    if (!editingAccount) {
      return;
    }

    setIsUpdating(true);
    try {
      const data = await updateAccount(editingAccount.access_token, {
        status: editStatus,
        proxy: resolveEditProxy(),
      });
      setAccounts(data.items);
      setSelectedIds((prev) => prev.filter((id) => data.items.some((item) => item.access_token === id)));
      setEditingAccount(null);
      void loadProxies();
      toast.success("账号信息已更新");
    } catch (error) {
      const message = error instanceof Error ? error.message : "更新账号失败";
      toast.error(message);
    } finally {
      setIsUpdating(false);
    }
  };

  // 筛选栏各项的数量（基于全部账号，不受当前筛选影响）
  const facetCounts = useMemo(() => {
    const status: Record<string, number> = { all: accounts.length, 正常: 0, 限流: 0, 异常: 0, 禁用: 0, edit_cooldown: 0 };
    const types = new Map<string, number>();
    const proxyKeys = new Map<string, number>();
    let noProxy = 0;
    let inflight = 0;
    for (const account of accounts) {
      status[account.status] = (status[account.status] ?? 0) + 1;
      if (isEditUploadCooled(account)) status.edit_cooldown += 1;
      const type = displayAccountType(account);
      types.set(type, (types.get(type) ?? 0) + 1);
      if ((account.proxy || "").trim() && account.proxy_key) {
        proxyKeys.set(account.proxy_key, (proxyKeys.get(account.proxy_key) ?? 0) + 1);
      } else {
        noProxy += 1;
      }
      inflight += account.image_inflight ?? 0;
    }
    return { status, types, proxyKeys, noProxy, inflight };
  }, [accounts]);

  const todaySuccess = imageCounts ? (imageCounts.edits.today || 0) + (imageCounts.generations.today || 0) : null;

  // 删除前确认：写清删几个、影响什么
  const [pendingDelete, setPendingDelete] = useState<{ tokens: string[]; title: string } | null>(null);
  const confirmDelete = (tokens: string[], title: string) => {
    if (tokens.length === 0) {
      toast.error("请先选择要删除的账户");
      return;
    }
    setPendingDelete({ tokens, title });
  };

  const toggleSelectAll = (checked: boolean) => {
    if (checked) {
      setSelectedIds((prev) => Array.from(new Set([...prev, ...currentRows.map((item) => item.access_token)])));
      return;
    }
    setSelectedIds((prev) => prev.filter((id) => !currentRows.some((row) => row.access_token === id)));
  };

  const setFilter = (apply: () => void) => {
    apply();
    setPage(1);
  };

  const facetButton = (active: boolean) =>
    cn(
      "flex w-full items-center justify-between gap-2 rounded-[7px] px-[9px] py-1.5 text-left text-[13px] transition",
      active ? "bg-surface text-ink shadow-[inset_0_0_0_1px_var(--line)]" : "text-ink-2 hover:bg-surface-hover hover:text-ink",
    );
  const facetCount = "text-xs text-ink-3 tabular-nums";
  const pendingAccounts = pendingDelete
    ? accounts.filter((item) => pendingDelete.tokens.includes(item.access_token))
    : [];

  return (
    <section className="space-y-5">
      <div className="flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h1 className="text-[30px] font-medium text-ink">号池</h1>
          <div className="mt-1.5 flex flex-wrap gap-x-[18px] gap-y-1 text-[13px] text-ink-2">
            <span>
              可用额度 <b className="font-medium text-ink tabular-nums">{(refreshSummary ?? summary).quota}</b>
            </span>
            <span>
              今日成功 <b className="font-medium text-ink tabular-nums">{todaySuccess ?? "—"}</b> 张
            </span>
            <span title="正在生成、尚未结束的图片数">
              在途 <b className="font-medium text-ink tabular-nums">{facetCounts.inflight}</b>
            </span>
          </div>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <Button variant="ghost" size="icon" onClick={() => void loadAccounts()} disabled={isLoading || isRefreshing || isDeleting} title="重新加载列表">
            <RefreshCw className={cn("size-4", isLoading ? "animate-spin" : "")} />
          </Button>
          <ProxyManagerDialog proxies={proxies} onChange={setProxies} />
          <Button
            variant="outline"
            onClick={() => void handleRefreshAccounts(accounts.map((item) => item.access_token))}
            disabled={isLoading || isRefreshing || isDeleting || accounts.length === 0}
          >
            <RefreshCw className={cn("size-4", isRefreshing ? "animate-spin" : "")} />
            刷新全部额度
          </Button>
          <AccountImportDialog
            disabled={isLoading || isRefreshing || isDeleting}
            proxies={proxies}
            onImported={(items) => {
              setAccounts(items);
              void loadProxies();
              setSelectedIds([]);
              setPage(1);
            }}
          />
          <RowMenu
            label="更多"
            items={[
              { label: "导出全部 Token", icon: Download, onSelect: () => downloadTokens(accounts), disabled: accounts.length === 0 },
              {
                label: `移除全部异常账号（${abnormalTokens.length}）`,
                icon: Trash2,
                danger: true,
                disabled: abnormalTokens.length === 0 || isDeleting,
                onSelect: () => confirmDelete(abnormalTokens, `移除 ${abnormalTokens.length} 个异常账号？`),
              },
            ]}
          />
        </div>
      </div>

      {progress.visible ? (
        <div className="rounded-[10px] border border-line bg-surface px-4 py-3">
          <div className="flex items-center justify-between text-[13px]">
            <span className="text-ink-2">
              {progress.message}
              {progress.email ? <span className="ml-1 text-ink">{progress.email}</span> : null}
            </span>
            <span className="font-mono text-xs text-ink tabular-nums">
              {progress.current}/{progress.total}
            </span>
          </div>
          <div className="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-sunken">
            <div
              className="h-full rounded-full bg-ink transition-all duration-300 ease-out"
              style={{ width: `${progress.total > 0 ? (progress.current / progress.total) * 100 : 0}%` }}
            />
          </div>
        </div>
      ) : null}

      <div className="grid gap-[22px] lg:grid-cols-[210px_minmax(0,1fr)]">
        {/* 左侧筛选栏：状态 / 类型 / 代理，带数量，可以组合 */}
        <aside className="hidden min-w-0 content-start gap-[18px] lg:grid [&>div]:min-w-0">
          <div>
            <h4 className="mb-1.5 text-xs font-medium tracking-[0.04em] text-ink-3">状态</h4>
            {(
              [
                ["all", "全部", null],
                ["正常", "正常", "ok"],
                ["限流", "限流", "warn"],
                ["异常", "异常", "bad"],
                ["禁用", "禁用", "off"],
                ["edit_cooldown", "图生图冷却", "warn"],
              ] as const
            ).map(([value, label, tone]) => (
              <button key={value} type="button" className={facetButton(statusFilter === value)} onClick={() => setFilter(() => setStatusFilter(value))}>
                {tone ? <StatusDot tone={tone}>{label}</StatusDot> : <span>{label}</span>}
                <span className={facetCount}>{facetCounts.status[value] ?? 0}</span>
              </button>
            ))}
          </div>
          <div>
            <h4 className="mb-1.5 text-xs font-medium tracking-[0.04em] text-ink-3">类型</h4>
            <button type="button" className={facetButton(typeFilter === "all")} onClick={() => setFilter(() => setTypeFilter("all"))}>
              <span>全部</span>
              <span className={facetCount}>{accounts.length}</span>
            </button>
            {Array.from(facetCounts.types.entries()).map(([type, count]) => (
              <button key={type} type="button" className={facetButton(typeFilter === type)} onClick={() => setFilter(() => setTypeFilter(type))}>
                <span>{type}</span>
                <span className={facetCount}>{count}</span>
              </button>
            ))}
          </div>
          <div>
            <h4 className="mb-1.5 text-xs font-medium tracking-[0.04em] text-ink-3">代理</h4>
            <button type="button" className={facetButton(proxyFilter === "all")} onClick={() => setFilter(() => setProxyFilter("all"))}>
              <span>全部</span>
              <span className={facetCount}>{accounts.length}</span>
            </button>
            {/* 代理多时只露 10 行（每行 h-8），其余滚动；「全部」「未指定」留在外面 */}
            <div className="thin-scrollbar max-h-80 overflow-y-auto pr-1 [&>button]:h-8">
              {proxies.map((proxy) => (
                <button
                  key={proxy.key}
                  type="button"
                  className={facetButton(proxyFilter === proxy.key)}
                  onClick={() => setFilter(() => setProxyFilter(proxy.key))}
                  title={proxyLabel(proxy)}
                >
                  <StatusDot tone={proxy.health.abnormal ? "bad" : proxy.health.successes > 0 ? "ok" : "off"} className="min-w-0 overflow-hidden">
                    <span className="truncate">{proxyShortName(proxy)}</span>
                  </StatusDot>
                  <span className={facetCount}>{facetCounts.proxyKeys.get(proxy.key) ?? 0}</span>
                </button>
              ))}
            </div>
            <button type="button" className={facetButton(proxyFilter === "none")} onClick={() => setFilter(() => setProxyFilter("none"))}>
              <StatusDot tone="off">未指定</StatusDot>
              <span className={facetCount}>{facetCounts.noProxy}</span>
            </button>
          </div>
        </aside>

        <div className="min-w-0 space-y-3">
          <div className="flex flex-wrap items-center gap-2">
            <div className="relative min-w-[200px] flex-1">
              <Search className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-ink-3" />
              <Input
                value={query}
                onChange={(event) => setFilter(() => setQuery(event.target.value))}
                placeholder="搜索邮箱"
                className="h-[34px] rounded-[7px] border-line-strong bg-surface pl-9"
              />
            </div>
            {/* 窄屏没有筛选栏，用下拉代替 */}
            <div className="flex flex-wrap gap-2 lg:hidden">
              <Select value={statusFilter} onValueChange={(value) => setFilter(() => setStatusFilter(value as StatusFilter))}>
                <SelectTrigger className="h-[34px] w-[120px] rounded-[7px]"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {accountStatusOptions.map((option) => (
                    <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Select value={typeFilter} onValueChange={(value) => setFilter(() => setTypeFilter(value))}>
                <SelectTrigger className="h-[34px] w-[110px] rounded-[7px]"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {accountTypeOptions.map((option) => (
                    <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
              <Select value={proxyFilter} onValueChange={(value) => setFilter(() => setProxyFilter(value))}>
                <SelectTrigger className="h-[34px] w-[120px] rounded-[7px]"><SelectValue /></SelectTrigger>
                <SelectContent>
                  <SelectItem value="all">全部代理</SelectItem>
                  <SelectItem value="none">未指定代理</SelectItem>
                  {proxies.map((proxy) => (
                    <SelectItem key={proxy.key} value={proxy.key}>{proxyShortName(proxy)}</SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
            <Select value={pageSize} onValueChange={(value) => setFilter(() => setPageSize(value))}>
              <SelectTrigger className="h-[34px] w-[96px] rounded-[7px] text-xs text-ink-2"><SelectValue /></SelectTrigger>
              <SelectContent>
                <SelectItem value="10">每页 10</SelectItem>
                <SelectItem value="20">每页 20</SelectItem>
                <SelectItem value="50">每页 50</SelectItem>
                <SelectItem value="100">每页 100</SelectItem>
              </SelectContent>
            </Select>
          </div>

          {selectedIds.length > 0 ? (
            <div className="flex flex-wrap items-center gap-1.5 rounded-[10px] bg-ink py-2 pr-2.5 pl-4 text-[13px] text-page">
              <span>已选 {selectedIds.length} 个账号</span>
              <span className="flex-1" />
              {(
                [
                  ["刷新额度", RefreshCw, isRefreshing, () => void handleRefreshAccounts(selectedTokens)],
                  ["测试生图", ImageIcon, isTestingImages, () => void handleTestImages(selectedTokens)],
                  ["尝试恢复", LogIn, isRelogining, () => void handleReLogin(selectedTokens)],
                  ["导出", Download, false, () => downloadTokens(accounts.filter((item) => selectedTokens.includes(item.access_token)))],
                ] as const
              ).map(([label, Icon, busy, action]) => (
                <button
                  key={label}
                  type="button"
                  onClick={action}
                  disabled={busy}
                  className="inline-flex h-7 items-center gap-1.5 rounded-md border border-page/20 px-2.5 text-xs transition hover:bg-page/10 disabled:opacity-50"
                >
                  {busy ? <LoaderCircle className="size-3.5 animate-spin" /> : <Icon className="size-3.5" />}
                  {label}
                </button>
              ))}
              <button
                type="button"
                onClick={() => confirmDelete(selectedTokens, `删除 ${selectedTokens.length} 个账号？`)}
                disabled={isDeleting}
                className={cn(batchBarDangerClass, "h-7 text-xs")}
              >
                <Trash2 className="size-3.5" />
                删除
              </button>
              <button type="button" onClick={() => setSelectedIds([])} className="h-7 rounded-md px-2.5 text-xs transition hover:bg-page/10">
                取消
              </button>
            </div>
          ) : null}

          <div className="overflow-hidden rounded-[10px] border border-line bg-surface">
            {isLoading && accounts.length === 0 ? (
              <div className="space-y-3 p-5">
                {[80, 95, 60, 88, 72].map((width) => (
                  <div key={width} className="h-3 rounded-md bg-sunken" style={{ width: `${width}%` }} />
                ))}
              </div>
            ) : (
              <>
              {/* 手机：卡片列表 */}
              <div className="divide-y divide-line md:hidden">
                {currentRows.map((account) => {
                  const statusError =
                    account.status === "异常" ? (account.last_refresh_error || account.last_token_refresh_error || "").trim() : "";
                  const proxy = (account.proxy || "").trim() ? proxyForAccount(proxyByKey, account) : undefined;
                  const restore = formatRestoreAt(account.restore_at);
                  return (
                    <div key={account.access_token} className="flex items-start gap-3 px-3.5 py-3">
                      <Checkbox
                        className="mt-0.5"
                        checked={selectedIds.includes(account.access_token)}
                        onCheckedChange={(checked) =>
                          setSelectedIds((prev) =>
                            checked ? Array.from(new Set([...prev, account.access_token])) : prev.filter((item) => item !== account.access_token),
                          )
                        }
                      />
                      <div className="min-w-0 flex-1">
                        <div className="flex items-center justify-between gap-2">
                          <span className="truncate text-[13px] text-ink">{account.email ?? "—"}</span>
                          <StatusDot tone={STATUS_TONE[account.status]}>{account.status}</StatusDot>
                        </div>
                        <div className={cn("mt-0.5 truncate text-xs", statusError ? "text-bad" : "text-ink-3")}>
                          {statusError ||
                            [proxy ? proxyShortName(proxy) : "", displayAccountType(account), `额度 ${formatQuota(account)}`, restoreLabel(account, restore)]
                              .filter(Boolean)
                              .join(" · ")}
                        </div>
                      </div>
                      <RowMenu
                        items={[
                          { label: "复制 token", icon: Copy, onSelect: () => { void navigator.clipboard.writeText(account.access_token); toast.success("已复制 token"); } },
                          { label: "编辑", icon: Pencil, onSelect: () => openEditDialog(account), disabled: isUpdating },
                          { label: "刷新额度", icon: RefreshCw, onSelect: () => void handleRefreshAccounts([account.access_token]) },
                          { label: "删除", icon: Trash2, danger: true, onSelect: () => confirmDelete([account.access_token], "删除这个账号？") },
                        ]}
                      />
                    </div>
                  );
                })}
              </div>
              <div className="hidden overflow-x-auto md:block">
                <table className="w-full min-w-[760px] text-left text-[13px]">
                  <thead className="border-b border-line text-xs text-ink-3">
                    <tr>
                      <th className="w-10 py-2.5 pr-1 pl-3.5 font-normal">
                        <Checkbox checked={allCurrentSelected} onCheckedChange={(checked) => toggleSelectAll(Boolean(checked))} />
                      </th>
                      <th className="px-2.5 py-2.5 font-normal">账号</th>
                      <th className="px-2.5 py-2.5 font-normal">代理</th>
                      <th className="px-2.5 py-2.5 font-normal">状态</th>
                      <th className="px-2.5 py-2.5 text-right font-normal">额度</th>
                      <th className="px-2.5 py-2.5 font-normal">恢复时间</th>
                      <th className="px-2.5 py-2.5 text-right font-normal">成功 / 失败</th>
                      <th className="w-20 px-2.5 py-2.5" />
                    </tr>
                  </thead>
                  <tbody>
                    {currentRows.map((account) => {
                      const statusError =
                        account.status === "异常" ? (account.last_refresh_error || account.last_token_refresh_error || "").trim() : "";
                      const cooled = isEditUploadCooled(account);
                      const cooldown = cooled ? formatRestoreAt(account.edit_upload_blocked_until) : null;
                      const restore = formatRestoreAt(account.restore_at);
                      const url = (account.proxy || "").trim();
                      const proxy = url ? proxyForAccount(proxyByKey, account) : undefined;
                      const inflight = account.image_inflight ?? 0;
                      const busy = isRefreshing || refreshingTokens.has(account.access_token);
                      return (
                        <tr key={account.access_token} className="border-b border-line transition-colors last:border-b-0 hover:bg-surface-hover">
                          <td className="py-3 pr-1 pl-3.5">
                            <Checkbox
                              checked={selectedIds.includes(account.access_token)}
                              onCheckedChange={(checked) =>
                                setSelectedIds((prev) =>
                                  checked ? Array.from(new Set([...prev, account.access_token])) : prev.filter((item) => item !== account.access_token),
                                )
                              }
                            />
                          </td>
                          <td className="max-w-[280px] px-2.5 py-3">
                            <div className="truncate text-ink" title={account.email ?? undefined}>{account.email ?? "—"}</div>
                            {statusError ? (
                              <div className="truncate text-xs text-bad" title={statusError}>{statusError}</div>
                            ) : cooldown ? (
                              <div className="truncate text-xs text-warn">图生图冷却 · {cooldown.relative.replace("剩余 ", "还剩 ")}</div>
                            ) : (
                              <div className="truncate text-xs text-ink-3">
                                {displayAccountType(account)} · {displayAccountSource(account)} · {formatCreatedAt(account.created_at)}
                              </div>
                            )}
                          </td>
                          <td className="px-2.5 py-3 whitespace-nowrap">
                            {!url ? (
                              <span className="text-ink-3">—</span>
                            ) : (
                              <span
                                className={cn("block max-w-[150px] truncate", proxy?.health.abnormal ? "text-bad" : "text-ink-2")}
                                title={proxy ? `${proxyLabel(proxy)}${proxy.health.abnormal ? `\n代理异常：${proxy.health.last_error || "连续请求失败"}` : ""}` : undefined}
                              >
                                {proxy ? proxyShortName(proxy) : "…"}
                              </span>
                            )}
                          </td>
                          <td className="px-2.5 py-3">
                            <StatusDot tone={STATUS_TONE[account.status]} title={statusError || undefined}>
                              {account.status}
                            </StatusDot>
                          </td>
                          <td className="px-2.5 py-3 text-right tabular-nums text-ink">{formatQuota(account)}</td>
                          <td className="px-2.5 py-3 whitespace-nowrap">
                            {restoreLabel(account, restore) ? (
                              <>
                                <div className="text-ink">{restoreLabel(account, restore)}</div>
                                <div className="text-xs text-ink-3">{restore.absolute}</div>
                              </>
                            ) : (
                              <span className="text-ink-3">—</span>
                            )}
                          </td>
                          <td className="px-2.5 py-3 text-right font-mono text-xs whitespace-nowrap text-ink-2 tabular-nums">
                            {inflight > 0 ? (
                              <span className="mr-2 text-warn" title="正在生成的图片数；号池空闲时仍大于 0 说明并发槽位泄漏">
                                在途 {inflight}
                              </span>
                            ) : null}
                            {account.success} / {account.fail}
                          </td>
                          <td className="px-2.5 py-3">
                            <div className="flex items-center justify-end gap-0.5">
                              <button
                                type="button"
                                title="复制 token"
                                className="grid size-8 place-items-center rounded-[7px] text-ink-3 transition hover:bg-surface-hover hover:text-ink"
                                onClick={() => {
                                  void navigator.clipboard.writeText(account.access_token);
                                  toast.success("已复制 token");
                                }}
                              >
                                <Copy className="size-4" />
                              </button>
                              <RowMenu
                                items={[
                                  { label: "编辑", icon: Pencil, onSelect: () => openEditDialog(account), disabled: isUpdating },
                                  { label: busy ? "刷新中…" : "刷新额度", icon: RefreshCw, onSelect: () => void handleRefreshAccounts([account.access_token]), disabled: busy },
                                  { label: "删除", icon: Trash2, danger: true, onSelect: () => confirmDelete([account.access_token], "删除这个账号？"), disabled: isDeleting },
                                ]}
                              />
                            </div>
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
                {!isLoading && currentRows.length === 0 ? (
                  <div className="px-6 py-14 text-center text-[13px] text-ink-3">
                    {accounts.length === 0 ? (
                      <>
                        <div className="mb-1 font-serif text-[17px] text-ink">还没有账号</div>
                        导入账号后会显示在这里
                      </>
                    ) : (
                      <>
                        <div className="mb-1 font-serif text-[17px] text-ink">没有匹配的账号</div>
                        调整筛选条件或搜索关键字后重试
                      </>
                    )}
                  </div>
                ) : null}
              </div>
              </>
            )}
            <div className="flex flex-wrap items-center justify-between gap-3 border-t border-line px-3.5 py-2.5 text-xs text-ink-3">
              <span>
                {filteredAccounts.length === accounts.length ? `全部 ${accounts.length} 个` : `筛选后 ${filteredAccounts.length} 个，共 ${accounts.length} 个`}
              </span>
              <div className="flex items-center gap-1">
                <button type="button" className="grid size-7 place-items-center rounded-md text-ink-2 hover:bg-surface-hover disabled:opacity-40" disabled={safePage <= 1} onClick={() => setPage((prev) => Math.max(1, prev - 1))}>
                  <ChevronLeft className="size-4" />
                </button>
                {paginationItems.map((item, index) =>
                  item === "..." ? (
                    <span key={`ellipsis-${index}`} className="px-1">…</span>
                  ) : (
                    <button
                      key={item}
                      type="button"
                      onClick={() => setPage(item)}
                      className={cn("grid h-7 min-w-7 place-items-center rounded-md px-1.5 font-medium tabular-nums", item === safePage ? "bg-ink text-page" : "text-ink-2 hover:bg-surface-hover")}
                    >
                      {item}
                    </button>
                  ),
                )}
                <button type="button" className="grid size-7 place-items-center rounded-md text-ink-2 hover:bg-surface-hover disabled:opacity-40" disabled={safePage >= pageCount} onClick={() => setPage((prev) => Math.min(pageCount, prev + 1))}>
                  <ChevronRight className="size-4" />
                </button>
              </div>
            </div>
          </div>
        </div>
      </div>

      <Dialog open={Boolean(editingAccount)} onOpenChange={(open) => (!open ? setEditingAccount(null) : null)}>
        <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-[520px]">
          <DialogHeader className="gap-1">
            <DialogTitle className="font-serif text-[22px] font-medium">编辑账号</DialogTitle>
            <DialogDescription className="truncate font-mono text-xs text-ink-2">{editingAccount?.email || "—"}</DialogDescription>
          </DialogHeader>
          <div className="space-y-4">
            <label className="grid gap-1.5 text-[13px] text-ink-2">
              状态
              <Select value={editStatus} onValueChange={(value) => setEditStatus(value as AccountStatus)}>
                <SelectTrigger className="h-[38px] rounded-[7px]"><SelectValue /></SelectTrigger>
                <SelectContent>
                  {accountStatusOptions
                    .filter((option) => option.value !== "all" && option.value !== "edit_cooldown")
                    .map((option) => (
                      <SelectItem key={option.value} value={option.value}>{option.label}</SelectItem>
                    ))}
                </SelectContent>
              </Select>
            </label>
            <div className="grid gap-1.5 text-[13px] text-ink-2">
              代理
              <Select value={editProxyChoice} onValueChange={setEditProxyChoice}>
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
              {editProxyChoice === NEW_PROXY ? (
                <Input value={editProxy} onChange={(event) => setEditProxy(event.target.value)} placeholder="http://user:pass@host:port" className="h-[38px] rounded-[7px]" />
              ) : null}
            </div>
            <div className="flex items-center justify-between gap-3">
              <span className="min-w-0 truncate text-xs text-ink-3">
                {editingAccount?.status === "异常" && (editingAccount.last_refresh_error || editingAccount.last_token_refresh_error)
                  ? `异常原因：${editingAccount.last_refresh_error || editingAccount.last_token_refresh_error}`
                  : "测试会经所选代理访问 chatgpt.com"}
              </span>
              <Button variant="outline" size="sm" onClick={() => void handleTestAccountProxy()} disabled={isTestingProxy}>
                {isTestingProxy ? <LoaderCircle className="size-4 animate-spin" /> : <Link2 className="size-4" />}
                测试代理
              </Button>
            </div>
          </div>
          <DialogFooter className="pt-2 sm:justify-between">
            <Button
              variant="outline"
              className="text-bad"
              onClick={() => {
                if (editingAccount) {
                  const token = editingAccount.access_token;
                  setEditingAccount(null);
                  confirmDelete([token], "删除这个账号？");
                }
              }}
              disabled={isUpdating}
            >
              删除账号
            </Button>
            <div className="flex gap-2">
              <Button variant="ghost" onClick={() => setEditingAccount(null)} disabled={isUpdating}>取消</Button>
              <Button onClick={() => void handleUpdateAccount()} disabled={isUpdating}>
                {isUpdating ? <LoaderCircle className="size-4 animate-spin" /> : null}
                保存
              </Button>
            </div>
          </DialogFooter>
        </DialogContent>
      </Dialog>

      <ConfirmDialog
        open={Boolean(pendingDelete)}
        onOpenChange={(open) => (!open ? setPendingDelete(null) : null)}
        title={pendingDelete?.title ?? ""}
        description="账号和它们的 token 会从号池移除，无法恢复。已生成的图片不受影响。"
        confirmLabel="删除"
        danger
        busy={isDeleting}
        onConfirm={() => {
          if (!pendingDelete) return;
          void handleDeleteTokens(pendingDelete.tokens).finally(() => setPendingDelete(null));
        }}
      >
        {pendingAccounts.length > 0 ? (
          <div className="max-h-40 overflow-y-auto rounded-[7px] bg-sunken px-3 py-2 font-mono text-xs leading-6 text-ink-2">
            {pendingAccounts.slice(0, 20).map((item) => (
              <div key={item.access_token} className="truncate">{item.email || item.access_token.slice(0, 16)}</div>
            ))}
            {pendingAccounts.length > 20 ? <div>…另外 {pendingAccounts.length - 20} 个</div> : null}
          </div>
        ) : null}
      </ConfirmDialog>
    </section>
  );
}


export default function AccountsPage() {
  const { isCheckingAuth, session } = useAuthGuard(["admin"]);

  if (isCheckingAuth || !session || session.role !== "admin") {
    return <SkeletonLines lines={6} className="pt-6" />;
  }

  return <AccountsPageContent />;
}
