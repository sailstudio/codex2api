package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是网关全部可调项。零第三方依赖：只读环境变量 + 可选 JSON 文件，
// 不引入 yaml 库，保证三架构交叉编译干净。
type Config struct {
	// ---- 服务面 ----
	Listen          string        `json:"listen"`           // 默认 127.0.0.1:8787
	APIKeys         []string      `json:"api_keys"`         // 客户端鉴权 key；空 = 不校验（仅本机）
	ShutdownGrace   time.Duration `json:"shutdown_grace"`   // 优雅退出
	MaxBodyBytes    int64         `json:"max_body_bytes"`   // 请求体上限
	UpstreamTimeout time.Duration `json:"upstream_timeout"` // 单轮总预算

	// ---- 上游面 ----
	Origin          string        `json:"origin"`            // https://prism.openai.com
	UserAgent       string        `json:"user_agent"`        // 必须与 cf_clearance 绑定的一致
	StartTimeout    time.Duration `json:"start_timeout"`     // 单次 start 上限
	PollCallTimeout time.Duration `json:"poll_call_timeout"` // 单次 status 上限
	PollBudget      time.Duration `json:"poll_budget"`       // 单轮轮询总预算
	SyncPollTries   int           `json:"sync_poll_tries"`   // wait-for-sync 重试次数
	StartAttempts   int           `json:"start_attempts"`    // start 换沙箱重试次数
	MaxReconnects   int           `json:"max_reconnects"`    // 同沙箱 reconnect 重试次数
	ForceHTTP1      bool          `json:"force_http1"`       // Cloudflare 下避免 HTTP/2 PROTOCOL_ERROR

	// ---- 号池 / 并发 ----
	SandboxTTL        time.Duration `json:"sandbox_ttl"`          // 沙箱热复用窗口
	PerAccountConc    int           `json:"per_account_conc"`     // 单账号并发上限
	MaxSandboxPerAcct int           `json:"max_sandbox_per_acct"` // 单账号沙箱池深
	PrewarmWorkers    int           `json:"prewarm_workers"`      // 后台预热并发
	WarmOnStart       bool          `json:"warm_on_start"`        // 启动即预热
	RetryBackoffMin   time.Duration `json:"retry_backoff_min"`
	RetryBackoffMax   time.Duration `json:"retry_backoff_max"`
	CooldownOnAuthErr time.Duration `json:"cooldown_on_auth_err"` // 401/403 账号冷却

	// ---- 流式合成 ----
	ChunkSize      int           `json:"chunk_size"`      // 合成 delta 的切片粒度（rune）
	ChunkInterval  time.Duration `json:"chunk_interval"`  // 切片间隔（低延迟=小值）
	KeepAliveEvery time.Duration `json:"keepalive_every"` // SSE 注释心跳
	EmitReasoning  bool          `json:"emit_reasoning"`  // 把上游 reasoning 摘要作为思考流下发

	// ---- 图片解析 ----
	InlineB64Total   int `json:"inline_b64_total"`    // 单请求全部图片 base64 总预算（字符）
	InlineB64PerFile int `json:"inline_b64_per_file"` // 单张图片 base64 上限
	MaxImageDim      int `json:"max_image_dim"`       // 缩图最长边
	ImageJPEGQuality int `json:"image_jpeg_quality"`  // 起始 JPEG 质量

	// ---- token 缓存计数 ----
	TokensPerMessage int           `json:"tokens_per_message"`
	TokensPerTool    int           `json:"tokens_per_tool"`
	ImageTokenFlat   int           `json:"image_token_flat"`
	CacheTTL         time.Duration `json:"cache_ttl"` // 前缀指纹存活
	CacheMaxEntries  int           `json:"cache_max_entries"`
	CacheNamespace   string        `json:"cache_namespace"` // 隔离不同部署

	// ---- 系统指令 ----
	SystemPrompt     string `json:"system_prompt"`
	SystemPromptFile string `json:"system_prompt_file"`

	// ---- 观测 ----
	LogLevel    string `json:"log_level"` // debug|info|warn|error
	MetricsOn   bool   `json:"metrics_on"`
	PprofListen string `json:"pprof_listen"`

	// ---- 透传型上游（OpenAI 兼容，如本地部署的 codex2api）----
	// 命中 Codex 家族的模型名会优先走这里：codex2api 自己管账号池/OAuth/计费，
	// 网关只做路由与透传，避免重复翻译造成信息损失。
	Codex2APIBase   string `json:"codex2api_base"`   // 如 http://127.0.0.1:8080（空=不启用）
	Codex2APIKey    string `json:"codex2api_key"`    // codex2api 的 /v1 API Key
	Codex2APIRoutes bool   `json:"codex2api_routes"` // 是否按模型名把 Codex 家族路由过去

	// inlineCookies 由环境变量 PG_COOKIES 直接注入，刻意不参与 JSON 序列化，
	// 避免把账号凭据写进配置文件。
	inlineCookies []string
}

// Default 返回生产默认值。数值均来自 12 个开源实现的实测经验（见 docs/research/）。
func Default() Config {
	return Config{
		Listen:          "127.0.0.1:8787",
		ShutdownGrace:   10 * time.Second,
		MaxBodyBytes:    32 << 20,
		UpstreamTimeout: 4 * time.Minute,

		Origin: "https://prism.openai.com",
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 " +
			"(KHTML, like Gecko) Chrome/152.0.0.0 Safari/537.36 Edg/152.0.0.0",
		StartTimeout:    75 * time.Second,
		PollCallTimeout: 30 * time.Second,
		PollBudget:      240 * time.Second,
		SyncPollTries:   4,
		StartAttempts:   3,
		MaxReconnects:   2,
		ForceHTTP1:      true,

		SandboxTTL:        20 * time.Minute,
		PerAccountConc:    8,
		MaxSandboxPerAcct: 2,
		PrewarmWorkers:    2,
		WarmOnStart:       true,
		RetryBackoffMin:   1500 * time.Millisecond,
		RetryBackoffMax:   10 * time.Second,
		CooldownOnAuthErr: 2 * time.Minute,

		ChunkSize:      24,
		ChunkInterval:  12 * time.Millisecond,
		KeepAliveEvery: 15 * time.Second,
		EmitReasoning:  true,

		InlineB64Total:   96 << 10,
		InlineB64PerFile: 48 << 10,
		MaxImageDim:      1400,
		ImageJPEGQuality: 82,

		TokensPerMessage: 4,
		TokensPerTool:    150,
		ImageTokenFlat:   1600,
		CacheTTL:         10 * time.Minute,
		CacheMaxEntries:  4096,
		CacheNamespace:   "default",

		LogLevel:  "info",
		MetricsOn: true,
		// codex2api 透传：默认按模型名路由（需显式配置 base 才真正启用）。
		Codex2APIRoutes: true,
	}
}

// Load 读默认值 → 可选 JSON 文件 → 环境变量覆盖（env 优先级最高）。
func Load(path string) (Config, error) {
	c := Default()
	if path == "" {
		path = os.Getenv("PRISM_GATEWAY_CONFIG")
	}
	if strings.TrimSpace(path) != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return c, fmt.Errorf("config: read %s: %w", path, err)
		}
		if err := json.Unmarshal(b, &c); err != nil {
			return c, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}
	applyEnv(&c)
	if err := c.Validate(); err != nil {
		return c, err
	}
	return c, nil
}

// applyEnv 环境变量覆盖。命名统一 PG_ 前缀。
func applyEnv(c *Config) {
	envStr(&c.Listen, "PG_LISTEN")
	envStr(&c.Origin, "PG_ORIGIN")
	envStr(&c.UserAgent, "PG_USER_AGENT")
	envStr(&c.SystemPrompt, "PG_SYSTEM_PROMPT")
	envStr(&c.SystemPromptFile, "PG_SYSTEM_PROMPT_FILE")
	envStr(&c.LogLevel, "PG_LOG_LEVEL")
	envStr(&c.CacheNamespace, "PG_CACHE_NAMESPACE")
	envStr(&c.PprofListen, "PG_PPROF_LISTEN")
	envDur(&c.UpstreamTimeout, "PG_UPSTREAM_TIMEOUT")
	envDur(&c.StartTimeout, "PG_START_TIMEOUT")
	envDur(&c.PollBudget, "PG_POLL_BUDGET")
	envDur(&c.SandboxTTL, "PG_SANDBOX_TTL")
	envDur(&c.ChunkInterval, "PG_CHUNK_INTERVAL")
	envDur(&c.KeepAliveEvery, "PG_KEEPALIVE_EVERY")
	envDur(&c.CacheTTL, "PG_CACHE_TTL")
	envInt(&c.ChunkSize, "PG_CHUNK_SIZE")
	envInt(&c.PerAccountConc, "PG_PER_ACCOUNT_CONC")
	envInt(&c.MaxSandboxPerAcct, "PG_MAX_SANDBOX_PER_ACCT")
	envInt(&c.PrewarmWorkers, "PG_PREWARM_WORKERS")
	envInt(&c.InlineB64Total, "PG_INLINE_B64_TOTAL")
	envInt(&c.InlineB64PerFile, "PG_INLINE_B64_PER_FILE")
	envInt(&c.MaxImageDim, "PG_MAX_IMAGE_DIM")
	envBool(&c.EmitReasoning, "PG_EMIT_REASONING")
	envBool(&c.WarmOnStart, "PG_WARM_ON_START")
	envBool(&c.MetricsOn, "PG_METRICS_ON")
	envStr(&c.Codex2APIBase, "PG_CODEX2API_BASE")
	envStr(&c.Codex2APIKey, "PG_CODEX2API_KEY")
	envBool(&c.Codex2APIRoutes, "PG_CODEX2API_ROUTES")
	if v, ok := os.LookupEnv("PG_API_KEYS"); ok {
		c.APIKeys = splitList(v)
	}
	if v, ok := os.LookupEnv("PG_COOKIES"); ok {
		// 便捷入口：PG_COOKIES="cookie1|||cookie2" 直接建号池，无需凭据文件。
		cookies := splitList(v)
		c.inlineCookies = cookies
	}
}

// inlineCookies 由 env 直接提供的 cookie 列表（Load 内部使用）。
// 不导出 JSON，避免把凭据写进配置文件。
func (c *Config) InlineCookies() []string { return c.inlineCookies }

func (c *Config) Validate() error {
	if c.Listen == "" {
		return fmt.Errorf("config: listen 不能为空")
	}
	if !strings.HasPrefix(c.Origin, "http") {
		return fmt.Errorf("config: origin 必须是 http(s) URL，当前 %q", c.Origin)
	}
	if c.ChunkSize < 1 {
		c.ChunkSize = 24
	}
	if c.PerAccountConc < 1 {
		c.PerAccountConc = 1
	}
	if c.InlineB64Total < c.InlineB64PerFile {
		c.InlineB64Total = c.InlineB64PerFile
	}
	c.Origin = strings.TrimRight(c.Origin, "/")
	return nil
}

// ------------------------------------------------------------------ helpers

func envStr(dst *string, key string) {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		*dst = strings.TrimSpace(v)
	}
}

func envDur(dst *time.Duration, key string) {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
			*dst = d
			return
		}
		// 纯数字按秒处理
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			*dst = time.Duration(n) * time.Second
		}
	}
}

func envInt(dst *int, key string) {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			*dst = n
		}
	}
}

func envBool(dst *bool, key string) {
	if v, ok := os.LookupEnv(key); ok {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			*dst = true
		case "0", "false", "no", "off":
			*dst = false
		}
	}
}

func splitList(v string) []string {
	fields := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' })
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if s := strings.TrimSpace(f); s != "" {
			out = append(out, s)
		}
	}
	return out
}
