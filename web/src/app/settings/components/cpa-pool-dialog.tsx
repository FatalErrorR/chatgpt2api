"use client";

import { Eye, EyeOff, Link2, LoaderCircle, Save, Unplug } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";

import { useSettingsStore } from "../store";

export function CPAPoolDialog() {
  const dialogOpen = useSettingsStore((state) => state.dialogOpen);
  const editingPool = useSettingsStore((state) => state.editingPool);
  const formName = useSettingsStore((state) => state.formName);
  const formBaseUrl = useSettingsStore((state) => state.formBaseUrl);
  const formSecretKey = useSettingsStore((state) => state.formSecretKey);
  const showSecret = useSettingsStore((state) => state.showSecret);
  const isSavingPool = useSettingsStore((state) => state.isSavingPool);
  const setDialogOpen = useSettingsStore((state) => state.setDialogOpen);
  const setFormName = useSettingsStore((state) => state.setFormName);
  const setFormBaseUrl = useSettingsStore((state) => state.setFormBaseUrl);
  const setFormSecretKey = useSettingsStore((state) => state.setFormSecretKey);
  const setShowSecret = useSettingsStore((state) => state.setShowSecret);
  const savePool = useSettingsStore((state) => state.savePool);

  return (
    <Dialog open={dialogOpen} onOpenChange={(open) => (!isSavingPool ? setDialogOpen(open) : null)}>
      <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-[480px]">
        <DialogHeader className="gap-1.5">
          <DialogTitle className="font-serif text-[22px] font-medium">{editingPool ? "编辑 CPA 连接" : "添加 CPA 连接"}</DialogTitle>
          <DialogDescription className="text-[13px] leading-6 text-ink-2">
            {editingPool ? "修改名称、地址或管理密钥。密钥留空则保持不变。" : "保存 CLIProxyAPI 的地址和管理密钥，之后可以按连接导入账号。"}
          </DialogDescription>
        </DialogHeader>
        <div className="space-y-4">
          <div className="space-y-2">
            <label className="text-[13px] text-ink-2">名称（可选）</label>
            <Input
              value={formName}
              onChange={(event) => setFormName(event.target.value)}
              placeholder="例如：主号池、备用池"
            />
          </div>
          <div className="space-y-2">
            <label className="flex items-center gap-1.5 text-[13px] text-ink-2">
              <Link2 className="size-3.5" />
              CPA 地址
            </label>
            <Input
              value={formBaseUrl}
              onChange={(event) => setFormBaseUrl(event.target.value)}
              placeholder="http://your-cpa-host:8317"
            />
          </div>
          <div className="space-y-2">
            <label className="flex items-center gap-1.5 text-[13px] text-ink-2">
              <Unplug className="size-3.5" />
              Management Secret Key
            </label>
            <div className="relative">
              <Input
                type={showSecret ? "text" : "password"}
                value={formSecretKey}
                onChange={(event) => setFormSecretKey(event.target.value)}
                placeholder={editingPool ? "留空则不修改密钥" : "CPA 管理密钥"}
                className="pr-10"
              />
              <button
                type="button"
                className="absolute top-1/2 right-3 -translate-y-1/2 text-ink-3 transition hover:text-ink"
                onClick={() => setShowSecret(!showSecret)}
              >
                {showSecret ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
              </button>
            </div>
          </div>
        </div>
        <DialogFooter className="pt-2">
          <Button variant="ghost" onClick={() => setDialogOpen(false)} disabled={isSavingPool}>
            取消
          </Button>
          <Button onClick={() => void savePool()} disabled={isSavingPool}>
            {isSavingPool ? <LoaderCircle className="size-4 animate-spin" /> : <Save className="size-4" />}
            {editingPool ? "保存修改" : "添加"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
