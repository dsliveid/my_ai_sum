package app

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/proxy"
	_ "modernc.org/sqlite"
)

func defaultConfig() Config {
	return Config{
		Host:                 "127.0.0.1",
		Port:                 8716,
		AutoOpen:             false,
		LogLevel:             "info",
		APIDebugLevel:        "info",
		APIDebugMaxBodyChars: 4000,
	}
}

func loadConfig() (Config, error) {
	cfg := defaultConfig()
	base, err := appBaseDir()
	if err != nil {
		return cfg, err
	}
	defaultDataDir := filepath.Join(base, "data")
	if env := os.Getenv("MY_AI_SUM_DATA_DIR"); env != "" {
		cfg.DataDir = resolveDataDir(env, base)
	} else {
		cfg.DataDir = defaultDataDir
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return cfg, err
	}
	path := filepath.Join(cfg.DataDir, "config.json")
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &cfg)
		loadedDataDir := cfg.DataDir
		if cfg.Host == "" {
			cfg.Host = "127.0.0.1"
		}
		if cfg.Port == 0 {
			cfg.Port = 8716
		}
		normalizeAPIDebugConfig(&cfg)
		if env := os.Getenv("MY_AI_SUM_DATA_DIR"); env != "" {
			cfg.DataDir = resolveDataDir(env, base)
		} else {
			cfg.DataDir = resolveDataDir(cfg.DataDir, base)
			if shouldUseCurrentDefaultDataDir(loadedDataDir, cfg.DataDir, defaultDataDir, path) {
				cfg.DataDir = defaultDataDir
			}
		}
		return cfg, saveConfig(cfg)
	}
	return cfg, saveConfig(cfg)
}

func saveConfig(cfg Config) error {
	base, err := appBaseDir()
	if err != nil {
		return err
	}
	cfg.DataDir = resolveDataDir(cfg.DataDir, base)
	normalizeAPIDebugConfig(&cfg)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	fileCfg := cfg
	fileCfg.DataDir = portableDataDirForConfig(cfg.DataDir, base)
	b, err := json.MarshalIndent(fileCfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(cfg.DataDir, "config.json"), b, 0o600)
}

func normalizeAPIDebugConfig(cfg *Config) {
	cfg.APIDebugLevel = strings.ToLower(strings.TrimSpace(cfg.APIDebugLevel))
	switch cfg.APIDebugLevel {
	case "error", "info", "debug", "trace":
	default:
		cfg.APIDebugLevel = "info"
	}
	if cfg.APIDebugMaxBodyChars <= 0 {
		cfg.APIDebugMaxBodyChars = 4000
	}
	if cfg.APIDebugMaxBodyChars > 200000 {
		cfg.APIDebugMaxBodyChars = 200000
	}
}

func appBaseDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

func resolveDataDir(configured, base string) string {
	configured = strings.TrimSpace(os.ExpandEnv(configured))
	if configured == "" {
		return filepath.Join(base, "data")
	}
	if filepath.IsAbs(configured) {
		return filepath.Clean(configured)
	}
	return filepath.Clean(filepath.Join(base, configured))
}

func portableDataDirForConfig(dataDir, base string) string {
	defaultDataDir := filepath.Join(base, "data")
	if samePath(dataDir, defaultDataDir) {
		return "data"
	}
	return dataDir
}

func shouldUseCurrentDefaultDataDir(configured, resolved, defaultDataDir, configPath string) bool {
	configured = strings.TrimSpace(configured)
	if configured == "" || !filepath.IsAbs(configured) || samePath(resolved, defaultDataDir) {
		return false
	}
	if !samePath(filepath.Dir(configPath), defaultDataDir) {
		return false
	}
	return strings.EqualFold(filepath.Base(resolved), "data")
}

func samePath(a, b string) bool {
	aa, err := filepath.Abs(filepath.Clean(a))
	if err != nil {
		aa = filepath.Clean(a)
	}
	bb, err := filepath.Abs(filepath.Clean(b))
	if err != nil {
		bb = filepath.Clean(b)
	}
	return strings.EqualFold(aa, bb)
}

func openStore(cfg Config) (*sql.DB, error) {
	db, err := sql.Open("sqlite", filepath.Join(cfg.DataDir, "my_ai_sum.db"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	if _, err := db.Exec(`PRAGMA foreign_keys = ON; PRAGMA journal_mode = WAL;`); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, migrate(db)
}

func migrate(db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY, applied_at TEXT NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS app_settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS admin_users (
			id TEXT PRIMARY KEY, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL, salt TEXT NOT NULL, created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS provider_keys (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, provider_type TEXT NOT NULL, base_url TEXT NOT NULL,
			api_key_enc TEXT NOT NULL, proxy_mode TEXT NOT NULL DEFAULT 'none', proxy_id TEXT NOT NULL DEFAULT '',
			request_protocol TEXT NOT NULL DEFAULT 'responses',
			enabled INTEGER NOT NULL DEFAULT 1, models TEXT NOT NULL DEFAULT '',
			last_check_status TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);`,
		`ALTER TABLE provider_keys ADD COLUMN request_protocol TEXT NOT NULL DEFAULT 'responses';`,
		`ALTER TABLE provider_keys DROP COLUMN ` + strings.Join([]string{"prior", "ity"}, "") + `;`,
		`CREATE TABLE IF NOT EXISTS proxies (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, type TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL,
			username TEXT NOT NULL DEFAULT '', password_enc TEXT NOT NULL DEFAULT '', enabled INTEGER NOT NULL DEFAULT 1,
			is_default INTEGER NOT NULL DEFAULT 0, last_check_status TEXT NOT NULL DEFAULT '', last_error TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS model_mappings (
			id TEXT PRIMARY KEY, local_model TEXT NOT NULL UNIQUE, upstream_model TEXT NOT NULL,
			provider_key_id TEXT NOT NULL, capability TEXT NOT NULL DEFAULT 'chat', enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS local_api_keys (
			id TEXT PRIMARY KEY, name TEXT NOT NULL, key_hash TEXT NOT NULL, salt TEXT NOT NULL,
			key_enc TEXT NOT NULL DEFAULT '', provider_key_id TEXT NOT NULL DEFAULT '',
			protocol_conversion_enabled INTEGER NOT NULL DEFAULT 0, client_protocol TEXT NOT NULL DEFAULT '', upstream_protocol TEXT NOT NULL DEFAULT '',
			enabled INTEGER NOT NULL DEFAULT 1, created_at TEXT NOT NULL, last_used_at TEXT NOT NULL DEFAULT ''
		);`,
		`ALTER TABLE local_api_keys ADD COLUMN key_enc TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE local_api_keys ADD COLUMN provider_key_id TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE local_api_keys ADD COLUMN protocol_conversion_enabled INTEGER NOT NULL DEFAULT 0;`,
		`ALTER TABLE local_api_keys ADD COLUMN client_protocol TEXT NOT NULL DEFAULT '';`,
		`ALTER TABLE local_api_keys ADD COLUMN upstream_protocol TEXT NOT NULL DEFAULT '';`,
		`CREATE TABLE IF NOT EXISTS request_logs (
			id TEXT PRIMARY KEY, source TEXT NOT NULL, provider_key_id TEXT NOT NULL DEFAULT '', local_api_key_id TEXT NOT NULL DEFAULT '',
			local_model TEXT NOT NULL DEFAULT '', upstream_model TEXT NOT NULL DEFAULT '', method TEXT NOT NULL, path TEXT NOT NULL,
			status_code INTEGER NOT NULL, latency_ms INTEGER NOT NULL, success INTEGER NOT NULL, error_message TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS usage_records (
			id TEXT PRIMARY KEY, request_id TEXT NOT NULL, provider_key_id TEXT NOT NULL, provider_type TEXT NOT NULL,
			local_api_key_id TEXT NOT NULL DEFAULT '', source TEXT NOT NULL, local_model TEXT NOT NULL DEFAULT '', upstream_model TEXT NOT NULL DEFAULT '',
			prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
			usage_source TEXT NOT NULL, stream INTEGER NOT NULL, success INTEGER NOT NULL, status_code INTEGER NOT NULL,
			latency_ms INTEGER NOT NULL, created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS usage_daily_aggregates (
			date TEXT NOT NULL, provider_key_id TEXT NOT NULL, provider_type TEXT NOT NULL DEFAULT '', model TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT '', request_count INTEGER NOT NULL DEFAULT 0, success_count INTEGER NOT NULL DEFAULT 0,
			prompt_tokens INTEGER NOT NULL DEFAULT 0, completion_tokens INTEGER NOT NULL DEFAULT 0, total_tokens INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(date, provider_key_id, model, source)
		);`,
		`CREATE TABLE IF NOT EXISTS test_chat_sessions (
			id TEXT PRIMARY KEY, target_type TEXT NOT NULL, provider_key_id TEXT NOT NULL DEFAULT '', local_api_key_id TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL, stream INTEGER NOT NULL, request_summary TEXT NOT NULL DEFAULT '', response_summary TEXT NOT NULL DEFAULT '',
			status_code INTEGER NOT NULL, latency_ms INTEGER NOT NULL, success INTEGER NOT NULL, error_message TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_provider_time ON usage_records(provider_key_id, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_model_time ON usage_records(local_model, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_request_logs_time ON request_logs(created_at);`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			msg := strings.ToLower(err.Error())
			if strings.Contains(msg, "duplicate column") || strings.Contains(msg, "no such column") {
				continue
			}
			return err
		}
	}
	return nil
}

func loadMasterKey(dataDir string) ([]byte, error) {
	path := filepath.Join(dataDir, "master.key")
	if b, err := os.ReadFile(path); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err == nil && len(raw) == 32 {
			return raw, nil
		}
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	return key, os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600)
}

func (a *App) encryptText(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	block, err := aes.NewCipher(a.masterKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	out := append(nonce, gcm.Seal(nil, nonce, []byte(s), nil)...)
	return base64.StdEncoding.EncodeToString(out), nil
}

func (a *App) decryptText(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(a.masterKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("invalid encrypted value")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func randomID(prefix string) string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return prefix + "_" + hex.EncodeToString(b)
}

func randomSecret(prefix string) string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return prefix + "_" + base64.RawURLEncoding.EncodeToString(b)
}

func hashSecret(secret, salt string) string {
	sum := sha256.Sum256([]byte(salt + ":" + secret))
	return hex.EncodeToString(sum[:])
}

func newSalt() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func now() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func intBool(v int) bool {
	return v != 0
}

func normalizeBaseURL(s string) string {
	s = strings.TrimSpace(s)
	return strings.TrimRight(s, "/")
}

func defaultBaseURL(providerType, baseURL string) string {
	if strings.TrimSpace(baseURL) != "" {
		return normalizeBaseURL(baseURL)
	}
	switch providerType {
	case "deepseek":
		return "https://api.deepseek.com/v1"
	case "grok":
		return "https://api.x.ai/v1"
	default:
		return normalizeBaseURL(baseURL)
	}
}

func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return "****"
	}
	return s[:4] + "..." + s[len(s)-4:]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, apiError{Error: msg})
}

func readJSON(r *http.Request, dst any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(dst)
}

func queryLimit(r *http.Request, fallback int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 || n > 1000 {
		return fallback
	}
	return n
}

func (a *App) isInitialized() bool {
	var n int
	_ = a.db.QueryRow(`SELECT COUNT(*) FROM admin_users`).Scan(&n)
	return n > 0
}

func (a *App) httpClientForProvider(p ProviderKey) (*http.Client, error) {
	transport := &http.Transport{}
	var px *ProxyConfig
	if p.ProxyMode == "default" {
		proxyCfg, err := a.getDefaultProxy()
		if err == nil && proxyCfg.Enabled {
			px = &proxyCfg
		}
	} else if p.ProxyMode == "custom" && p.ProxyID != "" {
		proxyCfg, err := a.getProxy(p.ProxyID, true)
		if err == nil && proxyCfg.Enabled {
			px = &proxyCfg
		}
	}
	if px != nil {
		switch strings.ToLower(px.Type) {
		case "http", "https":
			u := &url.URL{Scheme: strings.ToLower(px.Type), Host: fmt.Sprintf("%s:%d", px.Host, px.Port)}
			if px.Username != "" {
				pw, _ := a.decryptProxyPassword(*px)
				u.User = url.UserPassword(px.Username, pw)
			}
			transport.Proxy = http.ProxyURL(u)
		case "socks5":
			addr := fmt.Sprintf("%s:%d", px.Host, px.Port)
			var auth *proxy.Auth
			if px.Username != "" {
				pw, _ := a.decryptProxyPassword(*px)
				auth = &proxy.Auth{User: px.Username, Password: pw}
			}
			dialer, err := proxy.SOCKS5("tcp", addr, auth, proxy.Direct)
			if err != nil {
				return nil, err
			}
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				type contextDialer interface {
					DialContext(context.Context, string, string) (net.Conn, error)
				}
				if d, ok := dialer.(contextDialer); ok {
					return d.DialContext(ctx, network, address)
				}
				return dialer.Dial(network, address)
			}
		}
	}
	return &http.Client{Timeout: 120 * time.Second, Transport: transport}, nil
}

func (a *App) decryptProviderKey(p ProviderKey) (string, error) {
	var enc string
	if err := a.db.QueryRow(`SELECT api_key_enc FROM provider_keys WHERE id = ?`, p.ID).Scan(&enc); err != nil {
		return "", err
	}
	return a.decryptText(enc)
}

func (a *App) decryptProxyPassword(p ProxyConfig) (string, error) {
	var enc string
	if err := a.db.QueryRow(`SELECT password_enc FROM proxies WHERE id = ?`, p.ID).Scan(&enc); err != nil {
		return "", err
	}
	return a.decryptText(enc)
}

func (a *App) getDefaultProxy() (ProxyConfig, error) {
	var p ProxyConfig
	var enabled, isDefault int
	err := a.db.QueryRow(`SELECT id,name,type,host,port,username,enabled,is_default,last_check_status,last_error,created_at,updated_at FROM proxies WHERE is_default=1 LIMIT 1`).
		Scan(&p.ID, &p.Name, &p.Type, &p.Host, &p.Port, &p.Username, &enabled, &isDefault, &p.LastCheckStatus, &p.LastError, &p.CreatedAt, &p.UpdatedAt)
	p.Enabled = intBool(enabled)
	p.IsDefault = intBool(isDefault)
	return p, err
}

func (a *App) getProxy(id string, includeSecret bool) (ProxyConfig, error) {
	var p ProxyConfig
	var enabled, isDefault int
	var passwordEnc string
	err := a.db.QueryRow(`SELECT id,name,type,host,port,username,password_enc,enabled,is_default,last_check_status,last_error,created_at,updated_at FROM proxies WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.Type, &p.Host, &p.Port, &p.Username, &passwordEnc, &enabled, &isDefault, &p.LastCheckStatus, &p.LastError, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	p.Enabled = intBool(enabled)
	p.IsDefault = intBool(isDefault)
	if includeSecret {
		pw, _ := a.decryptText(passwordEnc)
		p.Password = pw
	}
	return p, nil
}

func (a *App) getProvider(id string, includeSecret bool) (ProviderKey, error) {
	var p ProviderKey
	var enabled int
	var enc string
	err := a.db.QueryRow(`SELECT id,name,provider_type,base_url,request_protocol,api_key_enc,proxy_mode,proxy_id,enabled,models,last_check_status,last_error,created_at,updated_at FROM provider_keys WHERE id=?`, id).
		Scan(&p.ID, &p.Name, &p.ProviderType, &p.BaseURL, &p.RequestProtocol, &enc, &p.ProxyMode, &p.ProxyID, &enabled, &p.Models, &p.LastCheckStatus, &p.LastError, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return p, err
	}
	p.Enabled = intBool(enabled)
	p.RequestProtocol = normalizeProtocolName(p.RequestProtocol)
	if p.RequestProtocol == "" {
		p.RequestProtocol = protocolResponses
	}
	if includeSecret {
		key, _ := a.decryptText(enc)
		p.APIKey = key
	} else {
		key, _ := a.decryptText(enc)
		p.APIKey = maskSecret(key)
	}
	return p, nil
}

func (a *App) findMapping(localModel string) (ModelMapping, ProviderKey, error) {
	var m ModelMapping
	var enabled int
	err := a.db.QueryRow(`SELECT id,local_model,upstream_model,provider_key_id,capability,enabled,created_at,updated_at FROM model_mappings WHERE local_model=? AND enabled=1`, localModel).
		Scan(&m.ID, &m.LocalModel, &m.UpstreamModel, &m.ProviderKeyID, &m.Capability, &enabled, &m.CreatedAt, &m.UpdatedAt)
	if err == nil {
		m.Enabled = intBool(enabled)
		p, err := a.getProvider(m.ProviderKeyID, true)
		return m, p, err
	}
	rows, err := a.db.Query(`SELECT id,name,provider_type,base_url,request_protocol,api_key_enc,proxy_mode,proxy_id,enabled,models,last_check_status,last_error,created_at,updated_at FROM provider_keys WHERE enabled=1 ORDER BY created_at ASC`)
	if err != nil {
		return m, ProviderKey{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var p ProviderKey
		var enc string
		var pe int
		if err := rows.Scan(&p.ID, &p.Name, &p.ProviderType, &p.BaseURL, &p.RequestProtocol, &enc, &p.ProxyMode, &p.ProxyID, &pe, &p.Models, &p.LastCheckStatus, &p.LastError, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return m, ProviderKey{}, err
		}
		p.RequestProtocol = normalizeProtocolName(p.RequestProtocol)
		if p.RequestProtocol == "" {
			p.RequestProtocol = protocolResponses
		}
		if containsModel(p.Models, localModel) {
			p.Enabled = intBool(pe)
			p.APIKey, _ = a.decryptText(enc)
			m = ModelMapping{LocalModel: localModel, UpstreamModel: localModel, ProviderKeyID: p.ID, Capability: "chat", Enabled: true}
			return m, p, nil
		}
	}
	return m, ProviderKey{}, sql.ErrNoRows
}

func (a *App) findMappingForProvider(localModel, providerKeyID string) (ModelMapping, bool) {
	var m ModelMapping
	var enabled int
	err := a.db.QueryRow(`SELECT id,local_model,upstream_model,provider_key_id,capability,enabled,created_at,updated_at FROM model_mappings WHERE local_model=? AND provider_key_id=? AND enabled=1`, localModel, providerKeyID).
		Scan(&m.ID, &m.LocalModel, &m.UpstreamModel, &m.ProviderKeyID, &m.Capability, &enabled, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return ModelMapping{}, false
	}
	m.Enabled = intBool(enabled)
	return m, true
}

func (a *App) getLocalAPIKeyByID(id string) (LocalAPIKey, error) {
	var k LocalAPIKey
	var enabled, protocolConversionEnabled int
	err := a.db.QueryRow(`SELECT id,name,provider_key_id,protocol_conversion_enabled,client_protocol,upstream_protocol,enabled,created_at,last_used_at FROM local_api_keys WHERE id=?`, id).
		Scan(&k.ID, &k.Name, &k.ProviderKeyID, &protocolConversionEnabled, &k.ClientProtocol, &k.UpstreamProtocol, &enabled, &k.CreatedAt, &k.LastUsedAt)
	k.Enabled = intBool(enabled)
	k.ProtocolConversionEnabled = intBool(protocolConversionEnabled)
	a.normalizeLocalKeyProtocolConfig(&k)
	return k, err
}

func containsModel(models, model string) bool {
	if strings.TrimSpace(models) == "" {
		return true
	}
	for _, item := range strings.Split(models, ",") {
		if strings.TrimSpace(item) == model {
			return true
		}
	}
	return false
}

func readAllLimit(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}
