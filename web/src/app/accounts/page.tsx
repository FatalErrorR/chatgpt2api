"use client";

import { useEffect } from "react";
import { useRouter } from "next/navigation";

import { SkeletonLines } from "@/components/ui/states";

/** 号池已并进总览（/）；旧链接和书签跳过去。 */
export default function AccountsPage() {
  const router = useRouter();
  useEffect(() => {
    router.replace("/");
  }, [router]);
  return <SkeletonLines lines={6} className="pt-6" />;
}
