// sentinel 铸造服务（宿主侧常驻）。
//
// 为什么需要它：sentinel 铸造依赖 node（跑 sentinel SDK 算 proof），而
// codex2api 的官方镜像是 Go 的 distroless 风格镜像，**容器内没有 node**。
// 因此把铸造放在宿主（有 node 的那一侧）常驻，容器内的网关通过
// PRISM_SENTINEL_CMD 取票即可。
//
// 用法（宿主）：
//   node cmd/prism-sentinel/daemon.js            # 默认监听 127.0.0.1:8791
//   PRISM_SENTINEL_PORT=8791 node daemon.js
//
// 容器内网关配置：
//   PRISM_SENTINEL_CMD="curl -sS --max-time 60 http://host.docker.internal:8791/token"
//
// 协议：
//   GET /token   → 200 纯文本 token（铸造失败时 502 + 错误信息）
//   GET /healthz → 200 {"ok":true,...}
//
// 设计：单飞（同时只允许一个铸造在飞），带失败负缓存，避免重试风暴打爆 node。
const http = require('http');
const fs = require('fs');
const path = require('path');
const { execFile } = require('child_process');

const PORT = Number(process.env.PRISM_SENTINEL_PORT || 8791);
const HOST = process.env.PRISM_SENTINEL_HOST || '127.0.0.1';
const FLOW = process.env.PRISM_SENTINEL_FLOW || 'prism_inference';
const NODE_BIN = process.env.PRISM_NODE_BIN || process.execPath;
const ASSETS = process.env.PRISM_SENTINEL_ASSETS || '/tmp/prism_sidecar/assets';
const SDK = process.env.PRISM_SENTINEL_SDK || path.join(ASSETS, 'sdk.js');
const RUNNER = process.env.PRISM_SENTINEL_RUNNER || path.join(__dirname, 'sentinel-runner.js');
const UA = process.env.PRISM_UA ||
  'codex-tui/0.156.0 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.156.0)';
const SENTINEL_ORIGIN = 'https://sentinel.openai.com';
const SENTINEL_SV = process.env.PRISM_SENTINEL_SV || '20260219f9f6';
const CHALLENGE_URL = `${SENTINEL_ORIGIN}/backend-api/sentinel/req`;

function log(...a) { console.error(`[sentinel ${new Date().toISOString().slice(11, 19)}]`, ...a); }

// 单飞已废弃：见 acquireToken（并发调用必须各自拿到**不同**的 token）。
// 保留变量名以便逐步迁移，但不再用于去重。
let inflight = null;

function uuid4() {
  return 'xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx'.replace(/[xy]/g, c => {
    const r = Math.random() * 16 | 0;
    return (c === 'x' ? r : (r & 0x3 | 0x8)).toString(16);
  });
}

// 等沙箱/资源就绪不需要；这里只需 sdk.js 存在。
function assertAssets() {
  if (!fs.existsSync(RUNNER)) throw new Error(`缺 runner: ${RUNNER}`);
  if (!fs.existsSync(SDK)) throw new Error(`缺 sdk.js: ${SDK}（先下载 sentinel SDK）`);
}

// 起 node runner 算 proof → 换 challenge → 得 token。
function mint() {
  return new Promise((resolve, reject) => {
    try { assertAssets(); } catch (e) { return reject(e); }
    const deviceId = uuid4();
    const args = [RUNNER, '--challenge-stdin',
      '--flow', FLOW, '--device-id', deviceId,
      '--page-url', 'https://prism.openai.com/',
      '--user-agent', UA, '--sdk', SDK,
      '--script-src', `${SENTINEL_ORIGIN}/sentinel/${SENTINEL_SV}/sdk.js`,
      '--width', '1920', '--height', '1080', '--cores', '32',
      '--language', 'zh-CN', '--languages', 'zh-CN,zh,en-US,en', '--no-cookie'];

    const proc = execFile(NODE_BIN, args,
      { env: { ...process.env, SENTINEL_CONFIG: '__none__' }, maxBuffer: 8 << 20 },
      () => {});
    const watchdog = setTimeout(() => { try { proc.kill(); } catch (_) {} }, 55_000);

    let out = '';
    proc.stdout.on('data', d => { out += d.toString(); });
    proc.on('error', e => { clearTimeout(watchdog); reject(e); });

    let done = false;
    proc.stdout.on('data', async () => {
      if (done) return;
      const lines = out.split('\n').filter(Boolean);
      let proof = null;
      for (const l of lines) {
        try { const m = JSON.parse(l); if (m.type === 'proof' && m.proof) proof = m; } catch (_) {}
      }
      if (!proof) return;
      done = true;
      try {
        const resp = await fetch(CHALLENGE_URL, {
          method: 'POST',
          headers: {
            'User-Agent': UA, 'Accept': '*/*',
            'Content-Type': 'text/plain;charset=UTF-8',
            'Origin': SENTINEL_ORIGIN,
            'Referer': `${SENTINEL_ORIGIN}/backend-api/sentinel/frame.html?sv=${SENTINEL_SV}`,
          },
          body: JSON.stringify({ p: proof.proof, id: deviceId, flow: FLOW }),
        });
        if (!resp.ok) throw new Error(`sentinel/req HTTP ${resp.status}`);
        const challenge = await resp.json();
        if (!challenge.token) throw new Error('sentinel/req 缺 challenge token');
        proc.stdin.write(JSON.stringify({ type: 'challenge', challenge }) + '\n');
        proc.stdin.end();
      } catch (e) {
        clearTimeout(watchdog);
        try { proc.kill(); } catch (_) {}
        reject(e);
      }
    });

    let tail = '';
    proc.stdout.on('data', d => { tail += d.toString(); });
    proc.on('exit', () => {
      clearTimeout(watchdog);
      for (const l of tail.split('\n')) {
        try {
          const m = JSON.parse(l);
          if (m.type === 'token' && m.token) return resolve(m.token);
        } catch (_) {}
      }
      reject(new Error('runner 未产出 sentinel token'));
    });
  });
}

// ⚠️ 并发语义（实测钉死）：sentinel token **严格一次性**。
// 若多个并发请求拿到**同一枚** token，只有第一个能用，其余会
// 「Request verification failed」→ 403。
//
// 因此这里**不做单飞去重**：每次 HTTP 请求都必须得到一枚**全新的** token。
// 调度逻辑（退避 / 并发闸门 / 计数）在 sentinel-core.js，便于单测覆盖；
// 本文件只负责真实 mint（起 node runner 算 proof）与 HTTP 服务。
const { createSentinel } = require('./sentinel-core.js');

const MAX_CONCURRENT_MINT = Math.max(1, Number(process.env.PRISM_SENTINEL_CONCURRENCY || 4));
const FAIL_BACKOFF_MS = 30_000;

const sentinel = createSentinel({
  mint,
  maxConcurrent: MAX_CONCURRENT_MINT,
  backoffMs: FAIL_BACKOFF_MS,
  log: log,
});

const acquireToken = sentinel.acquireToken;

const server = http.createServer(async (req, res) => {
  const url = (req.url || '').split('?')[0];
  if (url === '/healthz') {
    res.writeHead(200, { 'Content-Type': 'application/json' });
    res.end(JSON.stringify({ ok: true, ...sentinel.stats(), flow: FLOW }));
    return;
  }
  if (url === '/token') {
    try {
      const tok = await acquireToken();
      res.writeHead(200, { 'Content-Type': 'text/plain' });
      res.end(tok);
    } catch (e) {
      res.writeHead(502, { 'Content-Type': 'text/plain' });
      res.end('sentinel mint failed: ' + e.message);
    }
    return;
  }
  res.writeHead(404, { 'Content-Type': 'text/plain' });
  res.end('not found');
});

server.listen(PORT, HOST, () => log(`监听 http://${HOST}:${PORT}  (runner=${RUNNER})`));
