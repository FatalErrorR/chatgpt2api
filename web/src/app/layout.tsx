import type { Metadata, Viewport } from "next";
import { Inter, JetBrains_Mono, Source_Serif_4 } from "next/font/google";
import { Toaster } from "sonner";
import "./globals.css";
import { AppShell } from "@/components/app-shell";
import { ThemeScript } from "@/components/theme-script";

// 标题衬线、正文无衬线、代码等宽；中文回退字体在 globals.css 的 --font-* 里
const inter = Inter({ subsets: ["latin"], variable: "--font-inter", display: "swap" });
const sourceSerif = Source_Serif_4({ subsets: ["latin"], variable: "--font-source-serif", display: "swap" });
const jetbrainsMono = JetBrains_Mono({ subsets: ["latin"], variable: "--font-jetbrains-mono", display: "swap" });

export const metadata: Metadata = {
  title: "ChatGPT 号池管理",
  description: "ChatGPT account pool management dashboard",
};

export const viewport: Viewport = {
  width: "device-width",
  initialScale: 1,
  maximumScale: 1,
  userScalable: false,
  themeColor: [
    { media: "(prefers-color-scheme: light)", color: "#F5F4EE" },
    { media: "(prefers-color-scheme: dark)", color: "#1F1E1C" },
  ],
};

export default function RootLayout({
  children,
}: Readonly<{
  children: React.ReactNode;
}>) {
  return (
    <html lang="zh-CN" suppressHydrationWarning className={`${inter.variable} ${sourceSerif.variable} ${jetbrainsMono.variable}`}>
      <head>
        <ThemeScript />
      </head>
      <body className="bg-page font-sans text-ink antialiased">
        <Toaster
          position="bottom-right"
          toastOptions={{
            classNames: {
              toast: "!rounded-[10px] !border-0 !bg-ink !text-page !shadow-[0_12px_24px_-16px_rgba(0,0,0,.5)] !text-[13px]",
              description: "!text-page/70",
            },
          }}
        />
        <main className="min-h-screen overflow-x-hidden pt-[env(safe-area-inset-top)]">
          <AppShell>{children}</AppShell>
        </main>
      </body>
    </html>
  );
}
