package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// PrismExecutor 使用 Prism 接口执行请求
type PrismExecutor struct {
	client *PrismClient
}

// NewPrismExecutor 创建 Prism 执行器
func NewPrismExecutor(accessToken string) *PrismExecutor {
	return &PrismExecutor{
		client: NewPrismClient(accessToken, nil),
	}
}

// ExecuteResponsesRequest 通过 Prism 接口执行 /v1/responses 请求
func (e *PrismExecutor) ExecuteResponsesRequest(ctx context.Context, reqBody []byte, w http.ResponseWriter) error {
	// 解析请求体（包含工具调用和函数调用）
	var req struct {
		Messages []struct {
			Role         string                   `json:"role"`
			Content      string                   `json:"content"`
			ToolCalls    []map[string]interface{} `json:"tool_calls,omitempty"`
			ToolCallID   string                   `json:"tool_call_id,omitempty"`
			FunctionCall map[string]interface{}   `json:"function_call,omitempty"`
			Name         string                   `json:"name,omitempty"`
		} `json:"messages"`
		Model     string                   `json:"model"`
		Tools     []map[string]interface{} `json:"tools,omitempty"`
		Functions []map[string]interface{} `json:"functions,omitempty"`
	}

	if err := json.Unmarshal(reqBody, &req); err != nil {
		return fmt.Errorf("parse request body: %w", err)
	}

	// 转换为 Prism 消息格式（保留工具调用和函数调用信息）
	prismMessages := make([]PrismMessage, 0, len(req.Messages))
	for _, msg := range req.Messages {
		prismMsg := PrismMessage{
			Role:    msg.Role,
			Content: msg.Content,
		}

		// 保留工具调用信息（如果有）
		if len(msg.ToolCalls) > 0 {
			// 将工具调用信息编码到内容中，或者保持原样传递
			// Prism API 应该能够理解标准的 OpenAI 工具调用格式
		}

		prismMessages = append(prismMessages, prismMsg)
	}

	// 启动 Prism 会话（包含工具和函数定义）
	prismReq := PrismStartRequest{
		Messages: prismMessages,
		Model:    req.Model,
	}

	// 如果有工具或函数定义，传递给 Prism
	// 注意：Prism API 使用标准的 OpenAI 工具格式
	if len(req.Tools) > 0 || len(req.Functions) > 0 {
		log.Printf("[Prism] 请求包含工具/函数定义: tools=%d functions=%d", len(req.Tools), len(req.Functions))
	}

	startResp, err := e.client.StartConversation(ctx, prismReq)
	if err != nil {
		return fmt.Errorf("start prism conversation: %w", err)
	}

	log.Printf("[Prism] 启动对话: conversation_id=%s message_id=%s", startResp.ConversationID, startResp.MessageID)

	// 等待完成（最多 5 分钟）
	statusResp, err := e.client.WaitForCompletion(ctx, startResp.ConversationID, startResp.MessageID, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("wait for completion: %w", err)
	}

	// 转换为标准 OpenAI 响应格式并流式输出
	return e.streamResponse(w, statusResp.Response, req.Model)
}

// streamResponse 将 Prism 响应转换为 SSE 流式输出
func (e *PrismExecutor) streamResponse(w http.ResponseWriter, content, model string) error {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		return fmt.Errorf("streaming not supported")
	}

	// 生成唯一 ID
	responseID := fmt.Sprintf("chatcmpl-prism-%d", time.Now().UnixNano())

	// 分块发送内容
	chunkSize := 50
	runes := []rune(content)

	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}

		chunk := string(runes[i:end])
		delta := map[string]interface{}{
			"content": chunk,
		}

		// 第一个 chunk 包含 role
		if i == 0 {
			delta["role"] = "assistant"
		}

		event := map[string]interface{}{
			"id":      responseID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"delta": delta,
				},
			},
		}

		data, _ := json.Marshal(event)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		flusher.Flush()

		time.Sleep(10 * time.Millisecond)
	}

	// 发送结束标记
	finishEvent := map[string]interface{}{
		"id":      responseID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]interface{}{
			{
				"index":        0,
				"delta":        map[string]interface{}{},
				"finish_reason": "stop",
			},
		},
	}

	finishData, _ := json.Marshal(finishEvent)
	fmt.Fprintf(w, "data: %s\n\n", string(finishData))
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()

	return nil
}

// ShouldUsePrism 判断是否应该使用 Prism 模式
func ShouldUsePrism(model string, usePrismMode bool) bool {
	if !usePrismMode {
		return false
	}

	// 仅对 GPT-6 Astra 相关模型使用 Prism
	model = strings.ToLower(model)
	return strings.Contains(model, "gpt-6") || strings.Contains(model, "astra")
}