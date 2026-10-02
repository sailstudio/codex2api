package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PrismClient 调用 OpenAI Prism LaTeX 编辑器接口的客户端
type PrismClient struct {
	baseURL    string
	httpClient *http.Client
	accessToken string
}

// NewPrismClient 创建 Prism 客户端实例
func NewPrismClient(accessToken string, httpClient *http.Client) *PrismClient {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 60 * time.Second,
		}
	}
	return &PrismClient{
		baseURL:     "https://chatgpt.com",
		httpClient:  httpClient,
		accessToken: accessToken,
	}
}

// PrismStartRequest 启动 Prism 会话的请求体
type PrismStartRequest struct {
	Messages []PrismMessage `json:"messages"`
	Model    string         `json:"model"`
}

// PrismMessage Prism 消息格式
type PrismMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// PrismStartResponse 启动响应
type PrismStartResponse struct {
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id"`
}

// PrismStatusResponse 轮询状态响应
type PrismStatusResponse struct {
	Status   string `json:"status"`    // "pending", "completed", "failed"
	Response string `json:"response"`  // 完整响应内容
	Error    string `json:"error,omitempty"`
}

// StartConversation 启动 Prism 对话
func (c *PrismClient) StartConversation(ctx context.Context, req PrismStartRequest) (*PrismStartResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/api/llm/response_with_tools_start", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+c.accessToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("prism start failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var startResp PrismStartResponse
	if err := json.NewDecoder(resp.Body).Decode(&startResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &startResp, nil
}

// PollStatus 轮询对话状态
func (c *PrismClient) PollStatus(ctx context.Context, conversationID, messageID string) (*PrismStatusResponse, error) {
	url := fmt.Sprintf("%s/api/llm/response_with_tools_status?conversation_id=%s&message_id=%s",
		c.baseURL, conversationID, messageID)

	httpReq, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+c.accessToken)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("prism poll failed: status=%d body=%s", resp.StatusCode, string(respBody))
	}

	var statusResp PrismStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	return &statusResp, nil
}

// WaitForCompletion 等待对话完成（带轮询）
func (c *PrismClient) WaitForCompletion(ctx context.Context, conversationID, messageID string, maxWait time.Duration) (*PrismStatusResponse, error) {
	deadline := time.Now().Add(maxWait)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("timeout waiting for completion")
			}

			status, err := c.PollStatus(ctx, conversationID, messageID)
			if err != nil {
				return nil, err
			}

			switch status.Status {
			case "completed":
				return status, nil
			case "failed":
				return nil, fmt.Errorf("prism conversation failed: %s", status.Error)
			case "pending":
				// 继续轮询
				continue
			default:
				return nil, fmt.Errorf("unknown status: %s", status.Status)
			}
		}
	}
}