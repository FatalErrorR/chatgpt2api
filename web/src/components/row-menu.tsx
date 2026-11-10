"use client";

import { useState } from "react";
import { MoreHorizontal } from "lucide-react";

import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";

export type RowMenuItem = {
  label: string;
  icon?: React.ComponentType<{ className?: string }>;
  onSelect: () => void;
  danger?: boolean;
  disabled?: boolean;
};

/** 「⋯」菜单：把低频操作收起来，常驻按钮控制在最少。 */
export function RowMenu({ items, label = "更多操作", className }: { items: RowMenuItem[]; label?: string; className?: string }) {
  const [open, setOpen] = useState(false);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger
        aria-label={label}
        title={label}
        className={cn("grid size-8 place-items-center rounded-[7px] text-ink-3 transition hover:bg-surface-hover hover:text-ink", className)}
      >
        <MoreHorizontal className="size-4" />
      </PopoverTrigger>
      <PopoverContent align="end" className="w-44 p-1">
        {items.map((item) => {
          const Icon = item.icon;
          return (
            <button
              key={item.label}
              type="button"
              disabled={item.disabled}
              onClick={() => {
                setOpen(false);
                item.onSelect();
              }}
              className={cn(
                "flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-left text-[13px] transition disabled:opacity-40",
                item.danger ? "text-bad hover:bg-bad-soft" : "text-ink-2 hover:bg-surface-hover hover:text-ink",
              )}
            >
              {Icon ? <Icon className="size-4" /> : null}
              {item.label}
            </button>
          );
        })}
      </PopoverContent>
    </Popover>
  );
}
