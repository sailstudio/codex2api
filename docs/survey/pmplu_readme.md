# prism-plugin

CLIProxyAPI（CPA）插件。用 `prism.openai.com` 的浏览器会话把 GPT-6-Astra / GPT-5.6 接进 CPA，
无需 API key、无需付费计划。移植自本仓库的 Prism2API 轻量版，插件 ABI 与
[ex-plugin](https://github.com/spacex-3/ex-plugin) 一致。

## 模型

插件注册以下名字，请求上游 Prism 时使用同一个模型 ID：

- `gpt-6-astra`（别名 `prism-astra`）
- `gpt-5.6-sol`（别名 `prism-sol`）
- `gpt-5.6-terra`（别名 `prism-terra`）

`model_aliases` 可以把客户端模型名指到上面任意模型，例如 `gpt-5.5: gpt-5.6-sol`。
思考等级后缀有效：`gpt-5.6-sol(high)`。

Prism 的模型白名单随时会变（2026-09-17 曾在白天下架 `gpt-6-astra`）。当上游返回
`400: Unsupported assistant model` 时，插件自动回退到白名单内的其他模型并继续任务。

## 工作方式

Prism 不是公共 API，它的后端是一个 start+poll 的远程沙箱代理：

```
POST /api/backend/1/new                  -> {url, token}   铸造沙箱
POST /api/llm/response_with_tools_start  -> {request_id, turn_state}
POST /api/llm/response_with_tools_status -> 轮询到 completed
```

插件做了上游强加的四件事：

1. **整段对话折叠进一条 user 消息**——Prism 只保留最后一条 user 消息。
2. **把模型框成 next-action emitter**——Prism 的模型自带沙箱，直接让它"做任务"会在它自己的
   沙箱里做；改成只输出一个 JSON 动作，插件再解析成标准 `function_call` 交回客户端执行。
3. **丢弃 Codex 注入的超长 developer 指令**，只保留 shell 环境（cwd 等）。
4. **沙箱 token 必须是热的**——新铸的沙箱要冷启动几分钟然后 504，凭证里的 token 从真实浏览器
   请求中截取，指向已经在运行的沙箱。

## 凭证

`~/.cliproxy/auths/` 放一个 `type: prism` 的会话文件（内容即本仓库 `data/session.json`）：

```json
{
  "type": "prism",
  "cookie": "__Secure-next-auth.session-token=...; 其他cookie",
  "sandbox_url": "https://xxx.prism.openai.com/",
  "sandbox_token": "从请求体 metadata 拷贝",
  "project_id": "可选",
  "user_id": "可选"
}
```

生成方式见主 README 的 `scripts/import_session.py`：登录 `prism.openai.com`，DevTools → Network
发一条消息，右键 `/api/llm/response_with_tools_start` → Copy as cURL，然后：

```bash
python3 scripts/import_session.py prism-curl.txt -o ~/.cliproxy/auths/prism.json
# 手动把 "type" 字段改成 "prism"
```

Cookie 约 12 小时过期，没有程序化刷新：过期后重新抓一次覆盖文件即可，CPA 会在下一次
凭证刷新周期自动重读，无需重启。

## 配置

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    prism-plugin:
      enabled: true
      priority: 100
      # base_url: https://prism.openai.com
      # effort: medium
      # timeout_seconds: 240
      # concurrency: 1          # 沙箱池大小 = 并行 turn 数（上限 60，对齐 Pro 账号）
      # keepalive_seconds: 600  # 每 N 秒 ping 空闲沙箱保温
      # max_transcript_chars: 24000
      model_aliases:
        gpt-5.5: gpt-5.6-sol
```

## 并发与沙箱池

Prism 的一个沙箱一次只处理一个 turn，并发多发会被上游 400。插件因此内置**沙箱池**：

- `concurrency: N` 表示池里最多 N 个沙箱，即最多 N 个请求真正并行，其余排队（共享同一个
  总超时预算，排队不会额外延长等待）。
- 池中所有沙箱共享同一个 cookie：第一次请求用凭证里导入的沙箱，之后按需调用
  `POST /api/backend/1/new` 铸新沙箱，用完归还复用，不重复铸造。
- 沙箱失效（`sandbox_reconnecting`/504）时自动丢弃重铸；空闲沙箱每 `keepalive_seconds`
  ping 一次保温，避免冷启动 504。
- 一个 Pro 账号上游最多 60 并发，`concurrency` 上限即 60。导入多个 auth 文件则每账号
  各有一个独立池。

`concurrency: 1` 时行为与单沙箱串行完全一致，只是多了保温与自愈。

## 已知限制

- Prism 是网页内部接口，随时可能失效或限制账号，本插件不提供任何保证。
- 响应是完成后分块吐出的伪流式，不是逐 token 流式（流式请求会先立即回
  `response.created`，客户端马上进入 thinking 状态）。
- `usage` 是按可见字符长度估算的，不是上游精确账单。

## 本地构建

```bash
go test ./...
make package   # 产物在 dist/prism-plugin_<version>_<goos>_<goarch>.zip
```

## 免责声明

本项目通过 OpenAI 未公开的接口驱动其产品，使用你自己的账号，可能违反你同意的服务条款，
账号存在被限制的风险。凭证只发往 `prism.openai.com`，不会上传到任何第三方。
