import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";

import { cn } from "@/lib/utils";

const badgeVariants = cva(
  "inline-flex items-center rounded-full border px-2.5 py-0.5 text-xs font-medium transition-colors",
  {
    variants: {
      variant: {
        default: "border-transparent bg-sunken text-ink",
        secondary: "border-transparent bg-sunken text-ink-2",
        outline: "border-line bg-surface text-ink-2",
        success: "border-transparent bg-ok-soft text-ok",
        warning: "border-transparent bg-warn-soft text-warn",
        danger: "border-transparent bg-bad-soft text-bad",
        info: "border-transparent bg-sunken text-info",
        violet: "border-transparent bg-sunken text-ink-2",
      },
    },
    defaultVariants: {
      variant: "default",
    },
  },
);

function Badge({
  className,
  variant,
  ...props
}: React.ComponentProps<"span"> & VariantProps<typeof badgeVariants>) {
  return (
    <span className={cn(badgeVariants({ variant }), className)} {...props} />
  );
}

export { Badge, badgeVariants };
