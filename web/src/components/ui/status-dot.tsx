import { cn } from "@/lib/utils";

export type StatusTone = "ok" | "warn" | "bad" | "off";

const DOT: Record<StatusTone, string> = {
  ok: "bg-ok",
  warn: "bg-warn",
  bad: "bg-bad",
  off: "bg-ink-3",
};

/** 状态：圆点 + 文字，颜色只表达含义，不用大色块。 */
export function StatusDot({
  tone,
  children,
  className,
  title,
}: {
  tone: StatusTone;
  children?: React.ReactNode;
  className?: string;
  title?: string;
}) {
  return (
    <span className={cn("inline-flex items-center gap-[7px] text-[13px] whitespace-nowrap text-ink", className)} title={title}>
      <i className={cn("size-[7px] shrink-0 rounded-full", DOT[tone])} />
      {children}
    </span>
  );
}
