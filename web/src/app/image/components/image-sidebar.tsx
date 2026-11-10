"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { LoaderCircle, MessageSquarePlus, Pencil, Trash2 } from "lucide-react";
import { StatusDot } from "@/components/ui/status-dot";

import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";
import { getImageConversationStats, type ImageConversation } from "@/store/image-conversations";

type ImageSidebarProps = {
  conversations: ImageConversation[];
  isLoadingHistory: boolean;
  selectedConversationId: string | null;
  onCreateDraft: () => void;
  onClearHistory: () => void | Promise<void>;
  onSelectConversation: (id: string) => void;
  onDeleteConversation: (id: string) => void | Promise<void>;
  onRenameConversation: (id: string, title: string) => void | Promise<void>;
  formatConversationTime: (value: string) => string;
  hideActionButtons?: boolean;
};

function isToday(value: string) {
  const date = new Date(value);
  const now = new Date();
  return date.getFullYear() === now.getFullYear() && date.getMonth() === now.getMonth() && date.getDate() === now.getDate();
}

export function ImageSidebar({
  conversations,
  isLoadingHistory,
  selectedConversationId,
  onCreateDraft,
  onClearHistory,
  onSelectConversation,
  onDeleteConversation,
  onRenameConversation,
  formatConversationTime,
  hideActionButtons = false,
}: ImageSidebarProps) {
  const [editingId, setEditingId] = useState<string | null>(null);
  const [editingTitle, setEditingTitle] = useState("");
  const editInputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (editingId && editInputRef.current) {
      editInputRef.current.focus();
      editInputRef.current.select();
    }
  }, [editingId]);

  const startRename = useCallback((conversation: ImageConversation, e: React.MouseEvent) => {
    e.stopPropagation();
    setEditingId(conversation.id);
    setEditingTitle(conversation.title);
  }, []);

  const commitRename = useCallback(() => {
    const trimmed = editingTitle.trim();
    if (editingId && trimmed) {
      void onRenameConversation(editingId, trimmed);
    }
    setEditingId(null);
    setEditingTitle("");
  }, [editingId, editingTitle, onRenameConversation]);

  const cancelRename = useCallback(() => {
    setEditingId(null);
    setEditingTitle("");
  }, []);
  return (
    <aside className="h-full min-h-0 overflow-hidden">
      <div className="flex h-full min-h-0 flex-col gap-2 py-1 sm:gap-3 sm:py-2">
        {!hideActionButtons && (
          <div className="flex items-center gap-2">
            <Button variant="outline" className="h-[34px] flex-1" onClick={onCreateDraft}>
              <MessageSquarePlus className="size-4" />
              新建对话
            </Button>
            <Button
              variant="ghost"
              size="icon"
              className="size-[34px]"
              title="清空全部会话"
              onClick={() => void onClearHistory()}
              disabled={conversations.length === 0}
            >
              <Trash2 className="size-4" />
            </Button>
          </div>
        )}

        <div
          className={cn(
            "min-h-0 flex-1 overflow-y-auto [scrollbar-color:rgba(120,113,108,.45)_transparent] [scrollbar-width:thin] [&::-webkit-scrollbar]:w-1.5 [&::-webkit-scrollbar-thumb]:rounded-full [&::-webkit-scrollbar-thumb]:bg-stone-400/45 [&::-webkit-scrollbar-track]:bg-transparent",
            hideActionButtons ? "space-y-1 pr-0" : "space-y-2 pr-1",
          )}
        >
          {isLoadingHistory ? (
            <div className="flex items-center gap-2 px-2 py-3 text-sm text-stone-500">
              <LoaderCircle className="size-4 animate-spin" />
              正在读取会话记录
            </div>
          ) : conversations.length === 0 ? (
            <div className="px-2 py-3 text-sm leading-6 text-stone-500">还没有图片记录，输入提示词后会在这里显示。</div>
          ) : (
            conversations.map((conversation, index) => {
              const active = conversation.id === selectedConversationId;
              const group = isToday(conversation.updatedAt) ? "今天" : "更早";
              const prevGroup = index > 0 ? (isToday(conversations[index - 1].updatedAt) ? "今天" : "更早") : null;
              const stats = getImageConversationStats(conversation);
              return (
                <div key={conversation.id}>
                {group !== prevGroup ? <div className="px-2.5 pt-2 pb-1 text-[11px] tracking-[0.06em] text-ink-3">{group}</div> : null}
                <div
                  className={cn(
                    "group relative w-full rounded-[7px] text-left transition",
                    hideActionButtons ? "px-4 py-3" : "px-2.5 py-2",
                    active
                      ? "bg-surface text-ink shadow-[inset_0_0_0_1px_var(--line)]"
                      : "text-ink-2 hover:bg-surface-hover hover:text-ink",
                  )}
                >
                  <button
                    type="button"
                    onClick={() => onSelectConversation(conversation.id)}
                    className="block w-full pr-8 text-left"
                  >
                    <div className={cn("truncate", hideActionButtons ? "text-base" : "text-[13px]")}>
                      {editingId === conversation.id ? (
                        <input
                          ref={editInputRef}
                          value={editingTitle}
                          onChange={(e) => setEditingTitle(e.target.value)}
                          onBlur={commitRename}
                          onKeyDown={(e) => {
                            if (e.key === "Enter") commitRename();
                            if (e.key === "Escape") cancelRename();
                          }}
                          onClick={(e) => e.stopPropagation()}
                          className="w-full truncate rounded border border-stone-300 bg-white px-1 py-0.5 text-sm outline-none focus:border-stone-500"
                        />
                      ) : (
                        <span className="truncate">{conversation.title}</span>
                      )}
                    </div>
                    <div className="mt-0.5 text-[11px] text-ink-3">
                      {conversation.turns.length} 轮 · {formatConversationTime(conversation.updatedAt)}
                    </div>
                    {stats.running > 0 || stats.queued > 0 ? (
                      <div className="mt-2 flex flex-wrap items-center gap-2 text-[11px]">
                        {stats.running > 0 ? (
                          <StatusDot tone="ok" className="text-[11px] text-ink-2">处理中 {stats.running}</StatusDot>
                        ) : null}
                        {stats.queued > 0 ? (
                          <StatusDot tone="warn" className="text-[11px] text-ink-2">排队 {stats.queued}</StatusDot>
                        ) : null}
                      </div>
                    ) : null}
                  </button>
                  <div className="absolute top-2.5 right-1.5 flex items-center gap-0.5 opacity-100 transition sm:opacity-0 sm:group-hover:opacity-100">
                    <button
                      type="button"
                      onClick={(e) => startRename(conversation, e)}
                      className="inline-flex size-7 items-center justify-center rounded-md text-stone-400 hover:bg-stone-100 hover:text-stone-600"
                      aria-label="重命名会话"
                    >
                      <Pencil className="size-3.5" />
                    </button>
                    <button
                      type="button"
                      onClick={() => void onDeleteConversation(conversation.id)}
                      className="inline-flex size-7 items-center justify-center rounded-md text-stone-400 hover:bg-stone-100 hover:text-rose-500"
                      aria-label="删除会话"
                    >
                      <Trash2 className="size-4" />
                    </button>
                  </div>
                </div>
                </div>
              );
            })
          )}
        </div>
      </div>
    </aside>
  );
}
