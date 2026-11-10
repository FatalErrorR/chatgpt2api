"use client";

import type { ReactNode } from "react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import webConfig from "@/constants/common-env";
import { useVersionCheck } from "@/hooks/use-version-check";
import { cn } from "@/lib/utils";

function typeVariant(type: string): "success" | "danger" | "info" | "violet" | "outline" {
  if (type === "新增") return "success";
  if (type === "修复") return "danger";
  if (type === "调整") return "info";
  if (type === "文档") return "violet";
  return "outline";
}

export function VersionReleaseDialog({ className }: { className?: string }) {
  const {
    open,
    setOpen,
    openReleaseModal,
    latestVersion,
    releases,
    checking,
    hasNewVersion,
    checkLatestRelease,
  } = useVersionCheck();

  return (
    <>
      <button
        type="button"
        className={cn(
          "relative px-1 py-1 text-[11px] font-medium text-ink-3 transition hover:text-ink",
          className,
        )}
        onClick={openReleaseModal}
        title="查看版本更新"
      >
        v{webConfig.appVersion}
        {hasNewVersion ? (
          <span className="absolute -top-1 -right-1 size-2 rounded-full bg-clay" />
        ) : null}
      </button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent showCloseButton={false} className="w-[min(94vw,680px)] rounded-[14px] p-6">
          <DialogHeader className="gap-1.5">
            <DialogTitle className="font-serif text-[22px] font-medium">版本更新</DialogTitle>
            <DialogDescription className="text-[13px] leading-6 text-ink-2">
              当前 {webConfig.appVersion}{latestVersion ? `，最新 ${latestVersion}` : ""}。
            </DialogDescription>
          </DialogHeader>
          <div className="grid grid-cols-2 gap-3">
            <VersionCard label="当前版本" value={webConfig.appVersion} />
            <VersionCard
              label="最新版本"
              value={latestVersion}
              action={
                <button
                  type="button"
                  className="text-[11px] text-ink-3 underline-offset-2 hover:text-ink hover:underline"
                  onClick={() => void checkLatestRelease(true)}
                >
                  {checking ? "检查中..." : "检查更新"}
                </button>
              }
            />
          </div>
          <div className="max-h-[56vh] space-y-5 overflow-y-auto pr-1">
            {releases.map((release) => (
              <div key={release.version} className="border-l border-line pl-4">
                <div className="flex flex-wrap items-center gap-2">
                  <span className="text-sm font-medium text-ink">
                    {release.version === "Unreleased" ? "未发布" : release.version}
                  </span>
                  <span className="text-xs text-ink-3">{release.date}</span>
                  {release.version === latestVersion ? <Badge variant="success">最新</Badge> : null}
                  {release.version === webConfig.appVersion ? <Badge variant="outline">当前</Badge> : null}
                </div>
                <div className="mt-2 space-y-1.5">
                  {release.items.map((item, index) => (
                    <div key={index} className="flex items-start gap-2 text-sm leading-6 text-ink-2">
                      <Badge variant={typeVariant(item.type)} className="mt-0.5 shrink-0">
                        {item.type}
                      </Badge>
                      <span className="min-w-0 flex-1">{item.content}</span>
                    </div>
                  ))}
                </div>
              </div>
            ))}
          </div>
          <Button variant="outline" size="sm" asChild>
            <a href="https://github.com/basketikun/chatgpt2api" target="_blank" rel="noreferrer">
              前往 GitHub 更新
            </a>
          </Button>
        </DialogContent>
      </Dialog>
    </>
  );
}

function VersionCard({
  label,
  value,
  action,
}: {
  label: string;
  value: string;
  action?: ReactNode;
}) {
  return (
    <div className="rounded-[7px] border border-line bg-sunken p-3">
      <div className="flex items-center justify-between gap-2">
        <div className="text-xs text-ink-3">{label}</div>
        {action}
      </div>
      <div className="mt-1 text-base font-medium text-ink">{value}</div>
    </div>
  );
}
