package prism

import (
	"errors"
	"net/http"
	"testing"
)

// ============================================================================
// 本轮修复的两处缺陷的回归用例。
// ============================================================================

// TestClassify_PrismSession403NotAuth 钉死：Prism 会话级 403 **不得**被判成
// KindAuth（凭据失效）。
//
// 为什么关键：KindAuth 的处置是 Cool ✓ + Swit ✓ → 触发「换号重试」。
// 而是**账号级限流（带冷却期）**：换号零收益（新号同样受限），
// 而每次重试都会再打一次上游 —— 把 N 路请求放大成 N×attempts 次调用，
// 等于自己烧配额、把账号推入更深冷却。
func TestClassify_PrismSession403NotAuth(t *testing.T) {
	// 上游真实文案（藏在 HTTP 200 内层 payload 里）
	msg := "Error while processing conversation (403 Forbidden). Please submit prompt again."
	cls := Classify(errors.New(msg))

	if cls.Kind != KindQuota {
		t.Fatalf("Prism 会话 403 应判为 KindQuota（账号级限流），得到 %v", cls.Kind)
	}
	if cls.Swit {
		t.Fatal("Prism 会话 403 不得触发换号重试（换号绕不开账号级限流，只会放大上游压力）")
	}
	if !cls.Cool {
		t.Fatal("Prism 会话 403 应冷却账号（限流带冷却期）")
	}
	t.Logf("✓ Kind=%v Cool=%v Swit=%v", cls.Kind, cls.Cool, cls.Swit)
}

// TestClassify_Plain403StillAuth 反向保证：普通（非 Prism 会话）403 仍判 KindAuth，
// 即真实的凭据失效场景不该被误伤。
func TestClassify_Plain403StillAuth(t *testing.T) {
	cls := Classify(&UpstreamError{Status: http.StatusForbidden, Op: "start", Msg: "Forbidden"})
	if cls.Kind != KindAuth {
		t.Fatalf("普通 403 应仍是 KindAuth，得到 %v", cls.Kind)
	}
	if !cls.Swit {
		t.Fatal("真实凭据失效应允许换号")
	}
	t.Logf("✓ 普通 403 仍判 KindAuth 且允许换号")
}

// TestNewHTTPClient_FingerprintRequestedButMissing 钉死：**请求指纹但未注入实现
// 必须报错**，绝不能静默退回原生指纹。
//
// 理由：静默退回会让每次请求都 403（cf_clearance 绑定了浏览器 TLS 指纹），
// 形成「看起来在跑、实际全废」的隐蔽故障。
func TestNewHTTPClient_FingerprintRequestedButMissing(t *testing.T) {
	tc := DefaultTransportConfig()
	tc.Profile = ProfileChrome
	_, err := NewHTTPClient(tc, "https://prism.openai.com", nil)
	if err == nil {
		t.Fatal("请求 Chrome 指纹但未注入实现，应当报错而非静默退回原生指纹")
	}
	t.Logf("✓ 明确报错: %v", err)
}

// TestNewHTTPClient_PlaintextSkipsFingerprint 明文上游（httptest）跳过指纹注入，
// 否则本地测试无法运行（给明文套 uTLS 无法握手）。
func TestNewHTTPClient_PlaintextSkipsFingerprint(t *testing.T) {
	tc := DefaultTransportConfig()
	tc.Profile = ProfileChrome
	hc, err := NewHTTPClient(tc, "http://127.0.0.1:1234", nil)
	if err != nil {
		t.Fatalf("明文上游应跳过指纹注入（不报错），得到: %v", err)
	}
	if hc == nil {
		t.Fatal("应返回可用客户端")
	}
	t.Log("✓ 明文上游跳过指纹注入")
}

// TestNewHTTPClient_Injected 注入器被真正用上（防接线再次断开）。
func TestNewHTTPClient_Injected(t *testing.T) {
	var called bool
	tc := DefaultTransportConfig()
	tc.Profile = ProfileChrome
	hc, err := NewHTTPClient(tc, "https://prism.openai.com", func(string) (http.RoundTripper, error) {
		called = true
		return &http.Transport{}, nil
	})
	if err != nil {
		t.Fatalf("注入应成功: %v", err)
	}
	if !called {
		t.Fatal("注入器未被调用 —— 说明指纹注入被接线断开了")
	}
	if hc == nil {
		t.Fatal("应返回可用客户端")
	}
	t.Log("✓ 指纹注入器被真正调用")
}
