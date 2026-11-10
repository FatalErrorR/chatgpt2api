"use client";

import Link from "next/link";
import { useEffect, useState, type ComponentType } from "react";
import { usePathname, useRouter } from "next/navigation";
import {
  Code,
  Image as ImageIcon,
  Images,
  Infinity as InfinityIcon,
  LayoutGrid,
  LogOut,
  MoreHorizontal,
  ScrollText,
  Settings,
  Users,
} from "lucide-react";

import { ThemeToggle } from "@/components/theme-toggle";
import { VersionReleaseDialog } from "@/components/version-release-dialog";
import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import { Sheet, SheetClose, SheetContent, SheetHeader, SheetTitle, SheetTrigger } from "@/components/ui/sheet";
import webConfig from "@/constants/common-env";
import { fetchThirdPartyApps, type ThirdPartyAppsSettings } from "@/lib/api";
import { getValidatedAuthSession } from "@/lib/auth-session";
import { cn } from "@/lib/utils";
import { clearStoredAuthSession, getStoredAuthSession, type StoredAuthSession } from "@/store/auth";

type NavItem = { href: string; label: string; icon: ComponentType<{ className?: string }> };

// 管理员导航：分组之间画分隔线
const ADMIN_NAV: NavItem[][] = [
  [
    { href: "/", label: "概览", icon: LayoutGrid },
    { href: "/image", label: "生图", icon: ImageIcon },
  ],
  [
    { href: "/accounts", label: "号池", icon: Users },
    { href: "/image-manager", label: "图片", icon: Images },
    { href: "/logs", label: "日志", icon: ScrollText },
  ],
  [
    { href: "/settings", label: "设置", icon: Settings },
    { href: "/debug", label: "调试", icon: Code },
  ],
];

// 手机底栏放最常用的四个，其余进「更多」
const MOBILE_TABS = ["/", "/image", "/accounts", "/image-manager"];

function buildThirdPartyHref(appUrl: string, baseUrl: string, apiKey: string) {
  const url = appUrl.trim();
  try {
    const target = new URL(url);
    target.searchParams.set("apiKey", apiKey);
    target.searchParams.set("baseUrl", baseUrl);
    return target.toString();
  } catch {
    return `${url}${url.includes("?") ? "&" : "?"}apiKey=${encodeURIComponent(apiKey)}&baseUrl=${encodeURIComponent(baseUrl)}`;
  }
}

function isActive(pathname: string, href: string) {
  return href === "/" ? pathname === "/" : pathname === href || pathname.startsWith(`${href}/`);
}

function RailLink({ item, active }: { item: NavItem; active: boolean }) {
  const Icon = item.icon;
  return (
    <Link
      href={item.href}
      className={cn(
        "grid w-[52px] justify-items-center gap-1 rounded-[9px] py-2 text-[11px] transition",
        active
          ? "bg-surface text-ink shadow-[inset_0_0_0_1px_var(--line)] [&_svg]:text-clay"
          : "text-ink-2 hover:bg-surface-hover hover:text-ink",
      )}
    >
      <Icon className="size-[18px]" />
      {item.label}
    </Link>
  );
}

function AccountMenu({ session, onLogout, className }: { session: StoredAuthSession; onLogout: () => void; className?: string }) {
  const roleLabel = session.role === "admin" ? "管理员" : "普通用户";
  const displayName = session.name.trim() || roleLabel;
  return (
    <Popover>
      <PopoverTrigger
        className={cn(
          "grid size-8 place-items-center rounded-full bg-ink text-xs font-medium text-page transition hover:opacity-85",
          className,
        )}
        title={`${roleLabel} · ${displayName}`}
      >
        {displayName.slice(0, 1).toUpperCase()}
      </PopoverTrigger>
      <PopoverContent side="right" align="end" className="w-52 p-2">
        <div className="px-2 py-1.5">
          <div className="text-sm text-ink">{displayName}</div>
          <div className="text-xs text-ink-3">{roleLabel}</div>
        </div>
        <button
          type="button"
          onClick={onLogout}
          className="mt-1 flex w-full items-center gap-2 rounded-md px-2 py-1.5 text-sm text-ink-2 transition hover:bg-surface-hover hover:text-ink"
        >
          <LogOut className="size-4" />
          退出登录
        </button>
      </PopoverContent>
    </Popover>
  );
}

export function AppShell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const router = useRouter();
  const [session, setSession] = useState<StoredAuthSession | null | undefined>(undefined);
  const [thirdPartyApps, setThirdPartyApps] = useState<ThirdPartyAppsSettings | null>(null);
  const [isCanvasDialogOpen, setIsCanvasDialogOpen] = useState(false);

  useEffect(() => {
    let active = true;
    const load = async () => {
      if (pathname === "/login") {
        if (active) setSession(null);
        return;
      }
      // 先用本地会话定下布局（读本地存储，很快），再联网校验；否则校验期间按未登录渲染，
      // 没有 64px 侧栏，校验通过后侧栏出现、内容整体右移
      const storedSession = await getStoredAuthSession();
      if (active) setSession((current) => (current === undefined ? storedSession : current));
      const validatedSession = await getValidatedAuthSession();
      if (active) setSession(validatedSession);
    };
    void load();
    return () => {
      active = false;
    };
  }, [pathname]);

  // 本地会话和校验后的会话是两个对象，按 key 触发，避免重复请求
  const sessionKey = session?.key ?? "";
  useEffect(() => {
    if (!sessionKey) {
      setThirdPartyApps(null);
      return;
    }
    let active = true;
    const load = async () => {
      try {
        const data = await fetchThirdPartyApps();
        if (active) setThirdPartyApps(data.third_party_apps);
      } catch {
        if (active) setThirdPartyApps(null);
      }
    };
    const reload = () => void load();
    void load();
    window.addEventListener("third-party-apps-updated", reload);
    return () => {
      active = false;
      window.removeEventListener("third-party-apps-updated", reload);
    };
  }, [sessionKey]);

  const handleLogout = async () => {
    await clearStoredAuthSession();
    router.replace("/login");
  };

  // 还没读到本地会话：先不渲染，等布局确定（只有首屏的几毫秒）
  if (session === undefined && pathname !== "/login") {
    return null;
  }

  if (pathname === "/login" || !session) {
    return <div className="mx-auto w-full max-w-[1440px] px-4 sm:px-8">{children}</div>;
  }

  const baseUrl = webConfig.apiUrl.replace(/\/$/, "") || window.location.origin;
  const canvas = thirdPartyApps?.infinite_canvas;
  const canvasHref = canvas?.enabled && canvas.url.trim() ? buildThirdPartyHref(canvas.url, baseUrl, session.key) : "";
  const canvasDisplayHref = canvasHref ? decodeURIComponent(canvasHref) : "";
  const openCanvas = () => {
    if (canvasHref) setIsCanvasDialogOpen(true);
  };

  const canvasDialog = (
    <Dialog open={isCanvasDialogOpen} onOpenChange={setIsCanvasDialogOpen}>
      <DialogContent showCloseButton={false} className="rounded-2xl p-6">
        <DialogHeader className="gap-2">
          <DialogTitle>跳转到三方应用</DialogTitle>
          <DialogDescription className="text-sm leading-6">
            该入口仅供个人测试使用，建议自行本机部署后再长期使用。跳转地址会默认带上本项目地址和当前密钥，用于自动填充连接信息；如果不放心，可以取消后手动前往应用并自行输入。
          </DialogDescription>
        </DialogHeader>
        <div className="rounded-lg border border-line bg-sunken px-3 py-2 font-mono text-xs break-all text-ink-2">{canvasDisplayHref}</div>
        <DialogFooter className="pt-2">
          <Button variant="ghost" onClick={() => setIsCanvasDialogOpen(false)}>
            取消
          </Button>
          <Button
            onClick={() => {
              if (canvasHref) window.open(canvasHref, "_blank", "noopener,noreferrer");
              setIsCanvasDialogOpen(false);
            }}
          >
            继续跳转
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );

  // 普通用户只有画图：不显示侧栏，右上角一条小栏
  if (session.role !== "admin") {
    return (
      <>
        <header className="mx-auto flex w-full max-w-[1440px] items-center justify-between px-4 py-3 sm:px-8">
          <Link href="/image" className="flex items-center gap-2 font-serif text-lg text-ink">
            <span className="grid size-7 place-items-center rounded-lg bg-clay font-sans text-sm font-semibold text-white">C</span>
            画图
          </Link>
          <div className="flex items-center gap-2">
            {canvasHref ? (
              <button type="button" onClick={openCanvas} className="rounded-md px-2 py-1 text-sm text-ink-2 hover:text-ink">
                无限画布
              </button>
            ) : null}
            <ThemeToggle />
            <AccountMenu session={session} onLogout={() => void handleLogout()} />
          </div>
        </header>
        <div className="mx-auto w-full max-w-[1440px] px-4 pb-8 sm:px-8">{children}</div>
        {canvasDialog}
      </>
    );
  }

  const allItems = ADMIN_NAV.flat();
  const mobileTabs = allItems.filter((item) => MOBILE_TABS.includes(item.href));
  const moreItems = allItems.filter((item) => !MOBILE_TABS.includes(item.href));

  return (
    <>
      {/* 桌面：左侧 64px 图标栏 */}
      <aside className="fixed inset-y-0 left-0 z-30 hidden w-16 flex-col items-center gap-1 border-r border-line bg-sunken py-3.5 md:flex">
        <Link href="/" className="mb-3 grid size-[30px] place-items-center rounded-lg bg-clay text-sm font-semibold text-white" title="chatgpt2api">
          C
        </Link>
        {ADMIN_NAV.map((group, index) => (
          <div key={index} className="contents">
            {index > 0 ? <span className="my-1.5 h-px w-7 bg-line" /> : null}
            {group.map((item) => (
              <RailLink key={item.href} item={item} active={isActive(pathname, item.href)} />
            ))}
            {index === 0 && canvasHref ? (
              <button
                type="button"
                onClick={openCanvas}
                className="grid w-[52px] justify-items-center gap-1 rounded-[9px] py-2 text-[11px] text-ink-2 transition hover:bg-surface-hover hover:text-ink"
              >
                <InfinityIcon className="size-[18px]" />
                画布
              </button>
            ) : null}
          </div>
        ))}
        <div className="mt-auto grid justify-items-center gap-2">
          <ThemeToggle />
          <a
            href="https://github.com/basketikun/chatgpt2api"
            target="_blank"
            rel="noreferrer"
            className="grid size-8 place-items-center opacity-60 transition hover:opacity-100"
            aria-label="GitHub repository"
          >
            <img src="/github.svg" alt="" className="size-4 dark:invert" />
          </a>
          <VersionReleaseDialog />
          <AccountMenu session={session} onLogout={() => void handleLogout()} />
        </div>
      </aside>

      <div className="md:pl-16">
        <div className="mx-auto w-full max-w-[1440px] px-4 pt-4 pb-24 sm:px-8 sm:pt-8 md:pb-10">{children}</div>
      </div>

      {/* 手机：底部标签栏 */}
      <nav className="fixed inset-x-0 bottom-0 z-30 grid grid-cols-5 border-t border-line bg-sunken px-1 pt-1.5 pb-[max(10px,env(safe-area-inset-bottom))] md:hidden">
        {mobileTabs.map((item) => {
          const Icon = item.icon;
          const active = isActive(pathname, item.href);
          return (
            <Link key={item.href} href={item.href} className={cn("grid justify-items-center gap-0.5 text-[10px]", active ? "text-ink [&_svg]:text-clay" : "text-ink-3")}>
              <Icon className="size-5" />
              {item.label}
            </Link>
          );
        })}
        <Sheet>
          <SheetTrigger className={cn("grid justify-items-center gap-0.5 text-[10px]", moreItems.some((item) => isActive(pathname, item.href)) ? "text-ink" : "text-ink-3")}>
            <MoreHorizontal className="size-5" />
            更多
          </SheetTrigger>
          <SheetContent side="bottom" className="rounded-t-2xl">
            <SheetHeader>
              <SheetTitle className="font-serif">更多</SheetTitle>
            </SheetHeader>
            <nav className="grid gap-1 px-4 pb-6">
              {moreItems.map((item) => {
                const Icon = item.icon;
                return (
                  <SheetClose asChild key={item.href}>
                    <Link href={item.href} className="flex items-center gap-3 rounded-lg px-3 py-2.5 text-sm text-ink-2 hover:bg-surface-hover hover:text-ink">
                      <Icon className="size-4" />
                      {item.label}
                    </Link>
                  </SheetClose>
                );
              })}
              {canvasHref ? (
                <SheetClose asChild>
                  <button type="button" onClick={openCanvas} className="flex items-center gap-3 rounded-lg px-3 py-2.5 text-left text-sm text-ink-2 hover:bg-surface-hover hover:text-ink">
                    <InfinityIcon className="size-4" />
                    无限画布
                  </button>
                </SheetClose>
              ) : null}
              <div className="mt-2 flex items-center justify-between border-t border-line px-3 pt-3">
                <div className="flex items-center gap-1">
                  <ThemeToggle />
                  <VersionReleaseDialog />
                </div>
                <button type="button" onClick={() => void handleLogout()} className="flex items-center gap-2 text-sm text-ink-2 hover:text-ink">
                  <LogOut className="size-4" />
                  退出
                </button>
              </div>
            </nav>
          </SheetContent>
        </Sheet>
      </nav>
      {canvasDialog}
    </>
  );
}
