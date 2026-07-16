package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"my_ai_sum/internal/web"
)

func Run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	key, err := loadMasterKey(cfg.DataDir)
	if err != nil {
		return err
	}
	a := &App{cfg: cfg, db: db, masterKey: key, sessions: map[string]time.Time{}, serveErr: make(chan error, 4)}
	a.cfg.Initialized = a.isInitialized()
	_ = saveConfig(a.cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", a.serveAPI)
	mux.HandleFunc("/v1/", a.serveGateway)
	mux.HandleFunc("/models", a.serveGateway)
	mux.HandleFunc("/responses", a.serveGateway)
	mux.HandleFunc("/chat/completions", a.serveGateway)
	mux.Handle("/", a.staticHandler())
	a.handler = withRecover(mux)

	url, _, err := a.switchHTTPServer(a.cfg.Host, a.cfg.Port)
	if err != nil {
		return err
	}
	log.Printf("my_ai_sum started at %s, data=%s", url, a.cfg.DataDir)
	if a.cfg.AutoOpen {
		go openBrowser(url)
	}
	return <-a.serveErr
}

func (a *App) switchHTTPServer(host string, port int) (string, *http.Server, error) {
	if host == "" {
		host = "127.0.0.1"
	}
	if port <= 0 {
		return "", nil, errors.New("port is required")
	}
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	url := browserURL(host, port)
	a.serverMu.Lock()
	if a.server != nil && a.listenHost == host && a.listenPort == port {
		a.serverMu.Unlock()
		return url, nil, nil
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		a.serverMu.Unlock()
		return "", nil, fmt.Errorf("listen %s failed: %w", addr, err)
	}
	srv := &http.Server{Handler: a.handler}
	old := a.server
	a.server = srv
	a.listenHost = host
	a.listenPort = port
	a.serverMu.Unlock()

	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			a.serveErr <- err
		}
	}()
	return url, old, nil
}

func browserURL(host string, port int) string {
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		host = "localhost"
	}
	return "http://" + net.JoinHostPort(host, fmt.Sprintf("%d", port)) + "/"
}

func shutdownOldServer(srv *http.Server) {
	if srv == nil {
		return
	}
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
	}
}

func withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				writeError(w, http.StatusInternalServerError, fmt.Sprint(v))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (a *App) staticHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		b, err := web.StaticFiles.ReadFile("static/" + path)
		if err != nil {
			b, err = web.StaticFiles.ReadFile("static/index.html")
			if err != nil {
				writeError(w, 404, "not found")
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		} else if strings.HasSuffix(path, ".css") {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		} else if strings.HasSuffix(path, ".js") {
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		} else if strings.HasSuffix(path, ".html") {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		_, _ = w.Write(b)
	}
}

func openBrowser(url string) {
	time.Sleep(300 * time.Millisecond)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func (a *App) serveAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1")
	if path == "/setup/status" && r.Method == http.MethodGet {
		a.handleSetupStatus(w, r)
		return
	}
	if path == "/setup/init" && r.Method == http.MethodPost {
		a.handleSetupInit(w, r)
		return
	}
	if path == "/auth/login" && r.Method == http.MethodPost {
		a.handleLogin(w, r)
		return
	}
	if !a.requireAdmin(w, r) {
		return
	}
	switch {
	case path == "/auth/me" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"username": "admin"})
	case path == "/auth/logout" && r.Method == http.MethodPost:
		a.handleLogout(w, r)
	case strings.HasPrefix(path, "/provider-keys"):
		a.handleProviderKeys(w, r, strings.TrimPrefix(path, "/provider-keys"))
	case strings.HasPrefix(path, "/proxies"):
		a.handleProxies(w, r, strings.TrimPrefix(path, "/proxies"))
	case strings.HasPrefix(path, "/model-mappings"):
		a.handleModelMappings(w, r, strings.TrimPrefix(path, "/model-mappings"))
	case strings.HasPrefix(path, "/local-api-keys"):
		a.handleLocalAPIKeys(w, r, strings.TrimPrefix(path, "/local-api-keys"))
	case strings.HasPrefix(path, "/test-chat"):
		a.handleTestChat(w, r, strings.TrimPrefix(path, "/test-chat"))
	case strings.HasPrefix(path, "/usage"):
		a.handleUsage(w, r, strings.TrimPrefix(path, "/usage"))
	case strings.HasPrefix(path, "/request-logs"):
		a.handleRequestLogs(w, r, strings.TrimPrefix(path, "/request-logs"))
	case strings.HasPrefix(path, "/api-debug-logs"):
		a.handleAPIDebugLogs(w, r, strings.TrimPrefix(path, "/api-debug-logs"))
	case path == "/settings":
		a.handleSettings(w, r)
	case path == "/settings/reload":
		a.handleSettingsReload(w, r)
	case path == "/system/info" && r.Method == http.MethodGet:
		writeJSON(w, 200, map[string]any{"name": "my_ai_sum", "version": "0.1.0", "data_dir": a.cfg.DataDir, "host": a.cfg.Host, "port": a.cfg.Port})
	default:
		writeError(w, 404, "not found")
	}
}

func (a *App) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"initialized": a.isInitialized(), "host": a.cfg.Host, "port": a.cfg.Port})
}

func (a *App) handleSetupInit(w http.ResponseWriter, r *http.Request) {
	if a.isInitialized() {
		writeError(w, 409, "already initialized")
		return
	}
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	if req.Username == "" {
		req.Username = "admin"
	}
	if len(req.Password) < 6 {
		writeError(w, 400, "password must be at least 6 characters")
		return
	}
	salt := newSalt()
	_, err := a.db.Exec(`INSERT INTO admin_users(id,username,password_hash,salt,created_at) VALUES(?,?,?,?,?)`, randomID("adm"), req.Username, hashSecret(req.Password, salt), salt, now())
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	a.cfg.Initialized = true
	_ = saveConfig(a.cfg)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	var hash, salt string
	err := a.db.QueryRow(`SELECT password_hash,salt FROM admin_users WHERE username=?`, req.Username).Scan(&hash, &salt)
	if err != nil || hashSecret(req.Password, salt) != hash {
		writeError(w, 401, "invalid username or password")
		return
	}
	token := randomSecret("adm")
	a.mu.Lock()
	a.sessions[token] = time.Now().Add(24 * time.Hour)
	a.mu.Unlock()
	writeJSON(w, 200, map[string]any{"token": token})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func bearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

func (a *App) requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	token := bearerToken(r)
	if token == "" {
		writeError(w, 401, "missing admin token")
		return false
	}
	a.mu.Lock()
	exp, ok := a.sessions[token]
	if !ok || time.Now().After(exp) {
		delete(a.sessions, token)
		a.mu.Unlock()
		writeError(w, 401, "invalid or expired admin token")
		return false
	}
	a.sessions[token] = time.Now().Add(24 * time.Hour)
	a.mu.Unlock()
	return true
}

func (a *App) verifyLocalAPIKey(secret string) (LocalAPIKey, error) {
	rows, err := a.db.Query(`SELECT id,name,key_hash,salt,provider_key_id,protocol_conversion_enabled,client_protocol,upstream_protocol,enabled,created_at,last_used_at FROM local_api_keys WHERE enabled=1`)
	if err != nil {
		return LocalAPIKey{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var k LocalAPIKey
		var hash, salt string
		var enabled, protocolConversionEnabled int
		if err := rows.Scan(&k.ID, &k.Name, &hash, &salt, &k.ProviderKeyID, &protocolConversionEnabled, &k.ClientProtocol, &k.UpstreamProtocol, &enabled, &k.CreatedAt, &k.LastUsedAt); err != nil {
			return LocalAPIKey{}, err
		}
		if hashSecret(secret, salt) == hash {
			k.Enabled = true
			k.ProtocolConversionEnabled = intBool(protocolConversionEnabled)
			a.normalizeLocalKeyProtocolConfig(&k)
			_ = rows.Close()
			_, _ = a.db.Exec(`UPDATE local_api_keys SET last_used_at=? WHERE id=?`, now(), k.ID)
			return k, nil
		}
	}
	return LocalAPIKey{}, sql.ErrNoRows
}

func decodeRawJSON(r *http.Request) (map[string]any, []byte, error) {
	body, err := readAllLimit(r.Body, 20<<20)
	if err != nil {
		return nil, nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, nil, err
	}
	return m, body, nil
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func splitID(sub string) (string, string) {
	sub = strings.Trim(sub, "/")
	if sub == "" {
		return "", ""
	}
	parts := strings.SplitN(sub, "/", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], "/" + parts[1]
}

func required(v, name string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("%s is required", name)
	}
	return nil
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func isNoRows(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
