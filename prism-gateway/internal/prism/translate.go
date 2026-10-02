package prism

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ============================================================================
// 上下文 → 上游 input 的翻译。
//
// 上游语义（多份真机实测一致，全部踩过坑）：
//  1. 上游只把**最后一条 user 消息**当本轮请求，其余 system 项是上下文。
//  2. assistant/user 历史不会自动带上，必须折叠成文本，否则模型看不到上下文。
//  3. 不声明 tools 时 system 通道生效；**声明了 tools 上游就不采信 system 内容**
//     （实测同一句 system 指令不带 tools 生效、带上 tools 就被忽略）——此时所有
//     指令/历史/工具结果必须并进「最后一条 user 消息」。
//  4. 多条 system 并存时最后一条会盖掉前面的，所以一律合并成**一条**。
// ============================================================================

// Role 归一化后的角色。
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Message 是归一化后的会话消息（各协议先统一到这里）。
type Message struct {
	Role       string
	Content    string
	Files      []Attachment
	ToolCalls  []ToolCallRef // assistant 消息里发起的调用
	ToolCallID string        // tool 消息回灌时对应的 call id
	Name       string        // tool 消息的工具名（客户端常不带，靠 ToolCallID 反查）
}

// ToolCallRef 是历史里的一次工具调用（用于把结果归属到工具名）。
type ToolCallRef struct {
	ID        string
	Name      string
	Arguments string
}

// BuildInput 把整段对话摊平成上游 input 数组。
//
//	hasTools=false → 一条 system item（所有前文合并）+ 一条 user item
//	hasTools=true  → 只有一条 user item（含 [系统指令]/[客户端指令]/[对话历史]/
//	                 [已执行工具的结果]/[工具协议]/[用户当前消息] 六段块）
func BuildInput(msgs []Message, tools []ToolSpec, systemPrompt string,
	budget AttachmentBudget) []InputItem {

	out := make([]InputItem, 0, len(msgs)+2)
	sysTexts := make([]string, 0, 2)
	transcript := make([]string, 0, len(msgs))
	toolResults := make([]string, 0, 4)
	lastUser := ""
	lastUserIdx := -1
	var lastUserFiles []Attachment

	// 从后往前找本轮请求（图片-only 的请求没有文本，但同样是本轮请求）。
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Role == RoleUser && (strings.TrimSpace(m.Content) != "" || len(m.Files) > 0) {
			lastUserIdx = i
			lastUser = strings.TrimSpace(m.Content)
			lastUserFiles = m.Files
			break
		}
	}

	// 附件预算：本轮 user 优先，再往历史回填。
	b := budget
	lastUserBlocks := RenderBlocks(lastUserFiles, &b, 0)
	historyBlocks := make(map[int][]string, 2)
	for i := lastUserIdx - 1; i >= 0; i-- {
		if len(msgs[i].Files) > 0 && b.TotalChars > 0 {
			historyBlocks[i] = RenderBlocks(msgs[i].Files, &b, i)
		}
	}

	// 工具结果归属：优先按 tool_call_id 匹配，客户端没带 id 时按出现顺序认领。
	pending := make([]ToolCallRef, 0, 4)
	for _, m := range msgs {
		pending = append(pending, m.ToolCalls...)
	}
	claimToolName := func(id string) string {
		id = strings.TrimSpace(id)
		if id != "" {
			for i, tc := range pending {
				if tc.ID == id {
					pending = append(pending[:i], pending[i+1:]...)
					return tc.Name
				}
			}
		}
		for i, tc := range pending {
			if tc.Name == "" {
				continue
			}
			pending = append(pending[:i], pending[i+1:]...)
			return tc.Name
		}
		return ""
	}

	for i, m := range msgs {
		text := strings.TrimSpace(m.Content)
		switch {
		case m.Role == RoleSystem:
			if text != "" {
				sysTexts = append(sysTexts, text)
			}
		case i == lastUserIdx:
			// 本轮请求，最后再放。
		case m.Role == RoleAssistant:
			if text != "" {
				transcript = append(transcript, "Assistant: "+text)
			}
			for _, tc := range m.ToolCalls {
				if tc.Name != "" {
					transcript = append(transcript, "Assistant[tool_call]: "+tc.Name+" "+strings.TrimSpace(tc.Arguments))
				}
			}
		case m.Role == RoleTool:
			label := firstNonEmptyStr(strings.TrimSpace(m.Name), claimToolName(m.ToolCallID), "工具")
			if text == "" {
				text = "(空)"
			}
			toolResults = append(toolResults, "- "+label+": "+text)
		default: // user
			line := text
			if blocks := historyBlocks[i]; len(blocks) > 0 {
				line = strings.TrimSpace(line + "\n" + strings.Join(blocks, "\n\n"))
			} else if len(m.Files) > 0 {
				line = strings.TrimSpace(line + " " + FileMarkers(m.Files))
			}
			if line == "" {
				line = "(empty)"
			}
			transcript = append(transcript, "User: "+line)
		}
	}

	// ---- 带 tools：上游不采信 system，全部并进最后一条 user ----
	if len(tools) > 0 {
		blocks := make([]string, 0, 7)
		if sp := strings.TrimSpace(systemPrompt); sp != "" {
			blocks = append(blocks, "[系统指令]\n"+sp)
		}
		if len(sysTexts) > 0 {
			blocks = append(blocks, "[客户端指令]\n"+strings.Join(sysTexts, "\n\n"))
		}
		if len(transcript) > 0 {
			blocks = append(blocks, "[对话历史]\n"+strings.Join(transcript, "\n"))
		}
		// 已执行结果单独成块：混在[对话历史]里模型会声称"看不到返回结果"。
		if len(toolResults) > 0 {
			blocks = append(blocks, "[已执行工具的结果——客户端已在它自己的机器上真实执行，数据如下，直接用]\n"+
				strings.Join(toolResults, "\n")+
				"\n(不要回头用你自己的环境验证它，不要自己重做一遍，直接用上面的数据继续。)")
		}
		blocks = append(blocks, ToolContract(tools))
		if lastUser != "" {
			blocks = append(blocks, "[用户当前消息]\n"+lastUser)
		}
		blocks = append(blocks, lastUserBlocks...)
		out = append(out, messageItem(RoleUser, strings.Join(blocks, "\n\n")))
		return out
	}

	// ---- 不带 tools：所有前文合并成一条 system item ----
	blocks := make([]string, 0, 4)
	if sp := strings.TrimSpace(systemPrompt); sp != "" {
		blocks = append(blocks, sp)
	}
	if len(sysTexts) > 0 {
		blocks = append(blocks, "[客户端指令]\n"+strings.Join(sysTexts, "\n\n"))
	}
	if len(transcript) > 0 {
		blocks = append(blocks, "[对话历史]\n"+strings.Join(transcript, "\n"))
	}
	if len(toolResults) > 0 {
		blocks = append(blocks, "[已执行工具的结果]\n"+strings.Join(toolResults, "\n"))
	}
	if len(blocks) > 0 {
		out = append(out, messageItem(RoleSystem, strings.Join(blocks, "\n\n")))
	}
	if lastUser != "" || len(lastUserBlocks) > 0 {
		parts := make([]string, 0, len(lastUserBlocks)+1)
		if lastUser != "" {
			parts = append(parts, lastUser)
		}
		parts = append(parts, lastUserBlocks...)
		out = append(out, messageItem(RoleUser, strings.Join(parts, "\n\n")))
	}
	if len(out) == 0 {
		out = append(out, messageItem(RoleUser, "(empty)"))
	}
	return out
}

func messageItem(role, text string) InputItem {
	return InputItem{
		Type:    "message",
		Role:    role,
		Content: []ContentPart{{Type: "input_text", Text: text}},
	}
}

// ResolveModel 把客户端模型名解析成上游认的完整 ID + 思考档位。
// 上游只认白名单里的完整 ID：别名与带后缀的名字会原样被判 400。
func ResolveModel(catalog []ModelSpec, model, explicitEffort string) (serverModel, effort, clientID string) {
	m := strings.TrimSpace(model)
	base, suffix := ParseModelID(m)
	effort = normalizeEffort(explicitEffort)
	if effort == "" {
		effort = suffix
	}
	if effort == "" {
		effort = "medium"
	}
	clientID = m
	if m == "" {
		if len(catalog) > 0 {
			return catalog[0].ServerModelName, effort, catalog[0].ID
		}
		return "gpt-5.6-sol", effort, "gpt-5.6-sol"
	}
	for _, mi := range catalog {
		hit := mi.ID == m || mi.ID == base
		if !hit {
			for _, a := range mi.Aliases {
				if a == m || a == base {
					hit = true
					break
				}
			}
		}
		if hit {
			sm := strings.TrimSpace(mi.ServerModelName)
			if sm == "" {
				sm = mi.ID
			}
			return sm, effort, mi.ID
		}
	}
	// 目录外的名字原样发上去就是 400（sentinel 修好后立刻显形），兜底到默认模型。
	if len(catalog) > 0 {
		return catalog[0].ServerModelName, effort, catalog[0].ID
	}
	return m, effort, base
}

// ToolSpecsFromRaw 把协议的 tools 原始 JSON 归一化成 ToolSpec 列表。
// 兼容 OpenAI（{"type":"function","function":{...}}）与平铺（{"type":"function","name":...}）两种形状。
func ToolSpecsFromRaw(raw json.RawMessage) []ToolSpec {
	if len(raw) == 0 {
		return nil
	}
	var arr []map[string]any
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	out := make([]ToolSpec, 0, len(arr))
	for _, item := range arr {
		src := item
		if fn, ok := item["function"].(map[string]any); ok {
			src = fn
		}
		name, _ := src["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		desc, _ := src["description"].(string)
		var params json.RawMessage
		// 兼容两种 schema 键名：OpenAI 用 parameters，Anthropic 用 input_schema。
		for _, key := range []string{"parameters", "input_schema"} {
			if p, ok := src[key]; ok {
				if b, err := json.Marshal(p); err == nil {
					params = b
					break
				}
			}
		}
		out = append(out, ToolSpec{Name: name, Description: desc, Parameters: params})
	}
	return out
}

// ToolsToUpstreamShape 把客户端 tools 编成上游 Responses 形状（平铺、不套 function 层）。
// 上游会忽略它，但真机抓包显示浏览器页面会带上，故保持形状一致。
func ToolsToUpstreamShape(tools []ToolSpec) json.RawMessage {
	if len(tools) == 0 {
		return nil
	}
	arr := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		def := map[string]any{"type": "function", "name": t.Name}
		if strings.TrimSpace(t.Description) != "" {
			def["description"] = t.Description
		}
		if len(t.Parameters) > 0 {
			var schema any
			if json.Unmarshal(t.Parameters, &schema) == nil {
				def["parameters"] = schema
			}
		}
		arr = append(arr, def)
	}
	b, err := json.Marshal(arr)
	if err != nil {
		return nil
	}
	return b
}

// PrepareTurn 把「归一化消息 + 客户端 tools + 系统指令」编成一上游轮次。
func PrepareTurn(pool *Pool, lease *Lease, msgs []Message, tools []ToolSpec,
	systemPrompt string, model, effort, convID string, budget AttachmentBudget,
	catalog []ModelSpec) *Turn {

	serverModel, eff, _ := ResolveModel(catalog, model, effort)
	return &Turn{
		Input:          BuildInput(msgs, tools, systemPrompt, budget),
		ConversationID: convID,
		Model:          serverModel,
		Effort:         eff,
		Tools:          ToolsToUpstreamShape(tools),
	}
}

// ------------------------------------------------------------------ 便捷：一次完整轮次

// TurnRunner 封装「取号 → start → poll → 可选协议纠正重试」的完整流程。
type TurnRunner struct {
	Pool         *Pool
	Catalog      []ModelSpec
	Budget       AttachmentBudget
	SystemPrompt string
	Logf         func(format string, args ...any)
}

// Run 执行一轮（含模型白名单回退与协议纠正重试）。
// onProgress 在 pending 阶段回调（可做保活）。
func (r *TurnRunner) Run(ctx context.Context, msgs []Message, tools []ToolSpec,
	model, effort, convID string) (*TurnResult, *Client, error) {

	attempts := r.Pool.opt.Client.StartAttempts
	if attempts < 1 {
		attempts = 1
	}
	// 模型白名单回退：上游 400 Unsupported assistant model 时换下一个模型。
	modelCandidates := modelFallbacks(r.Catalog, model)

	var lastErr error
	for _, mdl := range modelCandidates {
		var result *TurnResult
		var used *Client
		err := r.Pool.Do(ctx, 2, func(cctx context.Context, c *Client) error {
			turn := PrepareTurn(r.Pool, nil, msgs, tools, r.SystemPrompt, mdl, effort, convID, r.Budget, r.Catalog)
			turn.ProjectID = ""
			st, err := c.StartTurn(cctx, turn)
			if err != nil {
				return err
			}
			res, err := c.PollTurn(cctx, st, nil)
			if err != nil {
				// 客户端断连：取消上游轮次省钱。
				if cctx.Err() != nil {
					_ = c.StopTurn(context.Background(), st, turn.ConversationID)
				}
				return err
			}
			result, used = res, c
			return nil
		})
		if err == nil {
			if result != nil && result.Err != "" {
				// 上游以 completed+error 形式返回：若是模型不支持，换下一个候选。
				if strings.Contains(strings.ToLower(result.Err), "unsupported assistant model") {
					lastErr = fmt.Errorf("%s", result.Err)
					continue
				}
				return result, used, nil
			}
			if used != nil {
				used.Account().NoteOK()
			}
			return result, used, nil
		}
		lastErr = err
		if !isModelUnsupported(err) {
			break
		}
	}
	return nil, nil, lastErr
}

// modelFallbacks 生成模型尝试顺序：首选 → 目录内其余（白名单会变，需要回退）。
func modelFallbacks(catalog []ModelSpec, model string) []string {
	server, _, _ := ResolveModel(catalog, model, "")
	out := []string{server}
	for _, mi := range catalog {
		sm := strings.TrimSpace(mi.ServerModelName)
		if sm == "" {
			sm = mi.ID
		}
		if sm != server {
			out = append(out, sm)
		}
	}
	return out
}

// isModelUnsupported 判断错误是否为「模型不在账号清单里」。
func isModelUnsupported(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unsupported assistant model") ||
		strings.Contains(s, "model not found")
}
