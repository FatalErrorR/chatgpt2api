import type { Account, ProxyEntry } from "./api";

type AccountProxy = Pick<Account, "proxy" | "proxy_key">;

/** 代理筛选："all" 全部，"none" 未指定代理，其余为代理清单的 key（同一代理不同写法共用）。 */
export function accountMatchesProxyFilter(account: AccountProxy, filter: string): boolean {
  const proxy = (account.proxy || "").trim();
  if (filter === "all") return true;
  if (filter === "none") return !proxy;
  return !!proxy && account.proxy_key === filter;
}

/** 按后端给的 key 找账号所用代理在清单里的那一条（名称、健康）；没配代理或清单还没加载时返回 undefined。 */
export function proxyForAccount(proxiesByKey: Map<string, ProxyEntry>, account: AccountProxy): ProxyEntry | undefined {
  if (!(account.proxy || "").trim() || !account.proxy_key) return undefined;
  return proxiesByKey.get(account.proxy_key);
}
