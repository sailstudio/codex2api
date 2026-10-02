'use strict';
// 材料侧车（cmd/prism-material）的纯逻辑内核。
//
// 为什么单独抽出：daemon.js 里浏览器/网络/定时器全都写死，无法做单元测试；
// 而正是这些「判定类」逻辑出过事故（会话未复位 → 材料前缀无限膨胀）。
// 这里只保留**无副作用**的判定与变换，依赖由调用方注入，便于 node:test 回归。
//
// 抽出的六件事：
//   1. 刷新周期换算（秒 → 毫秒，带下限）
//   2. 退避增长（首次 base，之后翻倍封顶）
//   3. 是否属于「浏览器级故障」（决定是否重建会话）
//   4. 账号解析与校验
//   5. 旧材料里取 project_id（保持身份一致）
//   6. 捕获结果归一化 + 膨胀检测（bug 回归网）

const MIN_REFRESH_SECONDS = 30;   // 更密会浪费沙箱额度（每次加载铸新沙箱）
const BACKOFF_BASE_MS = 5000;
const BACKOFF_MAX_MS = 60000;

// 材料膨胀阈值：正常一轮是 3 条 / ~12.5KB。
// 会话未复位时 input 会不断追加历史，实测会涨到 67 条 / 23KB。
const INPUT_ITEM_LIMIT = 40;
const INPUT_CHAR_LIMIT = 24000;

// 1. 刷新周期：秒 → 毫秒，下限 30s。
function resolveRefreshMs(envValue, fallbackSeconds = 75) {
  const raw = envValue != null && envValue !== '' ? envValue : fallbackSeconds;
  const n = Number(raw);
  const seconds = Number.isFinite(n) ? n : fallbackSeconds;
  return Math.max(MIN_REFRESH_SECONDS, seconds) * 1000;
}

// 2. 退避：首次给 base，之后逐次翻倍，封顶 cap。
function nextBackoff(prevMs, opts = {}) {
  const base = opts.base != null ? opts.base : BACKOFF_BASE_MS;
  const cap = opts.cap != null ? opts.cap : BACKOFF_MAX_MS;
  if (!Number.isFinite(prevMs) || prevMs < base) return base;
  return Math.min(prevMs * 2, cap);
}

// 3. 浏览器/页面级故障 → 需要彻底重建会话（而非只等退避）。
const REBUILD_PATTERNS = [
  /Target closed/i,
  /crash/i,
  /Protocol error/i,
  /browser has been closed/i,
];
function shouldRebuildSession(err) {
  const msg = err && err.message ? String(err.message) : String(err == null ? '' : err);
  return REBUILD_PATTERNS.some(re => re.test(msg));
}

// 4. 账号解析：支持「数组」或「单对象」两种形态；缺 access_token 直接抛。
function parseAccount(raw, opts = {}) {
  const arr = typeof raw === 'string' ? JSON.parse(raw) : raw;
  const a = Array.isArray(arr) ? arr[0] : arr;
  if (!a || !a.access_token) throw new Error('账号 JSON 缺 access_token');
  return { accessToken: a.access_token, projectID: opts.projectID || null };
}

// 5. 从旧材料里取 project_id（保持身份一致；坏文件视为无）。
function extractProject(prevMaterial) {
  try {
    const prev = typeof prevMaterial === 'string' ? JSON.parse(prevMaterial) : prevMaterial;
    return prev && prev.project_id ? prev.project_id : null;
  } catch (_) {
    return null;
  }
}

// 6a. 膨胀检测（⭐ bug 回归网）：会话没复位时 input 会累积历史消息。
function detectBloat(input, opts = {}) {
  const items = opts.items != null ? opts.items : INPUT_ITEM_LIMIT;
  const chars = opts.chars != null ? opts.chars : INPUT_CHAR_LIMIT;
  const n = Array.isArray(input) ? input.length : 0;
  let len = 0;
  if (typeof input === 'string') len = input.length;
  else len = JSON.stringify(input == null ? '' : input).length;
  const reasons = [];
  if (n > items) reasons.push(`input 条目 ${n} > ${items}`);
  if (len > chars) reasons.push(`input 长度 ${len} > ${chars}`);
  return { bloated: reasons.length > 0, reasons, items: n, chars: len };
}

// 6b. 捕获结果归一化 → 落盘材料。
function buildMaterial(captured, projectId, nowIso) {
  if (!captured || !captured.postData) throw new Error('captured 为空');
  const body = typeof captured.postData === 'string'
    ? JSON.parse(captured.postData)
    : captured.postData;
  return {
    captured_at: nowIso,
    project_id: projectId,
    start_url: captured.url,
    metadata: body.metadata,
    input: body.input,
    note: '由 cmd/prism-material/daemon.js 从真实页面周期捕获；metadata 必须原样复用',
  };
}

// 附加：材料新鲜度（captured_at 在 ttl 内）。
function isFresh(material, nowMs, ttlMs) {
  try {
    const m = typeof material === 'string' ? JSON.parse(material) : material;
    const t = Date.parse(m && m.captured_at);
    if (!Number.isFinite(t)) return false;
    return nowMs - t < ttlMs;
  } catch (_) {
    return false;
  }
}

module.exports = {
  MIN_REFRESH_SECONDS,
  BACKOFF_BASE_MS,
  BACKOFF_MAX_MS,
  INPUT_ITEM_LIMIT,
  INPUT_CHAR_LIMIT,
  resolveRefreshMs,
  nextBackoff,
  shouldRebuildSession,
  parseAccount,
  extractProject,
  detectBloat,
  buildMaterial,
  isFresh,
};
