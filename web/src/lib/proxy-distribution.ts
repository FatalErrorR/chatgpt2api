export type DistributionCandidate = {
  url: string;
  accountCount: number;
  abnormal: boolean;
};

export type DistributionPreview = DistributionCandidate & { assigned: number };

/**
 * 和后端 ProxyList.reserve 同一规则：每次把一个新账号分给当前负载最少的代理，
 * 平手按候选顺序。异常代理先跳过；全部异常时仍在它们之间分配。
 * 负载用现有 account_count，不含其它批次的预留。
 */
export function previewProxyDistribution(candidates: DistributionCandidate[], newCount: number): DistributionPreview[] {
  const usable = candidates.some((item) => !item.abnormal) ? candidates.filter((item) => !item.abnormal) : candidates;
  const assigned = new Map(candidates.map((item) => [item.url, 0]));
  const loads = new Map(usable.map((item) => [item.url, item.accountCount]));
  const count = Math.max(0, newCount);
  for (let index = 0; index < count; index += 1) {
    let pick = usable[0];
    if (!pick) break;
    for (const item of usable) {
      if ((loads.get(item.url) ?? 0) < (loads.get(pick.url) ?? 0)) pick = item;
    }
    loads.set(pick.url, (loads.get(pick.url) ?? 0) + 1);
    assigned.set(pick.url, (assigned.get(pick.url) ?? 0) + 1);
  }
  return candidates.map((item) => ({ ...item, assigned: assigned.get(item.url) ?? 0 }));
}
