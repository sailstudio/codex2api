# Prism sentinel SDK（`proxy/prism/assets/sdk.js`）身份 / 来源 / 必需性 调查报告

- 调查对象：`/Users/chiptest/codex-prism/codex2api/proxy/prism/assets/sdk.js`
- 调查时间：2026-10-02（CST），网络出口新加坡（FlClash TUN）
- 调查性质：**只读**（除本报告外未修改任何代码，未执行任何 `git add/commit/push`）
- 环境：macOS 15.3.1 arm64，Go 1.27.1，node v22.22.2

---

## 0. 结论（TL;DR）

1. **身份**：这是 **OpenAI Sentinel 反滥用 / proof-of-work SDK 的浏览器端 runtime bundle**，全局导出 `window.SentinelSDK`，公开 API 为 `init` / `token(flow)` / `sessionObserverToken`。**不是** Arkose/FunCaptcha/Turnstile 的产物（`turnstile` 仅作为 challenge JSON 里的一个字段名出现，无 Cloudflare/Arkose 代码）。
2. **来源**：确切来源就是 `https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js`。本次实测该 URL **可达（HTTP/2 200）**，`Content-Length: 30864`，且 **与磁盘文件 sha256 完全一致**（`4f8ef8d5…babbb5`）→ **无版本漂移**，磁盘文件确系该 URL 原样下载所得。
3. **必需性**：**生产真实链路（本地 node 铸造路径 B）必需**；**单元测试与构建不需要**（`go build`/`go test` 不依赖它，assets/ 未被 embed、未被跟踪）。
4. **自举能力**：Go 侧 `fetchSentinelAssets` **不会**把 sdk.js 落盘——它只 GET 这些 URL 拿 sentinel 域 cookie，**响应体全部丢弃**（`_ = resp.Body.Close()`）。因此 **首次部署必须人工（或由外部铸造服务）准备 sdk.js**。
5. **该不该入库：不该。** 该路径**已经被 `.gitignore:65` 明确忽略**（并附注释「上游运行时资产（混淆 bundle），由 sidecar 下载，不入库」），当前工作区状态已与推荐方案一致。理由：无 license 头的 OpenAI 专有混淆资产 + 再分发法律风险 + URL 版本段 `20260219f9f6` 会漂移 + 上游仓库不 vendor js 的惯例。
6. **推荐方案**：保持忽略 + **补一个「自动发现 SV 并带 sha256 校验的下载脚本」** + 文档化「版本会漂移 / 可用 `PRISM_SENTINEL_SV` 覆盖」。详见 §6。

---

## 1. 身份鉴定（Q1）

### 1.1 基本形态

| 项目 | 值 |
|---|---|
| 大小 | 30864 字节 |
| sha256 | `4f8ef8d5870894fd0101fc40ff45ea13c0f8e25c71c2ba28e5df5baf98babbb5` |
| md5 | `36d2d6cbd2f3e912bb06148edb081039` |
| 行数 | 2 行（单行混淆 + 结尾换行；最长行 30861 字符） |
| `file` 判定 | Unicode text, UTF-8 text, with very long lines |
| license/copyright 头 | **无**（`copyright`=0、`license`=0） |
| 入口 | `var SentinelSDK=function(t){"use strict";…}({})` → 挂到全局 `SentinelSDK` |

### 1.2 导出符号 / 内部方法清单

**公开 API**（runner 末尾与 `/backend-api/sentinel/sdk.js` 的 stub 双向印证）：

```
SentinelSDK.init()
SentinelSDK.token(flow)              # runner 实际调用：await context.SentinelSDK.token(flow)
SentinelSDK.sessionObserverToken(...)
SentinelSDK.timing()                 # 仅 stub 中声明；bundle 尾部未显式挂载
```

**内部方法名（从字符串表中可直接读出）**：

```
getEnforcementToken / getEnforcementTokenSync
getRequirementsToken / getRequirementsTokenBlocking
_generateRequirementsTokenAnswerBlocking
_generateAnswerSync / _generateAnswerAsync / _getAnswer
_runCheck / initializeAndGatherData / buildGenerateFailMessage
startEnforcement / createPRNG / proofofwork / snapshot_dx / collector_dx
```

**功能关键字/常量**：
- 设备指纹采集：`navigator.userAgent`、`languages`/`language`、`hardwareConcurrency`、`InstallTrigger`（Firefox 探测）、`solana`（Phantom 钱包探测）、`crypto.getRandomValues`、`performance.timeOrigin`、`performance.memory.jsHeapSizeLimit`、`document.currentScript`/`src`/`getAttribute('data-build')`。
- 网络端点：`https://chatgpt.com/backend-api/sentinel/`、`/backend-api/sentinel/`、`/sentinel/`、`frame.html?sv=`、`oai-did`、`req_`。
- 协议字段：`flow`、`proofofwork`、`token`、`turnstile`、`cachedProof`、`cachedChatReq`、`cachedSOChatReq`、`requirementsSeed`、`answers`。
- 常量：`wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D`（形如静态 key / HMAC 材料）、`gAAAAAB`（**Fernet** 密文前缀，`cryptography` 库格式）、`c/[^/]*/_`（正则）、`snapshot_dx`/`session_observer_vm_timeout`。
- iframe 保护文案：`"token() should not be called from within an iframe."`、`"init() should not be called from within an iframe."`、`"sessionObserverToken() should not be called from within an iframe."`
- 附带一个 cookie 序列化小库（`; SameSite=Strict`、`option expires is invalid`、`getDefaultExportFromCjs`、`__assign`、`__rest` → tslib + tough-cookie 风格）。

### 1.3 打包器 / 混淆器判断

| 指纹 | 计数 | 结论 |
|---|---|---|
| `__webpack_require__` | 0 | 非 webpack |
| `webpackChunk` | 0 | 非 webpack |
| `System.register` | 0 | 非 SystemJS |
| `esbuild` / `rollupPlugin` | 0 | 无 esbuild/rollup 运行时标记 |
| `__esModule` | 4 | 源 bundle 是 ESM/CJS 混合，**已被混淆层抹掉打包器外壳** |
| `_0x…` | 0 | 不是 `javascript-obfuscator` 默认的十六进制变量命名预设 |
| `eval(` | 0 | 不含运行时 eval |

**判定**：**无法归因到 webpack/rollup/esbuild 任一者**（外层打包标记已被移除）。混淆特征明确属于 **`javascript-obfuscator` 家族**的经典组合拳：
- **字符串数组 + 索引打乱**：`function c(){const t=["toString","Stringified UUID is invalid","push","apply","constructor","toLowerCase"];return(c=function(){return t})()}`，以及 `for(let t=0;t<256;++t)i[e(2)]((t+256)[e(0)](16).slice(1))` 这类字符串表初始化。
- **字符串数组轮转/位移**：`r[t-=0]` 形式的取用器（`n(t,e){const r=c();return(n=function(t,n){return r[t-=0]})(t,e)}`）。
- **self-defending / 反调试**：`o.search("(((.+)+)+)+$")…[t(4)](o).search("(((.+)+)+)+$")` 是 `javascript-obfuscator` 的 self-defending 模板（用 `constructor`/`toString` 自检）。
- 保留了可读的 `const`/`=>`/模板串结构 → 说明是「正常打包结果 + obfuscator 轻/中档预设」，而非极致压缩。

**第三方痕迹排查**：`arkose`/`Arkose`/`funcaptcha`/`FunCaptcha`/`cloudflare`/`Cloudflare`/`turnstile`(小写) → 全部 0，唯 `turnstile` 出现 **2 次**且均为 **challenge JSON 的字段名**（`challenge.turnstile.dx`，见 `sentinel-runner.js:730-732` 的 `decodeDx`）。**没有任何 Cloudflare/Arkose 的代码或域名**，故 **不能** 认定含第三方验证码产品代码；`turnstile` 只是上游对 challenge 结构的命名。

**内部版本号 / 构建标识**：bundle **内不含**版本号、buildId 或模块名。唯一的版本标识是 **URL 里的 SV 段**（`20260219f9f6`），且它是**运行期由调用方注入**的（runner 的 `--script-src` / `buildId`，默认值 `prod-4987068829830ddc3ae6683bd4e633f61b79dec9` 在 `sentinel-runner.js:677`，也不在该文件里）。

---

## 2. 来源与哈希比对（Q2）

### 2.1 可达性与响应头

```
$ curl -sS -o /tmp/sdk_direct.js -D /tmp/sdk_direct.hdr --max-time 25 -A "Mozilla/5.0" \
    "https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js"      # exit=0

HTTP/2 200
date: Fri, 02 Oct 2026 09:56:03 GMT
content-type: application/javascript; charset=UTF-8
content-length: 30864
set-cookie: oai-did=081a7fda-…; Path=/; Domain=chatgpt.com; Max-Age=31104000; SameSite=Lax
set-cookie: __cflb=…; HttpOnly; SameSite=None; Secure; Path=/
set-cookie: __cf_bm=…; HttpOnly; SameSite=None; Secure; Path=/; Domain=sentinel.openai.com
set-cookie: _cfuvid=…; HttpOnly; SameSite=None; Secure; Path=/; Domain=sentinel.openai.com
cf-ray: a442d5c26e5b5c33-SIN
cf-cache-status: DYNAMIC
accept-ranges: bytes
cache-control: public, max-age=0
etag: W/"7890-1a0fa94e890"
last-modified: Fri, 02 Oct 2026 03:07:38 GMT
server: cloudflare
x-content-type-options: nosniff
strict-transport-security: max-age=31536000; includeSubDomains; preload
cross-origin-opener-policy: same-origin-allow-popups
referrer-policy: strict-origin-when-cross-origin
alt-svc: h3=":443"; ma=86400
```

要点：
- **可达**，直接 curl 即可（**无需代理**；出口 `cf-ray …-SIN` 为新加坡 PoP）。
- **无重定向**（无 `location` 头，最终 URL == 请求 URL）。
- **无 license / 无 CSP 头**（无 `content-security-policy`，也无任何许可声明）。
- `cache-control: public, max-age=0` → 每次都回源校验，**不依赖 CDN 长缓存**。
- `etag: W/"7890-1a0fa94e890"` 中的 **`0x7890 = 30864**，即 etag 的十六进制段就是字节数，与 `content-length` 自洽。

### 2.2 内容哈希比对

```
disk : 4f8ef8d5870894fd0101fc40ff45ea13c0f8e25c71c2ba28e5df5baf98babbb5  …/proxy/prism/assets/sdk.js
url  : 4f8ef8d5870894fd0101fc40ff45ea13c0f8e25c71c2ba28e5df5baf98babbb5  /tmp/sdk_direct.js
IDENTICAL: byte-for-byte match
```

**结论：磁盘文件与 URL 返回内容完全一致（逐字节）**，大小 30864 也对得上 → **不是版本漂移，也不是别的资产**；磁盘文件就是 `20260219f9f6` 这版 sdk.js 的原样拷贝（下载时间与本机文件 mtime `Oct 1 23:00` 一致）。

### 2.3 注意：`/backend-api/sentinel/sdk.js` 是**另一个东西**

`fetchSentinelAssets` 拉的第一个 URL `/backend-api/sentinel/sdk.js` **不是**真正的 SDK，而是一个 **1482 字节的 bootstrap/stub**（sha256 `ecae8c6c2e84376359d13ab8f9def9e38c646d2423b09c3a33efe6118dddcf95`，`content-type: text/javascript`，携带 `x-openai-proxy-wasm: v0.1`）：

```js
window.SentinelSDK = window.SentinelSDK || {};
window.SentinelSDK.init  = …push 到 __sentinel_init_pending 的桩
window.SentinelSDK.token = …push 到 __sentinel_token_pending 的桩
window.SentinelSDK.timing = …return null 的桩
// 然后动态注入真正 SDK：
script.src = 'https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js';
script.async = true; script.defer = true;
document.head.appendChild(script);
```

即：**SV 段 `20260219f9f6` 由这个 stub 硬编码给出**（也是「当前有效 sdk.js 地址」的另一处权威来源，见 §7 Q6）。真正要落盘的永远是**带版本段那个** URL。

---

## 3. 必需性矩阵（Q3）

| 场景 | 是否需要 sdk.js | 依据 |
|---|---|---|
| **(a) 生产真实链路** | **需要**（走「本地 node 铸造」路径 B 时） | 见下 |
| **(b) 单元测试** | **不需要** | `grep -rn "sdk" --include="*_test.go" proxy/prism/` **零命中**；无任何测试引用该文件或 `PRISM_SENTINEL_SDK` |
| **(c) 构建** | **不需要** | 无 `go:embed` 引用 `assets/`；`assets/` 未被 git 跟踪；`go build ./...` 唯一报错是 `main.go:35:12: pattern frontend/dist/*: no matching files found`（与 sdk.js 无关的前端 embed 前置产物缺失） |

### (a) 生产链路 —— 两条路径，只有一条需要它

`proxy/prism/sentinel.go:134-249` `mintNow`：

- **路径 A（外部铸造服务，容器部署默认）**：若设置了 `PRISM_SENTINEL_URL`（:151）或 `PRISM_SENTINEL_CMD`（:154），Go 侧**只做 HTTP 取票**，**根本不读 sdk.js**。此时 sdk.js 的必需性转移到**外部铸造进程**（`cmd/prism-sentinel/daemon.js`）身上。
- **路径 B（宿主本地 node 铸造）**：`sentinel.go:159` 检查 runner 是否存在，然后 `:177` 传 `--sdk <sdk>` 给 node runner。

**sdk.js 缺失时的具体错误路径与文案**：

1. `cmd/prism-sentinel/daemon.js:57`
   ```js
   if (!fs.existsSync(SDK)) throw new Error(`缺 sdk.js: ${SDK}（先下载 sentinel SDK）`);
   ```
2. `proxy/prism/sentinel-runner.js:657`
   ```js
   if (!fs.existsSync(sdkPath)) throw new Error(`找不到 SDK 文件：${sdkPath}`);
   ```
3. 若文件存在但内容不对：`sentinel-runner.js:754-756`
   ```js
   vm.runInContext(sdkCode, context, { filename: sdkPath });
   if (!context.SentinelSDK?.token) throw new Error("SDK 加载后没有暴露 SentinelSDK.token");
   ```

**小结**：sdk.js 是**「谁真正铸造 sentinel token，谁就需要它」**的运行时资产；Go 网关本身在路径 A 下完全不需要它。但**任何一个铸造进程**（daemon 或本地 runner）在缺它时都会**直接抛错、无法出票**，进而 sentinel 门禁拿不到 token，Prism 真实推理链路失败。

---

## 4. 自举能力：Go 侧 `fetchSentinelAssets` 究竟做了什么（Q4）

**结论：它只拉 cookie，不落盘、不保存 sdk.js。首次部署必须人工准备 sdk.js。**

逐段走读 `proxy/prism/sentinel.go:257-279`：

```go
// fetchSentinelAssets 按 HAR 顺序拉 assets（只为拿 sentinel 域 cookie）。
func (c *Client) fetchSentinelAssets(ctx context.Context) error {
    page := strings.TrimRight(c.cfg.Base, "/") + "/"
    items := []struct{ url, accept, referer string }{
        {sentinelOrigin + "/backend-api/sentinel/sdk.js", "*/*", page},                             // (1)
        {sentinelOrigin + "/sentinel/" + sentinelSV + "/sdk.js", "*/*", page},                      // (2)
        {sentinelOrigin + "/backend-api/sentinel/frame.html?sv=" + sentinelSV, "text/html,…", page}, // (3)
        {sentinelOrigin + "/sentinel/" + sentinelSV + "/sdk.js", "*/*",
            sentinelOrigin + "/backend-api/sentinel/frame.html?sv=" + sentinelSV},                   // (4)
    }
    for _, it := range items {
        resp, err := c.do(ctx, "GET", it.url, "", "", map[string]string{"Accept": it.accept, "Referer": it.referer})
        if err != nil { return err }
        _ = resp.Body.Close()                        // ← 响应体被直接丢弃，无任何写入
        if resp.StatusCode != 200 && resp.StatusCode != 304 {
            return fmt.Errorf("asset %s HTTP %d", it.url, resp.StatusCode)
        }
    }
    return nil
}
```

- **请求了哪 4 个路径**：`/backend-api/sentinel/sdk.js` → `/sentinel/<SV>/sdk.js` → `/backend-api/sentinel/frame.html?sv=<SV>` → 再 `/sentinel/<SV>/sdk.js`（带 frame.html 作 Referer）。这是**模拟浏览器加载顺序**，目的在注释里已写明：**「只为拿 sentinel 域 cookie」**。
- **写了什么文件**：**什么都没有**。函数体里**没有** `os.Create` / `os.WriteFile` / `io.Copy` / `fs` 写操作，唯一的 `resp.Body` 操作是 `_ = resp.Body.Close()`。
- **调用点**：`sentinel.go:168`，且注释明确「失败不致命，仅告警」：
  ```go
  // 1) 拉 assets（建立 sentinel 域 cookie；失败不致命，仅告警）
  if err := c.fetchSentinelAssets(ctx); err != nil {
      c.cfg.Logf("prism: sentinel assets 拉取告警: %v", err)
  }
  ```
- **佐证：daemon.js 也从不下 sdk.js**。`grep -nE "download|curl|writeFile|createWriteStream" cmd/prism-sentinel/daemon.js` → **零命中**；daemon 只 `fs.existsSync(SDK)` 检查，缺失即抛错让人工下载。

**⇒ 首次部署必须人工下载 sdk.js**（`docs/prism-deployment.md:33-38` 的 `curl`），或依赖一个已经就绪的外部铸造服务。没有任何代码路径会自动把 sdk.js 落到本地。

---

## 5. 仓库惯例与当前 git 状态

- **`proxy/prism/assets/` 已在 `.gitignore` 中被忽略**（工作区未提交的改动，`git diff .gitignore`）：
  ```
  +# Prism: sentinel SDK 为上游运行时资产（混淆 bundle），由 sidecar 下载，不入库
  +proxy/prism/assets/
  ```
- `git check-ignore -v proxy/prism/assets/sdk.js` → `.gitignore:65:proxy/prism/assets/`；`git status --porcelain --ignored` → `!! proxy/prism/assets/`（**ignored，未跟踪**）。
- `git ls-files -- proxy/prism/assets/` → **空**（从未被跟踪）。`git log --all --full-history -- proxy/prism/assets/` → **空**（该路径从未进过历史）。
- **上游（james-6-23/codex2api）不 vendor js**：`git ls-files '*.js'` 仅列出项目自写的脚本（`cmd/prism-*/**.js`、`proxy/prism/sentinel-runner.js`、`frontend/vite.config.js`），**没有任何第三方运行时资产**。assets/ 下只有本地的 sdk.js 一个文件。
- 当前 HEAD：`613b5af`（pr #757 merge）；Prism 相关的 Go/JS/docs 文件都是**未提交的新增**（`git status --short` 显示为 `A`）。**注意：`.gitignore` 的忽略规则本身也还未提交**——它属于这批待提交的 Prism 变更集，需确保随之落地，否则有被误 `git add -A` 提交的风险（见 §6 风险 5）。

---

## 6. 处理建议（Q5）

### 明确推荐：**不入库，保持 `.gitignore` 忽略 + 补「带校验的下载脚本」+ 文档化版本漂移**

#### 支持「不入库」的五条依据

1. **许可证与再分发风险（决定性）**：该文件是 OpenAI 的专有混淆运行时资产，**无任何 license/copyright 头**，且是**刻意混淆**以抗篡改的产物。把它提交进公开仓库（origin 是 public GitHub `james-6-23/codex2api`）等于把 OpenAI 的专有代码再分发给全网，存在 **ToS/版权风险**，且无授权依据可用。
2. **版本漂移（工程决定性）**：URL 里的 SV 段 `20260219f9f6` 会随上游轮换，而它被**硬编码在 3 处**：`proxy/prism/sentinel.go:254`（`sentinelSV` 常量）、`cmd/prism-sentinel/daemon.js:35`（`SENTINEL_SV` 默认值）、`docs/prism-deployment.md:37`。一旦入库，仓库里就冻结了一份**与当前 SV 绑定**的二进制；SV 漂移后：`--script-src` 指向的 URL 与 vm 内执行的代码不再匹配 → **proof 与服务端预期不一致**，且是**静默失效**。而「运行期下载 + `PRISM_SENTINEL_SV`/`PRISM_SENTINEL_SDK` 可覆盖」的设计正是为漂移准备的。
3. **仓库惯例**：上游**零 vendor JS**（§5）。为一个通道单独引入第三方运行时资产会破坏惯例，也让仓库体积与合规审查面变大。
4. **必需性边界**：sdk.js **只被『铸造进程』需要**（daemon 或本地 runner），**Go 网关本身在路径 A 下不需要它**（§3）。把它放进 `proxy/prism/assets/`（Go 包目录旁）本身就是**位置语义错误**——它不属于 Go 构建产物，只属于宿主侧 sidecar。
5. **可复现性/离线**：入库确实能换来离线可装，但正确解法是**「下载脚本 + sha256 钉住」**，而不是把专有二进制写进历史（`git` 的历史是不可撤销的，一旦提交即使后续删除也仍在 pack 中）。

#### 若采纳「不入库」，需要补充的最小文档 / 脚本要点

1. **一个下载脚本**（建议 `scripts/fetch-sentinel-sdk.sh`，不放进 `proxy/prism/assets/`）：
   - **自动发现当前 SV**：GET `https://sentinel.openai.com/backend-api/sentinel/sdk.js`（1482B stub，内嵌 `script.src='…/sentinel/<SV>/sdk.js'`），用 `grep -oE '/sentinel/[0-9a-f]+/sdk\.js'` 提取；**备选**：GET `frame.html?sv=<SV>` 或直接问铸造服务。
   - 下载到 `${SENTINEL_SDK:-/tmp/prism_sidecar/assets/sdk.js}`，目录 `mkdir -p`。
   - **sha256 校验**：脚本内置「期望哈希」变量（当前 `4f8ef8d5…babbb5`），不匹配时**告警但仍继续**（因为漂移是常态），并把**实际 sha256 与新 SV 打印出来**，提示同步更新 `PRISM_SENTINEL_SV`。
   - **打印新 SV**，让用户 `export PRISM_SENTINEL_SV=<SV>`。
2. **文档新增（`docs/prism-deployment.md`）**：
   - 「sdk.js 是**上游运行时资产，不入库**，**首次部署必须人工准备**（或由外部铸造服务持有）；Go 侧 `fetchSentinelAssets` **只拉 cookie、不会下载它**」。
   - 「**版本会漂移**：URL 的 SV 段（如 `20260219f9f6`）由上游轮换；漂移时需重下 sdk.js **并同步** `PRISM_SENTINEL_SV` / daemon 的默认 SV，否则 proof 会静默失配。」
   - 「**离线/气隙环境**：把 sdk.js 放进配置的 `PRISM_SENTINEL_SDK` 路径即可，无需联网下载（但仍需能访问 sentinel 挑战接口）。」
   - 附**当前已知哈希**（`4f8ef8d5…babbb5`，2026-10-02 实测）作为审计锚点。
3. **可选镜像（对气隙部署）**：把 sdk.js 作为 **GitHub Release 附件**（而非 git 对象）发布，或用**独立 assets 仓库**；**都不进主仓库历史**。注意：这仍有 §6.1 的再分发风险，建议默认不提供，仅在明确合规评估后提供。
4. **CI/开发友好**：给 `go test ./proxy/prism/` 加一条**守卫测试**（或 make 目标），断言「缺 sdk.js 时单测仍全绿」，把「单测不依赖它」这一事实固化成契约，防止将来引入隐式依赖。
5. **落地动作**：确认 `.gitignore` 那 3 行（含注释）随 Prism 变更集一起提交（当前它还是**未提交**状态）。建议在 PR 描述里写明「`proxy/prism/assets/` 为本地运行时资产，已 ignore」。

---

## 7. Q6 —— 若文档 URL 失效，如何获得当前有效的 sdk.js

### 7.1 `sdk.js` 的 script src 由哪一步给出（三条权威来源）

| # | 来源 | 说明 |
|---|---|---|
| A | `GET /backend-api/sentinel/sdk.js` | 1482B bootstrap，**硬编码** `script.src='https://sentinel.openai.com/sentinel/<SV>/sdk.js'`（本报告 §2.3 原文） |
| B | `GET /backend-api/sentinel/frame.html?sv=<SV>` | 返回 `<!DOCTYPE html><html><body><script src='https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js'></script></body></html>`（实测） |
| C | 代码内的 SV 来源 | Go：`sentinel.go:254 sentinelSV = "20260219f9f6"`；daemon：`daemon.js:35 SENTINEL_SV = process.env.PRISM_SENTINEL_SV \|\| '20260219f9f6'`；`fetchSentinelAssets` 的路径清单见 §4（`/backend-api/sentinel/sdk.js`、`/sentinel/<SV>/sdk.js`、`frame.html?sv=<SV>`） |

### 7.2 可直接执行的 curl 命令

**(1) 直接下载（当前已知 SV，实测可用）**
```bash
mkdir -p /tmp/prism_sidecar/assets
curl -sS -o /tmp/prism_sidecar/assets/sdk.js \
  -H 'User-Agent: codex-tui/0.156.0' \
  https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js
shasum -a 256 /tmp/prism_sidecar/assets/sdk.js   # 期望 4f8ef8d5870894fd0101fc40ff45ea13c0f8e25c71c2ba28e5df5baf98babbb5
```

**(2) 自动发现当前 SV（URL 漂移后的正确取法）**
```bash
SV=$(curl -sS -A 'Mozilla/5.0' https://sentinel.openai.com/backend-api/sentinel/sdk.js \
      | grep -oE '/sentinel/[0-9a-f]+/sdk\.js' | head -1 | cut -d/ -f3)
echo "current SV = $SV"
curl -sS -o /tmp/prism_sidecar/assets/sdk.js \
  -H 'User-Agent: codex-tui/0.156.0' \
  "https://sentinel.openai.com/sentinel/$SV/sdk.js"
shasum -a 256 /tmp/prism_sidecar/assets/sdk.js
# 然后：export PRISM_SENTINEL_SV="$SV"（并同步 daemon 的 SENTINEL_SV）
```

**(3) 用 frame.html 交叉核对 SV**
```bash
curl -sS -A 'Mozilla/5.0' "https://sentinel.openai.com/backend-api/sentinel/frame.html?sv=$SV" \
  | grep -oE 'https://sentinel\.openai\.com/sentinel/[0-9a-f]+/sdk\.js'
```

**(4) 若直连被墙（本机可用 HTTP 代理备用）**
```bash
curl -sS --proxy http://127.0.0.1:7890 -o /tmp/sdk_proxy.js \
  https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js
```
> 注：本次实测**直连可用**（无需代理），备用命令仅为容灾。

---

## 8. 证据清单（命令 + 原始输出摘录）

| # | 命令 | 关键输出 |
|---|---|---|
| E1 | `ls -la proxy/prism/assets/` | `-rw-r--r-- … 30864 Oct 1 23:00 sdk.js`（assets/ 下仅此一文件） |
| E2 | `shasum -a 256 proxy/prism/assets/sdk.js` | `4f8ef8d5870894fd0101fc40ff45ea13c0f8e25c71c2ba28e5df5baf98babbb5` |
| E3 | `file … /sdk.js` | `Unicode text, UTF-8 text, with very long lines (30861)` |
| E4 | 关键字计数（python） | `Sentinel=1 sentinel=8 SentinelSDK=1 openai=0 arkose=0 cloudflare=0 webpack=0 rollup=0 esbuild=0 copyright=0 license=0 turnstile=2` |
| E5 | 字符串表抽取（242 条 distinct） | 导出/内部 API 清单见 §1.2（`token`/`init`/`sessionObserverToken`/`getEnforcementToken`/`proofofwork`/`gAAAAAB`/`wQ8Lk5Fb…`） |
| E6 | `curl … /sentinel/20260219f9f6/sdk.js -D hdr` | `HTTP/2 200`、`content-length: 30864`、`cache-control: public, max-age=0`、`etag: W/"7890-1a0fa94e890"`、`server: cloudflare`、`cf-ray …-SIN`，无 `location` / 无 license / 无 CSP |
| E7 | `cmp -s disk /tmp/sdk_direct.js` | `IDENTICAL: byte-for-byte match`（两侧 sha256 均为 `4f8ef8d5…`） |
| E8 | `printf "0x7890 = %d"` | `0x7890 = 30864`（etag 段 == 字节数，自洽） |
| E9 | `curl … /backend-api/sentinel/sdk.js` | `HTTP/2 200`、`content-length: 1482`、`x-openai-proxy-wasm: v0.1`；body 为 stub，内嵌 `script.src='…/sentinel/20260219f9f6/sdk.js'` |
| E10 | `curl … frame.html?sv=20260219f9f6` | `<script src='https://sentinel.openai.com/sentinel/20260219f9f6/sdk.js'></script>` |
| E11 | `sed -n '257,279p' proxy/prism/sentinel.go` | `fetchSentinelAssets` 全文：4 条 URL、`_ = resp.Body.Close()`、**无任何写文件调用** |
| E12 | `sed -n '134,180p' proxy/prism/sentinel.go` | `mintNow`：路径 A（`PRISM_SENTINEL_URL`/`PRISM_SENTINEL_CMD`，:151/:154）与路径 B（本地 node，:158 起）分流；:168 注释「失败不致命，仅告警」 |
| E13 | `grep -nE "download\|curl\|writeFile\|createWriteStream" cmd/prism-sentinel/daemon.js` | 零命中；仅有 `:57 throw new Error('缺 sdk.js: …（先下载 sentinel SDK）')` |
| E14 | `sed -n '632,657p;746,757p' proxy/prism/sentinel-runner.js` | `sdkPath` 解析（:635）、`vm.runInContext(sdkCode,…)`（:754）、`if (!fs.existsSync(sdkPath)) throw new Error("找不到 SDK 文件：…")`（:657）、`SDK 加载后没有暴露 SentinelSDK.token`（:756） |
| E15 | `grep -rn "sdk" --include="*_test.go" proxy/prism/` | **零命中** → 单测不依赖 |
| E16 | `go build ./...` | 唯一报错 `main.go:35:12: pattern frontend/dist/*: no matching files found`（与 sdk.js 无关） |
| E17 | `git check-ignore -v proxy/prism/assets/sdk.js` | `.gitignore:65:proxy/prism/assets/` |
| E18 | `git status --porcelain --ignored -- proxy/prism/assets/` | `!! proxy/prism/assets/`（ignored，未跟踪） |
| E19 | `git ls-files '*.js'` | 仅项目自写脚本 + `frontend/vite.config.js` → **上游不 vendor js** |
| E20 | `git diff .gitignore` | 新增 3 行：注释 + `proxy/prism/assets/`（**尚未提交**） |
| E21 | `git log --all --full-history -- proxy/prism/assets/` | 空（该路径从未进过 git 历史） |

---

## 9. 未解问题 / 待确认

1. **混淆器确切工具与版本无法离线证明**：外层打包标记（webpack/rollup/esbuild）已被剥离，只能从字符串数组 + 轮转 + self-defending 模板判定为 `javascript-obfuscator` 家族；**不能**给出确定的工具版本。
2. **`wQ8Lk5FbGpA2NcR9dShT6gYjU7VxZ4D` 的用途未验证**：形态像静态 key/HMAC 材料，但未执行代码验证其参与何种签名。
3. **`gAAAAAB`（Fernet）解密路径未验证**：runner 里的 `atob` + `JSON.parse` 提示用对称解密处理 challenge 载荷，但**未实际跑通**以确认。
4. **bundle 内无版本号/buildId**：唯一版本标识是 URL 的 SV 段，属外部输入；因此「该文件的版本」必须与 `sentinelSV`/`SENTINEL_SV` 一起记录，无法只靠文件自身判定。
5. **`.gitignore` 的忽略规则尚未提交**：属于当前未提交的 Prism 变更集，需确认它会随 PR 一起落地（否则 sdk.js 有被 `git add -A` 误提交的风险）。
6. **SV 漂移的检测机制仍缺失**：目前三处硬编码 SV 没有「不一致即告警」的校验；建议后续加脚本/启动日志比对（见 §6.2）。
