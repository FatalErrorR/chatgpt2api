"use client";

import * as React from "react";
import { ChevronLeft, ChevronRight } from "lucide-react";
import { DayPicker } from "react-day-picker";

import { cn } from "@/lib/utils";

function Calendar({
  className,
  classNames,
  showOutsideDays = true,
  ...props
}: React.ComponentProps<typeof DayPicker>) {
  return (
    <DayPicker
      showOutsideDays={showOutsideDays}
      className={cn("p-1 text-sm", className)}
      classNames={{
        months: "flex flex-col gap-4 sm:flex-row",
        month: "relative",
        month_caption: "flex h-9 items-center justify-center font-medium",
        nav: "absolute inset-x-2 top-2 flex items-center justify-between",
        button_previous: "inline-flex size-8 items-center justify-center rounded-[7px] text-ink-2 hover:bg-surface-hover hover:text-ink",
        button_next: "inline-flex size-8 items-center justify-center rounded-[7px] text-ink-2 hover:bg-surface-hover hover:text-ink",
        weekdays: "mt-2 grid grid-cols-7 text-xs text-ink-3",
        weekday: "flex h-8 items-center justify-center font-normal",
        week: "grid grid-cols-7",
        day: "size-9 p-0 text-center",
        day_button: "size-9 rounded-[7px] text-[13px] text-ink transition hover:bg-surface-hover",
        today: "font-semibold text-clay",
        selected: "[&_button]:bg-ink [&_button]:text-page [&_button]:hover:bg-ink/85",
        outside: "text-ink-3 opacity-60",
        disabled: "text-ink-3 opacity-40",
        ...classNames,
      }}
      components={{
        Chevron: ({ orientation }) =>
          orientation === "left" ? <ChevronLeft className="size-4" /> : <ChevronRight className="size-4" />,
      }}
      {...props}
    />
  );
}

export { Calendar };
