// Saves the failover setting from the settings page (the way an admin would), for verify-local.sh.
// usage: NODE_PATH=<playwright node_modules> CHROME_PATH=<chromium> node save-setting.js <base url> <auth key> <seconds>
const { chromium } = require("playwright");
const [base, key, seconds] = process.argv.slice(2);

(async () => {
  const browser = await chromium.launch(process.env.CHROME_PATH ? { executablePath: process.env.CHROME_PATH } : {});
  const page = await browser.newPage({ locale: "zh-CN" });
  await page.goto(base + "/login/");
  await page.fill('input[type="password"]', key);
  await page.click("button:has-text('登录')");
  await page.waitForTimeout(1500);
  await page.goto(base + "/settings/");
  await page.waitForLoadState("networkidle").catch(() => {});
  const toggle = page.locator("label", { hasText: "出图慢就换号重新生成" }).getByRole("checkbox");
  if ((await toggle.getAttribute("data-state")) !== "checked") await toggle.click();
  await page.getByLabel("换号前等待秒数").fill(seconds);
  const saved = page.waitForResponse((r) => r.url().endsWith("/api/settings") && r.request().method() === "POST");
  await page.getByRole("button", { name: "保存", exact: true }).last().click();
  const response = await saved;
  console.log(`POST /api/settings ${response.status()}`);
  await browser.close();
  if (!response.ok()) process.exitCode = 1;
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
