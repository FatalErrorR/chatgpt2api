// Visit every admin page on one server, record API calls, failures, console errors, screenshots.
// usage: NODE_PATH=<playwright node_modules> node walk.js <base url> <auth key> <out dir>
const { chromium } = require("playwright");
const fs = require("fs");
const [base, key, out] = process.argv.slice(2);
const pages = ["/", "/proxies/", "/image/", "/image-manager/", "/logs/", "/settings/", "/debug/", "/accounts/"];
(async () => {
  fs.mkdirSync(out, { recursive: true });
  const browser = await chromium.launch(process.env.CHROME_PATH ? { executablePath: process.env.CHROME_PATH } : {});
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, locale: "zh-CN" });
  const page = await context.newPage();
  const report = {};
  let current = "login";
  const record = () => (report[current] ||= { calls: [], failed: [], console: [], pageErrors: [] });
  page.on("console", (m) => { if (m.type() === "error") record().console.push(m.text().slice(0, 300)); });
  page.on("pageerror", (e) => record().pageErrors.push(String(e).slice(0, 300)));
  page.on("response", (r) => {
    const url = new URL(r.url());
    if (!url.pathname.startsWith("/api/") && !url.pathname.startsWith("/v1/") && !url.pathname.startsWith("/auth/") && url.pathname !== "/version") return;
    const entry = `${r.request().method()} ${url.pathname}${url.search} ${r.status()}`;
    record().calls.push(entry);
    if (r.status() >= 400) record().failed.push(entry);
  });
  page.on("requestfailed", (r) => record().failed.push(`${r.method()} ${r.url()} ${r.failure()?.errorText}`));
  await page.goto(base + "/login/");
  await page.fill('input[type="password"]', key);
  await page.click("button:has-text('登录')");
  await page.waitForTimeout(1500);
  for (const path of pages) {
    current = path;
    record();
    await page.goto(base + path, { waitUntil: "networkidle" }).catch((e) => record().pageErrors.push(String(e)));
    await page.waitForTimeout(1200);
    await page.screenshot({ path: `${out}/${path.replace(/\//g, "_") || "_"}.png`, fullPage: true });
    record().text = (await page.innerText("body")).slice(0, 4000);
  }
  fs.writeFileSync(`${out}/report.json`, JSON.stringify(report, null, 2));
  await browser.close();
})();
