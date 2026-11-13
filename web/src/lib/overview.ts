import { isEditUploadCooled, type StatusFilter } from "./account-format.ts";
import type { Account, ImageOutputCounts, ImageStorageStats, ProxyEntry, SystemLog } from "./api";
import { ABNORMAL_PROXY_FILTER } from "./proxy-match.ts";

/** 一块数据的加载结果：失败时不能拿空数组/0 冒充“没有问题”。 */
export type Loaded<T> = { ok: true; value: T } | { ok: false; error: string };

export function settle<T>(result: PromiseSettledResult<T>): Loaded<T> {
  if (result.status === "fulfilled") return { ok: true, value: result.value };
  const error = result.reason instanceof Error ? result.reason.message : String(result.reason || "");
  return { ok: false, error: error || "加载失败" };
}

export type OverviewSources = {
  accounts: Loaded<Account[]>;
  proxies: Loaded<ProxyEntry[]>;
  counts: Loaded<ImageOutputCounts>;
  storage: Loaded<ImageStorageStats>;
  logs: Loaded<SystemLog[]>;
};

export const SOURCE_LABELS: Record<keyof OverviewSources, string> = {
  accounts: "账号",
  proxies: "代理",
  counts: "生图统计",
  storage: "图片存储",
  logs: "调用日志",
};

export function failedSources(sources: OverviewSources) {
  return (Object.keys(SOURCE_LABELS) as Array<keyof OverviewSources>).flatMap((key) => {
    const source = sources[key];
    return source.ok ? [] : [{ key, label: SOURCE_LABELS[key], error: source.error }];
  });
}

/** 「需要处理」的一项：filter 在总览里直接筛选下面的账号表；href 跳到别的页面；retry 重新加载失败的那块。 */
export type Todo = {
  tone: "bad" | "warn";
  title: string;
  note: string;
  action: string;
  filter?: { status?: StatusFilter; proxy?: string };
  href?: string;
  retry?: keyof OverviewSources;
};

const LOW_DISK_MB = 2048;

export function formatSize(mb: number) {
  return mb >= 1024 ? `${(mb / 1024).toFixed(1)} GB` : `${mb} MB`;
}

/** 「需要处理」：加载失败的块排最前，可重试；只有全部加载成功且没有待办时才是空列表。 */
export function buildTodos(sources: OverviewSources, now = Date.now()): Todo[] {
  const todos: Todo[] = failedSources(sources).map((failure) => ({
    tone: "bad",
    title: `${failure.label}加载失败`,
    note: failure.error,
    action: "重试",
    retry: failure.key,
  }));
  if (sources.proxies.ok) {
    const bad = sources.proxies.value.filter((proxy) => proxy.health.abnormal);
    if (bad.length > 0) {
      const firstError = bad.find((proxy) => proxy.health.last_error)?.health.last_error ?? "";
      todos.push({
        tone: "bad",
        title: `${bad.length} 个代理异常`,
        note: firstError || "连续请求失败",
        action: "筛选账号",
        filter: { proxy: ABNORMAL_PROXY_FILTER },
      });
    }
  }
  if (sources.accounts.ok) {
    const abnormal = sources.accounts.value.filter((item) => item.status === "异常").length;
    const cooled = sources.accounts.value.filter((item) => isEditUploadCooled(item, now)).length;
    if (abnormal > 0) {
      todos.push({ tone: "bad", title: `${abnormal} 个账号异常`, note: "可选中后尝试恢复，或批量移除", action: "筛选", filter: { status: "异常" } });
    }
    if (cooled > 0) {
      todos.push({ tone: "warn", title: `${cooled} 个账号图生图冷却`, note: "冷却结束后自动恢复", action: "筛选", filter: { status: "edit_cooldown" } });
    }
  }
  if (sources.storage.ok && sources.storage.value.disk_free_mb < LOW_DISK_MB) {
    todos.push({ tone: "warn", title: "磁盘空间不足", note: `剩余 ${formatSize(sources.storage.value.disk_free_mb)}`, action: "去清理", href: "/image-manager" });
  }
  return todos;
}
