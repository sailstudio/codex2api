package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"prism-gateway/internal/config"
	"prism-gateway/internal/prism"
	"prism-gateway/internal/usage"
)

// ============================================================================
// HTTP 服务：路由、鉴权、中间件、观测端点。
// 零第三方依赖（标准库 net/http 的 ServeMux 足够，Go 1.22+ 支持方法与路径模式）。
// ============================================================================

// Server 是网关 HTTP 服务。
type Server struct {
	cfg       config.Config
	pool      *prism.Pool
	cache     *usage.Tracker
	counters  *usage.Counters
	est       usage.Estimator
	catalog   []prism.ModelSpec
	budget    prism.AttachmentBudget
	keys      map[string]bool
	startedAt time.Time

	// 观测
	mu        sync.Mutex
	inFlight  int64
	totalReqs int64
	inflightW sync.WaitGroup

	logf func(format string, args ...any)
}

// Deps 是 Server 的依赖。
type Deps struct {
	Config   config.Config
	Pool     *prism.Pool
	Catalog  []prism.ModelSpec
	Cache    *usage.Tracker
	Counters *usage.Counters
}

// New 建服务。
func New(d Deps) *Server {
	keys := map[string]bool{}
	for _, k := range d.Config.APIKeys {
		if s := strings.TrimSpace(k); s != "" {
			keys[s] = true
		}
	}
	sysPrompt := d.Config.SystemPrompt
	if sysPrompt == "" && strings.TrimSpace(d.Config.SystemPromptFile) != "" {
		if b, err := readFileLimited(d.Config.SystemPromptFile, 1<<20); err == nil {
			sysPrompt = string(b)
		} else {
			log.Printf("prism-gateway: 系统指令文件不可用: %v", err)
		}
	}
	d.Config.SystemPrompt = sysPrompt
	cat := d.Catalog
	if len(cat) == 0 {
		cat = prism.DefaultModels()
	}
	s := &Server{
		cfg:       d.Config,
		pool:      d.Pool,
		cache:     d.Cache,
		counters:  d.Counters,
		est:       usage.DefaultEstimator(),
		catalog:   cat,
		budget:    prism.DefaultAttachmentBudget(),
		keys:      keys,
		startedAt: time.Now(),
		logf:      log.Printf,
	}
	if s.cache == nil {
		s.cache = usage.NewTracker(d.Config.CacheTTL, d.Config.CacheMaxEntries, s.est)
	}
	if s.counters == nil {
		s.counters = usage.NewCounters()
	}
	return s
}

// Handler 返回已装配中间件的 http.Handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// OpenAI 兼容
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	// Anthropic 兼容（SDK 的 base_url 不带 /v1，故两个都注册）
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /messages", s.handleMessages)

	// 目录
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /models", s.handleModels)

	// 观测
	if s.cfg.MetricsOn {
		mux.HandleFunc("GET /metrics", s.handleMetrics)
		mux.HandleFunc("GET /admin/usage", s.handleUsageReport)
		mux.HandleFunc("GET /admin/health", s.handleHealth)
		mux.HandleFunc("GET /healthz", s.handleHealth)
	}
	return s.withMiddleware(mux)
}

// withMiddleware 组装日志 → 鉴权 → 并发闸 → panic 恢复。
func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// panic 恢复：一个坏请求不能打死服务。
		defer func() {
			if rec := recover(); rec != nil {
				s.logf("panic: %v %s: %v", r.Method, r.URL.Path, rec)
				if !headerWritten(w) {
					writeError(w, http.StatusInternalServerError, "server_error", "internal error")
				}
			}
		}()
		// 观测：在飞请求数。
		s.mu.Lock()
		s.inFlight++
		s.totalReqs++
		inflight := s.inFlight
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			s.inFlight--
			s.mu.Unlock()
		}()

		if r.URL.Path != "/healthz" {
			// 鉴权
			clientID, ok := s.authorize(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "authentication_error", "缺少或无效的 API key")
				return
			}
			r = r.WithContext(withClientID(r.Context(), clientID))
		}

		// CORS（方便浏览器/本机工具调试）
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version, anthropic-beta")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
		s.logf("%s %s inflight=%d dur=%s", r.Method, r.URL.Path, inflight, time.Since(start).Round(time.Millisecond))
	})
}

// authorize 校验 API key（未配置 key 时放行并标记为本地）。
// 支持 Authorization: Bearer、x-api-key、以及 Anthropic 的 client key 形态。
func (s *Server) authorize(r *http.Request) (string, bool) {
	token := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(h), "bearer ") {
		token = strings.TrimSpace(h[7:])
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("api_key"))
	}
	if len(s.keys) == 0 {
		return "local", true // 未配置 key：本机模式
	}
	for k := range s.keys {
		if subtle.ConstantTimeCompare([]byte(k), []byte(token)) == 1 {
			return k, true
		}
	}
	return "", false
}

// effectiveSystemPrompt 叠加网关注入指令与客户端自带 system。
func (s *Server) effectiveSystemPrompt(clientSystem string) string {
	def := strings.TrimSpace(s.cfg.SystemPrompt)
	cs := strings.TrimSpace(clientSystem)
	switch {
	case def == "" && cs == "":
		return prism.DefaultSystemPrompt
	case def == "":
		return prism.DefaultSystemPrompt + "\n\n[客户端附加指令]\n" + cs
	case cs == "":
		return def
	default:
		return def + "\n\n[客户端附加指令]\n" + cs
	}
}

// ------------------------------------------------------------------ 目录与观测

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	data := make([]ModelObject, 0, len(s.catalog)*2)
	for _, m := range s.catalog {
		data = append(data, ModelObject{ID: m.ID, Object: "model", Created: s.startedAt.Unix(), OwnedBy: "prism"})
		for _, a := range m.Aliases {
			data = append(data, ModelObject{ID: a, Object: "model", Created: s.startedAt.Unix(), OwnedBy: "prism"})
		}
	}
	writeJSON(w, http.StatusOK, ModelsResponse{Object: "list", Data: data})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	inflight, total := s.inFlight, s.totalReqs
	s.mu.Unlock()
	picked, waited, _, perAcct := s.pool.Stats()
	hits, misses, ns, entries := s.cache.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "ok",
		"uptime":    time.Since(s.startedAt).Round(time.Second).String(),
		"accounts":  s.pool.Len(),
		"inflight":  inflight,
		"total":     total,
		"pool":      map[string]any{"picked": picked, "waited": waited, "per_account": perAcct},
		"cache":     map[string]any{"hits": hits, "misses": misses, "namespaces": ns, "entries": entries},
		"sentinel":  sentinelStatus(),
		"breaker":   breakerStatus(s.pool),
		"models":    modelIDs(s.catalog),
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	global, byKey := s.counters.Snapshot()
	s.mu.Lock()
	inflight := s.inFlight
	s.mu.Unlock()
	fmt.Fprintf(w, "# HELP prism_gateway_requests_total 累计请求数\n# TYPE prism_gateway_requests_total counter\nprism_gateway_requests_total %d\n", global.Requests)
	fmt.Fprintf(w, "# TYPE prism_gateway_errors_total counter\nprism_gateway_errors_total %d\n", global.Errors)
	fmt.Fprintf(w, "# TYPE prism_gateway_inflight gauge\nprism_gateway_inflight %d\n", inflight)
	fmt.Fprintf(w, "# TYPE prism_gateway_tokens_total counter\nprism_gateway_tokens_total{kind=\"input\"} %d\n", global.InputTokens)
	fmt.Fprintf(w, "prism_gateway_tokens_total{kind=\"cache_read\"} %d\n", global.CacheRead)
	fmt.Fprintf(w, "prism_gateway_tokens_total{kind=\"cache_write\"} %d\n", global.CacheWrite)
	fmt.Fprintf(w, "prism_gateway_tokens_total{kind=\"output\"} %d\n", global.Output)
	fmt.Fprintf(w, "# TYPE prism_gateway_accounts gauge\nprism_gateway_accounts %d\n", s.pool.Len())
	picked, waited, _, perAcct := s.pool.Stats()
	fmt.Fprintf(w, "# TYPE prism_gateway_pool_picked_total counter\nprism_gateway_pool_picked_total %d\n", picked)
	fmt.Fprintf(w, "# TYPE prism_gateway_pool_waited_total counter\nprism_gateway_pool_waited_total %d\n", waited)
	for acct, n := range perAcct {
		fmt.Fprintf(w, "prism_gateway_account_inflight{account=%q} %d\n", acct, n)
	}
	hits, misses, _, entries := s.cache.Stats()
	fmt.Fprintf(w, "# TYPE prism_gateway_cache_hits_total counter\nprism_gateway_cache_hits_total %d\n", hits)
	fmt.Fprintf(w, "prism_gateway_cache_misses_total %d\n", misses)
	fmt.Fprintf(w, "prism_gateway_cache_entries %d\n", entries)
	for k, v := range byKey {
		fmt.Fprintf(w, "prism_gateway_client_tokens_total{key=%q,kind=\"input\"} %d\n", maskClient(k), v.InputTokens)
		fmt.Fprintf(w, "prism_gateway_client_tokens_total{key=%q,kind=\"output\"} %d\n", maskClient(k), v.Output)
	}
}

func (s *Server) handleUsageReport(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprint(w, s.counters.FormatReport())
	fmt.Fprintf(w, "\n缓存表: hits=%d misses=%d entries=%d\n", cacheHits(s), 0, 0)
	writeAccountHealth(w, s.pool)
}

// ------------------------------------------------------------------ 请求工具

// decodeJSON 读体 + 宽松解码（未知字段忽略，大小受限）。
func decodeJSON(r *http.Request, dst any, maxBytes int64) error {
	if maxBytes <= 0 {
		maxBytes = 32 << 20
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBytes+1))
	if err != nil {
		return fmt.Errorf("读取请求体失败: %w", err)
	}
	if int64(len(body)) > maxBytes {
		return fmt.Errorf("请求体超过上限 %d 字节", maxBytes)
	}
	if len(body) == 0 {
		return fmt.Errorf("请求体为空")
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("JSON 解析失败: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{Message: msg, Type: typ}})
}

// clientIDFromContext 取客户端标识（鉴权阶段写入）。
func clientIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(clientIDKey{}).(string); ok && v != "" {
		return v
	}
	return "local"
}

type clientIDKey struct{}

func withClientID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, clientIDKey{}, id)
}

// maskClient 遮蔽客户端 key（日志与面板不泄漏）。
func maskClient(k string) string {
	if k == "" {
		return "anonymous"
	}
	if len(k) <= 8 || k == "local" {
		return k
	}
	return k[:4] + "…" + k[len(k)-4:]
}

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func readFileLimited(path string, max int64) ([]byte, error) {
	f, err := openFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}
