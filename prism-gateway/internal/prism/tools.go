package prism

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// ============================================================================
// 工具调用仿真。
//
// 上游事实：**没有客户端 function calling 通道**。真机实测（tools 平铺 Responses
// 形状与 Chat-Completions 嵌套形状各发一次）：上游返回的 payload.output 只有
// message/reasoning item，模型完全无视 tools 声明；手动塞 tools 后模型回
// no_tool_available。工具清单在服务端，作用域在沙箱容器里。
//
// 因此工具只能靠「协议约定 + 文本解析」仿真：
//
//	客户端 tools[]            → 注入协议说明 + JSON Schema
//	模型输出 <tool_call>…</tool_call> → 解析成标准 tool_calls / tool_use
//	客户端回灌 role:"tool"    → 折进上下文再发下一轮
//
// 一次回复出现多个块 = 并行调用；回灌结果后模型再输出块 = 连续多轮调用。
// ============================================================================

const (
	toolCallOpen  = "<tool_call>"
	toolCallClose = "</tool_call>"
)

// toolCallRe 匹配信封；用非贪婪 .*? 到最近的 </tool_call>。
var toolCallRe = regexp.MustCompile(`(?is)<tool_call>\s*(\{.*?\})\s*</tool_call>`)

// ToolSpec 是客户端声明的一个工具。
type ToolSpec struct {
	Name        string
	Description string
	Parameters  json.RawMessage
}

// ToolContract 生成注入给模型的工具协议说明。
func ToolContract(tools []ToolSpec) string {
	if len(tools) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[工具协议] 宿主程序在本轮向你提供了下列工具。你需要工具返回的数据时，必须" +
		"在回复里输出工具调用块，格式严格如下（一行一个，JSON 必须是合法 JSON）：\n")
	sb.WriteString(toolCallOpen)
	sb.WriteString(`{"name":"工具名","arguments":{"参数名":值}}`)
	sb.WriteString(toolCallClose)

	name := firstToolName(tools)
	key := firstArgKey(tools)
	sb.WriteString("\n例如（必须用下面「可用工具」里的真实名字，不要改名、不要编新名字；示例里的参数值只是占位，" +
		"实际值必须按用户消息填写，严禁照抄）：\n")
	sb.WriteString(toolCallOpen)
	sb.WriteString(fmt.Sprintf(`{"name":%s,"arguments":{%s:%s}}`,
		strconv.Quote(name), strconv.Quote(key), strconv.Quote(exampleArgValue(key))))
	sb.WriteString(toolCallClose)

	sb.WriteString("\n\n可用工具：\n")
	for i, t := range tools {
		n := strings.TrimSpace(t.Name)
		if n == "" {
			continue
		}
		fmt.Fprintf(&sb, "%d. %s", i+1, n)
		if d := strings.TrimSpace(t.Description); d != "" {
			sb.WriteString(" —— " + d)
		}
		sb.WriteString("\n")
		if len(t.Parameters) > 0 {
			schema := strings.TrimSpace(string(t.Parameters))
			if len(schema) > 2000 {
				schema = schema[:2000]
			}
			sb.WriteString("   参数 JSON Schema：" + schema + "\n")
		}
	}

	sb.WriteString("\n规则：\n" +
		"- 需要工具数据就调用，不要凭记忆猜测；能直接回答就不调用。\n" +
		"- 上面「可用工具」由宿主程序提供且已就绪，与你自身环境里的内置工具无关：\n" +
		"  不要检查你是否内置了它们，不要以“未提供该工具”为由拒绝，不要在本地环境代为执行——\n" +
		"  你的执行结果客户端收不到；只有输出工具调用块，客户端才会执行并把结果带回来。\n" +
		"- 路径与文件由客户端判断：路径在工作区内还是外、文件存在与否、是否可写，\n" +
		"  全部由客户端在它自己的机器上验证——你不要审查路径，不要以“工作区外/不可写/文件不存在”为由拒绝；\n" +
		"  创建、写入、修改客户文件也照常发起调用。\n" +
		"- 交付物（代码/网页/文档）的完整内容必须写在回复正文里，禁止写进你环境里的文件后声称“已创建/已保存”——\n" +
		"  客户端收不到你环境里的文件，只收得到正文和工具调用块。\n" +
		"- 一次回复可以输出多个工具调用块（并行调用），也可以一个都不输出。\n" +
		"- 调用块之外可以写一句简短说明，但不要重复块里的 JSON。\n" +
		"- 宿主已经执行过的工具结果会出现在名为「[已执行工具的结果]」的段落里，格式为 `- 工具名: 内容`。\n" +
		"- 看到该段落就直接使用里面的数据作答，不要声称看不到结果，也不要为同样的数据重复调用工具。\n" +
		"- 只有「[已执行工具的结果]」里没有的数据才需要发起新的调用；拿到结果前不要编造结果。\n" +
		"- 需要多步时：先输出当前这一步的调用，等结果返回后再决定下一轮调用。\n")
	return sb.String()
}

// ToolReinforce 生成协议纠正块（模型翻了自己环境、没走工具协议时的同账号重试）。
func ToolReinforce(tools []ToolSpec, prevText string) string {
	if len(tools) == 0 {
		return ""
	}
	name := firstToolName(tools)
	key := firstArgKey(tools)
	var sb strings.Builder
	sb.WriteString("[协议纠正] 你上一轮没有输出任何工具调用块——这是协议违规。")
	if p := strings.TrimSpace(prevText); p != "" {
		sb.WriteString("你上一轮的回复是：\n「" + truncateRunes(p, 300) + "」\n")
	}
	sb.WriteString("这轮回复里，你在自己的沙箱环境里执行了动作，或声称工具未提供/文件不存在：" +
		"你的沙箱与客户端完全隔离，你在里面的任何执行、读取、写入客户端都收不到；" +
		"上面「可用工具」由客户端真实提供、此刻即可调用。\n" +
		"现在重新回答「用户当前消息」，只允许输出一个工具调用块，不要输出任何其他内容（参数值按用户消息填写）：\n")
	sb.WriteString(toolCallOpen + fmt.Sprintf(`{"name":%s,"arguments":{%s:%s}}`,
		strconv.Quote(name), strconv.Quote(key), strconv.Quote(exampleArgValue(key))) + toolCallClose)
	return sb.String()
}

// firstToolName 取首个客户端工具名（具名示例把名字钉死，避免模型按沙箱习惯改名）。
func firstToolName(tools []ToolSpec) string {
	for _, t := range tools {
		if n := strings.TrimSpace(t.Name); n != "" {
			return n
		}
	}
	return "工具名"
}

// firstArgKey 取首个工具 schema 的必填首键，让示例形状与真实 schema 一致。
func firstArgKey(tools []ToolSpec) string {
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		var spec struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if json.Unmarshal(t.Parameters, &spec) == nil {
			if len(spec.Required) > 0 {
				return spec.Required[0]
			}
			for k := range spec.Properties {
				return k
			}
		}
		break
	}
	return "path"
}

// exampleArgValue 给协议示例选一个形状真实的占位（形状错的示例会误导模型照抄）。
func exampleArgValue(key string) string {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "command", "cmd":
		return "ls -la"
	case "path", "file_path", "filepath", "file", "filename":
		return "README.md"
	case "content", "text", "data":
		return "hello"
	case "query", "q", "keyword":
		return "示例查询"
	case "city":
		return "北京"
	}
	return "值"
}

// EmulatedCall 是从模型正文里解析出的工具调用。
type EmulatedCall struct {
	Name      string
	Arguments string // JSON 字符串
	ID        string
}

// ExtractToolCalls 从模型正文里抽出工具调用块，并返回剥掉这些块之后的正文。
// 解析失败的块按普通文本保留（宁可让客户端看到原文，也不要吞掉内容）。
func ExtractToolCalls(text string) ([]EmulatedCall, string) {
	if !strings.Contains(text, toolCallOpen) {
		return nil, text
	}
	var calls []EmulatedCall
	kept := text
	for _, m := range toolCallRe.FindAllStringSubmatch(text, -1) {
		name, args, ok := parseToolCallJSON(strings.TrimSpace(m[1]))
		if !ok {
			continue
		}
		calls = append(calls, EmulatedCall{Name: name, Arguments: args, ID: NewCallID()})
		kept = strings.Replace(kept, m[0], "", 1)
	}
	if len(calls) == 0 {
		return nil, text
	}
	return calls, cleanAfterStrip(kept)
}

// parseToolCallJSON 兼容 {name,arguments} 与 {tool,arguments}，
// arguments 既可以是对象也可以是对象字符串。
func parseToolCallJSON(raw string) (string, string, bool) {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", "", false
	}
	name := strings.TrimSpace(strAt(m, "name"))
	if name == "" {
		name = strings.TrimSpace(strAt(m, "tool"))
	}
	if name == "" {
		return "", "", false
	}
	args := "{}"
	switch v := m["arguments"].(type) {
	case map[string]any:
		if b, err := json.Marshal(v); err == nil {
			args = string(b)
		}
	case string:
		s := strings.TrimSpace(v)
		switch {
		case s != "" && json.Valid([]byte(s)):
			args = s
		case s != "":
			if b, err := json.Marshal(map[string]any{"input": s}); err == nil {
				args = string(b)
			}
		}
	case nil:
	default:
		if b, err := json.Marshal(map[string]any{"value": v}); err == nil {
			args = string(b)
		}
	}
	return name, args, true
}

// cleanAfterStrip 剥掉调用块后收拾残留空白。
func cleanAfterStrip(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		if strings.TrimSpace(ln) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// truncateRunes 按 rune 截断（中文不拦腰）。
func truncateRunes(s string, maxLen int) string {
	if rs := []rune(s); len(rs) > maxLen {
		return string(rs[:maxLen]) + "…"
	}
	return s
}

// strAt 从 map 取字符串。
func strAt(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// ------------------------------------------------------------------ 上游工具调用翻译

// UpstreamCallToOpenAI 把上游沙箱执行过的工具调用翻成 OpenAI tool_calls 形状。
// 注意：这些调用是**沙箱自己执行完的**，流给客户端是让用户看见过程，
// 客户端不应再执行一遍。
func UpstreamCallToOpenAI(c UpstreamCall, index int) map[string]any {
	return map[string]any{
		"index": index,
		"id":    firstNonEmptyStr(c.ID, NewCallID()),
		"type":  "function",
		"function": map[string]any{
			"name":      c.Name,
			"arguments": c.Arguments,
		},
	}
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
