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

/**
 * 列表里显示的代理短名：有名字用名字，否则用 主机:端口。
 * masked_url 开头是账号名（同一家代理商的账号名都一样），截断后十个代理会显示成同一串。
 */
export function proxyShortName(proxy: Pick<ProxyEntry, "name" | "masked_url">): string {
  if (proxy.name.trim()) return proxy.name.trim();
  const rest = proxy.masked_url.replace(/^[a-z0-9+.-]+:\/\//i, "");
  const hostPort = rest.slice(rest.lastIndexOf("@") + 1).replace(/\/.*$/, "");
  return hostPort || proxy.masked_url;
}

/** 悬停提示：名字（或主机:端口）加完整的遮罩地址。 */
export function proxyTitle(proxy: Pick<ProxyEntry, "name" | "masked_url">): string {
  const short = proxyShortName(proxy);
  return short === proxy.masked_url ? short : `${short}（${proxy.masked_url}）`;
}
