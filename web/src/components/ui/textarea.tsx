import * as React from "react";

import { cn } from "@/lib/utils";

function Textarea({ className, ...props }: React.ComponentProps<"textarea">) {
  return (
    <textarea
      data-slot="textarea"
      className={cn(
        "placeholder:text-ink-3 focus-visible:border-clay focus-visible:ring-[3px] focus-visible:ring-clay-soft block min-h-24 w-full rounded-[7px] border border-line-strong bg-surface px-3 py-2.5 text-[13px] leading-6 text-ink outline-none disabled:cursor-not-allowed disabled:opacity-50",
        className,
      )}
      {...props}
    />
  );
}

export { Textarea };
