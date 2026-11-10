"use client";

import { Cloud, LoaderCircle, PlugZap, RefreshCw, Save } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { ErrorState, SkeletonLines } from "@/components/ui/states";
import { Textarea } from "@/components/ui/textarea";
import type { ImageStorageMode } from "@/lib/api";
import { testProxy, type ProxyTestResult } from "@/lib/api";

import { useSettingsStore } from "../store";
import { SettingGroup } from "./setting-group";

export function ConfigCard() {
  const [isTestingProxy, setIsTestingProxy] = useState(false);
  const [proxyTestResult, setProxyTestResult] = useState<ProxyTestResult | null>(null);
  const logLevelOptions = ["debug", "info", "warning", "error"];
  const config = useSettingsStore((state) => state.config);
  const isLoadingConfig = useSettingsStore((state) => state.isLoadingConfig);
  const configLoadError = useSettingsStore((state) => state.configLoadError);
  const loadConfig = useSettingsStore((state) => state.loadConfig);
  const isSavingConfig = useSettingsStore((state) => state.isSavingConfig);
  const setRefreshAccountIntervalMinute = useSettingsStore((state) => state.setRefreshAccountIntervalMinute);
  const setImageRetentionDays = useSettingsStore((state) => state.setImageRetentionDays);
  const setImagePollTimeoutSecs = useSettingsStore((state) => state.setImagePollTimeoutSecs);
  const setImageAccountConcurrency = useSettingsStore((state) => state.setImageAccountConcurrency);
  const setImageSettleEnabled = useSettingsStore((state) => state.setImageSettleEnabled);
  const setImageRefFitEnabled = useSettingsStore((state) => state.setImageRefFitEnabled);
  const setImageReqFitEnabled = useSettingsStore((state) => state.setImageReqFitEnabled);
  const setImageEditUploadCooldownEnabled = useSettingsStore((state) => state.setImageEditUploadCooldownEnabled);
  const setImageUpscaleEnabled = useSettingsStore((state) => state.setImageUpscaleEnabled);
  const setImageExactSizeEnabled = useSettingsStore((state) => state.setImageExactSizeEnabled);
  const setImageUpscaleTargetEdge = useSettingsStore((state) => state.setImageUpscaleTargetEdge);
  const setImageUpscaleMethod = useSettingsStore((state) => state.setImageUpscaleMethod);
  const setImageRemoveConversationAfterResult = useSettingsStore((state) => state.setImageRemoveConversationAfterResult);
  const setImageRemoveConversationAlways = useSettingsStore((state) => state.setImageRemoveConversationAlways);
  const setImageSettleSecs = useSettingsStore((state) => state.setImageSettleSecs);
  const setImageTimeoutRetrySecs = useSettingsStore((state) => state.setImageTimeoutRetrySecs);
  const setAutoRemoveInvalidAccounts = useSettingsStore((state) => state.setAutoRemoveInvalidAccounts);
  const setAutoRemoveRateLimitedAccounts = useSettingsStore((state) => state.setAutoRemoveRateLimitedAccounts);
  const setAutoReloginAfterRefresh = useSettingsStore((state) => state.setAutoReloginAfterRefresh);
  const setLogLevel = useSettingsStore((state) => state.setLogLevel);
  const setProxy = useSettingsStore((state) => state.setProxy);
  const setBaseUrl = useSettingsStore((state) => state.setBaseUrl);
  const setGlobalSystemPrompt = useSettingsStore((state) => state.setGlobalSystemPrompt);
  const setDefaultUpstreamModelName = useSettingsStore((state) => state.setDefaultUpstreamModelName);
  const setDefaultThinkingEffort = useSettingsStore((state) => state.setDefaultThinkingEffort);
  const setSensitiveWordsText = useSettingsStore((state) => state.setSensitiveWordsText);
  const setAIReviewField = useSettingsStore((state) => state.setAIReviewField);
  const setImageStorageField = useSettingsStore((state) => state.setImageStorageField);
  const testImageStorage = useSettingsStore((state) => state.testImageStorage);
  const syncImagesToWebDAV = useSettingsStore((state) => state.syncImagesToWebDAV);
  const isTestingImageStorage = useSettingsStore((state) => state.isTestingImageStorage);
  const isSyncingImageStorage = useSettingsStore((state) => state.isSyncingImageStorage);
  const saveConfig = useSettingsStore((state) => state.saveConfig);

  const handleTestProxy = async () => {
    const candidate = String(config?.proxy || "").trim();
    if (!candidate) {
      toast.error("请先填写代理地址");
      return;
    }
    setIsTestingProxy(true);
    setProxyTestResult(null);
    try {
      const data = await testProxy(candidate);
      setProxyTestResult(data.result);
      if (data.result.ok) {
        toast.success(`代理可用（${data.result.latency_ms} ms，HTTP ${data.result.status}）`);
      } else {
        toast.error(`代理不可用：${data.result.error ?? "未知错误"}`);
      }
    } catch (error) {
      toast.error(error instanceof Error ? error.message : "测试代理失败");
    } finally {
      setIsTestingProxy(false);
    }
  };

  if (isLoadingConfig && !config) {
    return <SkeletonLines lines={6} />;
  }
  if (!config) {
    return <ErrorState message={configLoadError || "加载系统配置失败"} onRetry={() => void loadConfig()} />;
  }

  return (
    <>
      <SettingGroup title="账号" description="刷新频率、异常账号处理和日志级别。管理员密钥仍从部署配置读取，不在这里展示。">
        <div className="rounded-[7px] border border-line bg-sunken px-4 py-3 text-[13px] leading-6 text-ink-2">
          管理员登录密钥继续从部署配置读取。分发给其他人时，到「用户密钥」创建普通用户密钥。
        </div>
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-2">
            <label className="text-sm text-ink-2">账号刷新间隔</label>
            <Input
              value={String(config?.refresh_account_interval_minute || "")}
              onChange={(event) => setRefreshAccountIntervalMinute(event.target.value)}
              placeholder="分钟"
             
            />
            <p className="text-xs text-ink-3">单位分钟，控制账号自动刷新频率。</p>
          </div>
          <div className="space-y-2">
            <label className="text-sm text-ink-2">全局代理</label>
            <Input
              value={String(config?.proxy || "")}
              onChange={(event) => {
                setProxy(event.target.value);
                setProxyTestResult(null);
              }}
              placeholder="http://127.0.0.1:7890"
             
            />
            <p className="text-xs leading-5 text-ink-3">
              留空表示不使用代理。支持协议://账号:密码@主机:端口，也可直接粘贴代理商的 主机:端口:账号:密码；示例 http://user:pass@127.0.0.1:7890、127.0.0.1:7890:user:pass。账号密码含 @/: 等特殊字符时需 URL 编码。
            </p>
            {proxyTestResult ? (
              <div
                className={`rounded-[7px] border px-3 py-2 text-xs leading-6 ${
                  proxyTestResult.ok
                    ? "border-ok-soft bg-ok-soft text-ok"
                    : "border-bad-soft bg-bad-soft text-bad"
                }`}
              >
                {proxyTestResult.ok
                  ? `代理可用：HTTP ${proxyTestResult.status}，用时 ${proxyTestResult.latency_ms} ms`
                  : `代理不可用：${proxyTestResult.error ?? "未知错误"}（用时 ${proxyTestResult.latency_ms} ms）`}
              </div>
            ) : null}
            <div className="flex justify-end">
              <Button
                type="button"
                variant="outline"
               
                onClick={() => void handleTestProxy()}
                disabled={isTestingProxy}
              >
                {isTestingProxy ? <LoaderCircle className="size-4 animate-spin" /> : <PlugZap className="size-4" />}
                测试代理
              </Button>
            </div>
          </div>
        </div>
      </SettingGroup>
      <SettingGroup title="请求默认值" description="图片访问地址、上游模型名和思考强度。模型名后缀优先于这里的思考强度。">
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-2">
            <label className="text-sm text-ink-2">图片访问地址</label>
            <Input
              value={String(config?.base_url || "")}
              onChange={(event) => setBaseUrl(event.target.value)}
              placeholder="https://example.com"
             
            />
            <p className="text-xs text-ink-3">用于生成图片结果的访问前缀地址。</p>
          </div>
          <div className="space-y-2">
            <label className="text-sm text-ink-2">默认请求上游模型名称</label>
            <Input
              value={String(config?.default_upstream_model_name || "")}
              onChange={(event) => setDefaultUpstreamModelName(event.target.value)}
              placeholder="gpt-5-5"
             
            />
            <p className="text-xs text-ink-3">仅 gpt-image-2 会使用该上游模型名（默认 gpt-5-5）。gpt-image-2.5 / flare / sunburst 会原样发给 ChatGPT。</p>
          </div>
          <div className="space-y-2">
            <label className="text-sm text-ink-2">默认思考强度</label>
            <Select
              value={String(config?.default_thinking_effort || "auto")}
              onValueChange={(value) => setDefaultThinkingEffort(value as "auto" | "standard" | "extended" | "max")}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="auto">Auto（不传递）</SelectItem>
                <SelectItem value="standard">Standard</SelectItem>
                <SelectItem value="extended">Extended</SelectItem>
                <SelectItem value="max">Max</SelectItem>
              </SelectContent>
            </Select>
            <p className="text-xs text-ink-3">模型名称以 -standard、-extended 或 -max 结尾时，模型后缀优先。</p>
          </div>
        </div>
      </SettingGroup>
      <SettingGroup title="生图" description="影响所有生图请求：清理、超时、并发、压缩、冷却和放大。">
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-2">
            <label className="text-sm text-ink-2">图片自动清理</label>
            <Input
              value={String(config?.image_retention_days || "")}
              onChange={(event) => setImageRetentionDays(event.target.value)}
              placeholder="30"
             
            />
            <p className="text-xs text-ink-3">自动删除多少天前的本地图片。</p>
          </div>
          <div className="space-y-2">
            <label className="text-sm text-ink-2">图片轮询超时</label>
            <Input
              value={String(config?.image_poll_timeout_secs || "")}
              onChange={(event) => setImagePollTimeoutSecs(event.target.value)}
              placeholder="120"
             
            />
            <p className="text-xs text-ink-3">单位秒，等待上游图片结果的最长时间。</p>
          </div>
          <div className="space-y-2">
            <label className="text-sm text-ink-2">单账号图片并发</label>
            <Input
              value={String(config?.image_account_concurrency || "")}
              onChange={(event) => setImageAccountConcurrency(event.target.value)}
              placeholder="1"
             
            />
            <p className="text-xs text-ink-3">限制每个账号同时处理的图片请求数量，默认 3。</p>
          </div>
          <div className="space-y-2">
            <label className="flex items-center gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3 text-sm text-ink-2">
              <Checkbox
                checked={Boolean(config?.auto_remove_invalid_accounts)}
                onCheckedChange={(checked) => setAutoRemoveInvalidAccounts(Boolean(checked))}
              />
              自动移除异常账号
            </label>
            <p className="text-xs text-ink-3">刷新时检测并移除</p>
          </div>
          <div className="space-y-2">
            <div className="flex items-center gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3">
              <Checkbox
                checked={Boolean(config?.image_settle_enabled !== false)}
                onCheckedChange={(checked) => setImageSettleEnabled(Boolean(checked))}
              />
              <span className="text-sm text-ink-2">图片二次确认机制</span>
            </div>
            <p className="text-xs text-ink-3">打开后能稍微提升获取图片的成功率。</p>
          </div>
          <div className="space-y-2">
            <div className="flex items-center gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3">
              <Checkbox
                checked={Boolean(config?.image_ref_fit_enabled !== false)}
                onCheckedChange={(checked) => setImageRefFitEnabled(Boolean(checked))}
              />
              <span className="text-sm text-ink-2">参考图超预算压缩</span>
            </div>
            <p className="text-xs text-ink-3">Web 生图参考图整包超过 1.4MB 时自动缩边/降质量，避免会话 413。关闭后按原图上传。</p>
          </div>
          <div className="space-y-2">
            <div className="flex items-center gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3">
              <Checkbox
                checked={Boolean(config?.image_req_fit_enabled !== false)}
                onCheckedChange={(checked) => setImageReqFitEnabled(Boolean(checked))}
              />
              <span className="text-sm text-ink-2">请求超预算压缩</span>
            </div>
            <p className="text-xs text-ink-3">提示词 + 参考图超过 1.4MB 时只压参考图，提示词原文不动。与上一开关独立。</p>
          </div>
          <div className="space-y-2">
            <div className="flex items-center gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3">
              <Checkbox
                checked={Boolean(config?.image_edit_upload_cooldown_enabled !== false)}
                onCheckedChange={(checked) => setImageEditUploadCooldownEnabled(Boolean(checked))}
              />
              <span className="text-sm text-ink-2">图生图上传冷却</span>
            </div>
            <p className="text-xs text-ink-3">谁返回「文件上传上限」就冻谁，按 ChatGPT 说的时间暂停该号图生图并立刻换号（最多 5 次）。文生图不受影响，不限套餐。</p>
          </div>
          <div className="space-y-2">
            <div className="flex items-center gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3">
              <Checkbox
                checked={Boolean(config?.image_upscale_enabled)}
                onCheckedChange={(checked) => setImageUpscaleEnabled(Boolean(checked))}
              />
              <span className="text-sm text-ink-2">出图放大</span>
            </div>
            <p className="text-xs text-ink-3">出图后把长边不足的图等比放大到目标长边（Lanczos + 轻度锐化，输出 PNG），已达标的图不处理，失败时返回原图。默认关闭。</p>
          </div>
          <div className="space-y-2">
            <div className="flex items-center gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3">
              <Checkbox
                checked={Boolean(config?.image_exact_size_enabled)}
                onCheckedChange={(checked) => setImageExactSizeEnabled(Boolean(checked))}
              />
              <span className="text-sm text-ink-2">按请求尺寸精确出图</span>
            </div>
            <p className="text-xs text-ink-3">请求里明确写了宽x高（如 1024x1024）时，把出图居中裁剪到该比例并缩放到精确尺寸，过大缩小、过小放大（放大需同时开启「出图放大」）。auto、2K 等写法不受影响，失败时返回原图。默认关闭。</p>
          </div>
          <div className="space-y-2">
            <label className="text-sm text-ink-2">出图放大目标长边</label>
            <Input
              value={String(config?.image_upscale_target_edge || "2048")}
              onChange={(event) => setImageUpscaleTargetEdge(event.target.value)}
              placeholder="2048"
              className=" disabled:cursor-not-allowed disabled:opacity-50"
              disabled={!config?.image_upscale_enabled}
            />
            <p className="text-xs text-ink-3">单位像素，范围 1024 到 4096，默认 2048。</p>
          </div>
          <div className="space-y-2">
            <label className="text-sm text-ink-2">出图放大方式</label>
            <Select
              value={config?.image_upscale_method === "model" ? "model" : "lanczos"}
              onValueChange={(value) => setImageUpscaleMethod(value as "lanczos" | "model")}
              disabled={!config?.image_upscale_enabled}
            >
              <SelectTrigger>
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="lanczos">Lanczos + 锐化（快）</SelectItem>
                <SelectItem value="model">RealESRGAN 小模型（CPU，慢）</SelectItem>
              </SelectContent>
            </Select>
            <p className="text-xs text-ink-3">小模型会补一些细节但很吃 CPU，同一时刻只处理一张；预计超过 60 秒或出错时自动回退 Lanczos。老旧 CPU 服务器上通常会回退。</p>
          </div>
          <div className="space-y-2">
            <div className="flex items-center gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3">
              <Checkbox
                checked={Boolean(config?.image_remove_conversation_after_result)}
                onCheckedChange={(checked) => setImageRemoveConversationAfterResult(Boolean(checked))}
              />
              <span className="text-sm text-ink-2">出图后移除本地对话</span>
            </div>
            <p className="text-xs text-ink-3">成功拿到图片后，异步隐藏 ChatGPT 侧对应的本地对话记录。</p>
          </div>
          <div className="space-y-2">
            <div className="flex items-center gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3">
              <Checkbox
                checked={Boolean(config?.image_remove_conversation_always)}
                onCheckedChange={(checked) => setImageRemoveConversationAlways(Boolean(checked))}
              />
              <span className="text-sm text-ink-2">没出图也移除本地对话</span>
            </div>
            <p className="text-xs text-ink-3">失败、超时或只返回文本时也一并隐藏对话记录（打开后包含出图成功的情况）。</p>
          </div>
          <div className="space-y-2">
            <label className="text-sm text-ink-2">图片超时继续等待时间</label>
            <Input
              value={String(config?.image_timeout_retry_secs || "30")}
              onChange={(event) => setImageTimeoutRetrySecs(event.target.value)}
              placeholder="30"
             
            />
            <p className="text-xs text-ink-3">单位秒，超时后点击"继续等待"额外等待的时间。</p>
          </div>
          <div className="space-y-2">
            <label className="text-sm text-ink-2">图片二次确认等待时间</label>
            <Input
              value={String(config?.image_settle_secs || "2.0")}
              onChange={(event) => setImageSettleSecs(event.target.value)}
              placeholder="2.0"
              className=" disabled:cursor-not-allowed disabled:opacity-50"
              disabled={!config?.image_settle_enabled}
            />
            <p className="text-xs text-ink-3">单位秒，找到图片后等待多久再次确认。需配合图片二次确认机制使用。</p>
          </div>
          <div className="flex gap-4 md:col-span-2">
            <div className="flex-1 space-y-2">
              <label className="flex items-center gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3 text-sm text-ink-2">
                <Checkbox
                  checked={Boolean(config?.auto_relogin_after_refresh)}
                  onCheckedChange={(checked) => setAutoReloginAfterRefresh(Boolean(checked))}
                />
                刷新后自动尝试移除异常状态
              </label>
              <p className="text-xs text-ink-3">开启后刷新时自动尝试密码登录恢复账号。</p>
            </div>
            <div className="flex-1" aria-hidden="true" />
          </div>
          <label className="flex items-center gap-3 rounded-[7px] border border-line bg-surface px-3.5 py-3 text-sm text-ink-2">
            <Checkbox
              checked={Boolean(config?.auto_remove_rate_limited_accounts)}
              onCheckedChange={(checked) => setAutoRemoveRateLimitedAccounts(Boolean(checked))}
            />
            自动移除限流账号
          </label>
          <div className="space-y-3 rounded-[7px] border border-line bg-surface px-3.5 py-3">
            <div>
              <label className="text-sm text-ink-2">控制台日志级别</label>
              <p className="mt-1 text-xs text-ink-3">不选择时使用默认 info / warning / error。</p>
            </div>
            <div className="grid grid-cols-2 gap-2">
              {logLevelOptions.map((level) => (
                <label key={level} className="flex items-center gap-2 text-sm capitalize text-ink-2">
                  <Checkbox
                    checked={Boolean(config?.log_levels?.includes(level))}
                    onCheckedChange={(checked) => setLogLevel(level, Boolean(checked))}
                  />
                  {level}
                </label>
              ))}
            </div>
          </div>
        </div>
      </SettingGroup>
      <SettingGroup title="请求约束" description="每次请求都会带上附加指令；命中敏感词或未通过 AI 审核时直接拒绝。">
        <div className="grid gap-4">
          <div className="space-y-2">
            <label className="text-sm text-ink-2">全局附加指令</label>
            <Textarea
              value={String(config?.global_system_prompt || "")}
              onChange={(event) => setGlobalSystemPrompt(event.target.value)}
              placeholder="例如：先判断用户提示词是否合规；遇到违法、色情、暴力、仇恨等请求时拒绝回答。"
              className="min-h-28 font-mono text-xs"
            />
            <p className="text-xs text-ink-3">每次请求都会作为 system 消息注入，可用于审核用户提示词、避免违规内容、统一约束模型行为或固定角色设定。</p>
          </div>
          <div className="space-y-2 md:col-span-2">
            <label className="text-sm text-ink-2">敏感词</label>
            <Textarea
              value={(config?.sensitive_words || []).join("\n")}
              onChange={(event) => setSensitiveWordsText(event.target.value)}
              placeholder="一行一个，命中即拒绝"
              className="min-h-28 font-mono text-xs"
            />
            <p className="text-xs text-ink-3">只要用户请求包含任意敏感词，就直接返回拒绝。</p>
          </div>
        </div>
      </SettingGroup>
      <SettingGroup title="图片存储" description="生成时只处理本次新图片。全量同步把已有本地图片补传到 WebDAV。">
        <div className="grid gap-4">
          <div className="space-y-4 rounded-[7px] border border-line bg-surface px-3.5 py-3">
            <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
              <label className="flex items-center gap-3 text-sm text-ink-2">
                <Checkbox
                  checked={Boolean(config.image_storage?.enabled)}
                  onCheckedChange={(checked) => setImageStorageField("enabled", Boolean(checked))}
                />
                启用 WebDAV 图片存储
              </label>
              <div className="flex flex-wrap gap-2">
                <Button
                  type="button"
                  variant="outline"
                 
                  onClick={() => void testImageStorage()}
                  disabled={isTestingImageStorage || !config?.image_storage?.enabled}
                >
                  {isTestingImageStorage ? <LoaderCircle className="size-4 animate-spin" /> : <Cloud className="size-4" />}
                  测试 WebDAV
                </Button>
                <Button
                  type="button"
                  variant="outline"
                 
                  onClick={() => void syncImagesToWebDAV()}
                  disabled={isSyncingImageStorage || !config?.image_storage?.enabled || config?.image_storage?.mode === "local"}
                >
                  {isSyncingImageStorage ? <LoaderCircle className="size-4 animate-spin" /> : <RefreshCw className="size-4" />}
                  全量同步
                </Button>
              </div>
            </div>
            <p className="text-xs leading-6 text-ink-3">
              生成时只处理本次新图片；全量同步用于把已有本地图片补传到 WebDAV。
            </p>
            <div className="rounded-[7px] border border-line bg-sunken px-3 py-2 text-xs text-ink-2">
              当前待保存模式：
              <span className="ml-1 font-medium text-ink">
                {config?.image_storage?.enabled
                  ? config.image_storage.mode === "both"
                    ? "本机 + WebDAV"
                    : config.image_storage.mode === "webdav"
                      ? "仅 WebDAV"
                      : "仅本机"
                  : "仅本机"}
              </span>
              <span className="ml-2 text-ink-3">修改后需要点保存，或通过测试/同步按钮自动保存。</span>
            </div>
            <div className="grid gap-4 md:grid-cols-3">
              <div className="space-y-2">
                <label className="text-sm text-ink-2">保存模式</label>
                <Select
                  value={String(config?.image_storage?.mode || "local")}
                  onValueChange={(value) => setImageStorageField("mode", value as ImageStorageMode)}
                  disabled={!config?.image_storage?.enabled}
                >
                  <SelectTrigger>
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value="local">仅本机</SelectItem>
                    <SelectItem value="webdav">仅 WebDAV</SelectItem>
                    <SelectItem value="both">本机 + WebDAV</SelectItem>
                  </SelectContent>
                </Select>
              </div>
              <div className="space-y-2 md:col-span-2">
                <label className="text-sm text-ink-2">WebDAV URL</label>
                <Input
                  value={String(config?.image_storage?.webdav_url || "")}
                  onChange={(event) => setImageStorageField("webdav_url", event.target.value)}
                  placeholder="https://example.com/dav"
                 
                  disabled={!config?.image_storage?.enabled}
                />
              </div>
              <div className="space-y-2">
                <label className="text-sm text-ink-2">用户名</label>
                <Input
                  value={String(config?.image_storage?.webdav_username || "")}
                  onChange={(event) => setImageStorageField("webdav_username", event.target.value)}
                 
                  disabled={!config?.image_storage?.enabled}
                />
              </div>
              <div className="space-y-2">
                <label className="text-sm text-ink-2">密码</label>
                <Input
                  type="password"
                  value={String(config?.image_storage?.webdav_password || "")}
                  onChange={(event) => setImageStorageField("webdav_password", event.target.value)}
                 
                  disabled={!config?.image_storage?.enabled}
                />
              </div>
              <div className="space-y-2">
                <label className="text-sm text-ink-2">远端目录</label>
                <Input
                  value={String(config?.image_storage?.webdav_root_path || "")}
                  onChange={(event) => setImageStorageField("webdav_root_path", event.target.value)}
                  placeholder="chatgpt2api/images"
                 
                  disabled={!config?.image_storage?.enabled}
                />
              </div>
              <div className="space-y-2 md:col-span-3">
                <label className="text-sm text-ink-2">公开访问前缀</label>
                <Input
                  value={String(config?.image_storage?.public_base_url || "")}
                  onChange={(event) => setImageStorageField("public_base_url", event.target.value)}
                  placeholder="https://cdn.example.com/chatgpt2api/images"
                 
                  disabled={!config?.image_storage?.enabled}
                />
                <p className="text-xs text-ink-3">留空时返回本应用 /images/... 代理地址；填入后直接返回公开图片地址。</p>
              </div>
            </div>
          </div>
        </div>
      </SettingGroup>
      <SettingGroup title="AI 审核" description="请求进入生图账号之前先调用审核模型。不通过则直接拒绝。">
        <div className="grid gap-4">
          <div className="space-y-4 rounded-[7px] border border-line bg-surface px-3.5 py-3">
            <label className="flex items-center gap-3 text-sm text-ink-2">
              <Checkbox
                checked={Boolean(config.ai_review?.enabled)}
                onCheckedChange={(checked) => setAIReviewField("enabled", Boolean(checked))}
              />
              启用 AI 审核
            </label>
            <p className="text-xs leading-6 text-ink-3">
              开启后会在请求进入生图账号前先调用审核模型，审核不通过会直接拒绝，减少违规提示词触达账号造成风控或封号的风险。
            </p>
            <div className="grid gap-4 md:grid-cols-3">
              <div className="space-y-2">
                <label className="text-sm text-ink-2">Base URL</label>
                <Input value={String(config?.ai_review?.base_url || "")} onChange={(event) => setAIReviewField("base_url", event.target.value)} placeholder="https://api.openai.com" />
              </div>
              <div className="space-y-2">
                <label className="text-sm text-ink-2">API Key</label>
                <Input value={String(config?.ai_review?.api_key || "")} onChange={(event) => setAIReviewField("api_key", event.target.value)} placeholder="sk-..." />
              </div>
              <div className="space-y-2">
                <label className="text-sm text-ink-2">Model</label>
                <Input value={String(config?.ai_review?.model || "")} onChange={(event) => setAIReviewField("model", event.target.value)} placeholder="gpt-5.4-mini" />
              </div>
            </div>
            <div className="space-y-2">
              <label className="text-sm text-ink-2">审核提示词</label>
              <Textarea value={String(config?.ai_review?.prompt || "")} onChange={(event) => setAIReviewField("prompt", event.target.value)} placeholder="判断用户请求是否允许。只回答 ALLOW 或 REJECT。" className="min-h-24 text-xs" />
            </div>
          </div>
        </div>
      </SettingGroup>
      <div className="flex justify-end border-t border-line pt-4">
        <Button onClick={() => void saveConfig()} disabled={isSavingConfig}>
          {isSavingConfig ? <LoaderCircle className="size-4 animate-spin" /> : <Save className="size-4" />}
          保存
        </Button>
      </div>
    </>
  );
}
