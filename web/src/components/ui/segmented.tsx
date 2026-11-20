import { cn } from "@/lib/utils";

/** 分段控件的外框和选项样式；Tabs 版（调试页）直接套这两组类名，保证各页一致。 */
export const segmentedListClass = "inline-flex h-auto w-fit max-w-full flex-wrap gap-0.5 rounded-[9px] bg-sunken p-[3px]";
export const segmentedItemClass =
  "h-7 flex-none rounded-[7px] px-3 text-[13px] font-normal text-ink-2 transition hover:text-ink data-[state=active]:bg-surface data-[state=active]:text-ink data-[state=active]:shadow-[0_1px_2px_rgba(0,0,0,.06)]";

export function Segmented<T extends string>({
  value,
  options,
  onChange,
  className,
}: {
  value: T;
  options: ReadonlyArray<readonly [T, string]>;
  onChange: (value: T) => void;
  className?: string;
}) {
  return (
    <div role="tablist" className={cn(segmentedListClass, className)}>
      {options.map(([option, label]) => (
        <button
          key={option}
          type="button"
          role="tab"
          aria-selected={value === option}
          data-state={value === option ? "active" : "inactive"}
          onClick={() => onChange(option)}
          className={segmentedItemClass}
        >
          {label}
        </button>
      ))}
    </div>
  );
}
