import * as React from "react";

import { cn } from "@/lib/utils";

function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <input
      type={type}
      data-slot="input"
      className={cn(
        "file:text-foreground placeholder:text-ink-3 selection:bg-primary selection:text-primary-foreground flex h-[34px] w-full min-w-0 rounded-[7px] border border-line-strong bg-surface px-[11px] py-1 text-[13px] text-ink transition-[color,box-shadow] outline-none file:inline-flex file:h-7 file:border-0 file:bg-transparent file:text-sm file:font-medium disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50 focus-visible:border-clay focus-visible:ring-[3px] focus-visible:ring-clay-soft",
        className,
      )}
      {...props}
    />
  );
}

export { Input };
