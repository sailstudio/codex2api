// prism-worker：**常驻会话工作池**。
//
// 为什么需要它（本轮实测结论）：
//   材料（sandbox_token + 会话快照）的**有效窗口只有 ~20s**，远短于
//   侧车 TTL(120s)。而"造一份材料"要启动浏览器、等沙箱、发消息并拦截，
//   耗时 ~20-48s —— 也就是说**材料造出来就已经过期了**，无法靠"批量造
//   材料再并发"达到 15 路。
//
// 正确做法：把**浏览器上下文常驻**（沙箱会话不过期），按需在里面发消息。
//   一个常驻上下文 = 一个项目 = 一个可持续使用的会话身份。
//   N 个常驻上下文 → N 路真并发（单账号即可）。
//
// HTTP 接口：
//   GET  /healthz        池状态（每槽是否忙、累计成功/失败）
//   POST /ask            {"prompt":"...","slot":<可选指定槽>}
//                        → {"ok":true,"text":"...","slot":N,"ms":1234}
//   POST /shutdown       优雅退出
//
// 环境变量：
//   PRISM_SLOTS       常驻槽数（默认 15）
//   PRISM_PROJ_LIST   项目 id 的 JSON 文件（默认 /tmp/prism_sidecar/proj15.json）
//   PRISM_WORKER_PORT 监听端口（默认 8792）
//   PRISM_UI_PROXY    浏览器代理（默认 http://127.0.0.1:7890）
//   PRISM_MIN_GAP_MS   相邻请求最小间隔（默认 0）——平滑放行，避免突发触发上游冷却
//   PRISM_MAX_INFLIGHT 单账号「在飞」上限（默认 4）——实测上游只放行 ~4 路并发，
//                      超出的会立刻 403 "Error while processing conversation"。
//                      因此 15 槽必须排在此闸门之后，而不是同时打进。
const fs = require('fs');
const http = require('http');

function loadPlaywright() {
  const cands = ['playwright', process.env.PRISM_PLAYWRIGHT_DIR,
    '/tmp/prism_sidecar/node_modules/playwright', '/usr/local/lib/node_modules/playwright'].filter(Boolean);
  let last;
  for (const c of cands) { try { return require(c); } catch (e) { last = e; } }
  throw new Error('找不到 playwright: ' + last.message);
}
const { chromium } = loadPlaywright();

const BASE = process.env.PRISM_BASE || 'https://prism.openai.com';
const PROXY = process.env.PRISM_UI_PROXY || 'http://127.0.0.1:7890';
const UA = 'codex-tui/0.156.0 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.156.0)';
const PORT = Number(process.env.PRISM_WORKER_PORT || 8792);
const NSLOT = Number(process.env.PRISM_SLOTS || 15);
const LIST = process.env.PRISM_PROJ_LIST || '/tmp/prism_sidecar/proj15.json';
const ACCT = process.env.PRISM_ACCOUNT || '/tmp/cpa_account.json';
const ACCT_RAW = JSON.parse(fs.readFileSync(ACCT, 'utf8'));
const A = Array.isArray(ACCT_RAW) ? ACCT_RAW[0] : ACCT_RAW;

// extractNewReply 只从「本次新增」的文本里取回复。
// 为什么必须这样：常驻会话的页面上**保留历史消息**，若直接在全文里找 prompt，
// 会把上一轮的回复当成本次回复 —— 造成**假成功**（实测踩过）。
function extractNewReply(base, now, prompt) {
  let inc = now || '';
  if (base && inc.startsWith(base)) inc = inc.slice(base.length);
  const lines = inc.split('\n').map(s => s.trim()).filter(Boolean)
    .filter(l => l !== prompt && !/Error while processing|submit prompt again/i.test(l));
  return lines.length ? lines.join(' ').slice(0, 2000) : '';
}

// ── 槽：一个常驻浏览器上下文 = 一个项目 = 一路会话 ──────────────
class Worker {
  constructor(id, project) {
    this.id = id;
    this.project = project;
    this.busy = false;
    this.ok = 0;
    this.fail = 0;
    this.ctx = null;
    this.page = null;
    this.ready = false;
    this.net = [];        // 最近一次交互的网络响应（用于定性 403 真因）
  }

  async init(browser) {
    this.ctx = await browser.newContext({
      userAgent: UA, viewport: { width: 1400, height: 900 }, locale: 'zh-CN',
    });
    await this.ctx.addCookies([
      { name: 'prism_oai_access_token', value: A.access_token, domain: 'prism.openai.com', path: '/' },
      { name: 'oai-sc', value: A.access_token, domain: 'prism.openai.com', path: '/' },
    ]);
    // 在页面里挂钩 fetch/XHR —— 应用看到的 403/限流真相只有这一层最可靠
    // （顶层 response 事件捕不到：错误可能在 200 里以 payload 形式返回）。
    await this.ctx.addInitScript(() => {
      window.__net = [];
      const rec = (u, status, body, extra) => {
        try {
          window.__net.push({ url: String(u).slice(0, 200), status, body: String(body || '').slice(0, 1500),
                              extra: extra || '', t: Date.now() });
          if (window.__net.length > 40) window.__net.shift();
        } catch (_) {}
      };
      const of = window.fetch;
      window.fetch = async function (...a) {
        const u = (a[0] && a[0].url) || a[0];
        try {
          const r = await of.apply(this, a);
          let b = '';
          try { b = await r.clone().text(); } catch (_) {}
          if (r.status >= 400 || /error|rate|limit/i.test(b)) rec(u, r.status, b, 'fetch');
          return r;
        } catch (e) { rec(u, 0, 'fetch throw: ' + e.message, 'fetch'); throw e; }
      };
      const oo = XMLHttpRequest.prototype.open;
      const os = XMLHttpRequest.prototype.send;
      XMLHttpRequest.prototype.open = function (m, u, ...r) { this.__u = u; return oo.call(this, m, u, ...r); };
      XMLHttpRequest.prototype.send = function (...a) {
        this.addEventListener('loadend', () => {
          let b = ''; try { b = this.responseText; } catch (_) {}
          if (this.status >= 400 || /error|rate|limit/i.test(b)) rec(this.__u, this.status, b, 'xhr');
        });
        return os.apply(this, a);
      };
    });

    this.page = await this.ctx.newPage();
    // 抓上游真实响应：403 的**原因**只有 API 层才说得清（UI 只显示笼统文案）。
    this.page.on('response', async r => {
      const st = r.status();
      if (st < 400) return;                       // 只关心失败（403 真因在此）
      const u = r.url();
      const rec = { url: u.replace(BASE, ''), status: st, t: Date.now() };
      try {
        const ct = (r.headers()['content-type'] || '');
        if (/json|text/.test(ct)) {
          rec.body = (await r.text()).slice(0, 1200);
        } else {
          rec.body = '(非文本 ' + ct + ')';
        }
        rec.retryAfter = r.headers()['retry-after'] || '';
        rec.ratelimit = Object.keys(r.headers()).filter(k => /ratelimit|retry/i.test(k))
          .map(k => k + '=' + r.headers()[k]).join(' ');
      } catch (e) { rec.body = '(读取失败 ' + e.message + ')'; }
      this.net.push(rec);
      if (this.net.length > 20) this.net.shift();
    });
    this.page.on('requestfailed', r => {
      this.net.push({ url: r.url().replace(BASE, ''), status: 0,
                      body: 'requestfailed: ' + (r.failure()?.errorText || '?'), t: Date.now() });
    });
    await this.page.goto(`${BASE}/?u=${this.project}&pg=1`, { waitUntil: 'domcontentloaded', timeout: 90000 });
    for (let i = 0; i < 60; i++) {
      await this.page.waitForTimeout(2000);
      const t = await this.page.evaluate(() => document.body.innerText).catch(() => '');
      if ((await this.page.locator('textarea').count()) > 0 && !/正在初始化|正在准备中/.test(t)) {
        this.ready = true;
        break;
      }
    }
    if (!this.ready) throw new Error(`槽${this.id} 沙箱未就绪`);
  }

  // ask：在该常驻会话里发一条消息并等回复。
  //
  // 上游错误文案是 "Please submit prompt again" —— 明确提示**可重试**，
  // 因此对 403 "Error while processing conversation" 做同槽重试（退避后）。
  async ask(prompt, tries = (Number(process.env.PRISM_WORKER_TRIES) || 3)) {
    if (!this.ready) throw new Error(`槽${this.id} 未就绪`);
    let lastErr;
    for (let a = 0; a < tries; a++) {
      if (a > 0) await new Promise(r => setTimeout(r, 1200 * a)); // 线性退避
      try {
        return await this._askOnce(prompt);
      } catch (e) {
        lastErr = e;
        if (!/submit prompt again|403|Error while processing/i.test(e.message)) throw e;
      }
    }
    this.fail++;
    throw lastErr;
  }

  async _askOnce(prompt) {
    const t0 = Date.now();
    this.net = [];
    const ta = this.page.locator('textarea').first();
    await ta.waitFor({ state: 'visible', timeout: 25000 });
    await ta.click({ timeout: 25000 });
    // 发送前的页面基线：只接受「新增」文本作为回复（防历史误判）
    const base = await this.page.evaluate(() => document.body.innerText).catch(() => '');
    await this.page.evaluate(() => { window.__net = []; }).catch(() => {});

    await ta.fill(prompt);
    await this.page.waitForTimeout(250);
    await ta.press('Enter');

    let lastApiErr = '';
    for (let i = 0; i < 40; i++) {
      await this.page.waitForTimeout(1200);
      const appNet = await this.page.evaluate(() => window.__net || []).catch(() => []);

      // ── 权威判据：应用层 API 响应 ───────────────────────────
      // 上游把错误藏在 **HTTP 200 + 内层 payload** 里：
      //   {"response":{"status":"error","payload":{"httpStatus":403,"message":"..."}}}
      // 只看页面文案会被误导，只有这一层可靠。
      const starts = appNet.filter(r => /response_with_tools_start/.test(r.url || ''));
      const last = starts[starts.length - 1];
      let apiOK = false;
      if (last && last.body) {
        const b = last.body;
        if (/"httpStatus"\s*:\s*403|Error while processing conversation/i.test(b)) {
          lastApiErr = b.replace(/\s+/g, ' ').slice(0, 400);
          this.net = appNet;
          throw new Error('上游错误(403): ' + lastApiErr);
        }
        if (/"status"\s*:\s*"completed"/.test(b) && !/"httpStatus"\s*:\s*\d{3}/.test(b)) {
          apiOK = true;
        }
      }

      const txt = await this.page.evaluate(() => document.body.innerText);
      if (/Error while processing conversation|submit prompt again/i.test(txt)) {
        this.net = appNet;
        throw new Error('上游错误(403): ' + (lastApiErr || txt.replace(/\s+/g, ' ').slice(-300)));
      }
      if (apiOK) {
        const reply = extractNewReply(base, txt, prompt);
        if (reply) { this.ok++; return { text: reply, ms: Date.now() - t0 }; }
      }
    }
    throw new Error('等待回复超时' + (lastApiErr ? '（' + lastApiErr + '）' : ''));
  }

  async close() { try { await this.ctx?.close(); } catch (_) {} }
}

// ── 单账号「在飞」闸门 ────────────────────────────────────────
// 实测：15 路同时打进，上游只放行 4 路（其余 ~1.9s 秒拒 403）；
// 换成不同槽、不同项目、不同材料结果都一样 → 瓶颈是**账号级并发闸门**。
// 因此工作池必须自己限流：让 15 个槽**排队**通过闸门，而不是同时发起。
const MAX_INFLIGHT = Number(process.env.PRISM_MAX_INFLIGHT || 4);
const MIN_GAP_MS = Number(process.env.PRISM_MIN_GAP_MS || 0);
let inflight = 0;
const permitWaiters = [];

// pace：相邻名额发放至少相隔 MIN_GAP_MS（串行化，防止同时放行）。
let lastGrant = 0;
let paceChain = Promise.resolve();
function pace() {
  if (MIN_GAP_MS <= 0) return Promise.resolve();
  paceChain = paceChain.then(async () => {
    const wait = MIN_GAP_MS - (Date.now() - lastGrant);
    if (wait > 0) await new Promise(r => setTimeout(r, wait));
    lastGrant = Date.now();
  });
  return paceChain;
}

async function acquirePermit() {
  await pace();
  if (inflight < MAX_INFLIGHT) { inflight++; return; }
  return new Promise(res => permitWaiters.push(res));
}
function releasePermit() {
  const nxt = permitWaiters.shift();
  if (nxt) { nxt(); return; }        // 名额直接转交，保持 inflight 不变
  inflight = Math.max(0, inflight - 1);
}

// ── 工作池 ────────────────────────────────────────────────────
const workers = [];
let rr = 0;

async function acquire() {
  // 优先空闲且就绪的槽，轮转摊平
  for (let k = 0; k < workers.length; k++) {
    const w = workers[(rr + k) % workers.length];
    if (!w.busy && w.ready) { rr = (rr + k + 1) % workers.length; return w; }
  }
  return null; // 全忙
}

(async () => {
  const projects = JSON.parse(fs.readFileSync(LIST, 'utf8')).slice(0, NSLOT);
  console.log(`[worker] 启动 ${projects.length} 个常驻槽（端口 ${PORT}）`);
  const browser = await chromium.launch({
    headless: true, channel: 'chrome',
    proxy: { server: PROXY }, args: ['--no-sandbox'],
  });

  // 分批初始化（避免同时开太多浏览器页把机器打满）
  const CONC = Number(process.env.PRISM_INIT_CONC || 4);
  for (let i = 0; i < projects.length; i += CONC) {
    const batch = projects.slice(i, i + CONC);
    const created = batch.map((p, j) => new Worker(i + j, p));
    workers.push(...created);
    await Promise.all(created.map(async w => {
      try { await w.init(browser); console.log(`  ✓ 槽${w.id} 就绪 proj=${w.project.slice(0, 8)}`); }
      catch (e) { console.log(`  ✗ 槽${w.id} 初始化失败: ${e.message.slice(0, 80)}`); }
    }));
  }
  console.log(`[worker] 就绪 ${workers.filter(w => w.ready).length}/${workers.length}`);

  const server = http.createServer(async (req, res) => {
    const send = (code, obj) => {
      res.writeHead(code, { 'content-type': 'application/json' });
      res.end(JSON.stringify(obj));
    };
    if (req.url === '/healthz') {
      return send(200, {
        ok: true, slots: workers.length, ready: workers.filter(w => w.ready).length,
        busy: workers.filter(w => w.busy).length,
        inflight, maxInflight: MAX_INFLIGHT, minGapMs: MIN_GAP_MS, queued: permitWaiters.length,
        detail: workers.map(w => ({ id: w.id, proj: w.project.slice(0, 8), ready: w.ready, busy: w.busy, ok: w.ok, fail: w.fail })),
      });
    }
    if (req.url === '/shutdown' && req.method === 'POST') {
      send(200, { ok: true });
      for (const w of workers) await w.close();
      await browser.close();
      process.exit(0);
    }
    if (req.url === '/ask' && req.method === 'POST') {
      let body = '';
      req.on('data', d => { body += d; });
      req.on('end', async () => {
        let prompt = '只回复：OK', wantSlot = null;
        try { const j = JSON.parse(body || '{}'); prompt = j.prompt || prompt; wantSlot = j.slot; } catch (_) {}
        // 先过单账号在飞闸门（排队），再挑空闲槽 —— 两道闸门顺序不可颠倒
        await acquirePermit();
        let w = null;
        try {
          for (let i = 0; i < 120 && !w; i++) {   // 等一个空闲槽（最多 ~60s）
            w = wantSlot != null ? workers.find(x => x.id === wantSlot && !x.busy)
                                 : await acquire();
            if (!w) await new Promise(r => setTimeout(r, 500));
          }
          if (!w) return send(503, { ok: false, error: '全部槽忙' });
          w.busy = true;
          const r = await w.ask(prompt);
          send(200, { ok: true, text: r.text, slot: w.id, ms: r.ms, inflight: MAX_INFLIGHT });
        } catch (e) {
          send(500, { ok: false, slot: w ? w.id : -1, error: e.message.slice(0, 200),
                      net: (w && w.net || []).slice(-6) });
        } finally {
          if (w) w.busy = false;
          releasePermit();
        }
      });
      return;
    }
    send(404, { ok: false, error: 'not found' });
  });
  server.listen(PORT, '127.0.0.1', () => console.log(`[worker] 监听 127.0.0.1:${PORT}`));

  process.on('SIGTERM', async () => {
    console.log('[worker] 收到停止信号');
    for (const w of workers) await w.close();
    await browser.close();
    process.exit(0);
  });
})().catch(e => { console.error('[worker] FATAL', e.message); process.exit(1); });
