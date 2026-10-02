# prism-gateway 已知约束与限流治理

> 本轮（403 根因排查 + 限流治理）在 `prism-gateway` 上做出的修改，以及**必须交代的约束**。

## 1. ⚠️ 硬约束：本 module 是**零第三方依赖**，自身无法做 TLS 指纹伪装

```
prism-gateway/go.mod:
    module prism-gateway
    go 1.24          ← 只有 module 与 go 两行，没有任何 require
```

上游（Cloudflare）按 **JA3/JA4** 判定客户端是否为浏览器，并把 `cf_clearance`
绑定到 **IP + UA + TLS 指纹**。若出站用 Go 原生指纹，就与材料里的 `cf_clearance`
不匹配 → **403**。

**本 module 无法自己解决这点**（uTLS 是第三方库，装进来就破坏零依赖约束）。

### 因此：用「注入器」把这件事交给宿主程序

```go
tc := prism.DefaultTransportConfig()
tc.Profile = prism.ProfileChrome                 // 显式要求 Chrome 指纹

pool := prism.NewPool(accounts, prism.PoolOptions{
    Client: prism.Options{
        Origin:      cfg.Origin,
        Transport:   tc,
        Fingerprint: myUTLSInjector,             // ← 宿主注入（如 codex2api 的
    },                                           //    proxy.NewUTLSTransport）
})
```

- **明文上游（http://…，如 httptest）跳过指纹注入** —— 给明文套 uTLS 无法握手。
- **要求指纹但没有注入器 → `NewHTTPClient` 直接返回 error，启动时炸响**。
  绝不静默退回原生指纹：那会让每次请求都 403，形成「看起来在跑、实际全废」
  的隐蔽故障。
- `Profile` 留空或 `ProfileGo` = 原生指纹（仅本地/明文场景）。

## 2. ✅ 已修：Prism 会话级 403 曾被判成「凭据失效」→ 放大上游压力

上游把这类失败**藏在 HTTP 200 的内层 payload** 里：

```json
POST /api/llm/response_with_tools_start   → HTTP 200
{"status":"completed","response":{"status":"error","payload":{
   "reason":"unknown",
   "message":"Error while processing conversation (403 Forbidden). Please submit prompt again.",
   "httpStatus":403}}}
```

- **只看 HTTP 状态码会漏判成「成功空正文」。**
- 语义是**账号级限流（带冷却期）**，不是凭据失效。

**修复前**：落入 `KindAuth`（`Swit=true`）→ 触发「换号重试」。
换号**绕不开账号级限流**（新身份同样受限），而每次重试都再打一次上游 ——
把 N 路请求放大成 **N×attempts** 次调用，等于自己烧配额、把账号推入更深冷却。

**修复后**：

| Kind | Cool | Swit | 语义 |
|---|---|---|---|
| `KindQuota` | ✓ | **✗** | 账号级限流（冷却退避，换号无收益）|

判据：`processing conversation (403` 或 `(403 forbidden)`（大小写不敏感）。
**反向保证**：普通 403（如 `start` 返回 403 Forbidden）**仍**判 `KindAuth` 且允许换号
—— 真实凭据失效不该被误伤。

> 顺带修正一处歧义：`Classify` 里原 `KindBroken` 分支含
> `"please submit prompt again"`，会让 Prism 会话 403 落到「上游劣化」，故新分支
> **必须前置**；并把泛化的 `error while processing conversation` 收窄为带 `403` 的形态，
> 以免吞掉 504/500 的同族文案（既有用例已钉死 504 属上游劣化）。

## 3. 回归测试（新增 5 用例，全绿）

| 用例 | 断言 |
|---|---|
| `TestClassify_PrismSession403NotAuth` | ⭐ Prism 会话 403 → `KindQuota`（Cool ✓ / **Swit ✗**）|
| `TestClassify_Plain403StillAuth` | 反向：普通 403 仍 `KindAuth` 且允许换号 |
| `TestNewHTTPClient_FingerprintRequestedButMissing` | ⭐ 请求指纹但无实现 → **报错**，不静默退回 |
| `TestNewHTTPClient_PlaintextSkipsFingerprint` | 明文上游跳过指纹注入 |
| `TestNewHTTPClient_Injected` | 指纹注入器被真正调用（防接线断开）|

```
go test ./...        # 61 用例全绿
gofmt -l internal/ cmd/   # 干净
```

## 4. 与 `codex2api/proxy/prism` 的关系

两个实现共享同一套治理思路，但**修复点各自独立**：

| | `codex2api/proxy/prism` | `prism-gateway` |
|---|---|---|
| TLS 指纹 | ✅ 已注入（`proxy.NewUTLSTransport`，走 `utls.HelloChrome_Auto`）| ⚠️ 靠宿主注入（本 module 零依赖）|
| 换槽/换号重试放大 | ✅ 限流型错误不换槽（`PRISM_SLOT_ATTEMPTS`）| ✅ 限流型 403 不换号（`Swit=false`）|
| 在飞闸门 / 平滑放行 / 熔断退避 | ✅ `PRISM_MAX_INFLIGHT`/`MIN_GAP_MS`/`COOLDOWN_MS` | 由 `Pool.Do` + `Breaker` 承担 |
| 构建注意 | ⚠️ 必须 `-tags http2legacy` | 无特殊要求 |
