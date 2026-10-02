# Prism 通道 · 部署手册

网关底座：`james-6-23/codex2api` ｜ 新增包：`proxy/prism` ｜ 接入点：relay-style 汇合点

---

## 一、为什么需要两个宿主侧常驻进程

Prism 上游有三道「必须真浏览器 / 必须 node」的门，而 codex2api 官方镜像是 Go 轻量镜像：

| 依赖 | 为什么需要 | 放在哪 |
|---|---|---|
| **对话材料包** | 上游把 start 与页面上下文强绑定，`metadata` 含 `proxy_request_debug`/`codex_listen_snapshot`，纯 HTTP 自建必 400 | **真浏览器**（playwright + 系统 Chrome） |
| **sentinel token** | `/api/*` 的严格一次性门禁；SDK 是纯 JS，需 node 跑 `vm` 算 proof | **node**（容器里没有） |
| 材料 TTL | `cf_bm`/`__cflb` 是分钟级 cookie，实测约 2 分钟失效 | 需**周期重采** |

因此架构是：**宿主跑两个常驻侧车，容器只消费文件与 HTTP**。

```
宿主                                    容器（codex2api）
├─ prism-material/daemon.js  ──写──▶  /tmp/prism_sidecar/material.json
│   （常驻浏览器，75s 重采）              ↓ 只读挂载 /prism:ro
└─ prism-sentinel/daemon.js  ◀──curl──  PRISM_SENTINEL_CMD
    （监听 127.0.0.1:8791）             （host.docker.internal:8791）
```

---

## 二、启动

### 1. 准备 sentinel SDK（只需一次）

`sdk.js` 是上游 OpenAI 的**混淆专有资产**（sentinel proof-of-work SDK），
**不随仓库分发**，也**不要硬编码版本段**。用自带脚本自动发现当前 SV 并下载：

```bash
bash cmd/prism-sentinel/fetch-sdk.sh          # 默认写到 /tmp/prism_sidecar/assets/sdk.js
cp proxy/prism/sentinel-runner.js /tmp/prism_sidecar/
```

脚本会：① 从 stub `/backend-api/sentinel/sdk.js` 自动发现当前 SV →
② 下载 `…/sentinel/<SV>/sdk.js` → ③ 校验（拒绝 HTML 错误页 / 缺 `SentinelSDK` 导出 / 过小文件）。

> ⚠️ **为何不写死 URL**：路径里的 `20260219f9f6` 这类 SV 段**会随上游漂移**，
> 硬编码会在上游升级后静默失效（表现：铸造失败，且极难排查）。
> 若确需手工操作：
> ```bash
> SV=$(curl -sS -A 'Mozilla/5.0' https://sentinel.openai.com/backend-api/sentinel/sdk.js \
>      | grep -oE 'sentinel/[0-9a-f]+/sdk\.js' | head -1 | cut -d/ -f2)
> curl -sS -o /tmp/prism_sidecar/assets/sdk.js \
>   "https://sentinel.openai.com/sentinel/$SV/sdk.js"
> ```
> 离线部署：在能出网的机器上跑脚本，再把产物拷到目标机的 `PRISM_SENTINEL_SDK`。
> 资产溯源与证据见 `docs/research/sdk-js-provenance.md`。

### 2. 起两个侧车（宿主）

> ⚠️ **材料目录必须是 colima 共享路径**：colima 默认**只共享 `/Users`**，
> 写成 `/tmp/prism_sidecar/material.json` 时容器内挂载点会是**空目录**，
> 表现为 `open /prism/material.json: no such file or directory`。用 `/Users` 下的目录。

```bash
export PATH="$HOME/.hermes/node/bin:$PATH"
mkdir -p "$HOME/prism_material"          # 必须落在 /Users 下

# 材料保温（常驻浏览器；headless 即可）
PRISM_ACCOUNT_JSON=/tmp/cpa_account.json \
PRISM_MATERIAL_PATH="$HOME/prism_material/material.json" \
PRISM_REFRESH_SECONDS=75 \
PRISM_UI_PROXY=http://127.0.0.1:7890 \
node cmd/prism-material/daemon.js

# sentinel 铸造（后台）
node cmd/prism-sentinel/daemon.js       # → http://127.0.0.1:8791
```

验证：`curl -sS http://127.0.0.1:8791/healthz` → `{"ok":true,"minted":N,...}`

### 3. 起网关（容器）

**用官方原生构建**（标准 Docker Desktop / 任何有 buildx 的环境直接跑）：

```bash
# ① 构建镜像（仓库根目录官方 Dockerfile，多阶段构建）
docker build -t codex2api-prism:local .
```

> ⚠️ **前置：需要 buildx**。官方 Dockerfile 用了 `--platform=$BUILDPLATFORM`
> 与 `--mount=type=cache`（BuildKit 语法）；标准 Docker Desktop 自带。
> 若 `docker buildx version` 报 unknown command：
> ```bash
> brew install docker-buildx && mkdir -p ~/.docker/cli-plugins
> ln -sfn "$(brew --prefix)/bin/docker-buildx" ~/.docker/cli-plugins/docker-buildx
> ```
> 缺 buildx 时的典型报错：`failed to parse platform : "" is an invalid OS component`。

非 colima / 已用官方镜像的环境也可直接 compose（跳过本地构建）：

```bash
docker compose -f docker-compose.sqlite.yml \
               -f docker-compose.local-override.yml up -d
```

> ⚠️ **colima 用户注意**（两个本机踩过的坑；官方原生构建同样可用）：
> 1. **缺 buildx** → `failed to parse platform : "" is an invalid OS component`。
>    装 `brew install docker-buildx` 并软链到 `~/.docker/cli-plugins/docker-buildx`（见上）。
> 2. **代理注入** → 若 `~/.docker/config.json` 的 `proxies.default` 或 Lima 的
>    `propagateProxyEnv` 把宿主代理（如 `127.0.0.1:7897`）带进构建容器，容器内
>    **所有**出网都会失败（apk/npm/git 报 `connection refused`，看着像"镜像源挂了"）。
>    处理：删掉 `~/.docker/config.json` 的 `proxies` 块，并在
>    `~/.colima/_lima/_config/override.yaml` 写 `propagateProxyEnv: false` 后重启 colima。

### 3.5 图片输入（base64 内联，唯一走通路）

Prism 上游**不支持** `input_image` / `input_file` 直传 —— 两者语法均被接受，
但模型实际**拿不到图**：`input_file` 只是让模型去**会话工作区**找文件，而文件
能否进工作区靠站点编辑器写协作文档（私有 WS 协议），外部无法复制。

因此本通道改为：把图片 **base64 内联进本轮 user 消息**，附「落盘还原命令」+
`view_image` 指引，模型先落盘再读；超预算先缩图（复用 `internal/imageproc`）。

| 预算 | 值 | 实测 |
|---|---|---|
| 单图 | ≤ 48k 字符 | 59,216 ✅ / 219,084 ❌（上游 502） |
| 单请求 | ≤ 96k 字符 | 超限先缩（JPEG 阶梯降质 `768px/78 → 64px/28`） |

> ⚠️ **验证图片是否真被感知，必须用多角度/客观判据**：
> 单次单角度方位提问**不可**作为通过依据 —— 模型对「左上」有强先验，
> 完全看不到图时也可能答对。可靠判据示例：换角度复测（黑块画到不同角）、
> 让模型读图内数字/文字。开发中曾因单角度命中而误判「已打通」。

---

### 4. 导入 prism 账号

⚠️ **`POST /api/admin/accounts` 建不出 prism 账号** —— 该端点对应 Codex 的
OAuth 导入（缺 `refresh_token`/`session_token` 会直接 400），且请求体里没有
`type` 字段。prism 账号必须走**专用端点**：

```bash
curl -sS -X POST http://localhost:8080/api/admin/accounts/prism \
  -H "Authorization: Bearer $ADMIN_SECRET" \
  -H 'Content-Type: application/json' \
  -d '{"access_token":"<OpenAI OAuth access_token>","email":"<邮箱>","name":"prism-1"}'
# → {"ok":true,"id":N,"email":"…"}     重复导入同一 token → 409（按凭据哈希去重）
```

也接受数组/包装形态：`[{...},{...}]` 或 `{"accounts":[{...}]}`。

### 5. 建一个 **prism 渠道 Key**（必须，否则永远 503）

Prism 是 relay-style 上游，**默认（auto）选号会排除所有 relay-style 账号** ——
即不限定渠道的 Key **永远选不到 Prism 账号**，表现为
`no_available_account`／「无可用账号，请稍后重试」（HTTP 503）。

```bash
curl -sS -X POST http://localhost:8080/api/admin/keys \
  -H "Authorization: Bearer $ADMIN_SECRET" -H 'Content-Type: application/json' \
  -d '{"name":"prism","limits":{"upstream_channel":"prism"}}'
```

用这把 Key 调用即可路由到 Prism 通道（与 Grok / Claude / Antigravity 同机制）。

---

## 三、环境变量总表

| 变量 | 默认 | 说明 |
|---|---|---|
| `PRISM_MATERIAL_PATH` | `/tmp/prism_sidecar/material.json` | 材料文件（**容器内填挂载路径 `/prism/material.json`**） |
| `PRISM_REFRESH_SECONDS` | `75` | 材料重采周期，须 **< 网关 TTL 120s** |
| `PRISM_UI_PROXY` | `http://127.0.0.1:7890` | 侧车浏览器代理（须与登录/材料同出口 IP） |
| `PRISM_SENTINEL_URL` | — | 外部铸造服务地址（宿主直跑时用） |
| `PRISM_SENTINEL_CMD` | — | 外部铸造命令（**容器场景用**，取 stdout 末行） |
| `PRISM_SENTINEL_RUNNER` | `/tmp/prism_sidecar/sentinel-runner.js` | 本地 node 铸造的 runner 路径 |
| `PRISM_SENTINEL_SDK` | `/tmp/prism_sidecar/assets/sdk.js` | sentinel SDK 本地缓存 |
| `PRISM_NODE_BIN` | `node` | node 可执行文件 |
| `PRISM_BASE` | `https://prism.openai.com` | 上游基址 |
| `PRISM_MATERIAL_TTL` | `2m` | 网关侧材料新鲜度窗口 |

取票优先级：`PRISM_SENTINEL_URL` → `PRISM_SENTINEL_CMD` → 本地 node 铸造。

---

## 三点五、用量（usage）语义 —— 诚实计数

**Prism 私协议终态 payload 不含 `usage` 字段**（实测：`payload` 的键只有
`id` / `output` / `conversationId` / `codexDebug` / `codexListenSnapshot`）。

因此本通道**不伪造 token 计数**：

- 上游未上报时，`/v1/responses` 终态事件给出
  `"usage":{"reported":false, 计数字段一律 null, "note":"上游未上报 usage…"}`；
- 透传到 `/v1/chat/completions` 时该 `usage` **整体不出现**（而不是补一组 0）；
- 若将来上游开始上报，`reported:true` 且给出实数，同时附带
  `prism.effective_input_tokens` / `cache_read_tokens` / `cache_write_tokens` / `cache_hit_rate`。

> 为什么不能填 0：零值会被下游当成真实用量写进计费/缓存统计，
> 属于伪造数据（`proxy/prism/usage_parse.go` 的 `Reported` 与
> `prismUsagePayload` 就是为此而设）。

---

## 四、验证

```bash
# 闭环（宿主直跑，最直观）
PRISM_MATERIAL_PATH=/tmp/prism_sidecar/material.json \
go run ./cmd/prism-live -account /tmp/cpa_account.json -prompt "只回复三个字：收到了"

# 期望：耗时 ~7-9s | status=completed | [正文] 收到了 | ✅ 闭环成功

# 看 Responses SSE 事件序列（下游消费形态）
go run ./cmd/prism-live -account /tmp/cpa_account.json -prompt "计算 1+1" -stream

# 走容器网关
curl -sS http://localhost:8080/v1/responses \
  -H "Authorization: Bearer ***" -H 'Content-Type: application/json' \
  -d '{"model":"gpt-5.6-sol","input":"只回复三个字：收到了","stream":true}'

# 一键冒烟（纯文本 + 带图，含手写 PNG 生成器，无需 PIL）
KEY=<prism渠道Key> bash deploy/prism/verify.sh

# 铸造侧车调度逻辑的单元测试（node:test，零依赖）
node --test cmd/prism-sentinel/*.test.js
```

---

## 五、排障

| 症状 | 根因 | 处理 |
|---|---|---|
| 403 `Request verification failed` | sentinel 缺失/复用 | 检查铸造服务 `healthz`；确保**每次请求现铸** |
| 403 `Error while processing conversation` | conversationId 被复用，或**沙箱被占用** | 侧车必须 `route.abort()` 偷票；确认没有别的客户端在跑 |
| 400 `Please submit prompt again` | 材料过期，或 `metadata` 被改写 | 查材料 `fresh`；**`model`/`effort`/`sandbox_*` 必须原样复用** |
| 400 `turn_state is required` | 轮询时 turn_state 未原样回传 | 检查 status 请求体 |
| 正文为空但 status=completed | 响应层级读错 | 必须剥到 `response.payload.output` |
| 材料一直 `fresh:false` | 侧车挂了 / 浏览器崩了 | 看 daemon 日志，重启侧车 |
| 容器内 502 | 代理注入 `127.0.0.1:7897` | 用 override 清空代理 |
| `no_available_account` / 503「无可用账号」 | Key **没限定** `upstream_channel=prism`（auto 选号排除 relay-style 账号） | 用 prism 渠道 Key |
| `open /prism/material.json: no such file` | colima 只共享 `/Users`，`/tmp` 挂载成空目录 | 材料放到 `/Users/...` 再挂载 |
| 建 prism 账号 400「refresh_token 必填」 | 用了 Codex 的 `/accounts` 而不是 `/accounts/prism` | 改走专用端点 |
| `failed to parse platform` 构建失败 | 缺 buildx，官方 Dockerfile 的 BuildKit 语法无法解析 | `brew install docker-buildx` + 软链到 `~/.docker/cli-plugins/`（见 §3） |
| 构建期 `apk`/`npm` 报 `connection refused` | Docker CLI / Lima 把宿主代理注入构建容器（容器内不可达） | 删 `~/.docker/config.json` 的 `proxies` 块 + `propagateProxyEnv: false` |
| 请求越来越慢 / 轮询超时（4m0s） | **材料前缀无限膨胀**：侧车每轮往同一会话追加，历史只增不减 | 侧车每轮**刷新页面**（会话复位）+ `loadMaterial` 只取开头连续 system 段（已内置） |
| 模型答「无法读取图片」 | 用了 `input_file`/`input_image`（上游不落工作区） | 走 base64 内联（本通道默认，见 §3.5） |
| 图片/长请求 **3 分钟无输出**、网关报 `取 sentinel 失败 502` | 铸造侧车**一次瞬时抖动**（`fetch failed`）写入退避标记后，成功路径未清除 → 整条通道被锁死 | 已修（成功即清 `failAt`）；排查时先 `curl 127.0.0.1:8791/healthz` 看 `failed`，再直接 `curl .../token` |

---

## 六、已知边界

- **无真流式**：Prism 是 start + 轮询模型，本层在终态后一次性发正文（思考摘要若存在则作为真增量先发）。
- **单材料身份**：一份材料对应一个浏览器身份，上游按身份限流（单身份约 80 RPM）。高并发需多份材料（多槽轮转）。
- **沙箱额度**：材料里的沙箱被所有请求复用；不要频繁 reload 页面（每次加载会铸新沙箱）。
- **容器无 node**：这是官方镜像的固有限制，故必须外置铸造。
- **材料新鲜度**：材料包必须 < 网关 TTL（默认 2m），侧车 75s 重采；材料过期表现为上游 400 `Please submit prompt again`。

---

## 七、`prism-gateway/` 是什么（与 `proxy/prism` 的关系）

仓库根下的 `prism-gateway/` 是本通道的**独立实现**（零第三方依赖，纯标准库）：
用于对比验证、以及在宿主直跑时排查问题。二者**共享同一套上游协议认知**，但修复点各自独立 ——
差异与约束见 [`prism-gateway/KNOWN-CONSTRAINTS.md`](../prism-gateway/KNOWN-CONSTRAINTS.md) §4。

| | `proxy/prism`（本通道主线，随 codex2api 一起构建部署）| `prism-gateway`（独立 module）|
|---|---|---|
| 依赖 | 随主仓（可用主仓的 utls 传输）| **零第三方依赖**，TLS 指纹靠宿主注入 |
| 用途 | 生产：作为 Prism 渠道跑在网关里 | 参考/对照：协议验证与排障 |

`prism-gateway/dist/` 下的交叉编译产物**不入库**（`.gitignore` 已排除），
需要时在 `prism-gateway/` 内 `go build ./cmd/prism-gateway` 自行生成。
