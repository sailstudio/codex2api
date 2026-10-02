// 材料侧车：用真实浏览器打开 Prism 项目页，发一条消息，拦截 start 请求
// 偷下「对话材料包」（metadata 全套 + system 前缀），写成网关可读的
// material.json（归一化形态）。
//
// 为什么必须用浏览器：材料里的 proxy_request_debug / codex_listen_snapshot
// 由页面自身产生，纯 HTTP 自建会 400 "Please submit prompt again"。
//
// 用法：
//   PROJ=<projectUuid> PROMPT='预热' node cmd/prism-material/produce.js
// 输出：${PRISM_MATERIAL_PATH:-/tmp/prism_sidecar/material.json}
// playwright 解析：按候选路径逐个尝试（避免依赖 cwd 或 NODE_PATH）。
function loadPlaywright() {
  const candidates = [
    'playwright',
    process.env.PRISM_PLAYWRIGHT_DIR,
    '/tmp/prism_sidecar/node_modules/playwright',
    '/usr/local/lib/node_modules/playwright',
  ].filter(Boolean);
  let lastErr;
  for (const c of candidates) {
    try { return require(c); } catch (e) { lastErr = e; }
  }
  throw new Error('找不到 playwright（试过: ' + candidates.join(', ') + '）: ' + lastErr.message);
}
const { chromium } = loadPlaywright();
const fs = require('fs');
const path = require('path');

const BASE = process.env.PRISM_BASE || 'https://prism.openai.com';
const PROXY = process.env.PRISM_UI_PROXY || 'http://127.0.0.1:7890';
const ACCT_JSON = process.env.PRISM_ACCOUNT_JSON || '/tmp/cpa_account.json';
const OUT = process.env.PRISM_MATERIAL_PATH || '/tmp/prism_sidecar/material.json';
const PROMPT = process.env.PROMPT || '预热';
const UA = process.env.PRISM_UA ||
  'codex-tui/0.156.0 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.156.0)';

function loadAccount() {
  const arr = JSON.parse(fs.readFileSync(ACCT_JSON, 'utf8'));
  const a = Array.isArray(arr) ? arr[0] : arr;
  return { accessToken: a.access_token, projectID: process.env.PROJ || null };
}

(async () => {
  const acct = loadAccount();
  if (!acct.accessToken) throw new Error('账号 JSON 缺 access_token');

  const browser = await chromium.launch({
    headless: process.env.PRISM_HEADFUL !== '1',
    channel: 'chrome',
    proxy: { server: PROXY },
    args: ['--no-sandbox', '--disable-blink-features=AutomationControlled'],
  });
  const ctx = await browser.newContext({ userAgent: UA, viewport: { width: 1600, height: 1000 }, locale: 'zh-CN' });
  await ctx.addCookies([
    { name: 'prism_oai_access_token', value: acct.accessToken, domain: 'prism.openai.com', path: '/' },
    { name: 'oai-sc', value: acct.accessToken, domain: 'prism.openai.com', path: '/' },
  ]);
  const page = await ctx.newPage();

  let cap = null;
  // ⭐ 关键：拦截 start 请求并 **abort**，只偷 body 不发出去。
  // 若放行，页面的这条消息会真占用沙箱并开启一轮对话，导致随后网关的
  // 复用请求撞上忙碌沙箱 → 403 "Error while processing conversation"。
  // 上游侧车也是这么做的（"请求被 abort，票据不消费"）。
  await page.route('**/api/llm/response_with_tools_start', async route => {
    const req = route.request();
    cap = { url: req.url(), headers: req.headers(), postData: req.postData() };
    await route.abort();
  });

  // 确定目标项目：优先 PROJ，否则从列表取第一个
  let proj = acct.projectID;
  if (!proj) {
    // 优先复用材料里已绑定的项目（保持身份一致），否则取列表第一个
    try {
      const prev = JSON.parse(fs.readFileSync(OUT, 'utf8'));
      if (prev && prev.project_id) { proj = prev.project_id; console.error('[material] 复用已有项目:', proj); }
    } catch (e) { /* 首次生成，无历史材料 */ }
  }
  if (!proj) {
    await page.goto(BASE + '/', { waitUntil: 'domcontentloaded', timeout: 60000 });
    await page.waitForSelector('a[href*="?u="]', { timeout: 60000 }).catch(() => {});
    await page.waitForTimeout(3000);
    proj = await page.evaluate(() => {
      const a = document.querySelector('a[href*="?u="]');
      if (!a) return null;
      const m = a.getAttribute('href').match(/u=([0-9a-f-]+)/i);
      return m ? m[1] : null;
    });
  }
  if (!proj) throw new Error('未找到项目（请先在 Prism 建一个项目）');
  console.error('[material] 目标项目:', proj);

  await page.goto(`${BASE}/?u=${proj}&pg=1`, { waitUntil: 'domcontentloaded', timeout: 60000 });

  // 等沙箱就绪
  const ta = page.locator('textarea').first();
  let ready = false;
  for (let i = 0; i < 40; i++) {
    await page.waitForTimeout(3000);
    const txt = await page.evaluate(() => document.body.innerText);
    const cnt = await page.locator('textarea').count();
    if (cnt > 0 && !/正在初始化|正在准备中|Initializing|Preparing/.test(txt)) { ready = true; break; }
    console.error(`  [${i}] 等待沙箱就绪… (textarea=${cnt})`);
  }
  if (!ready) throw new Error('沙箱未就绪（超时）');

  await ta.click();
  await ta.fill(PROMPT);
  await ta.press('Enter');
  for (let i = 0; i < 40 && !cap; i++) await page.waitForTimeout(1000);
  if (!cap) throw new Error('未捕获到 start 请求');

  const body = JSON.parse(cap.postData);
  const normalized = {
    captured_at: new Date().toISOString(),
    project_id: proj,
    start_url: cap.url,
    metadata: body.metadata,
    input: body.input,
    note: '由 cmd/prism-material/produce.js 从真实页面捕获；metadata 必须原样复用（含 proxy_request_debug / codex_listen_snapshot）',
  };
  fs.mkdirSync(path.dirname(OUT), { recursive: true });
  fs.writeFileSync(OUT, JSON.stringify(normalized, null, 1));
  console.error(`[material] 已写出 ${OUT}  (metadata 键: ${Object.keys(body.metadata).join(',')})`);
  console.log(JSON.stringify({ ok: true, out: OUT, project_id: proj, keys: Object.keys(body.metadata) }));
  await browser.close();
})().catch(e => { console.error('FATAL', e.message); process.exit(1); });
