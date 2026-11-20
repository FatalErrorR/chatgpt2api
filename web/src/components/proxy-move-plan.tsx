"use client";

import type { ProxyMovePlan } from "@/lib/api";

/** 账号迁移方案：做不了的原因、每个接收代理的账号数变化、每个账号的去向。 */
export function ProxyMovePlanView({ plan }: { plan: ProxyMovePlan }) {
  if (plan.error) {
    return <div className="rounded-[7px] bg-bad-soft px-3 py-2.5 text-[13px] text-bad">{plan.error}</div>;
  }
  if (plan.moves.length === 0) {
    return (
      <div className="rounded-[7px] bg-sunken px-3 py-2.5 text-[13px] text-ink-2">
        {plan.unchanged > 0 ? `这 ${plan.unchanged} 个账号已经在负载最少的代理上，不用迁移。` : "没有需要迁移的账号。"}
      </div>
    );
  }
  return (
    <div className="grid gap-3">
      <div className="text-[13px] text-ink-2">
        迁移 <b className="font-medium text-ink">{plan.moves.length}</b> 个账号到 <b className="font-medium text-ink">{plan.targets.length}</b> 个代理
        {plan.unchanged > 0 ? `，另有 ${plan.unchanged} 个保持不变` : ""}。每个代理最多 {plan.max_per_proxy} 个账号。
      </div>
      <div className="thin-scrollbar max-h-40 overflow-y-auto rounded-[7px] border border-line">
        <table className="w-full text-left text-[13px]">
          <thead className="text-xs text-ink-3 [&_th]:sticky [&_th]:top-0 [&_th]:bg-surface [&_th]:shadow-[inset_0_-1px_0_var(--line)]">
            <tr>
              <th className="px-3 py-2 font-normal">接收代理</th>
              <th className="px-3 py-2 text-right font-normal">账号数</th>
            </tr>
          </thead>
          <tbody>
            {plan.targets.map((target) => (
              <tr key={target.key} className="border-t border-line first:border-t-0">
                <td className="max-w-0 truncate px-3 py-1.5 text-ink">{target.label}</td>
                <td className="px-3 py-1.5 text-right font-mono text-xs whitespace-nowrap text-ink-2 tabular-nums">
                  {target.before} → <span className="text-ink">{target.after}</span> / {plan.max_per_proxy}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      <details>
        <summary className="cursor-pointer text-xs text-ink-3 hover:text-ink">查看每个账号的去向（{plan.moves.length}）</summary>
        <ul className="thin-scrollbar mt-2 max-h-40 space-y-1 overflow-y-auto rounded-[7px] bg-sunken px-3 py-2 text-xs text-ink-2">
          {plan.moves.map((move, index) => (
            <li key={`${move.email}-${index}`} className="grid grid-cols-[minmax(0,1fr)_auto] gap-3">
              <span className="truncate text-ink">{move.email || "（无邮箱）"}</span>
              <span className="font-mono whitespace-nowrap">
                {move.from} → {move.to}
              </span>
            </li>
          ))}
        </ul>
      </details>
    </div>
  );
}
