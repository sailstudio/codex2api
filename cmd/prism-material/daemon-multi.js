// 多槽材料保温侧车：**一个浏览器上下文 = 一个身份 = 一槽材料**。
//
// 为什么需要它：上游按**浏览器身份**限流（单身份约 80 RPM），而一份材料
// 绑定一个身份。串行侧车只能维护 1 个身份 → 吞吐被锁死在 ~80 RPM。
// 本进程并行维护 N 个独立上下文（各自 cookie / UA / 沙箱），
// 把材料写成 N 个文件；网关侧 MaterialPool 自动轮转 → 吞吐 ≈ N × 80 RPM。
//
// 用法：
//   PRISM_SLOTS=4 node cmd/prism-material/daemon-multi.js
// 环境变量：
//   PRISM_SLOTS           槽数（默认 4）；多身份需**多个 access_token**
//   PRISM_ACCOUNTS_JSON   账号数组文件（默认 /tmp/cpa_account.json）；
//                         单账号时会用该账号 × N 个独立上下文（共享身份额度）
//   PRISM_MATERIAL_DIR    输出目录（默认 /tmp/prism_sidecar/materials）
//   PRISM_REFRESH_SECONDS 每槽刷新周期（默认 75）
//   PRISM_UI_PROXY        浏览器代理（默认 http://127.0.0.1:7890）
//   PRISM_HEADFUL=1       有头模式（排障）
//
// 输出：<dir>/slot-0.json ... <dir>/slot-N.json（原子写盘）
//
// ⚠️ 重要：真正的吞吐提升需要**多个不同身份的 access_token**
// （每个 token 一个浏览器）。若只给一个 token，N 个槽共享同一身份额度，
// 吞吐不会提升，但可用性更好（单槽坏掉不影响其它槽）。
const fs = require('fs');
const path = require('path');

function loadPlaywright() {
  const candidates = [
    'playwright', process.env.PRISM_PLAYWRIGHT_DIR,
    '/tmp/prism_sidecar/node_modules/playwright',
    '/usr/local/lib/node_modules/playwright',
  ].filter(Boolean);
  let lastErr;
  for (const c of candidates) {
    try { return require(c); } catch (e) { lastErr = e; }
  }
  throw new Error('找不到 playwright: ' + lastErr.message);
}
const { chromium } = loadPlaywright();

const BASE = process.env.PRISM_BASE || 'https://prism.openai.com';
const PROXY = process.env.PRISM_UI_PROXY || 'http://127.0.0.1:7890';
const ACCT_JSON = process.env.PRISM_ACCOUNTS_JSON || '/tmp/cpa_account.json';
const OUT_DIR = process.env.PRISM_MATERIAL_DIR || '/tmp/prism_sidecar/materials';
const SLOTS = Math.max(1, Number(process.env.PRISM_SLOTS || 4));
const REFRESH_MS = Math.max(30, Number(process.env.PRISM_REFRESH_SECONDS || 75)) * 1000;
const UA = process.env.PRISM_UA ||
  'codex-tui/0.156.0 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.156.0)';

function log(...a) { console.error(`[multi ${new Date().toISOString().slice(11, 19)}]`, ...a); }

function loadAccounts() {
  const arr = JSON.parse(fs.readFileSync(ACCT_JSON, 'utf8'));
  const list = Array.isArray(arr) ? arr : [arr];
  return list.map(a => ({ accessToken: a.access_token, email: a.email }));
}

function writeAtomic(file, data) {
  const tmp = `${file}.tmp`;
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(tmp, data);
  fs.renameSync(tmp, file);
}

// ─────────────────────── 单槽 ───────────────────────

class Slot {
  constructor(idx, acct) {
    this.idx = idx;
    this.acct = acct;
    this.name = `slot-${idx}`;
    this.out = path.join(OUT_DIR, `${this.name}.json`);
    this.browser = null;
    this.ctx = null;
    this.page = null;
    this.captured = null;
    this.proj = null;
    this.backoff = 5000;
    this.okCount = 0;
    this.failCount = 0;
  }

  async ensureSession() {
    if (this.browser) return;
    log(`${this.name}: 启动浏览器…（${this.acct.email || 'n/a'}）`);
    this.browser = await chromium.launch({
      headless: process.env.PRISM_HEADFUL !== '1',
      channel: 'chrome',
      proxy: { server: PROXY },
      args: ['--no-sandbox', '--disable-blink-features=AutomationControlled'],
    });
    // ⭐ 每槽独立上下文：独立 cookie jar / 缓存 → 独立身份与沙箱
    this.ctx = await this.browser.newContext({
      userAgent: UA, viewport: { width: 1600, height: 1000 }, locale: 'zh-CN',
    });
    await this.ctx.addCookies([
      { name: 'prism_oai_access_token', value: this.acct.accessToken, domain: 'prism.openai.com', path: '/' },
      { name: 'oai-sc', value: this.acct.accessToken, domain: 'prism.openai.com', path: '/' },
    ]);
    this.page = await this.ctx.newPage();
    // 拦截 start 并 abort：只偷 body，不占沙箱（放行会 403）
    await this.page.route('**/api/llm/response_with_tools_start', async route => {
      this.captured = { url: route.request().url(), postData: route.request().postData() };
      await route.abort();
    });
    this.page.on('crash', () => { log(`${this.name}: 页面崩溃，下次循环重建`); this.reset(); });
  }

  reset() {
    try { this.browser?.close(); } catch (_) {}
    this.browser = this.ctx = this.page = this.captured = null;
  }

  async openProject() {
    if (this.proj) {
      await this.page.goto(`${BASE}/?u=${this.proj}&pg=1`, { waitUntil: 'domcontentloaded', timeout: 60000 });
      return this.proj;
    }
    // 复用该槽上一次写出的项目（保持身份一致）
    try {
      const prev = JSON.parse(fs.readFileSync(this.out, 'utf8'));
      if (prev.project_id) { this.proj = prev.project_id; }
    } catch (_) {}
    if (this.proj) {
      await this.page.goto(`${BASE}/?u=${this.proj}&pg=1`, { waitUntil: 'domcontentloaded', timeout: 60000 });
      return this.proj;
    }
    await this.page.goto(BASE + '/', { waitUntil: 'domcontentloaded', timeout: 60000 });
    await this.page.waitForSelector('a[href*="?u="]', { timeout: 60000 }).catch(() => {});
    await this.page.waitForTimeout(3000);
    this.proj = await this.page.evaluate(() => {
      const a = document.querySelector('a[href*="?u="]');
      if (!a) return null;
      const m = a.getAttribute('href').match(/u=([0-9a-f-]+)/i);
      return m ? m[1] : null;
    });
    if (!this.proj) throw new Error('未找到项目（请在 Prism 建一个）');
    log(`${this.name}: 目标项目 ${this.proj}`);
    await this.page.goto(`${BASE}/?u=${this.proj}&pg=1`, { waitUntil: 'domcontentloaded', timeout: 60000 });
    return this.proj;
  }

  async waitReady() {
    const ta = this.page.locator('textarea').first();
    for (let i = 0; i < 40; i++) {
      await this.page.waitForTimeout(3000);
      const txt = await this.page.evaluate(() => document.body.innerText);
      const cnt = await this.page.locator('textarea').count();
      if (cnt > 0 && !/正在初始化|正在准备中|Initializing|Preparing/.test(txt)) return ta;
    }
    throw new Error('沙箱未就绪（超时）');
  }

  async capture() {
    const ta = await this.waitReady();
    this.captured = null;
    await ta.click();
    await ta.fill('');
    await ta.fill('预热');
    await ta.press('Enter');
    for (let i = 0; i < 40 && !this.captured; i++) await this.page.waitForTimeout(1000);
    if (!this.captured) throw new Error('未捕获到 start 请求');

    const body = JSON.parse(this.captured.postData);
    writeAtomic(this.out, JSON.stringify({
      captured_at: new Date().toISOString(),
      project_id: this.proj,
      slot: this.idx,
      start_url: this.captured.url,
      metadata: body.metadata,
      input: body.input,
      note: '多槽侧车产出；metadata 必须原样复用',
    }, null, 1));
    this.okCount++;
    return body.metadata;
  }

  async runOnce() {
    await this.ensureSession();
    await this.openProject();
    const md = await this.capture();
    this.backoff = 5000;
    return md;
  }

  async loop() {
    for (;;) {
      try {
        const md = await this.runOnce();
        log(`${this.name}: 材料已刷新（模型=${md.model} 档位=${md.reasoning_effort}） 成功=${this.okCount}`);
      } catch (e) {
        this.failCount++;
        log(`${this.name}: 采集失败(${this.failCount}): ${e.message.slice(0, 160)}`);
        if (/Target closed|crash|Protocol error|browser has been closed|未找到项目/.test(e.message)) {
          this.reset();
          this.backoff = 5000;
        }
        await new Promise(r => setTimeout(r, this.backoff));
        this.backoff = Math.min(this.backoff * 2, 60000);
        continue;
      }
      await new Promise(r => setTimeout(r, REFRESH_MS));
    }
  }
}

// ─────────────────────── 主流程 ───────────────────────

function shutdown(slots) {
  return async () => {
    log('收到停止信号，关闭所有浏览器…');
    await Promise.all(slots.map(s => Promise.resolve(s.reset())));
    process.exit(0);
  };
}

async function main() {
  const accounts = loadAccounts();
  if (!accounts.length || !accounts[0].accessToken) throw new Error('账号文件缺 access_token');
  // 账号数 ≥ 槽数时一槽一账号（真多身份）；否则复用账号（共享额度）
  const slots = [];
  for (let i = 0; i < SLOTS; i++) {
    slots.push(new Slot(i, accounts[i % accounts.length]));
  }
  const realIdentities = Math.min(accounts.length, SLOTS);
  log(`启动 ${SLOTS} 槽（${accounts.length} 个账号 → ${realIdentities} 个真实身份）；输出目录 ${OUT_DIR}`);
  if (realIdentities < SLOTS) {
    log(`提示：只有 ${realIdentities} 个身份，其余槽共享同一额度。要真提升吞吐请提供更多 access_token。`);
  }
  process.on('SIGTERM', shutdown(slots));
  process.on('SIGINT', shutdown(slots));

  await Promise.all(slots.map(s => s.loop()));
}

main().catch(e => { log('FATAL', e.message); process.exit(1); });
