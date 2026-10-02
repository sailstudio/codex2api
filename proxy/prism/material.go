package prism

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

// ============================================================
// 对话材料包（chat material）。
//
// 上游自 2026-09-19 起把 start 与「页面上下文」强绑定：
//   - 自建会话 + 自铸沙箱的 body → 400 "Please submit prompt again"
//   - 必须带 metadata.proxy_request_debug 与 codex_listen_snapshot
//     （由浏览器页面产生，含 wait-for-sync 请求详情与沙箱会话绑定快照）
//
// 但材料**可跨会话复用**（实测：新材料 + 全新 conversationId + 新 input
// → 200 "started"）。因此一份材料可服务全池，只需低频刷新。
//
// 三条硬约束（都做过 A/B 实测）：
//  1. metadata.sandbox_* 必须与材料**同源**——不能自铸新沙箱（否则 400）
//  2. conversationId 必须每次全新（复用材料里的 conv → 403）
//  3. sentinel 必须每次现铸（复用 → 403）
//
// 材料由浏览器侧车产出（cmd/prism-material）：登录态打开项目页 → 发送一条消息
// → 拦截 start 请求偷走 body → 写盘。网关只读文件，推理路径不碰浏览器。
// ============================================================

// Material 是一份对话材料。
type Material struct {
	// Metadata 是原样保留的 metadata（含 proxy_request_debug / codex_listen_snapshot
	// / sandbox_url / sandbox_token）。这些字段必须原样回传，不可改写。
	Metadata map[string]any
	// InputPrefix 是材料里的 system 前缀（Prism Chat 系统提示词 + 上下文块），
	// 让模型保持 Prism 内的行为约定。
	InputPrefix []map[string]any
	// ProjectID / UserID 是材料绑定的项目与用户（复用以保证身份一致）。
	ProjectID string
	UserID    string
	// Model 是材料里的模型名（仅作默认值）。
	Model string
	// CapturedAt 是材料捕获时间（判 TTL）。
	CapturedAt time.Time
}

// matStore 是材料仓库：单份 + 到点重载。
//
// ⚠️ 必须是**每个 Client 一份**（不是包级全局）：PrismClientFor 按账号缓存
// client，若共享全局材料，第二个账号会拿到第一个账号的材料 →
// 跨账号身份错配（用错沙箱/projectId），上游会 400/403。
type materialStore struct {
	mu   sync.RWMutex
	mat  *Material
	path string
	ttl  time.Duration
}

// matOnce 已移除：材料仓库改为挂在 Client 上（避免跨账号串扰）。

// material 取当前材料；过期或缺文件则报错（由调用方决定是否降级）。
//
// 仓库挂在 Client 上（见 materialStore 注释），因此多账号互不串扰。
func (c *Client) material(ctx context.Context) (*Material, error) {
	path := envOr("PRISM_MATERIAL_PATH", c.cfg.MaterialPath)
	if path == "" {
		path = "/tmp/prism_sidecar/material.json"
	}
	ttl := c.cfg.MaterialTTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	c.materialOnce.Do(func() { c.matStore = &materialStore{path: path, ttl: ttl} })
	matStore := c.matStore

	matStore.mu.RLock()
	if matStore.mat != nil && time.Since(matStore.mat.CapturedAt) <= matStore.ttl {
		m := matStore.mat
		matStore.mu.RUnlock()
		return m, nil
	}
	matStore.mu.RUnlock()

	matStore.mu.Lock()
	defer matStore.mu.Unlock()
	if matStore.mat != nil && time.Since(matStore.mat.CapturedAt) <= matStore.ttl {
		return matStore.mat, nil
	}
	m, err := loadMaterial(matStore.path)
	if err != nil {
		// 文件读不到时，退而用内存里那份（哪怕已过 TTL，也比重建沙箱更接近可用）。
		if matStore.mat != nil {
			c.cfg.Logf("prism: 材料刷新失败(%v)，回退内存副本（age=%s）", err, time.Since(matStore.mat.CapturedAt))
			return matStore.mat, nil
		}
		return nil, err
	}
	matStore.mat = m
	c.cfg.Logf("prism: 材料已加载 project=%s user=%s input_prefix=%d 条",
		m.ProjectID, m.UserID, len(m.InputPrefix))
	return m, nil
}

// loadMaterial 从侧车写出的 JSON 读一份材料。
//
// 支持两种落盘形态：
//
//	A. 原始捕获（{start:{postData:"..."}}）——侧车直接写 playwrite 捕获结果
//	B. 归一化（{metadata:{...}, input:[...], captured_at:"..."}）
func loadMaterial(path string) (*Material, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}

	var bodyStr string
	if st, ok := doc["start"].(map[string]any); ok {
		bodyStr = strOf(st["postData"])
		if bodyStr == "" {
			bodyStr = strOf(st["body"])
		}
	}
	var parsed map[string]any
	if bodyStr != "" {
		if err := json.Unmarshal([]byte(bodyStr), &parsed); err != nil {
			return nil, err
		}
	} else {
		parsed = doc // 形态 B：本身就是 body
	}

	md, _ := parsed["metadata"].(map[string]any)
	if md == nil {
		return nil, errNoMaterial("metadata 缺失")
	}
	if strOf(md["sandbox_token"]) == "" || strOf(md["sandbox_url"]) == "" {
		return nil, errNoMaterial("metadata 缺 sandbox_url/sandbox_token")
	}

	m := &Material{Metadata: md, CapturedAt: time.Now()}
	if t := strOf(doc["captured_at"]); t != "" {
		if ts, err := time.Parse(time.RFC3339, t); err == nil {
			m.CapturedAt = ts
		}
	}
	m.ProjectID = strOf(md["projectId"])
	m.UserID = strOf(md["userId"])
	m.Model = strOf(md["model"])

	// 抽 system 前缀：只取**开头连续的 system 段**。
	//
	// input 的规范形状是 [system(Prism 提示词), system(上下文块), user(预热), …]，
	// 故「首个非 system 之前的所有 system」即干净前缀。
	//
	// ⚠️ 必须在此截断：材料由侧车周期性从**同一个会话**捕获，每轮会把历史
	// 再带一遍（实测累积到 32 份重复，材料 1KB→30KB，每轮白送 23K 字符，
	// 请求越来越慢甚至超时）。后续重复块一律丢弃。
	if arr, ok := parsed["input"].([]any); ok {
		for _, it := range arr {
			im, _ := it.(map[string]any)
			if im == nil {
				continue
			}
			if strOf(im["role"]) != "system" {
				break // 前缀到此结束
			}
			m.InputPrefix = append(m.InputPrefix, im)
		}
	}
	if m.ProjectID == "" {
		return nil, errNoMaterial("metadata 缺 projectId")
	}
	return m, nil
}

type materialError struct{ msg string }

func (e materialError) Error() string {
	return "prism: 材料不可用（" + e.msg + "）；请先用浏览器侧车刷新（cmd/prism-material）"
}
func errNoMaterial(m string) error { return materialError{m} }

// MaterialInfo 供观测端点展示材料状态。首次调用会惰性加载材料。
func (c *Client) MaterialInfo() map[string]any {
	path := envOr("PRISM_MATERIAL_PATH", c.cfg.MaterialPath)
	ttl := c.cfg.MaterialTTL
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	c.materialOnce.Do(func() { c.matStore = &materialStore{path: path, ttl: ttl} })
	matStore := c.matStore
	// 惰性加载（失败不报错，状态里体现 loaded=false）。
	_, _ = c.material(context.Background())

	matStore.mu.RLock()
	defer matStore.mu.RUnlock()
	out := map[string]any{"path": path, "loaded": matStore.mat != nil}
	if matStore.mat != nil {
		out["project_id"] = matStore.mat.ProjectID
		out["user_id"] = matStore.mat.UserID
		out["age_seconds"] = int(time.Since(matStore.mat.CapturedAt).Seconds())
		out["ttl_seconds"] = int(matStore.ttl.Seconds())
		out["fresh"] = time.Since(matStore.mat.CapturedAt) <= matStore.ttl
	}
	return out
}

// ------------------------------------------------------------------ 辅助

func sysMsg(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "system",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}

// toolProtocol 生成工具仿真提示词（上游无客户端 function calling 通道）。
func toolProtocol(tools []map[string]any) string {
	var sb strings.Builder
	sb.WriteString("You are the inference component of a LOCAL client.\n")
	sb.WriteString("Tool definitions below describe tools on the USER'S LOCAL MACHINE. Only the client can execute them.\n")
	sb.WriteString("Do NOT execute them yourself, and do not claim results before receiving tool outputs.\n\n")
	sb.WriteString("To request a tool, reply with ONLY one JSON object on its own line inside this envelope:\n")
	sb.WriteString("<tool_call>{\"name\":\"<exact tool name>\",\"arguments\":{...}}</tool_call>\n")
	sb.WriteString("To request several tools, emit several <tool_call> blocks (each on its own line).\n")
	sb.WriteString("For a final answer, reply with plain text and no <tool_call> blocks.\n")
	sb.WriteString("Function arguments MUST satisfy the supplied JSON Schema exactly.\n\n")
	sb.WriteString("Available tools:\n")
	for _, t := range tools {
		name := strOf(t["name"])
		if name == "" {
			continue
		}
		b, _ := json.Marshal(t)
		sb.WriteString("- " + name + ": " + truncStr(string(b), 1200) + "\n")
	}
	return sb.String()
}

// extractToolCalls 从正文里解析 <tool_call> 信封，并返回去掉信封的正文。
func extractToolCalls(text string) ([]ToolCall, string) {
	const open, close = "<tool_call>", "</tool_call>"
	var calls []ToolCall
	rest := text
	for {
		i := strings.Index(rest, open)
		if i < 0 {
			break
		}
		j := strings.Index(rest[i:], close)
		if j < 0 {
			break
		}
		seg := rest[i+len(open) : i+j]
		var obj struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(seg)), &obj); err == nil && obj.Name != "" {
			args := strings.TrimSpace(string(obj.Arguments))
			if args == "" || args == "null" {
				args = "{}"
			}
			calls = append(calls, ToolCall{
				CallID: "call_" + newUUID4(), Name: obj.Name, Arguments: args,
			})
		}
		rest = rest[:i] + rest[i+j+len(close):]
	}
	return calls, strings.TrimSpace(rest)
}

// isCustomTool 判断是否为自定义工具（走 custom_tool_call 事件）。
func isCustomTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "apply_patch", "shell", "exec", "exec_command":
		return true
	}
	return false
}

var _ = context.Background
