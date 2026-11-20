// Acceptance C2/C3: the login page, every admin page in dark mode, the canvas dialog, and the key
// flows, each step screenshotted so the Python and Go runs can be compared. The flows that change
// data (proxy reassign, check-and-retire, the failover setting) really submit, then read the data
// back through the API and assert the result. Exits non-zero if any step fails or a page throws.
// usage: NODE_PATH=<playwright node_modules> CHROME_PATH=<chromium> node flows.js <base url> <auth key> <out dir>
// Needs tunnel.py on TUNNEL_PORT (default 18200): the proxies that pass the check.
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
  const api = async (method, path, body) => {
    const response = await page.request.fetch(base + path, {
      method,
      headers: { Authorization: `Bearer ${key}`, "Content-Type": "application/json" },
      data: body === undefined ? undefined : JSON.stringify(body),
    });
    if (!response.ok()) throw new Error(`${method} ${path} -> ${response.status()} ${(await response.text()).slice(0, 200)}`);
    return response.json();
  };
  const expect = (ok, message) => {
    if (!ok) throw new Error("assert: " + message);
  };
  const dialogClosed = () => page.getByRole("dialog").waitFor({ state: "hidden", timeout: 30000 });
  const tunnelPort = process.env.TUNNEL_PORT || "18200";
  const isTunnel = (url) => String(url || "").includes(`@127.0.0.1:${tunnelPort}`);

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
  // C3: reassign proxies, for real: the selected account moves to one of three new working proxies
  await step("04a-setup-tunnel-proxies", async () => {
    const text = ["good1", "good2", "good3"].map((user) => `127.0.0.1:${tunnelPort}:${user}:pw`).join("\n");
    await api("POST", "/api/proxies/batch", { text, vendor: "隧道" });
    const { items } = await api("GET", "/api/proxies");
    expect(items.filter((proxy) => isTunnel(proxy.url) && !proxy.disabled).length === 3, "three tunnel proxies added");
  });
  await step("04-accounts-reassign", async () => {
    const before = new Map((await api("GET", "/api/accounts")).items.map((account) => [account.access_token, account.proxy]));
    await page.reload();
    await settle();
    await page.locator("tbody input[type=checkbox], tbody button[role=checkbox]").first().click();
    await page.getByRole("button", { name: "重新分配代理" }).click();
    const apply = page.getByRole("button", { name: /^迁移 \d+ 个账号$/ });
    await apply.waitFor({ timeout: 30000 });
    const planned = Number((await apply.innerText()).match(/\d+/)[0]);
    await shot("04-accounts-reassign-plan");
    await apply.click();
    await dialogClosed();
    const moved = (await api("GET", "/api/accounts")).items.filter((account) => before.has(account.access_token) && before.get(account.access_token) !== account.proxy);
    expect(planned === 1 && moved.length === 1, `planned ${planned}, moved ${moved.length}`);
    expect(isTunnel(moved[0].proxy), `moved to ${String(moved[0].proxy).replace(/\/\/[^@]*@/, "//***@")}`);
  });

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

  // C3: check and retire, for real: three working proxies and two dead ones that carry accounts; the
  // rest are retired first (through the API) so the dead ones are a minority, not a provider outage
  let dead = [];
  await step("07b-setup-check-and-retire", async () => {
    const { config } = await api("GET", "/api/settings");
    await api("POST", "/api/settings", { ...config, proxy_max_accounts: 1000 });
    const { items } = await api("GET", "/api/proxies");
    const enabled = items.filter((proxy) => !proxy.disabled && !isTunnel(proxy.url));
    dead = enabled.filter((proxy) => proxy.account_count > 0).slice(0, 2).map((proxy) => proxy.key);
    expect(dead.length === 2, "two dead proxies with accounts");
    await api("POST", "/api/proxies/retire", { urls: enabled.filter((proxy) => !dead.includes(proxy.key)).map((proxy) => proxy.key), dry_run: false });
    const after = (await api("GET", "/api/proxies")).items.filter((proxy) => !proxy.disabled);
    expect(after.length === 5, `5 enabled proxies, got ${after.length}`);
  });
  await step("07c-proxies-check-and-retire", async () => {
    const onDead = (await api("GET", "/api/proxies")).items.filter((proxy) => dead.includes(proxy.key)).reduce((sum, proxy) => sum + proxy.account_count, 0);
    await page.reload();
    await settle();
    await page.getByRole("button", { name: "检测并清理" }).click();
    const retire = page.getByRole("button", { name: /^停用 \d+ 个/ });
    await retire.waitFor({ timeout: 90000 });
    expect((await retire.innerText()).startsWith(`停用 2 个并迁移 ${onDead} 个账号`), `button "${await retire.innerText()}", ${onDead} accounts on the dead proxies`);
    await shot("07c-proxies-check-plan");
    await retire.click();
    await dialogClosed();
    const proxies = (await api("GET", "/api/proxies")).items;
    for (const proxy of proxies.filter((proxy) => dead.includes(proxy.key))) {
      expect(proxy.disabled && proxy.account_count === 0, `dead proxy still enabled or carrying ${proxy.account_count} accounts`);
    }
    for (const proxy of proxies.filter((proxy) => isTunnel(proxy.url))) {
      expect(!proxy.disabled && proxy.health.successes > 0 && !proxy.health.abnormal, "tunnel proxy passed the check");
    }
    const deadUrls = proxies.filter((proxy) => dead.includes(proxy.key)).map((proxy) => proxy.url);
    const stranded = (await api("GET", "/api/accounts")).items.filter((account) => deadUrls.includes(account.proxy));
    expect(stranded.length === 0, `${stranded.length} accounts still on retired proxies`);
  });

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

  // C3: the account failover setting (出图慢就换), saved and read back from the API and the page
  await step("11-settings-failover", async () => {
    const before = (await api("GET", "/api/settings")).config;
    expect(!before.image_failover_enabled, "failover starts off");
    await page.goto(base + "/settings/");
    await settle();
    const toggle = page.locator("label", { hasText: "出图慢就换号重新生成" }).getByRole("checkbox");
    await toggle.scrollIntoViewIfNeeded();
    await toggle.click();
    await page.getByLabel("换号前等待秒数").fill("90");
    await page.getByRole("button", { name: "保存", exact: true }).last().click();
    await page.waitForTimeout(1500);
    const after = (await api("GET", "/api/settings")).config;
    expect(after.image_failover_enabled === true && Number(after.image_failover_secs) === 90, `saved ${after.image_failover_enabled} ${after.image_failover_secs}`);
    await page.reload();
    await settle();
    expect((await toggle.getAttribute("data-state")) === "checked", "toggle checked after reload");
    expect((await page.getByLabel("换号前等待秒数").inputValue()) === "90", "90 seconds after reload");
    await toggle.scrollIntoViewIfNeeded();
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
  const failed = log.steps.filter((line) => !line.endsWith(" ok"));
  if (failed.length || log.pageErrors.length) {
    console.error(`${base}: ${failed.length} failed steps, ${log.pageErrors.length} page errors\n` + failed.concat(log.pageErrors).join("\n"));
    process.exitCode = 1;
  }
})().catch((error) => {
  console.error(error);
  process.exitCode = 1;
});
