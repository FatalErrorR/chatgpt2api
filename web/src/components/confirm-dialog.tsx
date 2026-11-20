"use client";

import { LoaderCircle } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";

/** 统一的确认对话框：衬线标题、写清影响范围的说明、可选的明细、右下角操作。 */
export function ConfirmDialog({
  open,
  onOpenChange,
  title,
  description,
  children,
  confirmLabel = "确认",
  danger = false,
  busy = false,
  onConfirm,
}: {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: string;
  description?: React.ReactNode;
  children?: React.ReactNode;
  confirmLabel?: string;
  danger?: boolean;
  busy?: boolean;
  onConfirm: () => void;
}) {
  return (
    <Dialog open={open} onOpenChange={(next) => (!busy ? onOpenChange(next) : null)}>
      <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-[480px]">
        <DialogHeader className="gap-1.5">
          <DialogTitle className="font-serif text-[22px] font-medium">{title}</DialogTitle>
          {description ? <DialogDescription className="text-[13px] leading-6 text-ink-2">{description}</DialogDescription> : null}
        </DialogHeader>
        {children}
        <DialogFooter className="pt-2">
          <Button variant="ghost" onClick={() => onOpenChange(false)} disabled={busy}>
            取消
          </Button>
          <Button variant={danger ? "destructive" : "default"} onClick={onConfirm} disabled={busy}>
            {busy ? <LoaderCircle className="size-4 animate-spin" /> : null}
            {confirmLabel}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
