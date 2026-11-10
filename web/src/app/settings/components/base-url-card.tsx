"use client";

import { LoaderCircle, Save } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ErrorState, SkeletonLines } from "@/components/ui/states";

import { useSettingsStore } from "../store";
import { SettingGroup } from "./setting-group";

const title = "基础地址";
const description = "设置 CHATGPT2API_BASE_URL 的本地配置值，保存后立即生效。";

export function BaseUrlCard() {
  const config = useSettingsStore((state) => state.config);
  const isLoadingConfig = useSettingsStore((state) => state.isLoadingConfig);
  const configLoadError = useSettingsStore((state) => state.configLoadError);
  const loadConfig = useSettingsStore((state) => state.loadConfig);
  const isSavingConfig = useSettingsStore((state) => state.isSavingConfig);
  const setBaseUrl = useSettingsStore((state) => state.setBaseUrl);
  const saveConfig = useSettingsStore((state) => state.saveConfig);

  if (isLoadingConfig && !config) {
    return (
      <SettingGroup title={title} description={description}>
        <SkeletonLines lines={3} />
      </SettingGroup>
    );
  }
  if (!config) {
    return (
      <SettingGroup title={title} description={description}>
        <ErrorState message={configLoadError || "加载系统配置失败"} onRetry={() => void loadConfig()} />
      </SettingGroup>
    );
  }

  const baseUrl = String(config.base_url || "");

  return (
    <SettingGroup title={title} description={description}>
      <div className="flex justify-end">
        <Badge variant={baseUrl.trim() ? "success" : "secondary"} className="w-fit rounded-md px-2.5 py-1">
          {baseUrl.trim() ? "已配置" : "未配置"}
        </Badge>
      </div>
      <div className="space-y-2">
        <label className="text-sm font-medium text-ink-2">Base URL</label>
        <Input
          value={baseUrl}
          onChange={(event) => setBaseUrl(event.target.value)}
          placeholder="https://example.com"
        />
        <p className="text-sm text-ink-3">留空则使用环境变量或默认值，保存时会自动去掉首尾空格。</p>
      </div>
      <div className="flex justify-end">
        <Button onClick={() => void saveConfig()} disabled={isSavingConfig}>
          {isSavingConfig ? <LoaderCircle className="size-4 animate-spin" /> : <Save className="size-4" />}
          保存配置
        </Button>
      </div>
    </SettingGroup>
  );
}
