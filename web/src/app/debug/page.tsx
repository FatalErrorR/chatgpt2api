"use client";

import { LoaderCircle } from "lucide-react";

import { PageHeader } from "@/components/page-header";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { useAuthGuard } from "@/lib/use-auth-guard";

import { ChatPanel } from "./components/chat-panel";
import { PptPanel } from "./components/ppt-panel";
import { PsdPanel } from "./components/psd-panel";
import { SearchPanel } from "./components/search-panel";
import { SkillPanel } from "./components/skill-panel";

const tabs = [
  { value: "skills", title: "搜索Skills" },
  { value: "search", title: "搜索" },
  { value: "ppt", title: "PPT生成" },
  { value: "psd", title: "PSD生成" },
  { value: "chat", title: "对话" },
];

export default function DebugPage() {
  const { isCheckingAuth, session } = useAuthGuard(["admin"]);

  if (isCheckingAuth || !session || session.role !== "admin") {
    return (
      <div className="flex min-h-[40vh] items-center justify-center">
        <LoaderCircle className="size-5 animate-spin text-muted-foreground" />
      </div>
    );
  }

  return (
    <section className="space-y-5">
    <PageHeader title="调试" description="直接调用上游能力，用于排查问题，不计入号池统计。" />
    <Tabs defaultValue="skills" className="flex w-full flex-col gap-4">
      <TabsList className="h-auto w-fit max-w-full flex-wrap justify-start gap-0.5 rounded-[9px] bg-sunken p-[3px]">
        {tabs.map(({ value, title }) => (
          <TabsTrigger
            key={value}
            value={value}
            className="h-7 flex-none rounded-[7px] px-3 text-[13px] font-normal text-ink-2 data-[state=active]:bg-surface data-[state=active]:text-ink data-[state=active]:shadow-[0_1px_2px_rgba(0,0,0,.06)]"
          >
            {title}
          </TabsTrigger>
        ))}
      </TabsList>
      <TabsContent value="skills">
        <SkillPanel />
      </TabsContent>
      <TabsContent value="search" className="min-h-0">
        <SearchPanel />
      </TabsContent>
      <TabsContent value="ppt" className="min-h-0">
        <PptPanel />
      </TabsContent>
      <TabsContent value="psd" className="min-h-0">
        <PsdPanel />
      </TabsContent>
      <TabsContent value="chat" className="min-h-0">
        <ChatPanel />
      </TabsContent>
    </Tabs>
    </section>
  );
}
