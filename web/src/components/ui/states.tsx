import { RefreshCw } from "lucide-react";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

/** 加载中：骨架占位，不用转圈遮罩。 */
export function SkeletonLines({ lines = 4, className }: { lines?: number; className?: string }) {
  const widths = ["80%", "95%", "60%", "88%", "72%", "90%"];
  return (
    <div aria-busy="true" aria-label="加载中" className={cn("space-y-2.5 py-2", className)}>
      {Array.from({ length: lines }, (_, index) => (
        <div
          key={index}
          className="h-3 animate-pulse rounded-[6px] bg-gradient-to-r from-sunken via-surface-hover to-sunken"
          style={{ width: widths[index % widths.length] }}
        />
      ))}
    </div>
  );
}

/** 没有数据：说明原因并给出下一步。 */
export function EmptyState({ title, description, action, className }: { title: string; description?: React.ReactNode; action?: React.ReactNode; className?: string }) {
  return (
    <div className={cn("grid justify-items-center gap-1 px-6 py-12 text-center text-[13px] text-ink-3", className)}>
      <span className="font-serif text-[17px] text-ink">{title}</span>
      {description ? <span>{description}</span> : null}
      {action ? <div className="mt-3">{action}</div> : null}
    </div>
  );
}

/** 出错：写清原因，可重试。 */
export function ErrorState({ message, onRetry, className }: { message: string; onRetry?: () => void; className?: string }) {
  return (
    <div className={cn("grid justify-items-start gap-3", className)}>
      <div className="w-full rounded-[7px] border border-bad-soft bg-bad-soft px-3 py-2.5 text-[13px] text-bad">{message}</div>
      {onRetry ? (
        <Button variant="outline" size="sm" onClick={onRetry}>
          <RefreshCw className="size-3.5" />
          重试
        </Button>
      ) : null}
    </div>
  );
}
