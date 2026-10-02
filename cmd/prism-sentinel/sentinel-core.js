'use strict';

// 铸造调度核心：失败退避 + 并发闸门 + 计数。
//
// 抽成独立模块是为了**可测**：daemon.js 负责真实 mint（起 node runner 算 proof）
// 与 HTTP 服务；本模块只做调度，把 mint / now / log 注入进来后即可在单测里
// 完整跑状态机（含时间推进），不需要真的联网或起子进程。
//
// ⚠️ 并发语义（实测钉死）：sentinel token **严格一次性**。
// 多个并发请求若拿到**同一枚** token，只有第一个能用，其余会
// 「Request verification failed」→ 403。
// 因此这里**不做单飞去重**：每次调用都必须得到一枚**全新的** token，
// 只用队列限制同时在飞的铸造数（避免 node 进程风暴打爆机器）。
//
// ⚠️ 退避语义：`lastFailAt` 必须在**成功时清零**。一次瞬时抖动
// （如 `fetch failed`）不该毒住后续全部请求 —— 曾因漏了清零，一次抖动后
// 即便紧接着铸造成功，仍会拒绝后面 30s 的所有取票，表现为整条通道
// 502「铸造处于退避窗口」，网关侧只看到「取 sentinel 失败」，排查成本极高。
// 见 sentinel-core.test.js 的回归用例。
function createSentinel(opts = {}) {
  const mint = opts.mint;
  if (typeof mint !== 'function') throw new TypeError('createSentinel 需要 mint 函数');

  const backoffMs = opts.backoffMs ?? 30_000;
  const maxConcurrent = Math.max(1, Number(opts.maxConcurrent ?? 4));
  const now = opts.now || (() => Date.now());
  const log = opts.log || (() => {});

  // null = 无退避。用 null 而非 0：0 是合法时间戳，假时钟（如从 0 起步）下
  // `now() - 0 < backoffMs` 会误判成「处于退避窗口」。
  let lastFailAt = null;
  let minted = 0;
  let failed = 0;
  let running = 0;
  const waiting = [];

  function inBackoff() {
    return lastFailAt !== null && now() - lastFailAt < backoffMs;
  }

  function acquireSlot() {
    if (running < maxConcurrent) {
      running++;
      return Promise.resolve();
    }
    return new Promise(resolve => waiting.push(resolve));
  }

  function releaseSlot() {
    const next = waiting.shift();
    if (next) {
      next(); // 名额直接转交，不递减 running
    } else {
      running--;
    }
  }

  // 每次调用都**新铸一枚**（带失败负缓存）。
  async function acquireToken() {
    if (inBackoff()) {
      throw new Error('铸造处于退避窗口（上次失败）');
    }
    await acquireSlot();
    try {
      const tok = await mint();
      minted++;
      lastFailAt = null; // 成功即清退避（回归钉死，勿删）
      log(`铸造成功 len=${tok.length}（并发 ${running}/${maxConcurrent}）`);
      return tok;
    } catch (e) {
      failed++;
      lastFailAt = now();
      log('铸造失败:', String(e && e.message).slice(0, 160));
      throw e;
    } finally {
      releaseSlot();
    }
  }

  function stats() {
    return {
      minted,
      failed,
      running,
      max_concurrent: maxConcurrent,
      queued: waiting.length,
    };
  }

  return { acquireToken, stats, inBackoff };
}

module.exports = { createSentinel };
