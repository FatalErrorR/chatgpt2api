"use client";

import { useRouter } from "next/navigation";
import { useState } from "react";
import { LoaderCircle } from "lucide-react";
import { toast } from "sonner";

import { ThemeToggle } from "@/components/theme-toggle";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import webConfig from "@/constants/common-env";
import { login } from "@/lib/api";
import { useRedirectIfAuthenticated } from "@/lib/use-auth-guard";
import { getDefaultRouteForRole, setStoredAuthSession } from "@/store/auth";

export default function LoginPage() {
  const router = useRouter();
  const [authKey, setAuthKey] = useState("");
  const [isSubmitting, setIsSubmitting] = useState(false);
  const { isCheckingAuth } = useRedirectIfAuthenticated();

  const handleLogin = async () => {
    const normalizedAuthKey = authKey.trim();
    if (!normalizedAuthKey) {
      toast.error("请输入密钥");
      return;
    }

    setIsSubmitting(true);
    try {
      const data = await login(normalizedAuthKey);
      await setStoredAuthSession({
        key: normalizedAuthKey,
        role: data.role,
        subjectId: data.subject_id,
        name: data.name,
      });
      router.replace(getDefaultRouteForRole(data.role));
    } catch (error) {
      const message = error instanceof Error ? error.message : "登录失败";
      toast.error(message);
    } finally {
      setIsSubmitting(false);
    }
  };

  if (isCheckingAuth) {
    return (
      <div className="grid min-h-screen w-full place-items-center">
        <LoaderCircle className="size-5 animate-spin text-ink-3" />
      </div>
    );
  }

  return (
    <div className="relative grid min-h-screen w-full place-items-center px-4">
      <div className="fixed top-4 right-4 z-10">
        <ThemeToggle />
      </div>
      <div className="w-full max-w-[380px] text-center">
        <div className="mx-auto mb-6 grid size-10 place-items-center rounded-[11px] bg-clay text-lg font-semibold text-white">C</div>
        <h1 className="mb-2 text-[34px] font-medium text-ink">欢迎回来</h1>
        <p className="mb-7 text-sm text-ink-2">输入管理员或用户密钥继续。</p>
        <label htmlFor="auth-key" className="sr-only">
          密钥
        </label>
        <Input
          id="auth-key"
          type="password"
          autoFocus
          value={authKey}
          onChange={(event) => setAuthKey(event.target.value)}
          onKeyDown={(event) => {
            if (event.key === "Enter") {
              void handleLogin();
            }
          }}
          placeholder="请输入密钥"
          className="mb-3 h-[42px] rounded-lg border-line-strong bg-surface px-3"
        />
        <Button variant="clay" className="h-[42px] w-full rounded-lg text-sm" onClick={() => void handleLogin()} disabled={isSubmitting}>
          {isSubmitting ? <LoaderCircle className="size-4 animate-spin" /> : null}
          登录
        </Button>
      </div>
      <div className="absolute bottom-5 text-xs text-ink-3">chatgpt2api · v{webConfig.appVersion}</div>
    </div>
  );
}
