// Acceptance C2/C3: the login page, every admin page in dark mode, the canvas dialog, and the key
// flows, each step screenshotted so the Python and Go runs can be compared.
// usage: NODE_PATH=<playwright node_modules> CHROME_PATH=<chromium> node flows.js <base url> <auth key> <out dir>
const { chromium } = require("playwright");
const fs = require("fs");
const [base, key, out] = process.argv.slice(2);

(async () => {
  fs.mkdirSync(out, { recursive: true });
  const browser = await chromium.launch(process.env.CHROME_PATH ? { executablePath: process.env.CHROME_PATH } : {});
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, locale: "zh-CN" });
  const page = await context.newPage();
  const log = { steps: [], console: [], pageErrors: [], failed: [] };
  page.on("console", (m) => m.type() === "error" && !m.text().includes("ERR_CONNECTION_REFUSED") && log.console.push(m.text().slice(0, 200)));
  page.on("pageerror", (e) => log.pageErrors.push(String(e).slice(0, 200)));
  page.on("response", (r) => {
    const path = new URL(r.url()).pathname;
    if (r.status() >= 400 && (path.startsWith("/api/") || path.startsWith("/v1/"))) log.failed.push(`${r.request().method()} ${path} ${r.status()}`);
  });
  const shot = async (name) => page.screenshot({ path: `${out}/${name}.png`, fullPage: false });
  const step = async (name, fn) => {
    try {
      await fn();
      await page.waitForTimeout(800);
      await shot(name);
      log.steps.push(`${name} ok`);
    } catch (error) {
      log.steps.push(`${name} FAILED ${String(error).split("\n")[0].slice(0, 160)}`);
      await shot(name).catch(() => {});
    }
  };
  const settle = () => page.waitForLoadState("networkidle").catch(() => {});

  // C2: the login page itself
  await step("01-login", async () => {
    await page.goto(base + "/login/");
    await settle();
  });
  await page.fill('input[type="password"]', key);
  await page.click("button:has-text('登录')");
  await page.waitForTimeout(1500);

  // C2: the canvas entry (a third-party app link) opens its dialog
  await step("01b-canvas-dialog", async () => {
    await page.goto(base + "/");
    await settle();
    await page.getByRole("button", { name: "画布" }).click();
  });
  await page.keyboard.press("Escape");

  // C3: account filters and paging
  await step("02-accounts-abnormal", async () => {
    await page.goto(base + "/");
    await settle();
    await page.getByText("异常", { exact: true }).first().click();
    await settle();
  });
  await step("03-accounts-page-2", async () => {
    await page.getByRole("button", { name: "2", exact: true }).first().click();
    await settle();
  });
  await step("04-accounts-reassign-dialog", async () => {
    await page.locator("tbody input[type=checkbox], tbody button[role=checkbox]").first().click();
    await page.getByRole("button", { name: "重新分配代理" }).click();
    await settle();
  });
  await page.keyboard.press("Escape");

  // C3: proxies: batch add, check and clean
  await step("05-proxies-batch-add-preview", async () => {
    await page.goto(base + "/proxies/");
    await settle();
    await page.getByRole("button", { name: "批量添加" }).click();
    await page.locator("textarea").fill("10.0.0.1:8080:alice:pw\n10.0.0.2:8080\nnot a proxy\n10.0.0.1:8080:alice:pw");
    await page.getByPlaceholder("厂商名称（可选）").fill("测试厂商");
  });
  await step("06-proxies-batch-add-done", async () => {
    await page.getByRole("button", { name: /^添加/ }).click();
    await settle();
  });
  await page.keyboard.press("Escape");
  await step("07-proxies-check-and-clean", async () => {
    await page.getByRole("button", { name: "检测并清理" }).click();
    await page.getByText(/检测 \d+ 个/).waitFor({ timeout: 60000 });
  });
  await page.keyboard.press("Escape");

  // C3: images: retention rule, delete by date with progress
  await step("08-images-retention-rule", async () => {
    await page.goto(base + "/image-manager/");
    await settle();
    await page.getByRole("button", { name: "存储" }).click();
    await page.getByText("定时清理规则…").click();
    await settle();
  });
  await page.keyboard.press("Escape");
  await step("09-images-delete-by-date-preview", async () => {
    await page.getByRole("button", { name: "存储" }).click();
    await page.getByText("按日期删除…").click();
    await page.locator('input[type="date"]').fill("2026-11-05");
    await page.getByText(/将删除 [\d,]+ 张图片/).waitFor({ timeout: 30000 });
  });
  await step("10-images-delete-by-date-done", async () => {
    await page.getByRole("button", { name: /^删除/ }).last().click();
    await page.getByText(/已删除 [\d,]+ 张图片/).waitFor({ timeout: 120000 });
  });

  // C3: the account failover setting (出图慢就换)
  await step("11-settings-failover", async () => {
    await page.goto(base + "/settings/");
    await settle();
    await page.getByText("出图慢就换").first().scrollIntoViewIfNeeded();
  });

  // C2: every page in dark mode
  await page.evaluate(() => localStorage.setItem("chatgpt2api-theme", "dark"));
  for (const [name, path] of [["20-dark-overview", "/"], ["21-dark-proxies", "/proxies/"], ["22-dark-image", "/image/"], ["23-dark-images", "/image-manager/"],
    ["24-dark-logs", "/logs/"], ["25-dark-settings", "/settings/"], ["26-dark-debug", "/debug/"]]) {
    await step(name, async () => {
      await page.goto(base + path);
      await settle();
    });
  }
  fs.writeFileSync(`${out}/flows.json`, JSON.stringify(log, null, 2));
  await browser.close();
})();
