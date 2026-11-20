"use client";

import { AnimatedThemeToggler } from "@/components/ui/animated-theme-toggler";

export function ThemeToggle() {
  return (
    <AnimatedThemeToggler
      aria-label="切换主题"
      title="切换主题"
      variant="circle"
      className="inline-flex size-8 shrink-0 items-center justify-center text-ink-3 transition hover:text-ink [&_svg]:size-4"
    />
  );
}
