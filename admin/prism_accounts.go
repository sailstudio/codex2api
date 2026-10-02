package admin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"

	"github.com/gin-gonic/gin"
)

// ============================================================
// Prism 渠道（prism.openai.com）账号导入。
//
// Prism 是 relay-style 上游，接入点见 proxy/prism_channel.go。账号池里以
// credentials.upstream_type="prism" 标记，调度器据此把请求路由到 Prism 适配器。
//
// 与 Codex 账号的差别：
//   - 凭据是 OpenAI OAuth 的 access_token（作 Cookie），**没有 refresh_token**，
//     故不参与换 AT / 用量探测；
//   - sentinel 一次性门禁 token 由网关侧或侧车铸造，不进凭据；
//   - 默认模型沿用对话材料里的值（见 proxy/prism/responses.go），账号无需模型白名单。
// ============================================================

type importPrismAccountReq struct {
	AccessToken string   `json:"access_token"`
	Email       string   `json:"email"`
	Name        string   `json:"name"`
	AccountID   string   `json:"account_id"`
	ProxyURL    string   `json:"proxy_url"`
	Models      []string `json:"models"`
	Tags        []string `json:"tags"`
}

type prismImportItem struct {
	OK    bool   `json:"ok"`
	ID    int64  `json:"id,omitempty"`
	Email string `json:"email,omitempty"`
	Error string `json:"error,omitempty"`
}

// ImportPrismAccounts 导入一个或多个 Prism 凭据。
//
// 请求体兼容三种形态（都是 JSON）：
//   - 单条：  {"access_token":"...","email":"..."}
//   - 数组：  [{"access_token":"..."},{"access_token":"..."}]
//   - 包装：  {"accounts":[{"access_token":"..."}]}
func (h *Handler) ImportPrismAccounts(c *gin.Context) {
	documents, err := decodePrismImportDocuments(c)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if len(documents) == 0 {
		writeError(c, http.StatusBadRequest, "未找到有效凭据（需要 access_token）")
		return
	}
	if len(documents) > 100 {
		writeError(c, http.StatusBadRequest, "单次最多导入 100 个账号")
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	clientIP := c.ClientIP()
	items := make([]prismImportItem, 0, len(documents))
	for _, doc := range documents {
		items = append(items, h.importOnePrismAccount(ctx, doc, clientIP))
	}

	if len(items) == 1 {
		item := items[0]
		if !item.OK {
			status := http.StatusBadRequest
			if strings.Contains(item.Error, "已存在") {
				status = http.StatusConflict
			}
			writeError(c, status, item.Error)
			return
		}
		c.JSON(http.StatusOK, item)
		return
	}

	ok := 0
	for _, it := range items {
		if it.OK {
			ok++
		}
	}
	c.JSON(http.StatusOK, gin.H{"ok": ok, "total": len(items), "items": items})
}

// decodePrismImportDocuments 解析三种请求体形态，丢弃无 access_token 的条目。
func decodePrismImportDocuments(c *gin.Context) ([]importPrismAccountReq, error) {
	if c.Request.Body == nil {
		return nil, fmt.Errorf("请求格式错误")
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("读取凭据失败")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, fmt.Errorf("请求体为空")
	}

	switch raw[0] {
	case '{':
		// 先看是不是 {"accounts":[...]} 的包装形态。
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, fmt.Errorf("凭据 JSON 解析失败")
		}
		if _, wrapped := probe["accounts"]; wrapped {
			var bundle struct {
				Accounts []importPrismAccountReq `json:"accounts"`
			}
			if err := json.Unmarshal(raw, &bundle); err != nil {
				return nil, fmt.Errorf("accounts 解析失败")
			}
			return normalizePrismDocs(bundle.Accounts), nil
		}
		var single importPrismAccountReq
		if err := json.Unmarshal(raw, &single); err != nil {
			return nil, fmt.Errorf("凭据 JSON 解析失败")
		}
		return normalizePrismDocs([]importPrismAccountReq{single}), nil
	case '[':
		var arr []importPrismAccountReq
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("凭据数组解析失败")
		}
		return normalizePrismDocs(arr), nil
	default:
		return nil, fmt.Errorf("请求体必须是 JSON 对象或数组")
	}
}

func normalizePrismDocs(in []importPrismAccountReq) []importPrismAccountReq {
	out := make([]importPrismAccountReq, 0, len(in))
	for _, d := range in {
		d.AccessToken = strings.TrimSpace(security.SanitizeInput(d.AccessToken))
		d.Email = strings.TrimSpace(security.SanitizeInput(d.Email))
		d.Name = strings.TrimSpace(security.SanitizeInput(d.Name))
		d.AccountID = strings.TrimSpace(security.SanitizeInput(d.AccountID))
		d.ProxyURL = strings.TrimSpace(d.ProxyURL)
		if d.AccessToken == "" {
			continue
		}
		out = append(out, d)
	}
	return out
}

func (h *Handler) importOnePrismAccount(ctx context.Context, doc importPrismAccountReq, clientIP string) prismImportItem {
	item := prismImportItem{Email: doc.Email}

	proxyURL := doc.ProxyURL
	if proxyURL != "" {
		if err := security.ValidateProxyURL(proxyURL); err != nil {
			item.Error = "代理URL无效"
			return item
		}
	}

	name := doc.Name
	if name == "" {
		name = doc.Email
	}
	if name == "" {
		name = "prism-" + shortTokenTag(doc.AccessToken)
	}

	models, err := normalizePrismModels(doc.Models)
	if err != nil {
		item.Error = err.Error()
		return item
	}

	credentials := map[string]interface{}{
		"upstream_type": auth.UpstreamPrism,
		"access_token":  doc.AccessToken,
	}
	if doc.Email != "" {
		credentials["email"] = doc.Email
	}
	if doc.AccountID != "" {
		credentials["account_id"] = doc.AccountID
	}
	if len(models) > 0 {
		credentials["models"] = models
	}

	// 查重与插入置于同一临界区，避免并发导入同一凭据各插一条（TOCTOU）。
	// 复用 antigravity/grok/claude 相同的合并去重锁，保持跨 provider 一致。
	h.mergeDuplicateMu.Lock()
	rows, listErr := h.db.ListActiveByChannel(ctx, "")
	if listErr != nil {
		h.mergeDuplicateMu.Unlock()
		item.Error = "查询账号失败: " + listErr.Error()
		return item
	}
	for _, row := range rows {
		if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamPrism) {
			continue
		}
		if strings.TrimSpace(row.GetCredential("access_token")) == doc.AccessToken {
			h.mergeDuplicateMu.Unlock()
			item.Error = fmt.Sprintf("Prism 凭据已存在 (id=%d)", row.ID)
			return item
		}
	}
	id, err := h.db.InsertAccountWithUpstream(ctx, name, "openai", auth.UpstreamPrism, credentials, proxyURL)
	h.mergeDuplicateMu.Unlock()
	if err != nil {
		item.Error = "保存 Prism 账号失败: " + err.Error()
		return item
	}

	if h.store != nil {
		h.store.AddAccount(&auth.Account{
			DBID:         id,
			ProxyURL:     proxyURL,
			UpstreamType: auth.UpstreamPrism,
			AccessToken:  doc.AccessToken,
			AccountID:    doc.AccountID,
			Email:        doc.Email,
			HealthTier:   auth.HealthTierHealthy,
			Models:       models,
		})
	}
	if len(doc.Tags) > 0 {
		if err := h.db.UpdateAccountTags(ctx, id, doc.Tags); err != nil {
			// 标签写入失败不影响账号可用性，如实附加提示。
			item.Error = "账号已导入，但标签写入失败: " + err.Error()
		}
	}

	security.SecurityAuditLog("PRISM_ACCOUNT_IMPORTED", fmt.Sprintf("account_id=%d ip=%s", id, clientIP))
	item.OK = true
	item.ID = id
	return item
}

func normalizePrismModels(values []string) ([]string, error) {
	models := auth.NormalizeAccountModels(values)
	for _, m := range models {
		if err := security.ValidateModelName(m); err != nil {
			return nil, fmt.Errorf("模型名称无效: %s", m)
		}
	}
	return models, nil
}

// shortTokenTag 返回 access_token 的短哈希，用作缺省账号名后缀。
// 用哈希而非明文前缀，避免把凭据片段写进账号名（账号名会出现在日志/管理台）。
func shortTokenTag(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum[:4])
}
