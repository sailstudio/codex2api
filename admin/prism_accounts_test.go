package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// invokePrismImport 直调 ImportPrismAccounts，返回 recorder。
func invokePrismImport(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/prism", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")
	h.ImportPrismAccounts(ctx)
	return recorder
}

func newPrismImportHandler(t *testing.T) (*Handler, *database.DB, *auth.Store) {
	t.Helper()
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	t.Cleanup(func() { store.Stop() })
	return &Handler{db: db, store: store}, db, store
}

// TestImportPrismAccounts_Single 单条导入：落库的 credentials 必须带
// upstream_type=prism，且账号进入运行时池时被识别为 Prism 账号。
func TestImportPrismAccounts_Single(t *testing.T) {
	h, db, store := newPrismImportHandler(t)

	rec := invokePrismImport(t, h, `{"access_token":"at-dummy-1","email":"p1@example.com"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var item prismImportItem
	if err := json.Unmarshal(rec.Body.Bytes(), &item); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rec.Body.String())
	}
	if !item.OK || item.ID <= 0 {
		t.Fatalf("expected ok item with id, got %+v", item)
	}

	rows, err := db.ListActiveByChannel(context.Background(), database.UpstreamChannelPrism)
	if err != nil {
		t.Fatalf("ListActiveByChannel(prism): %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("prism channel rows = %d, want 1", len(rows))
	}
	row := rows[0]
	if got := row.GetCredential("upstream_type"); got != auth.UpstreamPrism {
		t.Fatalf("upstream_type = %q, want %q", got, auth.UpstreamPrism)
	}
	if got := row.GetCredential("access_token"); got != "at-dummy-1" {
		t.Fatalf("access_token round-trip failed: %q", got)
	}
	// 账号名缺省应回落到邮箱，而不是暴露凭据前缀。
	if !strings.Contains(row.Name, "p1@example.com") {
		t.Fatalf("name = %q, want it to fall back to email", row.Name)
	}

	// 运行时池里必须是 Prism 账号，否则调度器不会走到 Prism 适配器。
	acc := store.FindByID(item.ID)
	if acc == nil {
		t.Fatalf("account %d not in runtime pool", item.ID)
	}
	if !acc.IsPrismAPI() {
		t.Fatalf("account %d IsPrismAPI() = false; Prism routing would never fire", item.ID)
	}
	if acc.GetAccessToken() != "at-dummy-1" {
		t.Fatalf("runtime access token mismatch")
	}
}

// TestImportPrismAccounts_BatchAndWrapper 数组形态与 {"accounts":[...]} 包装形态。
func TestImportPrismAccounts_BatchAndWrapper(t *testing.T) {
	h, db, _ := newPrismImportHandler(t)

	rec := invokePrismImport(t, h, `[{"access_token":"at-a"},{"access_token":"at-b","name":"named-b"}]`)
	if rec.Code != http.StatusOK {
		t.Fatalf("array form status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var batch struct {
		OK    int               `json:"ok"`
		Total int               `json:"total"`
		Items []prismImportItem `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &batch); err != nil {
		t.Fatalf("decode batch: %v (body=%s)", err, rec.Body.String())
	}
	if batch.OK != 2 || batch.Total != 2 {
		t.Fatalf("batch = ok %d / total %d, want 2/2", batch.OK, batch.Total)
	}

	rec = invokePrismImport(t, h, `{"accounts":[{"access_token":"at-c"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("wrapper form status = %d, body=%s", rec.Code, rec.Body.String())
	}

	rows, _ := db.ListActiveByChannel(context.Background(), database.UpstreamChannelPrism)
	if len(rows) != 3 {
		t.Fatalf("prism rows = %d, want 3", len(rows))
	}
}

// TestImportPrismAccounts_RejectsMissingToken 无 access_token 必须 400，
// 且不得写入任何账号（防止建出永远不可用的空壳账号）。
func TestImportPrismAccounts_RejectsMissingToken(t *testing.T) {
	h, db, _ := newPrismImportHandler(t)

	rec := invokePrismImport(t, h, `{"email":"no-token@example.com"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	rows, _ := db.ListActiveByChannel(context.Background(), database.UpstreamChannelPrism)
	if len(rows) != 0 {
		t.Fatalf("expected no rows written, got %d", len(rows))
	}
}

// TestImportPrismAccounts_Duplicate 同一 access_token 重复导入必须 409，
// 且不产生第二条记录。
func TestImportPrismAccounts_Duplicate(t *testing.T) {
	h, db, _ := newPrismImportHandler(t)

	if rec := invokePrismImport(t, h, `{"access_token":"at-dup"}`); rec.Code != http.StatusOK {
		t.Fatalf("first import status = %d, body=%s", rec.Code, rec.Body.String())
	}
	rec := invokePrismImport(t, h, `{"access_token":"at-dup"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409, body=%s", rec.Code, rec.Body.String())
	}
	rows, _ := db.ListActiveByChannel(context.Background(), database.UpstreamChannelPrism)
	if len(rows) != 1 {
		t.Fatalf("prism rows = %d, want 1 after duplicate rejection", len(rows))
	}
}

// TestImportPrismAccounts_NotInCodexChannel 关键安全约束：Prism 账号
// 绝不能被 Codex 通道选中（否则会被 OpenAI 适配器拒绝，表现为账号在线却永远失败）。
func TestImportPrismAccounts_NotInCodexChannel(t *testing.T) {
	h, db, _ := newPrismImportHandler(t)

	// 先放一个正常的 Codex 账号，确认 Codex 通道仍能工作。
	if _, err := db.InsertAccount(context.Background(), "codex-1", "rt-codex", ""); err != nil {
		t.Fatalf("insert codex account: %v", err)
	}
	if rec := invokePrismImport(t, h, `{"access_token":"at-prism-only"}`); rec.Code != http.StatusOK {
		t.Fatalf("prism import status = %d, body=%s", rec.Code, rec.Body.String())
	}

	codexRows, err := db.ListActiveByChannel(context.Background(), database.UpstreamChannelCodex)
	if err != nil {
		t.Fatalf("ListActiveByChannel(codex): %v", err)
	}
	for _, row := range codexRows {
		if strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamPrism) {
			t.Fatalf("prism account %d leaked into the Codex channel", row.ID)
		}
	}
	prismRows, _ := db.ListActiveByChannel(context.Background(), database.UpstreamChannelPrism)
	if len(prismRows) != 1 {
		t.Fatalf("prism rows = %d, want 1", len(prismRows))
	}
}

// TestNormalizePrismModels 模型白名单校验与去重。
func TestNormalizePrismModels(t *testing.T) {
	got, err := normalizePrismModels([]string{"gpt-5.6-sol", " gpt-5.6-sol ", ""})
	if err != nil {
		t.Fatalf("normalizePrismModels: %v", err)
	}
	if len(got) != 1 || got[0] != "gpt-5.6-sol" {
		t.Fatalf("models = %v, want [gpt-5.6-sol]", got)
	}
	if _, err := normalizePrismModels([]string{"bad model name!"}); err == nil {
		t.Fatalf("expected error for invalid model name")
	}
}

// TestShortTokenTag 缺省账号名后缀必须是短哈希，绝不泄漏凭据明文。
func TestShortTokenTag(t *testing.T) {
	tag := shortTokenTag("super-secret-token")
	if len(tag) != 8 {
		t.Fatalf("tag length = %d, want 8", len(tag))
	}
	if strings.Contains(tag, "super") || strings.Contains(tag, "secret") {
		t.Fatalf("tag leaks credential material: %q", tag)
	}
	// 稳定性：同输入同输出，便于查重对照。
	if tag != shortTokenTag("super-secret-token") {
		t.Fatalf("tag is not deterministic")
	}
}
