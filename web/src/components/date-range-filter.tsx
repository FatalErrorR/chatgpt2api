"use client";

import { format, parse } from "date-fns";
import { CalendarIcon } from "lucide-react";
import type { DateRange } from "react-day-picker";

import { Button } from "@/components/ui/button";
import { Calendar } from "@/components/ui/calendar";
import { Field } from "@/components/ui/field";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { cn } from "@/lib/utils";

type DateRangeFilterProps = {
  startDate: string;
  endDate: string;
  onChange: (startDate: string, endDate: string) => void;
};

export function DateRangeFilter({ startDate, endDate, onChange }: DateRangeFilterProps) {
  const selected: DateRange | undefined = startDate
    ? {
        from: parse(startDate, "yyyy-MM-dd", new Date()),
        to: endDate ? parse(endDate, "yyyy-MM-dd", new Date()) : undefined,
      }
    : undefined;

  const label = startDate ? `${startDate} 至 ${endDate || startDate}` : "全部日期";

  return (
    <Field className="w-auto">
      <Popover>
        <PopoverTrigger asChild>
          <Button variant="outline" className={cn("h-[34px] justify-start px-[11px] text-[13px] font-normal", startDate ? "text-ink" : "text-ink-3")}>
            <CalendarIcon className="size-4 text-ink-3" />
            {label}
          </Button>
        </PopoverTrigger>
        <PopoverContent className="w-auto p-3" align="start">
          <Calendar
            mode="range"
            defaultMonth={selected?.from}
            selected={selected}
            onSelect={(value) => onChange(value?.from ? format(value.from, "yyyy-MM-dd") : "", value?.to ? format(value.to, "yyyy-MM-dd") : "")}
            numberOfMonths={2}
          />
        </PopoverContent>
      </Popover>
    </Field>
  );
}
