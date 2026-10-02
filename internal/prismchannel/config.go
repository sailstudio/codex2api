package prismchannel

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	MaterialURL         string
	MaterialBearer      string `json:"-"`
	MaterialHeadersOnly bool
	Enabled             bool
	Force               bool
	BaseURL             string
	Credentials         []Credential `json:"-"`
	SandboxPoolSize     int
	PrewarmSlots        int
	AccountRPM          int
	AccountConcurrency  int
	MaxWaiters          int
	QueueTimeout        time.Duration
	RequestTimeout      time.Duration
	HTTPTimeout         time.Duration
	PollMin             time.Duration
	PollMax             time.Duration
	SandboxTTL          time.Duration
	KeepWarm            time.Duration
	CacheTTL            time.Duration
	CacheBytes          int64
	CacheEntries        int
	ImageBytes          int64
	ImageInline         bool
	AllowRemoteImages   bool
	StreamKeepalive     time.Duration
}

func DefaultConfig() Config {
	return Config{BaseURL: "https://prism.openai.com", AccountConcurrency: 4, MaxWaiters: 64,
		QueueTimeout: 5 * time.Second, RequestTimeout: 3 * time.Minute, HTTPTimeout: 20 * time.Second,
		PollMin: 100 * time.Millisecond, PollMax: time.Second, SandboxTTL: 2 * time.Minute,
		KeepWarm: time.Minute, CacheTTL: 10 * time.Minute, CacheBytes: 64 << 20, CacheEntries: 2000, ImageBytes: 10 << 20, StreamKeepalive: 10 * time.Second}
}

func LoadEnv() (Config, error) {
	c := DefaultConfig()
	for _, v := range []struct {
		name   string
		target *bool
	}{
		{"PRISM_ENABLED", &c.Enabled}, {"PRISM_FORCE", &c.Force}, {"PRISM_ALLOW_REMOTE_IMAGES", &c.AllowRemoteImages}, {"PRISM_IMAGE_INLINE", &c.ImageInline},
	} {
		if s := os.Getenv(v.name); s != "" {
			b, e := strconv.ParseBool(s)
			if e != nil {
				return c, fmt.Errorf("%s must be boolean", v.name)
			}
			*v.target = b
		}
	}
	if s := os.Getenv("PRISM_ACCOUNT_RPM"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 4096 {
			return c, fmt.Errorf("PRISM_ACCOUNT_RPM must be in 0..4096")
		}
		c.AccountRPM = n
	}
	if s := os.Getenv("PRISM_SANDBOX_POOL_SIZE"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 256 {
			return c, fmt.Errorf("PRISM_SANDBOX_POOL_SIZE must be in 0..256")
		}
		c.SandboxPoolSize = n
	}
	if s := os.Getenv("PRISM_PREWARM_SLOTS"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 4096 {
			return c, fmt.Errorf("PRISM_PREWARM_SLOTS must be in 0..4096")
		}
		c.PrewarmSlots = n
	}
	if s := os.Getenv("PRISM_BASE_URL"); s != "" {
		c.BaseURL = s
	}
	c.MaterialURL = os.Getenv("PRISM_MATERIAL_URL")
	c.MaterialHeadersOnly = parseBoolEnv("PRISM_MATERIAL_HEADERS_ONLY")
	c.MaterialBearer = os.Getenv("PRISM_MATERIAL_BEARER")
	for _, v := range []struct {
		name   string
		target *int
	}{
		{"PRISM_ACCOUNT_CONCURRENCY", &c.AccountConcurrency}, {"PRISM_MAX_WAITERS", &c.MaxWaiters}, {"PRISM_CACHE_ENTRIES", &c.CacheEntries},
	} {
		if s := os.Getenv(v.name); s != "" {
			n, e := strconv.Atoi(s)
			if e != nil || n < 1 || n > 100000 {
				return c, fmt.Errorf("%s must be in 1..100000", v.name)
			}
			*v.target = n
		}
	}
	for _, v := range []struct {
		name   string
		target *time.Duration
	}{
		{"PRISM_QUEUE_TIMEOUT", &c.QueueTimeout}, {"PRISM_REQUEST_TIMEOUT", &c.RequestTimeout}, {"PRISM_HTTP_TIMEOUT", &c.HTTPTimeout},
		{"PRISM_POLL_MIN", &c.PollMin}, {"PRISM_POLL_MAX", &c.PollMax}, {"PRISM_SANDBOX_TTL", &c.SandboxTTL},
		{"PRISM_KEEPWARM_INTERVAL", &c.KeepWarm}, {"PRISM_CACHE_TTL", &c.CacheTTL},
		{"PRISM_STREAM_KEEPALIVE", &c.StreamKeepalive},
	} {
		if s := os.Getenv(v.name); s != "" {
			d, e := time.ParseDuration(s)
			if e != nil || d < time.Millisecond || d > 24*time.Hour {
				return c, fmt.Errorf("%s must be a duration in 1ms..24h", v.name)
			}
			*v.target = d
		}
	}
	for _, v := range []struct {
		name   string
		target *int64
	}{{"PRISM_CACHE_BYTES", &c.CacheBytes}, {"PRISM_IMAGE_MAX_BYTES", &c.ImageBytes}} {
		if s := os.Getenv(v.name); s != "" {
			n, e := strconv.ParseInt(s, 10, 64)
			if e != nil || n <= 0 || n > 1<<30 {
				return c, fmt.Errorf("%s must be in 1..1073741824", v.name)
			}
			*v.target = n
		}
	}
	if !c.Enabled {
		if c.Force {
			return c, fmt.Errorf("PRISM_FORCE requires PRISM_ENABLED")
		}
		return c, nil
	}
	if p := os.Getenv("PRISM_ACCOUNTS_FILE"); p != "" {
		b, e := os.ReadFile(p)
		if e != nil {
			return c, fmt.Errorf("cannot read PRISM_ACCOUNTS_FILE")
		}
		if len(b) > 1<<20 || json.Unmarshal(b, &c.Credentials) != nil {
			return c, fmt.Errorf("invalid PRISM_ACCOUNTS_FILE JSON")
		}
	} else {
		c.Credentials = []Credential{{UserID: os.Getenv("PRISM_USER_ID"), SessionToken: os.Getenv("PRISM_SESSION_TOKEN"), AccessToken: os.Getenv("PRISM_ACCESS_TOKEN")}}
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	u, e := url.Parse(c.BaseURL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return fmt.Errorf("PRISM_BASE_URL must be an origin")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
		return fmt.Errorf("Prism requires HTTPS (HTTP only for loopback mocks)")
	}
	if len(c.Credentials) < 1 || len(c.Credentials) > 256 {
		return fmt.Errorf("Prism requires 1..256 credentials")
	}
	for _, cr := range c.Credentials {
		if (c.MaterialURL == "" || c.MaterialHeadersOnly) && strings.TrimSpace(cr.UserID) == "" {
			return fmt.Errorf("Prism requires user_id or PRISM_USER_ID for locally built contexts")
		}
		if c.MaterialURL == "" && (cr.SessionToken == "" || cr.AccessToken == "") || strings.ContainsAny(cr.SessionToken+cr.AccessToken, ";\r\n\x00") {
			return fmt.Errorf("Prism requires valid session/access tokens from operator")
		}
	}
	if c.MaterialURL != "" {
		u, e := url.Parse(c.MaterialURL)
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")) {
			return fmt.Errorf("PRISM_MATERIAL_URL must be HTTPS or loopback HTTP without URL credentials")
		}
		if strings.ContainsAny(c.MaterialBearer, "\r\n\x00") {
			return fmt.Errorf("invalid PRISM_MATERIAL_BEARER")
		}
	}
	if c.AccountRPM < 0 || c.AccountRPM > 4096 || c.SandboxPoolSize < 0 || c.SandboxPoolSize > c.AccountConcurrency || c.PrewarmSlots < 0 || c.PrewarmSlots > 4096 || c.AccountConcurrency < 1 || c.AccountConcurrency > 256 || c.MaxWaiters < 1 || c.CacheEntries < 1 || c.CacheBytes < 1 || c.ImageBytes < 1 || c.ImageBytes > 1<<30 {
		return fmt.Errorf("invalid Prism capacity configuration")
	}
	if len(c.Credentials)*c.AccountConcurrency > 4096 {
		return fmt.Errorf("Prism capacity must be <=4096 slots")
	}
	if c.PollMin <= 0 || c.PollMax < c.PollMin || c.HTTPTimeout <= 0 || c.RequestTimeout <= 0 || c.QueueTimeout <= 0 || c.SandboxTTL <= 0 || c.CacheTTL <= 0 || c.KeepWarm <= 0 || c.StreamKeepalive <= 0 {
		return fmt.Errorf("invalid Prism timeout configuration")
	}
	return nil
}

func parseBoolEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}
