# Prism 通道 · 部署附件

Prism 通道本身**不修改官方构建流程** —— 用仓库根目录的官方 `Dockerfile` 原生构建即可：

```bash
docker build -t codex2api-prism:local .
```

本目录只放**部署期**的辅助脚本，不参与镜像构建。

| 文件 | 用途 |
|---|---|
| `verify.sh` | 一键冒烟：纯文本 + 带图（内置手写 PNG 生成器，不依赖 PIL） |

## 完整部署步骤

见 [`docs/prism-deployment.md`](../../docs/prism-deployment.md)。要点：

1. **宿主两个常驻侧车**（材料 + sentinel）—— 容器内既无真浏览器也无 node。
2. **原生构建镜像**：`docker build -t codex2api-prism:local .`（需 buildx）。
3. **起容器**：`docker compose -f docker-compose.sqlite.yml -f docker-compose.local-override.yml up -d`，
   或手动 `docker run`（环境变量见文档 §三）。
4. **建 prism 账号**：走专用端点 `POST /api/admin/accounts/prism`（普通 `/accounts` 建不出）。
5. **建 prism 渠道 Key**：`limits.upstream_channel=prism`（不限定则永远 503）。
6. **验证**：`KEY=<prism渠道Key> bash deploy/prism/verify.sh`

## 本机踩过的两个环境坑（与 patch 无关，但会伪装成"构建失败"）

| 症状 | 根因 | 处理 |
|---|---|---|
| `failed to parse platform : "" is an invalid OS component` | 缺 buildx，官方 Dockerfile 的 BuildKit 语法无法解析 | `brew install docker-buildx` + 软链到 `~/.docker/cli-plugins/` |
| 构建期 `apk`/`npm` 报 `connection refused`（像"镜像源挂了"） | Docker CLI 或 Lima 把宿主代理注入构建容器，容器内不可达 | 删 `~/.docker/config.json` 的 `proxies` 块；`~/.colima/_lima/_config/override.yaml` 写 `propagateProxyEnv: false` 后重启 colima |
