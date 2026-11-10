package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestConfigAtomicConcurrentAccess 验证 config()/setConfig() 在并发读写下的安全性。
// 环境无 CGO 无法开 -race，这里用「高并发 + 值合理性检查」替代：
// 只要能稳定跑完不 panic、不死锁（Go 运行时会对 atomic.Pointer 的非法复制等做检查），
// 且最终读到的值确实来自某次 Store，就说明原子访问改造生效。
func TestConfigAtomicConcurrentAccess(t *testing.T) {
	a := &App{}
	base := defaultConfig()
	base.DataDir = t.TempDir()
	base.Port = 8000 // 初始值也落在写入区间内，避免读方在首次 Store 前读到区间外的合法初始值
	a.setConfig(base)

	const writers = 8
	const rounds = 200
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				cfg := a.config()
				cfg.Port = 8000 + idx
				cfg.LogLevel = "debug"
				a.setConfig(cfg)
			}
		}(i)
	}
	// 并发读方：与写方同时进行，验证读到的永远是某个完整快照（不出现半更新或零值）。
	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
				cfg := a.config()
				if cfg.Port < 8000 || cfg.Port >= 8000+writers {
					t.Errorf("unexpected port snapshot: %d", cfg.Port)
					return
				}
			}
		}
	}()
	wg.Wait()
	close(stop)
	readerWG.Wait()

	final := a.config()
	if final.Port < 8000 || final.Port >= 8000+writers {
		t.Fatalf("final config port out of expected range: %d", final.Port)
	}
	if final.LogLevel != "debug" {
		t.Fatalf("expected final LogLevel debug, got %q", final.LogLevel)
	}
}

// TestConfigFallbackWhenNeverStored 验证未 Store 时 config() 回退默认配置而不是解引用 nil 导致 panic。
func TestConfigFallbackWhenNeverStored(t *testing.T) {
	a := &App{}
	cfg := a.config()
	def := defaultConfig()
	if cfg.Port != def.Port || cfg.Host != def.Host {
		t.Fatalf("expected default config fallback, got %+v", cfg)
	}
	if cfg.DataDir != def.DataDir {
		t.Fatalf("expected default data dir %q, got %q", def.DataDir, cfg.DataDir)
	}
}

// TestConfigReturnsValueCopy 验证 config() 返回的是值副本：调用方修改返回值不会污染已发布的配置。
func TestConfigReturnsValueCopy(t *testing.T) {
	a := &App{}
	orig := defaultConfig()
	orig.DataDir = t.TempDir()
	orig.Port = 8716
	a.setConfig(orig)

	mutated := a.config()
	mutated.Port = 9999
	mutated.DataDir = "/tmp/should-not-leak"
	mutated.APIDebugEnabled = true

	again := a.config()
	if again.Port != 8716 {
		t.Fatalf("config copy leaked port mutation: %d", again.Port)
	}
	if again.DataDir != orig.DataDir {
		t.Fatalf("config copy leaked data dir mutation: %q", again.DataDir)
	}
	if again.APIDebugEnabled {
		t.Fatal("config copy leaked APIDebugEnabled mutation")
	}
}

// TestSaveProxyDefaultIsExclusive 验证 proxies.is_default 的互斥更新在事务内完成：
// 依次把两个代理设为默认后，全表必须恰好只有一个 is_default=1，且是最后设置的那个。
func TestSaveProxyDefaultIsExclusive(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	firstID := createProxyViaHandler(t, a, "proxy-a", true)
	if got := countDefaultProxies(t, a); got != 1 {
		t.Fatalf("after first default proxy, expected 1 default row, got %d", got)
	}

	secondID := createProxyViaHandler(t, a, "proxy-b", true)
	if got := countDefaultProxies(t, a); got != 1 {
		t.Fatalf("after second default proxy, expected exactly 1 default row, got %d", got)
	}
	var id string
	if err := a.db.QueryRow(`SELECT id FROM proxies WHERE is_default=1`).Scan(&id); err != nil {
		t.Fatalf("query default proxy: %v", err)
	}
	if id != secondID {
		t.Fatalf("expected default proxy to be %s, got %s (first was %s)", secondID, id, firstID)
	}

	// 再把第一个设回默认（走更新分支），仍然必须只有一个是默认。
	updateProxyDefaultViaHandler(t, a, firstID, "proxy-a")
	if got := countDefaultProxies(t, a); got != 1 {
		t.Fatalf("after switching default back, expected exactly 1 default row, got %d", got)
	}
	if err := a.db.QueryRow(`SELECT id FROM proxies WHERE is_default=1`).Scan(&id); err != nil {
		t.Fatalf("query default proxy after switch: %v", err)
	}
	if id != firstID {
		t.Fatalf("expected default proxy %s after switch, got %s", firstID, id)
	}
}

// TestSaveProxyDefaultConcurrentKeepsSingleRow 并发把多个代理设为默认，
// 事务化后仍不允许出现「双默认」或「零默认」。
func TestSaveProxyDefaultConcurrentKeepsSingleRow(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()

	const n = 6
	ids := make([]string, n)
	for i := 0; i < n; i++ {
		// 先创建为非默认，避免创建阶段就参与默认竞争。
		ids[i] = createProxyViaHandler(t, a, "concurrent-"+string(rune('a'+i)), false)
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id, name string) {
			defer wg.Done()
			updateProxyDefaultViaHandler(t, a, id, name)
		}(ids[i], "concurrent-"+string(rune('a'+i)))
	}
	wg.Wait()

	if got := countDefaultProxies(t, a); got != 1 {
		t.Fatalf("after concurrent default switches, expected exactly 1 default row, got %d", got)
	}
}

func countDefaultProxies(t *testing.T, a *App) int {
	t.Helper()
	var n int
	if err := a.db.QueryRow(`SELECT COUNT(*) FROM proxies WHERE is_default=1`).Scan(&n); err != nil {
		t.Fatalf("count default proxies: %v", err)
	}
	return n
}

// createProxyViaHandler 通过 saveProxy 依次新增代理，返回其 ID（与真实 HTTP 路径同一入口）。
func createProxyViaHandler(t *testing.T, a *App, name string, isDefault bool) string {
	t.Helper()
	body := proxyRequestBody(name, isDefault)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/proxies", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	a.saveProxy(rec, req, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("create proxy %s failed: %d %s", name, rec.Code, rec.Body.String())
	}
	var p ProxyConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode created proxy: %v", err)
	}
	if p.ID == "" {
		t.Fatalf("expected created proxy id, got empty")
	}
	return p.ID
}

// updateProxyDefaultViaHandler 通过 saveProxy 的更新分支把指定代理置为默认。
func updateProxyDefaultViaHandler(t *testing.T, a *App, id, name string) {
	t.Helper()
	body := proxyRequestBody(name, true)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/proxies/"+id, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	a.saveProxy(rec, req, id)
	if rec.Code != http.StatusOK {
		t.Fatalf("update proxy %s failed: %d %s", name, rec.Code, rec.Body.String())
	}
}

func proxyRequestBody(name string, isDefault bool) string {
	b, _ := json.Marshal(ProxyConfig{
		Name:      name,
		Type:      "http",
		Host:      "127.0.0.1",
		Port:      8080,
		Username:  "u",
		Password:  "p",
		Enabled:   true,
		IsDefault: isDefault,
	})
	return string(b)
}
