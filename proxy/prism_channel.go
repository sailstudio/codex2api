package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/prism"
)

// ============================================================
// Prism 渠道（prism.openai.com）接入层。
//
// 定位：把 Prism 的私协议（start/poll）适配成 codex2api 内部统一的
// **Responses SSE** 形态，因此下游 /v1/responses、/v1/chat/completions、
// /v1/messages 全部零改动（与 Antigravity 通道同构）。
//
// 接入点：ExecuteRelayStyleProtocolRequest 的三行分叉——prism 账号与
// Grok / Antigravity / Claude / openai_responses 同属 relay-style，
// 共享同一段 handler 流程（重试/日志/用量）。
//
// 与 Codex 通道的差别（务必知悉）：
//   - Prism 是 start + 轮询模型，**没有真流式**；本层在拿到终态后
//     一次性发出正文（思考摘要若存在则作为 reasoning 事件真增量先发）。
//   - 必须复用「对话材料包」（浏览器侧车产出），材料里含与沙箱绑定的
//     metadata，任何改写都会 400。
// ============================================================

// prismClientCache 按账号缓存 client（复用会话/材料/sentinel 池）。
var (
	prismClientsMu sync.Mutex
	prismClients   = map[int64]*prism.Client{}
)

// PrismClientFor 取（或建）某账号的 Prism 客户端。
func PrismClientFor(account *auth.Account) *prism.Client {
	if account == nil {
		return nil
	}
	id := account.ID()
	prismClientsMu.Lock()
	defer prismClientsMu.Unlock()
	if c, ok := prismClients[id]; ok {
		return c
	}
	cfg := prism.DefaultConfig()
	cfg.Logf = func(f string, a ...any) {
		// 统一前缀，便于与 Codex 通道日志区分。
		prismLogf("[prism] "+f, a...)
	}
	// 单账号在飞闸门（实测上游只放行 ~4 路并发，超出的秒拒 403）。
	// 可通过 PRISM_MAX_INFLIGHT 调整；0 表示不限流。
	cfg.MaxInflight = envInt("PRISM_MAX_INFLIGHT", 4)
	// 平滑放行：相邻上游请求最小间隔（毫秒）。实测突发会触发上游限流冷却，
	// 错峰可让 15 槽稳定在线而不被拒。
	cfg.MinGap = time.Duration(envInt("PRISM_MIN_GAP_MS", 0)) * time.Millisecond
	// 上游限流型 403（可重试）→ 账号级熔断冷却，递增退避（重复触发翻倍，上限 15min）。
	cfg.CooldownOnRateLimit = time.Duration(envInt("PRISM_COOLDOWN_MS", 20000)) * time.Millisecond
	// 换槽重试上限（上游调用放大器）；突发场景建议 1（即不换槽重试）。
	cfg.SlotAttempts = envInt("PRISM_SLOT_ATTEMPTS", 3)
	// 材料有效期。默认 2min；但**平滑放行/熔断退避会让一轮 15 路拉长到数分钟**，
	// 若 TTL 短于该时长，材料会在中途过期 → 出现「无可用材料槽」。按需上调。
	if ms := envInt("PRISM_MATERIAL_TTL_MS", 0); ms > 0 {
		cfg.MaterialTTL = time.Duration(ms) * time.Millisecond
	}
	// ⚠️ 关键：注入 Chrome TLS 指纹 transport。
	// prism.openai.com 在 Cloudflare 后面；cf_clearance 与 TLS 指纹绑定，
	// 裸 Go 指纹会被判非浏览器 → 403。这与仓库自身出站（proxy/auth）保持同一指纹。
	//
	// 门槛（两处都必须满足）：
	//   ① PRISM_TLS_PROFILE != 0（默认开；排障可关）
	//   ② 目标是 **https** —— utls 只做 TLS 握手，无法对 http:// 建连
	//      （单测用 httptest 明文上游，必须跳过指纹注入，否则接线测试会失败）
	if envInt("PRISM_TLS_PROFILE", 1) != 0 && strings.HasPrefix(prism.CurrentBase(cfg), "https://") {
		px := os.Getenv("PRISM_PROXY")
		cfg.Transport = NewUTLSTransport(px)
		prismLogf("[prism] 已启用 Chrome TLS 指纹（utls/HelloChrome_Auto，代理=%q）", px)
	}
	c := prism.NewClient(cfg, prism.Cookie{
		AccessToken: account.GetAccessToken(),
		UserAgent:   account.PrismUserAgent(),
	})
	prismClients[id] = c
	return c
}

// InvalidatePrismClient 在凭据轮换/账号删除时丢弃缓存。
func InvalidatePrismClient(accountID int64) {
	prismClientsMu.Lock()
	defer prismClientsMu.Unlock()
	if c, ok := prismClients[accountID]; ok {
		c.Close()
		delete(prismClients, accountID)
	}
}

// PrismChannelStatus 供管理台/诊断展示通道状态（材料池、槽健康度）。
func PrismChannelStatus(account *auth.Account) map[string]any {
	c := PrismClientFor(account)
	if c == nil {
		return map[string]any{"ready": false, "reason": "account is nil"}
	}
	out := map[string]any{
		"channel": "prism",
		"account": account.ID(),
	}
	// 多槽优先：用材料池状态（含各槽新鲜度/在飞/成功失败计数）
	if p := c.Pool(); p != nil {
		if st := p.Stats(); st != nil {
			out["pool"] = st
			fresh, _ := st["slots_fresh"].(int)
			total, _ := st["slots_total"].(int)
			out["slots_fresh"] = fresh
			out["slots_total"] = total
			if total == 0 {
				out["ready"] = false
				out["reason"] = "材料池为空（请起材料侧车）"
				return out
			}
			if fresh == 0 {
				out["ready"] = false
				out["reason"] = "无新鲜材料槽（请检查材料侧车是否在刷新）"
				return out
			}
			out["ready"] = true
			if total > 1 {
				out["note"] = "多槽模式：多身份轮转，理论吞吐 = 槽数 × 单身份限流"
			}
			return out
		}
	}
	// 退化为单份材料状态
	info := c.MaterialInfo()
	out["material"] = info
	if fresh, _ := info["fresh"].(bool); !fresh {
		out["ready"] = false
		out["reason"] = "材料缺失或已过期（请跑 cmd/prism-material 刷新）"
		return out
	}
	out["ready"] = true
	return out
}

// ExecutePrismResponsesRequest 把一次 Responses 请求转发到 Prism，
// 并以 **Responses SSE** 的形式返回给下游。
func ExecutePrismResponsesRequest(ctx context.Context, account *auth.Account, responsesBody []byte) (*http.Response, error) {
	if account == nil {
		return nil, fmt.Errorf("prism: account is nil")
	}
	if !account.IsPrismAPI() {
		return nil, fmt.Errorf("prism: 账号 %d 不是 prism 渠道", account.ID())
	}
	c := PrismClientFor(account)
	if c == nil {
		return nil, fmt.Errorf("prism: 客户端不可用")
	}

	req, responseID, err := prismRequestFromResponses(account, responsesBody)
	if err != nil {
		return nil, err
	}

	pr, pw := io.Pipe()
	flusher := &pipeFlusher{pw: pw}

	go func() {
		defer func() { _ = pw.Close() }()
		ew := prism.NewEventWriter(pw, flusher, req.Model)

		turn, err := c.Run(ctx, req)
		if err != nil {
			// 传输层/材料层失败：以 failed 事件收尾，让下游能干净结束。
			ew.WriteFailure(responseID, err.Error())
			return
		}
		if turn.ErrMessage != "" && turn.Text == "" {
			ew.WriteFailure(responseID, turn.ErrMessage)
			return
		}
		ew.WriteTurn(turn, responseID)
	}()

	// 下游取消时把 ctx 的取消传导到上游轮询（省额度）。
	go func() {
		<-ctx.Done()
		_ = pw.CloseWithError(ctx.Err())
	}()

	hdr := make(http.Header)
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	hdr.Set("X-Prism-Request-Id", responseID)

	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     hdr,
		Body:       pr,
	}, nil
}

// pipeFlusher 让 EventWriter 能逐帧刷出（io.Pipe 本身即逐写透传，
// 这里保留接口形态以便未来换成带缓冲的 writer）。
type pipeFlusher struct{ pw *io.PipeWriter }

func (p *pipeFlusher) Flush() {}

// prismLogf 统一日志出口（与仓库其它通道一致走标准 log）。
// envInt 读整型环境变量（缺省/非法时返回 def）。
func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func prismLogf(format string, args ...any) {
	log.Printf(format, args...)
}

// prismRequestFromResponses 把 Responses 请求体翻译成 Prism 请求。
//
// 映射规则：
//
//	instructions               → system 消息
//	input(string)              → user 消息
//	input[].role/content[]     → 逐条消息（input_text / output_text / 图片）
//	input[].function_call_output → user 消息（工具结果回灌）
//	tools                      → 提示词仿真（上游无客户端 tool 通道）
//	model                      → 模型名（默认沿用材料里的值，见 prism.Run）
func prismRequestFromResponses(account *auth.Account, body []byte) (prism.Request, string, error) {
	var parsed struct {
		Model        string           `json:"model"`
		Instructions string           `json:"instructions"`
		Input        json.RawMessage  `json:"input"`
		Tools        []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return prism.Request{}, "", fmt.Errorf("prism: 请求体解析失败: %w", err)
	}

	out := prism.Request{
		Model:            strings.TrimSpace(parsed.Model),
		InstructionsText: strings.TrimSpace(parsed.Instructions),
		Tools:            parsed.Tools,
	}

	// input 可能是字符串，也可能是条目数组。
	if len(parsed.Input) > 0 {
		var asString string
		if err := json.Unmarshal(parsed.Input, &asString); err == nil {
			out.Input = append(out.Input, userMsg(asString))
		} else {
			var items []map[string]any
			if err := json.Unmarshal(parsed.Input, &items); err != nil {
				return prism.Request{}, "", fmt.Errorf("prism: input 结构无法识别: %w", err)
			}
			for _, it := range items {
				msg, imgs := responsesItemToPrism(it)
				if msg != nil {
					out.Input = append(out.Input, msg)
				}
				out.Images = append(out.Images, imgs...)
			}
		}
	}
	if len(out.Input) == 0 {
		return prism.Request{}, "", fmt.Errorf("prism: 请求无可发送内容（input 为空）")
	}

	// response id 用于事件引用（下游据此串联）。
	responseID := "resp_prism_" + newPrismHex(8)
	return out, responseID, nil
}

// responsesItemToPrism 把一条 Responses 条目翻译成 Prism input 条目。
//
// 返回值含义：msg 是文本消息（可为 nil）；imgs 是本条携带的图片。
func responsesItemToPrism(it map[string]any) (msg map[string]any, imgs []prism.ImagePart) {
	typ, _ := it["type"].(string)
	switch typ {
	case "function_call_output", "custom_tool_call_output":
		text, _ := it["output"].(string)
		if text == "" {
			text = "(工具无输出)"
		}
		return sysMsgPrism("Tool result:\n" + text), nil
	case "function_call", "custom_tool_call":
		name, _ := it["name"].(string)
		args, _ := it["arguments"].(string)
		if args == "" {
			if in, ok := it["input"].(string); ok {
				args = in
			}
		}
		return sysMsgPrism(fmt.Sprintf("Previously requested tool: %s(%s)", name, args)), nil
	case "reasoning":
		return nil, nil // 思考不回灌（上游会自行重建）
	}

	role, _ := it["role"].(string)
	if role == "" {
		return nil, nil
	}
	var sb strings.Builder
	switch c := it["content"].(type) {
	case string:
		sb.WriteString(c)
	case []any:
		for _, part := range c {
			pm, _ := part.(map[string]any)
			if pm == nil {
				continue
			}
			// 图片：解析出来单独收集（上游私协议当前不消费图片内容，
			// 由 prism 层如实告知模型，见 proxy/prism/image.go）。
			if img, ok := prism.ParseImagePart(pm); ok {
				imgs = append(imgs, img)
				continue
			}
			if t, ok := pm["text"].(string); ok {
				sb.WriteString(t)
			}
		}
	}
	// 纯图片消息也必须有文本占位，否则上游会认为该消息为空。
	if sb.Len() == 0 && len(imgs) == 0 {
		return nil, nil
	}
	text := sb.String()
	if text == "" {
		text = "(用户发送了图片)"
	}
	return map[string]any{
		"type": "message", "role": role,
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}, imgs
}

func userMsg(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}

func sysMsgPrism(text string) map[string]any {
	return map[string]any{
		"type": "message", "role": "system",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}
}

// newPrismHex 生成 n 字节的随机 hex（避免引入 uuid 依赖）。
func newPrismHex(n int) string {
	b := make([]byte, n)
	seed := time.Now().UnixNano()
	for i := range b {
		seed = seed*6364136223846793005 + 1442695040888963407
		b[i] = byte(seed >> 33)
	}
	const hexd = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hexd[v>>4]
		out[i*2+1] = hexd[v&0x0f]
	}
	return string(out)
}
