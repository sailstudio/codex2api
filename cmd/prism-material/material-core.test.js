'use strict';
// 材料侧车纯逻辑的回归测试（node:test，零依赖）。
//   node --test cmd/prism-material/*.test.js
//
// 重点钉死「材料前缀无限膨胀」这个已修 bug：
// 会话没复位 → input 不断累积历史 → 材料膨胀 → 请求越来越慢甚至超时。
const test = require('node:test');
const assert = require('node:assert');
const core = require('./material-core.js');

test('resolveRefreshMs: 默认 75s，且强制下限 30s（更密会浪费沙箱额度）', () => {
  assert.strictEqual(core.resolveRefreshMs(undefined), 75000);
  assert.strictEqual(core.resolveRefreshMs(''), 75000);
  assert.strictEqual(core.resolveRefreshMs('90'), 90000);
  // 下限：即便给 5s 也必须抬到 30s
  assert.strictEqual(core.resolveRefreshMs('5'), 30000);
  assert.strictEqual(core.resolveRefreshMs('10'), 30000);
  // 非数字回退默认
  assert.strictEqual(core.resolveRefreshMs('abc'), 75000);
});

test('nextBackoff: 首次 base，之后翻倍并封顶', () => {
  assert.strictEqual(core.nextBackoff(0), 5000);
  assert.strictEqual(core.nextBackoff(undefined), 5000);
  assert.strictEqual(core.nextBackoff(5000), 10000);
  assert.strictEqual(core.nextBackoff(20000), 40000);
  // 封顶 60s（不应无限增长）
  assert.strictEqual(core.nextBackoff(40000), 60000);
  assert.strictEqual(core.nextBackoff(60000), 60000);
});

test('shouldRebuildSession: 仅浏览器/页面级故障才重建会话', () => {
  assert.strictEqual(core.shouldRebuildSession(new Error('Target closed')), true);
  assert.strictEqual(core.shouldRebuildSession(new Error('page crash')), true);
  assert.strictEqual(core.shouldRebuildSession(new Error('Protocol error (Runtime.callFunctionOn)')), true);
  assert.strictEqual(core.shouldRebuildSession(new Error('browser has been closed')), true);
  // 业务级失败：只等退避，不重建
  assert.strictEqual(core.shouldRebuildSession(new Error('未捕获到 start 请求')), false);
  assert.strictEqual(core.shouldRebuildSession(new Error('沙箱未就绪（超时）')), false);
  assert.strictEqual(core.shouldRebuildSession(null), false);
});

test('parseAccount: 支持数组/单对象两种形态；缺 access_token 抛错', () => {
  const arr = core.parseAccount(JSON.stringify([{ access_token: 'tok-A', other: 1 }]), { projectID: 'p1' });
  assert.strictEqual(arr.accessToken, 'tok-A');
  assert.strictEqual(arr.projectID, 'p1');

  const obj = core.parseAccount({ access_token: 'tok-B' });
  assert.strictEqual(obj.accessToken, 'tok-B');
  assert.strictEqual(obj.projectID, null);

  assert.throws(() => core.parseAccount({ nope: 1 }), /缺 access_token/);
  assert.throws(() => core.parseAccount([{ nope: 1 }]), /缺 access_token/);
});

test('extractProject: 从旧材料取 project_id，坏文件返回 null（不抛）', () => {
  assert.strictEqual(core.extractProject('{"project_id":"abc-123"}'), 'abc-123');
  assert.strictEqual(core.extractProject({ project_id: 'xyz' }), 'xyz');
  assert.strictEqual(core.extractProject('{"no_project":1}'), null);
  assert.strictEqual(core.extractProject('{ 坏 json'), null);
  assert.strictEqual(core.extractProject(null), null);
});

test('REGRESSION detectBloat: 正常材料（3 条）不判膨胀', () => {
  const normal = [
    { type: 'message', role: 'system' },
    { type: 'message', role: 'system' },
    { type: 'message', role: 'user', content: '预热' },
  ];
  const r = core.detectBloat(normal);
  assert.strictEqual(r.bloated, false);
  assert.strictEqual(r.items, 3);
});

test('REGRESSION detectBloat: 会话未复位累积 67 条 → 判膨胀', () => {
  // 复现 bug 现场：input 被反复追加历史（实测 67 条 / 23KB）
  const bloated = Array.from({ length: 67 }, (_, i) => ({
    type: 'message', role: i % 3 === 0 ? 'system' : 'user', content: '预热',
  }));
  const r = core.detectBloat(bloated);
  assert.strictEqual(r.bloated, true);
  assert.strictEqual(r.items, 67);
  assert.match(r.reasons.join(' '), /条目 67/);
});

test('REGRESSION detectBloat: 条目数没超但字符数畸大 → 也判膨胀', () => {
  const fat = [{ type: 'message', role: 'user', content: 'x'.repeat(30000) }];
  const r = core.detectBloat(fat);
  assert.strictEqual(r.bloated, true);
  assert.match(r.reasons.join(' '), /长度/);
});

test('buildMaterial: 归一化捕获结果，metadata 原样保留', () => {
  const captured = {
    url: 'https://prism.openai.com/api/llm/response_with_tools_start',
    postData: JSON.stringify({
      input: [{ role: 'system' }, { role: 'user', content: '预热' }],
      metadata: { model: 'gpt-5', reasoning_effort: 'high', sandbox_token: 'x' },
    }),
  };
  const m = core.buildMaterial(captured, 'proj-1', '2026-10-02T10:00:00Z');
  assert.strictEqual(m.project_id, 'proj-1');
  assert.strictEqual(m.captured_at, '2026-10-02T10:00:00Z');
  assert.strictEqual(m.metadata.model, 'gpt-5');
  assert.strictEqual(m.metadata.reasoning_effort, 'high');
  assert.strictEqual(m.input.length, 2);
  assert.ok(m.note.includes('prism-material'));
  assert.throws(() => core.buildMaterial(null, 'p', 'now'), /captured 为空/);
});

test('isFresh: 材料新鲜度（决定网关是否接受）', () => {
  const now = Date.parse('2026-10-02T10:00:00Z');
  const freshMat = { captured_at: '2026-10-02T09:59:00Z' };
  const staleMat = { captured_at: '2026-10-02T09:50:00Z' };
  assert.strictEqual(core.isFresh(freshMat, now, 120000), true);
  assert.strictEqual(core.isFresh(staleMat, now, 120000), false);
  assert.strictEqual(core.isFresh({}, now, 120000), false);
  assert.strictEqual(core.isFresh('坏 json', now, 120000), false);
});
