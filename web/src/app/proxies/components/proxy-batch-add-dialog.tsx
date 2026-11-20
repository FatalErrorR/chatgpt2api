"use client";

import { useState } from "react";
import { LoaderCircle, Plus } from "lucide-react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Input } from "@/components/ui/input";
import { Textarea } from "@/components/ui/textarea";
import { addProxies, type ProxyBatchAddResult, type ProxyEntry } from "@/lib/api";

/** 批量添加代理：每行一个，可按厂商依次命名「厂商-N」。 */
export function ProxyBatchAddDialog({ onChange }: { onChange: (items: ProxyEntry[]) => void }) {
  const [open, setOpen] = useState(false);
  const [lines, setLines] = useState("");
  const [vendor, setVendor] = useState("");
  const [result, setResult] = useState<ProxyBatchAddResult | null>(null);
  const [busy, setBusy] = useState(false);
  const lineCount = lines.split("\n").filter((line) => line.trim()).length;

  const handleAdd = async () => {
    if (lineCount === 0) {
      toast.error("请填写代理，每行一个");
      return;
    }
    setBusy(true);
    try {
      const data = await addProxies(lines, vendor.trim());
      onChange(data.items);
      setResult(data);
      const summary = `新增 ${data.added.length} 个，跳过 ${data.skipped.length} 个，无效 ${data.invalid.length} 个`;
      if (data.added.length > 0) toast.success(summary);
      else toast.error(summary);
      if (data.invalid.length === 0) {
        setLines("");
        if (data.skipped.length === 0) setOpen(false);
      } else {
        // 只留下无效的行，改完可以直接再提交
        const invalidLines = new Set(data.invalid.map((item) => item.line));
        setLines(lines.split("\n").filter((line, index) => line.trim() && invalidLines.has(index + 1)).join("\n"));
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "添加代理失败");
    } finally {
      setBusy(false);
    }
  };

  const notes = result
    ? [
        ...result.invalid.map((item) => ({ line: item.line, text: item.text, note: item.error, bad: true })),
        ...result.skipped.map((item) => ({ line: item.line, text: item.text, note: item.name ? `${item.reason}（${item.name}）` : item.reason, bad: false })),
      ].sort((a, b) => a.line - b.line)
    : [];

  return (
    <Dialog
      open={open}
      onOpenChange={(next) => {
        if (busy) return;
        setOpen(next);
        if (next) setResult(null);
      }}
    >
      <Button variant="outline" onClick={() => setOpen(true)}>
        <Plus className="size-4" />
        批量添加
      </Button>
      <DialogContent showCloseButton={false} className="rounded-[14px] p-6 sm:max-w-xl">
        <DialogHeader className="gap-1.5">
          <DialogTitle className="font-serif text-[22px] font-medium">批量添加代理</DialogTitle>
          <DialogDescription className="text-[13px] text-ink-2">
            每行一个：host:port:账号:密码、host:port 或完整地址。已在清单里的会跳过。
          </DialogDescription>
        </DialogHeader>
        <Textarea
          value={lines}
          onChange={(event) => setLines(event.target.value)}
          placeholder={"194.39.33.194:5903:账号:密码\n1.2.3.4:8080\nsocks5://user:pass@host:1080"}
          className="min-h-36 font-mono text-xs leading-5"
          disabled={busy}
          autoFocus
        />
        <div className="grid gap-1.5">
          <Input value={vendor} onChange={(event) => setVendor(event.target.value)} placeholder="厂商名称（可选）" className="w-48" disabled={busy} />
          <span className="text-xs text-ink-3">
            {vendor.trim() ? `依次命名为「${vendor.trim()}-序号」，接着这个厂商已有的最大序号` : "不填厂商则不命名，之后可逐个修改"}
          </span>
        </div>
        {notes.length > 0 ? (
          <ul className="thin-scrollbar max-h-28 space-y-0.5 overflow-y-auto text-xs">
            {notes.map((item) => (
              <li key={item.line} className={item.bad ? "text-bad" : "text-ink-3"}>
                第 {item.line} 行 <span className="font-mono">{item.text}</span>：{item.note}
              </li>
            ))}
          </ul>
        ) : null}
        <DialogFooter className="pt-1">
          <Button variant="ghost" onClick={() => setOpen(false)} disabled={busy}>
            取消
          </Button>
          <Button onClick={() => void handleAdd()} disabled={busy || lineCount === 0}>
            {busy ? <LoaderCircle className="size-4 animate-spin" /> : null}
            添加{lineCount > 0 ? ` ${lineCount} 个` : ""}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  );
}
