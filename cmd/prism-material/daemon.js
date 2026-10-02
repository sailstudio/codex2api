// 材料常驻侧车：保持一个浏览器存活，按 TTL 周期重采「对话材料包」。
//
// 为什么需要它：材料里的 cf_bm/__cflb 是分钟级 cookie，实测 TTL 约 2 分钟；
// 过期后上游 start 会静默失败（400 Please submit prompt again）。单次
// produce.js 只产一份，无法支撑生产。本守护进程常驻浏览器 → 周期性
// 重采 → 原子写盘，网关侧只需读文件。
//
// 用法：
//   node cmd/prism-material/daemon.js
// 环境变量：
//   PRISM_MATERIAL_PATH   输出路径（默认 /tmp/prism_sidecar/material.json）
//   PRISM_REFRESH_SECONDS 刷新周期（默认 75，须 < 网关 TTL 120）
//   PRISM_ACCOUNT_JSON    账号 JSON
//   PRISM_UI_PROXY        浏览器代理（默认 http://127.0.0.1:7890）
//   PROJ                  指定项目 uuid（默认复用已有材料/列表第一个）
//   PRISM_HEADFUL=1       有头模式（排障用）
//
// 设计取舍：
//   - **不 reload 页面**：页面加载会触发 POST /api/backend/1/new 铸新沙箱，
//     频繁重载会浪费沙箱额度。改为复用同一页面连续偷票。
//   - **原子写盘**：先写 .tmp 再 rename，避免网关读到半截 JSON。
//   - **捕获失败不清空旧文件**：宁可让旧材料自然过期，也不要写出坏文件。
const fs = require('fs');
const path = require('path');

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

const BASE = process.env.PRISM_BASE || 'https://prism.openai.com';
const PROXY = process.env.PRISM_UI_PROXY || 'http://127.0.0.1:7890';
const ACCT_JSON = process.env.PRISM_ACCOUNT_JSON || '/tmp/cpa_account.json';
const OUT = process.env.PRISM_MATERIAL_PATH || '/tmp/prism_sidecar/material.json';
const REFRESH_MS = Math.max(30, Number(process.env.PRISM_REFRESH_SECONDS || 75)) * 1000;
const UA = process.env.PRISM_UA ||
  'codex-tui/0.156.0 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.156.0)';

function log(...a) {
  console.error(`[daemon ${new Date().toISOString().slice(11, 19)}]`, ...a);
}

function loadAccount() {
  const arr = JSON.parse(fs.readFileSync(ACCT_JSON, 'utf8'));
  const a = Array.isArray(arr) ? arr[0] : arr;
  return { accessToken: a.access_token, projectID: process.env.PROJ || null };
}

// 原子写盘：tmp → rename。
function writeAtomic(file, data) {
  const tmp = `${file}.tmp`;
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(tmp, data);
  fs.renameSync(tmp, file);
}

// 读取已有材料里的项目（保持身份一致）。
function existingProject() {
  try {
    const prev = JSON.parse(fs.readFileSync(OUT, 'utf8'));
    return prev && prev.project_id ? prev.project_id : null;
  } catch (e) {
    return null;
  }
}

let browser = null;
let ctx = null;
let page = null;
let captured = null;

async function ensureSession(acct) {
  if (browser) return;
  log('启动浏览器…');
  browser = await chromium.launch({
    headless: process.env.PRISM_HEADFUL !== '1',
    channel: 'chrome',
    proxy: { server: PROXY },
    args: ['--no-sandbox', '--disable-blink-features=AutomationControlled'],
  });
  ctx = await browser.newContext({
    userAgent: UA, viewport: { width: 1600, height: 1000 }, locale: 'zh-CN',
  });
  await ctx.addCookies([
    { name: 'prism_oai_access_token', value: acct.accessToken, domain: 'prism.openai.com', path: '/' },
    { name: 'oai-sc', value: acct.accessToken, domain: 'prism.openai.com', path: '/' },
  ]);
  page = await ctx.newPage();

  // ⭐ 拦截 start 并 abort：只偷 body，不真发（否则占用沙箱 → 网关复用 403）
  await page.route('**/api/llm/response_with_tools_start', async route => {
    const req = route.request();
    captured = { url: req.url(), headers: req.headers(), postData: req.postData() };
    await route.abort();
  });

  page.on('crash', () => { log('页面崩溃，下次循环重建'); browser = null; });
}

async function openProject(acct) {
  let proj = acct.projectID || existingProject();
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
  if (!proj) throw new Error('未找到项目（请先在 Prism 建一个）');
  log('目标项目:', proj);
  await page.goto(`${BASE}/?u=${proj}&pg=1`, { waitUntil: 'domcontentloaded', timeout: 60000 });
  return proj;
}

async function waitSandboxReady() {
  const ta = page.locator('textarea').first();
  for (let i = 0; i < 40; i++) {
    await page.waitForTimeout(3000);
    const txt = await page.evaluate(() => document.body.innerText);
    const cnt = await page.locator('textarea').count();
    if (cnt > 0 && !/正在初始化|正在准备中|Initializing|Preparing/.test(txt)) return ta;
    if (i % 4 === 3) log(`  等沙箱就绪… (${i + 1})`);
  }
  throw new Error('沙箱未就绪（超时）');
}

// 采一轮材料。
async function captureOnce(acct, proj) {
  // 先刷新页面：会话复位后捕获到的 input 只含「本轮」内容。
  // ⚠️ 不刷新会一直往同一个会话追加（实测累积到 32 份重复 system/预热消息，
  // 材料 1KB→30KB，每轮请求白送 23K 字符，越来越慢甚至超时）。
  await page.goto(`${BASE}/?u=${proj}&pg=1`, { waitUntil: 'domcontentloaded', timeout: 60000 }).catch(() => {});
  const ta = await waitSandboxReady();
  captured = null;

  // 清空输入框（上一轮 abort 后可能残留文本）
  await ta.click();
  await ta.fill('');
  await ta.fill('预热');
  await ta.press('Enter');

  for (let i = 0; i < 40 && !captured; i++) await page.waitForTimeout(1000);
  if (!captured) throw new Error('未捕获到 start 请求');

  const body = JSON.parse(captured.postData);
  const normalized = {
    captured_at: new Date().toISOString(),
    project_id: proj,
    start_url: captured.url,
    metadata: body.metadata,
    input: body.input,
    note: '由 cmd/prism-material/daemon.js 从真实页面周期捕获；metadata 必须原样复用',
  };
  writeAtomic(OUT, JSON.stringify(normalized, null, 1));
  log(`材料已刷新 → ${OUT} (模型=${body.metadata.model} 档位=${body.metadata.reasoning_effort})`);

  // 输入框复位，为下一轮做准备
  await ta.fill('').catch(() => {});
}

async function main() {
  const acct = loadAccount();
  if (!acct.accessToken) throw new Error('账号 JSON 缺 access_token');
  log(`周期 ${REFRESH_MS / 1000}s，输出 ${OUT}`);

  let proj = null;
  let backoff = 5000;
  // eslint-disable-next-line no-constant-condition
  while (true) {
    try {
      await ensureSession(acct);
      if (!proj) proj = await openProject(acct);
      await captureOnce(acct, proj);
      backoff = 5000;
      await new Promise(r => setTimeout(r, REFRESH_MS));
    } catch (e) {
      log('采集失败:', e.message.slice(0, 200));
      // 页面/浏览器级故障 → 彻底重建
      if (/Target closed|crash|Protocol error|browser has been closed/.test(e.message)) {
        try { await browser?.close(); } catch (_) {}
        browser = null; ctx = null; page = null; captured = null;
      }
      log(`退避 ${backoff / 1000}s 后重试`);
      await new Promise(r => setTimeout(r, backoff));
      backoff = Math.min(backoff * 2, 60000);
    }
  }
}

process.on('SIGTERM', async () => { log('收到 SIGTERM，关闭浏览器'); try { await browser?.close(); } catch (_) {} process.exit(0); });
process.on('SIGINT', async () => { log('收到 SIGINT，关闭浏览器'); try { await browser?.close(); } catch (_) {} process.exit(0); });

main().catch(e => { log('FATAL', e.message); process.exit(1); });
