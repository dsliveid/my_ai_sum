package app

import (
	"database/sql"
	"net/http"
	"sync"
	"time"
)

type Config struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	DataDir     string `json:"data_dir"`
	AutoOpen    bool   `json:"auto_open"`
	LogLevel    string `json:"log_level"`
	Initialized bool   `json:"initialized"`
}

type App struct {
	cfg       Config
	db        *sql.DB
	masterKey []byte
	sessions  map[string]time.Time
	mu        sync.Mutex

	handler    http.Handler
	server     *http.Server
	listenHost string
	listenPort int
	serveErr   chan error
	serverMu   sync.Mutex
}

type ProviderKey struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ProviderType    string `json:"provider_type"`
	BaseURL         string `json:"base_url"`
	APIKey          string `json:"api_key,omitempty"`
	ProxyMode       string `json:"proxy_mode"`
	ProxyID         string `json:"proxy_id"`
	Priority        int    `json:"priority"`
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
	CreatedAt        string `json:"created_at"`
}

type UsageRecord struct {
	ID               string `json:"id"`
	RequestID        string `json:"request_id"`
	ProviderKeyID    string `json:"provider_key_id"`
	ProviderType     string `json:"provider_type"`
	LocalAPIKeyID    string `json:"local_api_key_id"`
	Source           string `json:"source"`
	LocalModel       string `json:"local_model"`
	UpstreamModel    string `json:"upstream_model"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	TotalTokens      int    `json:"total_tokens"`
	UsageSource      string `json:"usage_source"`
	Stream           bool   `json:"stream"`
	Success          bool   `json:"success"`
	StatusCode       int    `json:"status_code"`
	LatencyMS        int64  `json:"latency_ms"`
	CreatedAt        string `json:"created_at"`
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
