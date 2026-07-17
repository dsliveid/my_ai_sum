package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestChangePasswordUpdatesAdminPasswordAndInvalidatesOtherSessions(t *testing.T) {
	a := newTestApp(t)
	defer a.db.Close()
	salt := newSalt()
	_, err := a.db.Exec(`INSERT INTO admin_users(id,username,password_hash,salt,created_at) VALUES(?,?,?,?,?)`,
		"adm_test", "admin", hashSecret("old-secret", salt), salt, now())
	if err != nil {
		t.Fatalf("insert admin: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", a.serveAPI)
	server := httptest.NewServer(withRecover(mux))
	defer server.Close()

	token := loginForTest(t, server.URL, "admin", "old-secret")
	otherToken := "other-admin-token"
	a.sessions[otherToken] = time.Now().Add(time.Hour)

	wrongOld := postJSON(t, server.URL+"/api/v1/auth/password", "Bearer "+token, map[string]any{
		"old_password": "wrong-secret",
		"new_password": "new-secret",
	})
	wrongOld.Body.Close()
	if wrongOld.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong old password status = %d, want 401", wrongOld.StatusCode)
	}

	changed := postJSON(t, server.URL+"/api/v1/auth/password", "Bearer "+token, map[string]any{
		"old_password": "old-secret",
		"new_password": "new-secret",
	})
	changed.Body.Close()
	if changed.StatusCode != http.StatusOK {
		t.Fatalf("change password status = %d, want 200", changed.StatusCode)
	}

	oldLogin := postJSON(t, server.URL+"/api/v1/auth/login", "", map[string]any{"username": "admin", "password": "old-secret"})
	oldLogin.Body.Close()
	if oldLogin.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password login status = %d, want 401", oldLogin.StatusCode)
	}
	if newToken := loginForTest(t, server.URL, "admin", "new-secret"); newToken == "" {
		t.Fatal("new password login returned empty token")
	}

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/v1/auth/me", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+otherToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("auth me: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other session status = %d, want 401", resp.StatusCode)
	}
}

func loginForTest(t *testing.T, serverURL, username, password string) string {
	t.Helper()
	resp := postJSON(t, serverURL+"/api/v1/auth/login", "", map[string]any{"username": username, "password": password})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return body["token"]
}
