import { Checkbox } from "@/components/ui/checkbox";

/** 设置组：左边说明，右边表单。窄屏改为上下排列。 */
export function SettingGroup({
  title,
  description,
  children,
}: {
  title: string;
  description?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <section className="grid gap-4 border-t border-line py-[26px] first:border-t-0 first:pt-0 lg:grid-cols-[260px_minmax(0,1fr)] lg:gap-8">
      <div className="min-w-0">
        <h2 className="text-[19px] font-medium text-ink">{title}</h2>
        {description ? <p className="mt-1.5 text-[13px] leading-6 text-ink-3">{description}</p> : null}
      </div>
      <div className="grid min-w-0 content-start gap-4">{children}</div>
    </section>
  );
}

export function SettingField({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: React.ReactNode;
  children: React.ReactNode;
}) {
  return (
    <label className="grid gap-1.5 text-[13px] text-ink-2">
      {label}
      {children}
      {hint ? <span className="text-xs leading-5 text-ink-3">{hint}</span> : null}
    </label>
  );
}

/** 开关行：标题和说明在左，复选框在右。 */
export function ToggleRow({
  title,
  description,
  checked,
  onCheckedChange,
  disabled,
}: {
  title: string;
  description?: string;
  checked: boolean;
  onCheckedChange: (checked: boolean) => void;
  disabled?: boolean;
}) {
  return (
    <label className="flex items-center justify-between gap-5 rounded-[7px] border border-line bg-surface px-3.5 py-3">
      <span className="min-w-0">
        <span className="block text-[13px] font-medium text-ink">{title}</span>
        {description ? <span className="mt-0.5 block text-xs leading-5 text-ink-3">{description}</span> : null}
      </span>
      <Checkbox checked={checked} onCheckedChange={(value) => onCheckedChange(Boolean(value))} disabled={disabled} />
    </label>
  );
}
