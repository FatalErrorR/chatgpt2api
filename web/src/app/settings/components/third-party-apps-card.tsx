"use client";

import { LoaderCircle, Save } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { ErrorState, SkeletonLines } from "@/components/ui/states";

import { useSettingsStore } from "../store";
import { SettingGroup } from "./setting-group";

const title = "无限画布入口";
const description = "开启后会在顶部导航显示入口，跳转时自动附带本项目地址和当前密钥。";

export function ThirdPartyAppsCard() {
  const config = useSettingsStore((state) => state.config);
  const isLoadingConfig = useSettingsStore((state) => state.isLoadingConfig);
  const configLoadError = useSettingsStore((state) => state.configLoadError);
  const loadConfig = useSettingsStore((state) => state.loadConfig);
  const isSavingConfig = useSettingsStore((state) => state.isSavingConfig);
  const setInfiniteCanvasField = useSettingsStore((state) => state.setInfiniteCanvasField);
  const saveConfig = useSettingsStore((state) => state.saveConfig);

  if (isLoadingConfig && !config) {
    return (
      <SettingGroup title={title} description={description}>
        <SkeletonLines lines={4} />
      </SettingGroup>
    );
  }
  if (!config?.third_party_apps) {
    return (
      <SettingGroup title={title} description={description}>
        <ErrorState message={configLoadError || "加载系统配置失败"} onRetry={() => void loadConfig()} />
      </SettingGroup>
    );
  }

  const canvas = config.third_party_apps.infinite_canvas;

  return (
    <SettingGroup title={title} description={description}>
      <div className="flex justify-end">
        <span className={`rounded-full px-3 py-1 text-xs ${canvas.enabled ? "bg-ok-soft text-ok" : "bg-sunken text-ink-3"}`}>
          {canvas.enabled ? "已启用" : "未启用"}
        </span>
      </div>

      <div className="space-y-4 rounded-[7px] border border-line bg-surface px-3.5 py-3">
        <label className="flex items-center gap-3 text-sm text-ink-2">
          <Checkbox
            checked={Boolean(canvas.enabled)}
            onCheckedChange={(checked) => setInfiniteCanvasField("enabled", Boolean(checked))}
          />
          启用无限画布
        </label>
        <div className="space-y-2">
          <label className="text-sm text-ink-2">无限画布地址</label>
          <Input
            value={canvas.url}
            onChange={(event) => setInfiniteCanvasField("url", event.target.value)}
            placeholder="https://canvas.best"
          />
          <p className="text-xs leading-5 text-ink-3">
            顶部入口跳转时会追加 apiKey 和 baseUrl 参数；关闭后顶部导航不显示无限画布。
          </p>
          <p className="text-xs leading-5 text-warn">
            该入口仅供个人测试使用；长期使用建议自行本机部署无限画布。
          </p>
        </div>
      </div>

      <div className="flex justify-end">
        <Button onClick={() => void saveConfig()} disabled={isSavingConfig}>
          {isSavingConfig ? <LoaderCircle className="size-4 animate-spin" /> : <Save className="size-4" />}
          保存
        </Button>
      </div>
    </SettingGroup>
  );
}
