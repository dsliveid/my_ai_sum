package app

import (
	"database/sql"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Host                 string `json:"host"`
	Port                 int    `json:"port"`
	DataDir              string `json:"data_dir"`
	AutoOpen             bool   `json:"auto_open"`
	LogLevel             string `json:"log_level"`
	APIDebugEnabled      bool   `json:"api_debug_enabled"`
	APIDebugLevel        string `json:"api_debug_level"`
	APIDebugRequestBody  bool   `json:"api_debug_request_body"`
	APIDebugResponseBody bool   `json:"api_debug_response_body"`
	APIDebugMaxBodyChars int    `json:"api_debug_max_body_chars"`
	Initialized          bool   `json:"initialized"`
}

type App struct {
	// cfg 用 atomic.Pointer 承载配置：配置在运行时会被系统设置保存、监听地址热切换、
	// api_debug 开关切换等多处并发改写，同时网关请求路径会高频读取，
	// 裸值字段读写在没有锁保护时构成数据竞争，故改为原子指针做整体替换。
	cfg       atomic.Pointer[Config]
	db        *sql.DB
	masterKey []byte
	sessions  map[string]time.Time
	mu        sync.Mutex

	// clients 缓存 per-provider 的 http.Client，使同一 provider 的连续请求复用底层 TCP 连接。
	// 上游若为多副本集群，连接复用是缓存命中（KV Cache）的必要前提：负载均衡按连接分发，
	// 每请求新建连接会导致请求被分散到不同副本，缓存前缀无法命中。
	clients          map[string]*http.Client
	clientsMu        sync.Mutex
	sharedTransport  *http.Transport
	sharedTransportM sync.Mutex

	handler    http.Handler
	server     *http.Server
	listenHost string
	listenPort int
	serveErr   chan error
	serverMu   sync.Mutex
}

// config 返回当前配置的**值副本**。返回副本而非指针，
// 一方面避免调用方顺手修改穿透到共享状态，另一方面让调用方可安全地多次读取同一快照。
// 若尚未 Store 过（测试里可能只构造了 App 而未设置配置），回退到默认配置，避免解引用 nil 指针 panic。
func (a *App) config() Config {
	if p := a.cfg.Load(); p != nil {
		return *p
	}
	return defaultConfig()
}

// setConfig 以整体替换的方式更新配置。
// 先把入参拷贝到局部变量再取其地址，确保存入的指针指向堆上独立副本，
// 不随后续调用方对原变量的修改而变化（避免出现"已发布配置被外部改动"的隐性竞争）。
func (a *App) setConfig(c Config) {
	next := c
	a.cfg.Store(&next)
}

type ProviderKey struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ProviderType    string `json:"provider_type"`
	BaseURL         string `json:"base_url"`
	RequestProtocol string `json:"request_protocol"`
	APIKey          string `json:"api_key,omitempty"`
	ProxyMode       string `json:"proxy_mode"`
	ProxyID         string `json:"proxy_id"`
	Enabled         bool   `json:"enabled"`
	Models          string `json:"models"`
	LastCheckStatus string `json:"last_check_status"`
	LastError       string `json:"last_error"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type ProxyConfig struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Type            string `json:"type"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Username        string `json:"username"`
	Password        string `json:"password,omitempty"`
	Enabled         bool   `json:"enabled"`
	IsDefault       bool   `json:"is_default"`
	LastCheckStatus string `json:"last_check_status"`
	LastError       string `json:"last_error"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

type ModelMapping struct {
	ID            string `json:"id"`
	LocalModel    string `json:"local_model"`
	UpstreamModel string `json:"upstream_model"`
	ProviderKeyID string `json:"provider_key_id"`
	Capability    string `json:"capability"`
	Enabled       bool   `json:"enabled"`
	CreatedAt     string `json:"created_at"`
	UpdatedAt     string `json:"updated_at"`
}

type LocalAPIKey struct {
	ID                        string `json:"id"`
	Name                      string `json:"name"`
	Key                       string `json:"key,omitempty"`
	KeyMasked                 string `json:"key_masked,omitempty"`
	ProviderKeyID             string `json:"provider_key_id"`
	ProtocolConversionEnabled bool   `json:"protocol_conversion_enabled"`
	ClientProtocol            string `json:"client_protocol"`
	UpstreamProtocol          string `json:"upstream_protocol"`
	Enabled                   bool   `json:"enabled"`
	CreatedAt                 string `json:"created_at"`
	LastUsedAt                string `json:"last_used_at"`
}

type RequestLog struct {
	ID               string `json:"id"`
	Source           string `json:"source"`
	ProviderKeyID    string `json:"provider_key_id"`
	LocalAPIKeyID    string `json:"local_api_key_id"`
	ClientIP         string `json:"client_ip"`
	LocalModel       string `json:"local_model"`
	UpstreamModel    string `json:"upstream_model"`
	Method           string `json:"method"`
	Path             string `json:"path"`
	StatusCode       int    `json:"status_code"`
	LatencyMS        int64  `json:"latency_ms"`
	Success          bool   `json:"success"`
	ErrorMessage     string `json:"error_message"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	CachedTokens     int    `json:"cached_tokens"`
	CreatedAt        string `json:"created_at"`
}

type UsageRecord struct {
	ID               string `json:"id"`
	RequestID        string `json:"request_id"`
	ProviderKeyID    string `json:"provider_key_id"`
	ProviderType     string `json:"provider_type"`
	LocalAPIKeyID    string `json:"local_api_key_id"`
	ClientIP         string `json:"client_ip"`
	Source           string `json:"source"`
	LocalModel       string `json:"local_model"`
	UpstreamModel    string `json:"upstream_model"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	// CachedTokens / CacheCreationTokens 用于计算上游缓存命中率（CachedTokens / PromptTokens）。
	CachedTokens        int    `json:"cached_tokens"`
	CacheCreationTokens int    `json:"cache_creation_tokens"`
	UsageSource         string `json:"usage_source"`
	Stream              bool   `json:"stream"`
	Success             bool   `json:"success"`
	StatusCode          int    `json:"status_code"`
	LatencyMS           int64  `json:"latency_ms"`
	CreatedAt           string `json:"created_at"`
}

type TestChatSession struct {
	ID              string `json:"id"`
	TargetType      string `json:"target_type"`
	ProviderKeyID   string `json:"provider_key_id"`
	LocalAPIKeyID   string `json:"local_api_key_id"`
	Model           string `json:"model"`
	Stream          bool   `json:"stream"`
	RequestSummary  string `json:"request_summary"`
	ResponseSummary string `json:"response_summary"`
	StatusCode      int    `json:"status_code"`
	LatencyMS       int64  `json:"latency_ms"`
	Success         bool   `json:"success"`
	ErrorMessage    string `json:"error_message"`
	CreatedAt       string `json:"created_at"`
}

type ChatRequest struct {
	Model       string           `json:"model"`
	Messages    []map[string]any `json:"messages"`
	Stream      bool             `json:"stream"`
	Temperature *float64         `json:"temperature,omitempty"`
	MaxTokens   *int             `json:"max_tokens,omitempty"`
	Extra       map[string]any   `json:"-"`
	Raw         map[string]any   `json:"-"`
}

type apiError struct {
	Error string `json:"error"`
}
