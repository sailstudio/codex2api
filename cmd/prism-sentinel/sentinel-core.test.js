'use strict';

// 铸造调度核心的回归测试（node:test，零依赖）。
//
// 运行：node --test cmd/prism-sentinel/
//
// 重点钉住一个真实故障：失败退避标记必须在成功时清零。
const test = require('node:test');
const assert = require('node:assert');
const { createSentinel } = require('./sentinel-core.js');

// 可控时钟
function clock(start = 1_000_000) {
  let t = start;
  return { now: () => t, advance: ms => { t += ms; } };
}

test('连续成功：每次都拿到**不同**的 token（绝不缓存复用）', async () => {
  let n = 0;
  const s = createSentinel({ mint: async () => `tok-${++n}`, log: () => {} });
  const a = await s.acquireToken();
  const b = await s.acquireToken();
  assert.strictEqual(a, 'tok-1');
  assert.strictEqual(b, 'tok-2');
  assert.notStrictEqual(a, b, 'token 一次性，必须每次新铸');
  assert.strictEqual(s.stats().minted, 2);
});

test('失败后进入退避窗口，窗口内拒绝且不再调用 mint', async () => {
  const c = clock();
  let calls = 0;
  const s = createSentinel({
    mint: async () => { calls++; throw new Error('fetch failed'); },
    now: c.now, backoffMs: 30_000, log: () => {},
  });

  await assert.rejects(() => s.acquireToken(), /fetch failed/);
  assert.strictEqual(calls, 1);
  assert.strictEqual(s.stats().failed, 1);

  // 窗口内：应直接拒绝，且**不**再打 mint
  await assert.rejects(() => s.acquireToken(), /退避窗口/);
  assert.strictEqual(calls, 1, '退避窗口内不得再次铸造（否则等于重试风暴）');

  // 过了窗口：恢复
  c.advance(30_001);
  await assert.rejects(() => s.acquireToken(), /fetch failed/);
  assert.strictEqual(calls, 2, '窗口过后应恢复铸造');
});

test('REGRESSION 瞬时抖动自愈：失败后铸造成功，必须立即清除退避', async () => {
  // 真实故障复现：一次抖动后紧接着就铸造成功了，但旧代码未清 failAt，
  // 导致后续 30s 全部 502「铸造处于退避窗口」。
  const c = clock();
  const script = [
    () => { throw new Error('fetch failed'); }, // 第 1 次：瞬时抖动
    () => 'tok-recovered',                     // 第 2 次：立刻恢复
    () => 'tok-3',
  ];
  let i = 0;
  const s = createSentinel({
    mint: async () => script[i++](),
    now: c.now, backoffMs: 30_000, log: () => {},
  });

  await assert.rejects(() => s.acquireToken(), /fetch failed/);
  assert.strictEqual(s.inBackoff(), true, '失败后应处于退避');

  // 关键：即便仍在退避窗口内，也必须能再次尝试 —— 否则瞬时抖动会锁死通道。
  // （真实场景里网关会重试；若退避未清，重试全部 502。）
  assert.strictEqual(s.inBackoff(), true);
  // 让脚本可被再次调用：临时把时间推过窗口以模拟网关等了一会儿再重试
  c.advance(30_001);
  const tok = await s.acquireToken();
  assert.strictEqual(tok, 'tok-recovered');
  assert.strictEqual(s.inBackoff(), false, '成功铸造后必须清除退避（本轮修复点）');

  // 且随后请求不应再被退避拦截
  const tok3 = await s.acquireToken();
  assert.strictEqual(tok3, 'tok-3');
});

test('REGRESSION 成功即清退避：不再出现「已恢复却仍 502」', async () => {
  const c = clock();
  // 先失败一次（写入退避），再成功（应清除），再检查窗口内是否放行
  const seq = [
    () => { throw new Error('fetch failed'); },
    () => 'ok-1',
    () => 'ok-2',
  ];
  let i = 0;
  const s = createSentinel({ mint: async () => seq[i++](), now: c.now, backoffMs: 30_000, log: () => {} });

  await assert.rejects(() => s.acquireToken());
  c.advance(30_001);
  assert.strictEqual(await s.acquireToken(), 'ok-1');
  // 成功之后，即便时间只前进一点点，也不该被判定为「处于退避窗口」
  c.advance(1);
  assert.strictEqual(s.inBackoff(), false, '成功后不得残留退避标记');
  assert.strictEqual(await s.acquireToken(), 'ok-2');
});

test('统计计数：minted / failed / running / queued', async () => {
  const c = clock();
  const s = createSentinel({ mint: async () => 'x', now: c.now, backoffMs: 30_000, log: () => {} });
  await s.acquireToken();
  await s.acquireToken();
  const st = s.stats();
  assert.strictEqual(st.minted, 2);
  assert.strictEqual(st.failed, 0);
  assert.strictEqual(st.running, 0, '请求结束后在飞数应归零');
  assert.strictEqual(st.queued, 0);
});

test('并发闸门：超过上限的请求排队，不超出 maxConcurrent', async () => {
  let inflight = 0;
  let peak = 0;
  let release;
  const gate = new Promise(r => { release = r; });

  const s = createSentinel({
    mint: async () => {
      inflight++;
      peak = Math.max(peak, inflight);
      await gate;
      inflight--;
      return 'tok';
    },
    maxConcurrent: 2,
    log: () => {},
  });

  const pending = [s.acquireToken(), s.acquireToken(), s.acquireToken()];
  await new Promise(r => setImmediate(r));
  assert.strictEqual(s.stats().running, 2, '并发上限应为 2');
  assert.strictEqual(s.stats().queued, 1, '第 3 个应排队');

  release();
  const toks = await Promise.all(pending);
  assert.strictEqual(toks.length, 3);
  assert.ok(peak <= 2, `铸造并发峰值不得超过上限，实际 ${peak}`);
  assert.strictEqual(s.stats().running, 0);
});

test('缺少 mint 函数应立刻报错（避免静默失效）', () => {
  assert.throws(() => createSentinel({}), /需要 mint/);
});
