package proxy

import (
	"context"
	"log"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// HandlePrismResponsesIfEnabled 检查账户是否启用了 Prism 模式，如果启用则通过 Prism 执行请求
// 返回 true 表示已经通过 Prism 处理，false 表示应该继续正常流程
func HandlePrismResponsesIfEnabled(
	ctx context.Context,
	c *gin.Context,
	account *auth.Account,
	model string,
	rawBody []byte,
) bool {
	// 检查模型是否适合使用 Prism（GPT-6/Astra 系列）
	if !ShouldUsePrism(model, true) {
		return false
	}

	// 获取账户的访问令牌
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		log.Printf("[Prism] 账户 %d 没有有效的访问令牌", account.DBID)
		return false
	}

	log.Printf("[Prism] 账户 %d 使用 Prism 模式处理模型 %s 的请求", account.DBID, model)

	// 创建 Prism 执行器
	executor := NewPrismExecutor(accessToken)

	// 执行请求并流式返回结果
	if err := executor.ExecuteResponsesRequest(ctx, rawBody, c.Writer); err != nil {
		log.Printf("[Prism] 执行失败 account=%d model=%s: %v", account.DBID, model, err)
		// Prism 失败时返回 false，让调用方尝试正常流程
		return false
	}

	return true
}