"use client";

import { useState } from "react";
import { LoaderCircle, PlugZap, Save } from "lucide-react";
import { toast } from "sonner";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { ErrorState, SkeletonLines } from "@/components/ui/states";
import { testProxy, type ProxyTestResult } from "@/lib/api";

import { useSettingsStore } from "../store";
import { SettingGroup } from "./setting-group";

const title = "全局代理";
const description = "为系统中的出站请求配置统一代理，保存后会立即生效。";

export function ProxySettingsCard() {
  const [isTesting, setIsTesting] = useState(false);
  const [testResult, setTestResult] = useState<ProxyTestResult | null>(null);
  const config = useSettingsStore((state) => state.config);
  const isLoadingConfig = useSettingsStore((state) => state.isLoadingConfig);
  const configLoadError = useSettingsStore((state) => state.configLoadError);
  const loadConfig = useSettingsStore((state) => state.loadConfig);
  const isSavingConfig = useSettingsStore((state) => state.isSavingConfig);
  const setProxy = useSettingsStore((state) => state.setProxy);
  const saveConfig = useSettingsStore((state) => state.saveConfig);

  const proxy = config?.proxy ?? "";

  const handleTest = async () => {
    const candidate = proxy.trim();
    if (!candidate) {
      toast.error("请先填写代理地址");
      return;
    }
    setIsTesting(true);
    setTestResult(null);
    try {
      const data = await testProxy(candidate);
      setTestResult(data.result);
      if (data.result.ok) {
        toast.success(`代理可用（${data.result.latency_ms} ms，HTTP ${data.result.status}）`);
      } else {
        toast.error(`代理不可用：${data.result.error ?? "未知错误"}`);
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "测试代理失败");
    } finally {
      setIsTesting(false);
    }
  };

  if (isLoadingConfig && !config) {
    return (
      <SettingGroup title={title} description={description}>
        <SkeletonLines lines={4} />
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

  return (
    <SettingGroup title={title} description={description}>
      <div className="flex justify-end">
        <Badge variant={proxy.trim() ? "success" : "secondary"} className="w-fit rounded-md px-2.5 py-1">
          {proxy.trim() ? "已配置" : "未配置"}
        </Badge>
      </div>
      <div className="space-y-2">
        <label className="text-sm font-medium text-ink-2">代理地址</label>
        <Input
          value={proxy}
          onChange={(event) => {
            setProxy(event.target.value);
            setTestResult(null);
          }}
          placeholder="http://user:pass@127.0.0.1:7890"
        />
        <p className="text-sm text-ink-3">
          留空表示不使用代理。请按完整地址填写，例如 `http://127.0.0.1:7890`、`http://用户名:密码@127.0.0.1:7890` 或 `socks5://127.0.0.1:7890`。
        </p>
      </div>

      {testResult ? (
        <div
          className={`rounded-[7px] border px-4 py-3 text-sm leading-6 ${
            testResult.ok
              ? "border-ok-soft bg-ok-soft text-ok"
              : "border-bad-soft bg-bad-soft text-bad"
          }`}
        >
          {testResult.ok
            ? `代理可用：HTTP ${testResult.status}，用时 ${testResult.latency_ms} ms`
            : `代理不可用：${testResult.error ?? "未知错误"}（用时 ${testResult.latency_ms} ms）`}
        </div>
      ) : null}

      <div className="flex justify-end gap-2">
        <Button
          variant="outline"
          onClick={() => void handleTest()}
          disabled={isTesting || isLoadingConfig}
        >
          {isTesting ? <LoaderCircle className="size-4 animate-spin" /> : <PlugZap className="size-4" />}
          测试代理
        </Button>
        <Button onClick={() => void saveConfig()} disabled={isSavingConfig}>
          {isSavingConfig ? <LoaderCircle className="size-4 animate-spin" /> : <Save className="size-4" />}
          保存配置
        </Button>
      </div>
    </SettingGroup>
  );
}
