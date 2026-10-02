package prism

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeRT 记录是否被调用，并返回一个空 JSON 响应。
type fakeRT struct {
	used bool
	last *http.Request
}

func (f *fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.used = true
	f.last = req
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"content-type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
		Request:    req,
	}, nil
}

// TestTransportInjected 钉死：Config.Transport 会被真正用作出站 RoundTripper。
//
// 为什么重要：prism.openai.com 在 Cloudflare 后面，会校验 JA3/JA4 TLS 指纹，
// 且 cf_clearance 与 TLS 指纹绑定 —— 生产必须注入 Chrome 指纹的 utls transport，
// 否则一律 403。若这条接线断掉，403 会重新出现且极难定位。
func TestTransportInjected(t *testing.T) {
	f := &fakeRT{}
	c := NewClient(Config{
		Base:      "https://prism.example.test",
		Transport: f,
	}, Cookie{AccessToken: "t", UserAgent: "ua"})

	resp, err := c.do(context.Background(), "GET", "/auth/session", "", "tok", nil)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_ = resp.Body.Close()

	if !f.used {
		t.Fatal("注入的 Transport 未被使用（TLS 指纹会退回裸 Go → 上游 403）")
	}
	if f.last == nil || !strings.Contains(f.last.URL.String(), "prism.example.test") {
		t.Fatalf("请求未走注入的 transport: %+v", f.last)
	}
}

// TestTransportNilFallsBack 验证：未注入时退回默认（不影响离线单测）。
func TestTransportNilFallsBack(t *testing.T) {
	c := NewClient(Config{Base: "http://127.0.0.1:1"}, Cookie{UserAgent: "ua"})
	if c.http.Transport != nil {
		t.Fatal("未注入 Transport 时应使用默认 transport（nil 表示 DefaultTransport）")
	}
}
