# Prism AI Gateway (OpenAI / Codex / Claude 全兼容高可用网关服务)

高性能、全功能的 OpenAI Codex (GPT-6 Astra / Sol) 企业级 AI 网关服务。提供标准 OpenAI 兼容的 `/v1/chat/completions`、官方 VS Code Codex 智能体协议 `/v1/responses`、Anthropic Claude `/v1/messages` 与 `/v1/models` 接口，支持 Web 原生控制台、100% 真实原生思考流透明透传、智能代码补丁落地、后端多账号号池高可用调度以及最大 **102.4万 Token (1.024M)** 的超大物理上下文推理。

---

## 目录
- [核心特性](#核心特性)
- [项目规范目录结构](#项目规范目录结构)
- [多账号号池管理体系 (Account Pool)](#多账号号池管理体系-account-pool)
- [单账号高并发极限实测数据](#单账号高并发极限实测数据)
- [OpenAI Responses API 与 VS Code Codex 原生插件对接](#openai-responses-api-与-vs-code-codex-原生插件对接)
  - [1. 官方智能体调用规范与流式事件](#1-官方智能体调用规范与流式事件)
  - [2. 100% 真实原生思维流透传（“是什么就得是什么”）](#2-100-真实原生思维流透传是什么就得是什么)
  - [3. 智能工作区补丁落地与原生 Diff 联动 (Apply Patch)](#3-智能工作区补丁落地与原生-diff-联动-apply-patch)
- [Anthropic Claude Messages API (`/v1/messages`)](#anthropic-claude-messages-api-v1messages)
- [多模态与绘图/看图能力实测说明](#多模态与绘图看图能力实测说明)
- [模型支持与套餐权限矩阵](#模型支持与套餐权限矩阵)
- [显式模型透传规范](#显式模型透传规范)
- [思考强度 (Reasoning Effort) 等级](#思考强度-reasoning-effort-等级)
- [超大物理上下文压测结果](#超大物理上下文压测结果)
- [API Key 鉴权体系与管理](#api-key-鉴权体系与管理)
- [全景统计大盘、Token 分析、官方 API 计费折算与详细日志](#全景统计大盘token-分析官方-api-计费折算与详细日志)
- [快速开始与启动](#快速开始与启动)
- [第三方客户端接入指南](#第三方客户端接入指南)
  - [VS Code 官方 Codex 插件](#0-vs-code-官方-codex-插件)
  - [NextChat (ChatGPT-Next-Web)](#1-nextchat-chatgpt-next-web)
  - [Chatbox](#2-chatbox)
  - [Cherry Studio](#3-cherry-studio)
  - [Cline / VSCode 插件](#4-cline--vscode-插件)
  - [cURL 命令行调用](#5-curl-命令行调用)
- [Web 控制台功能与套餐自动感知](#web-控制台功能与套餐自动感知)
- [常见问题与说明](#常见问题与说明)

---

## 核心特性

- **多协议全生态原生兼容**：
  - 标准 OpenAI Chat 协议：`/v1/chat/completions`、`/v1/models`；
  - 官方 Codex 智能体协议：`/v1/responses`、`/responses` (原生对接 VS Code Codex 插件)；
  - Anthropic 协议：`/v1/messages` (对接 Claude 生态工具)。
- **100% 官方真实思维流透传**：实时拦截上游轮询中的 `codex_live_progress.reasoningSummaries`，模型思考什么就流式呈现什么，**彻底杜绝任何写死模拟、假步骤与伪时钟**，是什么就得是什么！
- **Codex 原生智能体补丁与 Diff 联动**：代码生成请求自动提取，转换为符合 Codex 原生语法的 `exec` 工具调用，生成 `*** Begin Patch ... *** End Patch` 补丁，直接触发 Codex 客户端工作区 Diff 对比并在右侧面板打开。
- **多账号号池智能调度 (Account Pool)**：支持配置多组 Prism 账号凭证，独立沙盒运行，基于 **Least-Connections (最小活跃并发优先)** 进行负载均衡与软会话黏性亲和。
- **套餐等级感知路由**：`gpt-6-astra` 严格路由给号池中的 Plus/Pro 付费账号，`gpt-5.6-sol` / `auto` 优先调度 Free 账号以节约付费配额。
- **无感故障转移 (Failover) 与自愈**：当某个账号遭遇 401 (过期) 或 429 (上游限流) 时，自动进入隔离/冷却状态并无缝将请求切换至下一个可用账号。
- **超强并发吞吐**：经实测，单账号单沙盒可稳定支撑 **50+ 真实并行会话** (100% 成功率且零 429 速率限制)；结合号池后可线性拓展数倍吞吐。
- **严格显式透传**：模型名称严格按请求参数向官方上游透传，零静默降级或偷换模型。
- **独家 102.4万 超大物理上下文**：实测支持单次 4,000 KB (1,024,000 Token) 超长输入无阶段衰减。
- **6档思考等级**：完整支持 `low`、`medium`、`high`、`xhigh`、`max`、`ultra` 全谱系深度思考。
- **内置网关 API Key 机制**：支持后台本地持久化存储 (`api_keys.json`)，支持 Web 控制台直接创建、复制与删除密钥。

---

## 项目规范目录结构

整理后的项目结构精简、模块化且生产就绪：

```text
d:\Project\TS
├── .gitignore              # Git 忽略配置（排除敏感凭证、运行时状态、日志等）
├── .env.example            # 环境变量配置模版
├── README.md               # 完整技术架构、接口协议与使用文档
├── server.js               # 核心服务网关（号池调度、协议转换、实时流处理、状态轮询）
├── index.html              # 核心 Web 控制台、API 管理大盘与对话窗口
├── accounts.example.json   # 号池多账号配置模版
├── api_keys.example.json   # 网关 API 密钥模版（多租户、额度与权限控制）
├── package.json            # 依赖与脚本配置
├── package-lock.json
│
└── scratch/                # 核心回归测试与压测套件
    ├── test_different_prompt.js     # 多技术栈提示词真实验证测试
    ├── test_cpp_prompt.js           # C++ 多语言代码生成实测
    ├── test_responses_live.js       # /v1/responses 真实事件流测试
    ├── test_compatibility_suite.js  # 全兼容性自动化套件
    ├── test_extreme_scenarios.js    # 边界与极限场景测试
    └── bench_concurrency.js         # 并发性能压力基准
```

## 多账号号池管理体系 (Account Pool)

为满足多用户高并发、配额互补以及故障隔离的需求，网关内置了生产级**多账号号池系统**：

### 1. 核心运行机制
- **数据持久化与自动迁移**：
  - 账号列表存储于 `accounts.json`；
  - 首次启动时，系统会自动将默认配置迁移入库作为首个主账号，保持绝对向后兼容。
- **自动 JWT 凭证解析**：
  - 用户粘贴任意账号的 `prism.openai.com` 完整 Cookie 时，系统会自动解码 JWT 载荷并提取：
    - `chatgpt_plan_type`（自动识别 `free`、`plus`、`pro` 等）；
    - 用户绑定邮箱与昵称；
    - 独立项目空间与用户唯一标识；
    - 凭证有效期限 (`exp`)。
- **账号级独立沙盒隔离**：
  - 每个账号维护专属的 YSweet 协同信道与 Resources 渲染凭证 (`accountSandboxes`)，彻底避免多账号之间的上下文串线与会话竞争。

### 2. 智能调度与软会话黏性 (Smart Soft Session Affinity)
针对团队多成员（如 10 人协作）并发调用场景，网关实现了 **「软会话黏性 + 负载均衡 + 故障自动漂移」** 综合算法：
1. **多级会话特征指纹识别 (Session Fingerprinting)**：
   - 客户端传入显式 `conversationId` 或 `session_id` 时，以该 ID 作为会话主键；
   - 对 NextChat、Chatbox、Cline 等标准客户端，提取每轮完整对话的首条用户提问计算哈希 `md5(apiKey + first_user_message)`，在多轮对话中形成稳定的会话线程指纹；
   - 保底按客户端 `API Key` 实现成员级轻量亲和绑定。
2. **多轮对话上下文连续性**：
   - 同一会话在 30 分钟生命周期内（TTL 30m，自驱定期释放），后续轮次**优先定向路由至同一物理账号及其独立沙盒环境**，最大化复用上游 Prefix/KV Cache，提升首字响应速度并保证项目文件状态连续；
   - 网关自动将前序历史对话聚合为结构化上下文，模型拥有 100% 连贯的多轮记忆。
3. **过载保护与健康感知**：
   - 若当前绑定的账号活跃并发数达到安全阈值（`activeTurns > 8`），自动解除单号绑定，由号池其他空闲节点分担；
   - 若绑定的账号遭遇 401（失效）或 429（限流），系统**立即解绑并透明漂移**至号池下一个健康可用账号，保障请求 100% 成功。
4. **套餐等级过滤 (Tier Restriction)**：
   - 当调用 `gpt-6-astra` 时，**仅筛选出 Plus/Pro 付费账号**；若号池中无付费账号则明确拦截并报错，防止向上游发送无效请求；
   - 当调用 `gpt-5.6-sol` 或 `auto` 时，**优先使用 Free 账号**，最大化保留付费账号的宝贵配额。
5. **最小并发连接优先 (Least-Connections)**：
   - 新会话优先分配给当前正在执行中并发会话数最少、且最久未被使用的账号。

### 3. 透明故障转移与自愈 (Failover & Cooldown)
- **401 凭证过期隔离**：若账号 Cookie 失效，自动将状态标记为 `expired` 并即刻从活跃候选池剥离。
- **429 速率限制冷却**：若上游触发限流，自动将账号置入 `cooling` 状态并设定 60 秒冷却倒计时。冷却期结束后自动复苏为 `active`。
- **无缝自动重试**：当选定账号因故失败时，网关透明且即时切换至下一个合格候选账号重试，上层客户端无需任何重试逻辑即可获得稳定响应。

### 4. 号池管理 API 接口
| 接口方法 | 路径 | 功能说明 |
| :--- | :--- | :--- |
| `GET` | `/api/accounts` | 获取号池全部账号列表（脱敏 Cookie，包含并发数与统计） |
| `POST` | `/api/accounts` | 向号池添加新账号（自动解析并验证 JWT） |
| `POST` | `/api/accounts/toggle` | 启用 / 停用指定账号 |
| `DELETE` | `/api/accounts` | 从号池中永久移除指定账号 |
| `POST` | `/api/accounts/test` | 对指定账号执行实时沙盒连通性测试 |

---

## 单账号高并发极限实测数据

为探究 Prism 单个官方账号在上游基础设施下的并发承载极限，我们在真实官方环境中进行了阶梯式压力实测（同一账号、同一沙盒、不同并发会话 `conversationId`）：

| 设定并发数 | 成功请求数 | 失败 / 限流数 | 总耗时 (s) | 平均单请求延迟 | 官方 429 限制 | 状态与表现 |
| :---: | :---: | :---: | :---: | :---: | :---: | :---: |
| **2** | 2 / 2 | 0 | 7.91s | ~7.9s | ❌ 无限制 | 纯并行执行，零阻塞 |
| **5** | 5 / 5 | 0 | 8.24s | ~8.2s | ❌ 无限制 | 毫秒级无感知排队 |
| **10** | 10 / 10 | 0 | 8.45s | ~8.4s | ❌ 无限制 | 纯并行，吞吐效率极高 |
| **20** | 20 / 20 | 0 | 8.77s | ~8.7s | ❌ 无限制 | 100% 成功，吞吐无衰减 |
| **30** | 30 / 30 | 0 | 12.28s | ~12.2s | ❌ 无限制 | 全部会话均获得完整回复 |
| **50** | **50 / 50** | **0** | **43.26s** | ~28.5s | **❌ 无限制 (零 429)** | **50 路会话全部 100% 成功返回！** |

> 📊 **压测核心结论**：
> 1. **单账号并发极大**：单个 Prism 账号在上游具有极其宽容的并发容量，一次性发起 **50 路完全并发请求** 依然保持 **100% 成功率**，没有触发任何 429 Rate Limit。
> 2. **独立会话隔离**：通过为每个并发请求动态生成唯一的 `conversationId`，可在单个账号沙盒内实现会话状态完全隔离，不会发生上下文串线。
> 3. **多账号号池倍增效应**：结合网关的 Least-Connections 号池调度器，拥有 $N$ 个账号即可轻松支撑 $50 \times N$ 以上的并行生产级请求吞吐。

---

## OpenAI Responses API 与 VS Code Codex 原生插件对接

网关原生实现了 OpenAI 专门为代码编辑器与智能体打造的 **Responses API 规范 (`/v1/responses` 与 `/responses`)**，可直接驱动 VS Code 官方 Codex 扩展插件与 Codex CLI，提供与官方完全一致的端到端交互体验：

### 1. 官方智能体调用规范与流式事件
- **事件流序列控制**：完整按照 OpenAI 官方 SSE 规范实现 `sequence_number` 严格递增序列机制；
- **全生命周期事件流**：
  - `response.created`：即刻创建响应上下文，保障首包零延迟，彻底杜绝客户端 `stream_open` 超时；
  - `response.output_item.added` / `done`：动态管理思维流与消息实体；
  - `response.reasoning_summary_part.added` / `delta` / `done`：实时推送深度推理思维链；
  - `response.custom_tool_call_input.delta` / `done`：分块流式传输工具调用脚本；
  - `response.completed`：交付完整响应对象及精确到个位的 Token 消耗统计。

### 2. 100% 真实原生思维流透传（“是什么就得是什么”）
- **拒绝虚假模拟**：彻底废弃所有预设静态数组与轮询伪时钟定时器，保证思维流与当前项目和提示词 100% 真实对应。
- **底层实时捕获机制**：
  - 在网关轮询官方沙盒状态 `/api/llm/response_with_tools_status` 期间，实时提取 `codex_live_progress.reasoningSummaries` 字段；
  - 上游模型在思考什么（例如定位文件路径、分析 Python 依赖、设计算法结构等），网关就原汁原味地向 Codex IDE 实时推送对应的思维链标题与内容；
  - 若当前任务不需要长链推理或上游尚未产生思维链，网关**绝不捏造任何虚假文本**，Codex 界面保持原生的思考加载状态。

### 3. 智能工作区补丁落地与原生 Diff 联动 (Apply Patch)
- **精准代码与文件名提取**：智能识别模型输出中的目标文件（支持 HTML、Python、JavaScript、TypeScript、C++ 等全语言）与代码块；
- **统一 Diff 补丁语法生成**：动态生成完全符合 Codex 官方标准格式的 `*** Begin Patch ... *** End Patch` 补丁内容；
- **原生双面板对比与落盘**：
  - 将操作封装为标准的 `exec` 工具调用；
  - 调用 `tools.mcp__codex_app__open_in_codex` 自动唤起 VS Code 右侧拆分编辑器展示实时 Diff 对比；
  - 配备 PowerShell 兜底写入逻辑，确保不论本地环境是否具备 `apply_patch` 解释器，文件均能 100% 安全精准落盘。

---

## Anthropic Claude Messages API (`/v1/messages`)

为了兼容偏好 Anthropic 规范的第三方 Agent 与客户端工具，网关内置了高性能协议转换引擎：
- **路径**：`POST /v1/messages`
- **支持特性**：
  - 请求头鉴权：支持 `x-api-key: sk-prism-...` 或 `Authorization: Bearer sk-prism-...`；
  - 自动将 Claude 格式的 `system`、`messages` (含多轮交互) 转换为底层统一模型输入格式；
  - 支持 SSE 流式传输 (`message_start`, `content_block_start`, `content_block_delta`, `content_block_stop`, `message_delta`, `message_stop`)；
  - 支持标准非流式响应。

---

## 多模态与绘图/看图能力实测说明

基于官方通信规范与沙盒执行机制，对 Prism 的多模态能力进行了系统验证与全面支持：

### 1. 图像输入 (Vision / 多模态看图) —— 真实支持且精度极高！
- **此前误区**：如果在请求体的 prompt 里直接塞入 OpenAI 格式的 base64 `input_image` 数组，官方网关会抛出 HTTP 500。
- **官方底层真实运作机制**：
  1. **二进制独立上传**：前端将原始图片以二进制形式直接 `POST /api/project-files/upload`，并附带请求头：
     ```http
     x-prism-project-id: <projectId>
     x-prism-file-id: <uuid>
     x-prism-file-name: <uuid>.png
     x-prism-require-project-edit-access: true
     ```
     文件将挂载在项目沙盒工作区的 `/prism-uploads/` 目录下。
  2. **会话关联与结构化挂载**：在发送给 `/api/llm/response_with_tools_start` 的载荷中，传递 `input_file` 数据结构：
     ```json
     {
       "type": "message",
       "role": "user",
       "content": [
         { "type": "input_text", "text": "这个图片中的人在干嘛啊？" },
         {
           "type": "input_file",
           "filename": "4301799a-21be-4fb9-9000-9f111aa8c926.png",
           "project_path": "/prism-uploads/4301799a-21be-4fb9-9000-9f111aa8c926.png"
         }
       ]
     }
     ```
  3. **沙盒原生视觉工具调用**：后台日志证实，模型在沙盒内自主调用了内置的专用多模态看图工具：
     ```javascript
     const r = await tools.view_image({
       path: "prism-uploads/4301799a-21be-4fb9-9000-9f111aa8c926.png",
       detail: "original"
     });
     ```
  4. **实测识别精度**：给它一张外卖骑手蹲在电动车旁的照片，模型立刻准确判断出：*“图片里的人蹲在电动车后轮旁，似乎正在检查或维修后轮、刹车一带的部件。”* 识别毫发毕现，零幻觉！
- **网关原生支持**：
  - 本项目 Web 控制台已完整集成 **📷 图片选择上传** 与 **全局剪贴板截图粘贴 (`Ctrl+V`)**，输入框实时缩略图预览与删除；
  - `/v1/chat/completions` 接口已兼容标准 OpenAI 多模态格式（包含 Base64 `image_url`）。

### 2. 图像生成 (Image Generation / 画图) —— 顶级智能体自省分析
- **实测表现**：向模型索求生成卡通动漫转绘图片时，模型表现出惊人的推理与自主工具探索能力：
  1. **读取系统技能**：模型首先调用内置技能定义 `sed -n '1,240p' /home/sandbox/.codex/skills/.system/imagegen/SKILL.md`；
  2. **工具自省内省**：执行 JS 探针 `text(ALL_TOOLS.filter(x => /image/i.test(x.name + " " + x.description)))`，发现当前沙盒仅暴露了 `view_image`，无直接 `generate_image`；
  3. **降级尝试 CLI Fallback**：阅读 `scripts/image_gen.py`，并自主在终端编写 Python 探针：
     ```python
     import importlib.util, os
     print('OPENAI_API_KEY=set' if os.getenv('OPENAI_API_KEY') else 'OPENAI_API_KEY=missing')
     print('openai_package=set' if importlib.util.find_spec('openai') else 'openai_package=missing')
     ```
     探测输出：`OPENAI_API_KEY=set`（官方沙箱预置了 API Key！），但 `openai_package=missing`；
  4. **严守安全规则**：由于 `AGENTS.md` 规则禁止用户私自安装依赖，模型没有进行非法提权或 `pip install`，而是非常严谨智能地向用户解释：*“CLI 所需的 openai 包未安装，而且项目规则明确禁止安装新依赖... 若后续内置图片生成工具，或预先配置好该包即可生成。”*
- **代码级制图支持**：模型深度支持使用 **TikZ 矢量绘图**、**SVG 矢量渲染** 以及沙盒内预装的 **matplotlib / Pillow** 输出高清科学图表。

---

## 模型支持与套餐权限矩阵

官方 Prism 上游对不同账号等级开放了不同的底层大模型权限，网关支持以下三种模式：

| 模型标识 (Model ID) | 适用账号套餐 | 上下文容量 | 官方上游调用说明 |
| :--- | :--- | :--- | :--- |
| **`gpt-5.6-sol`** | **Free (免费)**、Plus、Pro | **1,024,000 Tokens (4,000 KB)** | **Free 基础账号唯一可用**的满血高性能基座模型，拥有惊人的 1.024M 上下文，代码与逻辑推演极其强悍。 |
| **`gpt-6-astra`** | **Plus、Pro (仅限付费用户)** | 官方付费旗舰规格 | Prism 官方核心架构旗舰模型。**Free 账号调用会上游直接报错** (`400: Unsupported assistant model`)。 |
| **`auto`** | 全部账号 | 随官方分配 | **不向官方上游传递 `model` 字段**，由 Prism 上游路由系统根据账号当前订阅等级与负载策略自行分配最优模型。 |

> 📌 **注**：在 Web 界面中，当系统检测到当前账号为 **Free** 基础套餐时，下拉菜单将自动锁定并**置灰** `gpt-6-astra`，并提示 `🔒 gpt-6-astra (仅 Plus/Pro 付费账户可用)`，防止用户因误选而请求失败。

---

## 显式模型透传规范

本网关贯彻**官方透明代理原则**：

1. **指定即执行**：
   - 客户端请求 `model: "gpt-6-astra"` $\rightarrow$ 网关严格向上游透传 `metadata.model = "gpt-6-astra"`。
   - 客户端请求 `model: "gpt-5.6-sol"` $\rightarrow$ 网关严格向上游透传 `metadata.model = "gpt-5.6-sol"`。
   - 绝不在后台做任何隐式降级替换，如果账号权限不足（例如 Free 账号硬调 astra），将直接真实返回官方错误提示。
2. **自动模式 (`auto`)**：
   - 客户端请求 `model: "auto"` 或未指定模型时，网关在发往上游的数据包中彻底剥离 `metadata.model` 字段，交由官方自主路由。

---

## 思考强度 (Reasoning Effort) 等级

Prism 底层支持深度推理能力，网关完整暴露了 6 档思考等级：

| 档位名称 | 标识值 | 适用场景 | 响应特征 |
| :--- | :--- | :--- | :--- |
| **低思考** | `low` | 日常简短问答、代码语法查找、快速翻译 | 思考耗时最少 (约 3~8 秒)，快速首字输出 |
| **中等思考** | `medium` *(默认)* | 标准编程任务、多轮对话、逻辑说明 | 平衡思考质量与耗时 (约 8~18 秒) |
| **高阶推导** | `high` | 算法设计、系统架构分析、复杂 Bug 定位 | 深度递归推演，思维链充实完整 (约 15~35 秒) |
| **超高强度** | `xhigh` | 复杂数学证明、多步骤逻辑演绎推导、超长上下文总结 | 全量推理链路激活 (约 30~60 秒) |
| **极限推演** | `max` | 高难度跨学科论证、百万 Token 代码库宏观推演 | 极限挖掘模型推导能力 |
| **旗舰满血** | `ultra` | 顶级科研级严谨思考任务 | 最高权重资源分配 |

> 💡 **客户端调用传参**：在调用 `/v1/chat/completions` 时，可在请求体中加入 `"reasoning_effort": "high"` 或在模型名称后缀携带（如 `gpt-5.6-sol:high`）。

---

## 超大物理上下文压测结果

在严谨的物理极限压力测试中，`gpt-5.6-sol` 在所有思考强度等级下均展现了惊人且一致的超大规模上下文吞吐能力：

| 思考等级 (Reasoning Effort) | 测试输入数据量 | Token 预估折合 | 物理测试状态 | 最终响应状态 |
| :---: | :---: | :---: | :---: | :---: |
| **low** | **4,000 KB (3.91 MB)** | **~1,024,000 Tokens** | **100% 成功 (无衰减)** | HTTP 200 OK |
| **medium** | **4,000 KB (3.91 MB)** | **~1,024,000 Tokens** | **100% 成功 (无衰减)** | HTTP 200 OK |
| **high** | **4,000 KB (3.91 MB)** | **~1,024,000 Tokens** | **100% 成功 (无衰减)** | HTTP 200 OK |
| **xhigh** | **4,000 KB (3.91 MB)** | **~1,024,000 Tokens** | **100% 成功 (无衰减)** | HTTP 200 OK |
| **max** | **4,000 KB (3.91 MB)** | **~1,024,000 Tokens** | **100% 成功 (无衰减)** | HTTP 200 OK |
| **ultra** | **4,000 KB (3.91 MB)** | **~1,024,000 Tokens** | **100% 成功 (无衰减)** | HTTP 200 OK |

- **结论**：Prism 的 `gpt-5.6-sol` 真实物理输入上限达 **1.024M Tokens (4,000 KB)**，即使一次性塞入数百万字符的代码库，模型仍能进行端到端逻辑解析。详细测试数据见 [BENCHMARK_RESULTS.md](file:///d:/Project/TS/BENCHMARK_RESULTS.md)。

---

## API Key 鉴权体系与管理

网关内置独立的 API Key 认证机制，保障服务暴露在局域网或公网时的安全性：

1. **密钥存储**：
   - 密钥信息持久化存储在服务端 `api_keys.json` 文件中。
   - 服务初次启动时会自动创建一把默认主密钥 (`sk-prism-...`)。
2. **请求验证**：
   - 标准 OpenAI 认证格式：请求头携带 `Authorization: Bearer sk-prism-xxx`。
   - 兼容支持：亦可直接携带 `x-api-key: sk-prism-xxx`。
   - 未携带或无效密钥将严格阻断并返回标准 OpenAI 401 报错：
     ```json
     { "error": { "message": "Invalid or missing API key", "type": "invalid_request_error", "code": "invalid_api_key" } }
     ```
3. **Web 端密钥管理**：
   - 点击 Web 右上角 **「⚙ 设置 & API Key」**，进入管理面板：
     - 一键复制当前有效 API Key；
     - 自定义备注名称并生成新 Key；
     - 一键删除废弃 Key。

---

## 全景统计大盘、Token 分析、官方 API 计费折算与详细日志

网关内置企业级全链路可观测体系，实时追踪请求流量、各维度 Token 真实开销、官方标准价格折算与审计日志。

### 1. 真实 Token 指标与捕获机制
网关深度拦截上游原生 `token_count` 实时事件流，提供未经估算的真实官方 Token 指标：
- **`input_tokens` (输入 Token)**：上游实际接收的模型输入 Token 总量；
- **`cached_input_tokens` (KV 缓存命中)**：官方 Prefix Prompt Cache 命中的 Token（享受 50% 费率折扣）；
- **`cache_creation_tokens` (缓存创建/未命中)**：未命中缓存的新写入上下文；
- **`cache_hit_rate` (缓存命中率)**：`cached_input_tokens / input_tokens * 100%`；
- **`output_tokens` (输出 Token)**：模型生成的完整正文 Token；
- **`reasoning_output_tokens` (思考 Token)**：推理模型思维链消耗的 Token；
- **`total_tokens` (总消耗)**：`input_tokens + output_tokens`。

### 2. 官方 API 计费折算引擎 (Official Cost Engine)
严格按照 OpenAI 官方标准模型 API 费率换算（支持 **USD ($)** 与 **CNY (¥)** 双币种，按 1 USD = 7.20 CNY）：

| 模型 | 输入价格 (每 1M Tokens) | 缓存命中价格 (每 1M Tokens) | 输出价格 (含思考, 每 1M Tokens) | 缓存优惠 |
| :--- | :--- | :--- | :--- | :--- |
| **`gpt-5.6-sol`** | **$2.50** | **$1.25** | **$10.00** | **50% OFF** |
| **`gpt-6-astra`** | **$15.00** | **$7.50** | **$60.00** | **50% OFF** |

- **公式**：
  $$\text{USD 费用} = \frac{\text{未缓存输入}}{10^6} \times P_{\text{input}} + \frac{\text{缓存命中输入}}{10^6} \times P_{\text{cached}} + \frac{\text{输出}}{10^6} \times P_{\text{output}}$$
  $$\text{节省费用} = \frac{\text{缓存命中输入}}{10^6} \times (P_{\text{input}} - P_{\text{cached}})$$

### 3. 全时序性能与审计追踪
- **请求时序**：精确毫秒级时间戳、总耗时 (`durationMs`)；
- **首字响应延迟 (TTFT)**：从网关接收入站请求到上游返回首个 Token / 思考摘要的毫秒时差；
- **思考耗时 (`thoughtDurationMs`)**：模型深度思考阶段的实际用时；
- **多维来源识别**：
  - `web_client`：内置 Web 控制台发起的交互；
  - `api_client`：第三方应用（如 NextChat / CherryStudio / Cursor / Cline）通过 `/v1/chat/completions` 发起的交互；
- **客户端 IP 与鉴权追踪**：记录来源 IP 与使用的 API Key 备注名称；
- **调度号池账号**：记录实际承接处理请求的号池账号名称与方案类型。

### 4. 接口与数据持久化
- **本地持久化**：日志存储在 `request_logs.json`，服务重启后自动加载历史统计；
- **`GET /api/logs`**：查询全局大盘 KPI + 详细日志，支持 `status`、`source`、`model` 过滤；
- **`DELETE /api/logs`**：清空日志记录并重置大盘统计；
- **`GET /api/logs/export`**：一键导出并下载 `prism_gateway_logs.json` 完整审计文件。

### 5. Web 可视化监控大盘
- 点击 Web 顶栏 **「📊 统计与日志」** 即可打开全景监控弹窗；
- 提供 **5 大 KPI 统计卡片**（请求与成功率、Token 消耗构成、KV 缓存加速与节省、延迟与首字、官方等效计费）；
- 实时日志明细表格支持**点击展开**查看：提示词前瞻、模型回复、Token 树状分解、官方费率计算拆解公式及客户端 IP。

---

## 快速开始与启动

### 1. 安装依赖与启动服务
```bash
# 进入项目目录
cd d:/Project/TS

# 启动网关服务
node server.js
```
控制台将输出服务启动状态、当前生效的 API Key 及登录套餐信息：
```text
============================================================
  ✦ Prism AI Gateway & OpenAI-Compatible Proxy (Port 3000)
  ✓ Account Plan  : FREE (gpt-5.6-sol available, gpt-6-astra requires Plus/Pro)
  ✓ API Key Auth  : Enabled (1 active keys)
============================================================
```

### 2. 访问 Web 原生控制台
在浏览器中打开：
```
http://localhost:3000
```
- 首次使用可在设置中粘贴 Prism 登录 Cookie（从 `prism.openai.com` 开发者工具 Network 选项卡复制）。
- 服务会自动解析 Cookie 中的 JWT 并展示您的套餐状态。

---

## 第三方客户端接入指南

由于网关完全兼容 OpenAI 官方接口规范与 Responses API 智能体协议，市面所有支持自定义 Base URL 的客户端及 VS Code 扩展均可直接无缝对接：

### 0. VS Code 官方 Codex 插件
- **服务地址 (Base URL)**: `http://localhost:3000` (或 `http://localhost:3000/v1`)
- **API Key**: 填入网关生成的有效 Key (如 `sk-prism-your-api-key-here`)
- **模型 (Model)**: `gpt-5.6-sol` (免费号推荐基座) 或 `gpt-6-astra` (付费号专属旗舰)
- **核心体验与效果**：
  - 侧边栏自然语言交互，智能感知当前打开的项目文件与工作区；
  - 底部**实时流式呈现模型真实思考过程**（如评估需求、分析结构、检索依赖等）；
  - 生成或修改文件时，自动在右侧拆分窗口唤起原生 Diff 对比面板，直观审阅代码变更并自动精准落盘。

### 1. NextChat (ChatGPT-Next-Web)
- **接口地址 (Base URL)**: `http://localhost:3000` (部分版本填 `http://localhost:3000/v1`)
- **API Key**: 填入后台生成的 Key (如 `sk-prism-your-api-key-here`)
- **自定义模型**: 填入 `gpt-5.6-sol`、`gpt-6-astra` 或 `auto`

### 2. Chatbox
- **AI 提供商**: 选择 `OpenAI API`
- **API 域名**: `http://localhost:3000`
- **API Key**: 填入网关 API Key
- **模型**: 选择或手动输入 `gpt-5.6-sol`

### 3. Cherry Studio
- **提供商**: 添加自定义提供商 (OpenAI 协议)
- **Base URL**: `http://localhost:3000/v1`
- **API 密钥**: 填入网关 API Key
- **模型列表**: 点击获取或手动添加 `gpt-5.6-sol`, `gpt-6-astra`

### 4. Cline / VSCode 插件
- **API Provider**: `OpenAI Compatible`
- **Base URL**: `http://localhost:3000/v1`
- **API Key**: `sk-prism-...`
- **Model ID**: `gpt-5.6-sol`

### 5. cURL 命令行调用

#### 流式对话 (Stream)
```bash
curl http://localhost:3000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-prism-your-api-key-here" \
  -d '{
    "model": "gpt-5.6-sol",
    "messages": [
      {"role": "user", "content": "请用Python写一个快速排序"}
    ],
    "stream": true,
    "reasoning_effort": "medium"
  }'
```

#### 非流式对话 (Non-Stream)
```bash
curl http://localhost:3000/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer sk-prism-your-api-key-here" \
  -d '{
    "model": "gpt-5.6-sol",
    "messages": [
      {"role": "user", "content": "测试网关连通性"}
    ],
    "stream": false
  }'
```

#### 获取模型列表
```bash
curl http://localhost:3000/v1/models \
  -H "Authorization: Bearer sk-prism-your-api-key-here"
```

---

## Web 控制台功能与套餐自动感知

网关前端不仅提供舒适的沉浸式对话界面，还包含以下智能特性：

1. **实时套餐徽标**：
   - 顶部导航栏直观显示当前账号状态（如 `Free 账户` 或 `PLUS 付费账户`）。
2. **模型智能置灰与提示**：
   - 当检测为 `Free` 账户时：`gpt-6-astra` 选项自动禁用并附带锁图标 `🔒`，防止因无权调用而引发 400 报错。
   - 当检测为 `Plus` / `Pro` 账户时：自动解锁 `👑 gpt-6-astra` 选项。
3. **可视化 API 密钥中心**：
   - 位于设置弹窗中，便于用户随时生成提供给各种 CLI / Agent 客户端使用的专属 Token。

---

## 常见问题与说明

**Q: 为什么我用 Free 账号发送 gpt-6-astra 提示错误？**  
A: 这是 OpenAI 官方 Prism 上游服务端强加的权限校验（返回 `400: Unsupported assistant model`）。Free 账号请务必选用 `gpt-5.6-sol`，其不仅支持 1.024M 物理上下文，代码与分析推理能力同样极其强大。

**Q: 为什么选择 auto 模式？**  
A: `auto` 模式将完全不显式传递模型名称，让 Prism 官方服务根据当前服务器负载与您的账户类型自动调度最匹配的模型。

**Q: 如何更新或更换 Cookie？**  
A: 点击右上角「⚙ 设置 & API Key」，在 Cookie 输入框中替换为最新 Cookie 即可即时生效，无需重启服务。
