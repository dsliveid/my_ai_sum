package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	grokDeviceOAuthKey            = "grok_device_oauth"
	grokDeviceOAuthDefaultBaseURL = "https://cli-chat-proxy.grok.com/v1"
	grokOAuthClientID             = "b1a00492-073a-47ea-816f-4c329264a828"
	grokOAuthScope                = "openid profile email offline_access grok-cli:access api:access"
	grokOAuthDeviceURL            = "https://auth.x.ai/oauth2/device/code"
	grokOAuthTokenURL             = "https://auth.x.ai/oauth2/token"
	grokBuildClientVersion        = "0.2.101"
	grokBuildClientIdentifier     = "grok-shell"
	grokBuildTokenAuth            = "xai-grok-cli"
	grokBuildUserAgent            = "grok-shell/0.2.101 (linux; x86_64)"
)

var (
	errGrokOAuthPending = errors.New("authorization pending")
	errGrokOAuthSlow    = errors.New("authorization polling too fast")
	errGrokOAuthDenied  = errors.New("authorization denied")
)

type grokDeviceToken struct {
	Kind                    string `json:"kind"`
	AccessToken             string `json:"access_token,omitempty"`
	RefreshToken            string `json:"refresh_token,omitempty"`
	ExpiresAt               string `json:"expires_at,omitempty"`
	IDToken                 string `json:"id_token,omitempty"`
	DeviceCode              string `json:"device_code,omitempty"`
	UserCode                string `json:"user_code,omitempty"`
	VerificationURI         string `json:"verification_uri,omitempty"`
	VerificationURIComplete string `json:"verification_uri_complete,omitempty"`
	DeviceExpiresAt         string `json:"device_expires_at,omitempty"`
	Interval                int    `json:"interval,omitempty"`
}

type grokOAuthTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	IDToken          string `json:"id_token"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

type grokDeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	Interval                int    `json:"interval"`
	ExpiresIn               int    `json:"expires_in"`
}

func isGrokDeviceOAuthSelection(providerType, apiKey string) bool {
	return strings.TrimSpace(providerType) == "grok" && strings.TrimSpace(apiKey) == grokDeviceOAuthKey
}

func isGrokDeviceOAuthSecret(secret string) bool {
	secret = strings.TrimSpace(secret)
	if secret == grokDeviceOAuthKey {
		return true
	}
	var token grokDeviceToken
	return json.Unmarshal([]byte(secret), &token) == nil && token.Kind == grokDeviceOAuthKey
}

func displayProviderAPIKey(secret string) string {
	if isGrokDeviceOAuthSecret(secret) {
		var token grokDeviceToken
		if json.Unmarshal([]byte(strings.TrimSpace(secret)), &token) == nil && token.AccessToken != "" {
			return grokDeviceOAuthKey + " (authorized)"
		}
		return grokDeviceOAuthKey
	}
	return maskSecret(secret)
}

func revealProviderAPIKeyForAdmin(secret string) string {
	if isGrokDeviceOAuthSecret(secret) {
		return grokDeviceOAuthKey
	}
	return secret
}

func (a *App) providerBearerToken(ctx context.Context, provider ProviderKey) (string, error) {
	apiKey := provider.APIKey
	if apiKey == "" || strings.Contains(apiKey, "...") {
		var err error
		apiKey, err = a.decryptProviderKey(provider)
		if err != nil {
			return "", err
		}
	}
	if !isGrokDeviceOAuthSecret(apiKey) {
		return apiKey, nil
	}
	client, err := a.httpClientForProvider(provider)
	if err != nil {
		return "", err
	}
	token, _, err := a.ensureGrokDeviceOAuthToken(ctx, provider, client)
	if err != nil {
		return "", err
	}
	return token.AccessToken, nil
}

func (a *App) prepareGrokDeviceOAuthTest(ctx context.Context, provider ProviderKey, client *http.Client) (string, map[string]any, error) {
	secret := strings.TrimSpace(provider.APIKey)
	if secret == "" || strings.Contains(secret, "...") {
		var err error
		secret, err = a.decryptProviderKey(provider)
		if err != nil {
			return "", nil, err
		}
	}
	token := parseGrokDeviceToken(secret)
	if token.AccessToken != "" && token.ValidAt(time.Now().Add(2*time.Minute)) {
		return token.AccessToken, nil, nil
	}
	if token.RefreshToken != "" {
		refreshed, err := grokOAuthRefresh(ctx, client, token.RefreshToken)
		if err == nil {
			refreshed.Kind = grokDeviceOAuthKey
			if refreshed.RefreshToken == "" {
				refreshed.RefreshToken = token.RefreshToken
			}
			if err := a.saveGrokDeviceOAuthToken(provider.ID, refreshed); err != nil {
				return "", nil, err
			}
			return refreshed.AccessToken, nil, nil
		}
	}
	if token.DeviceCode != "" && token.PendingValidAt(time.Now()) {
		polled, err := grokOAuthPollDevice(ctx, client, token.DeviceCode)
		switch {
		case err == nil:
			if err := a.saveGrokDeviceOAuthToken(provider.ID, polled); err != nil {
				return "", nil, err
			}
			return polled.AccessToken, nil, nil
		case errors.Is(err, errGrokOAuthPending), errors.Is(err, errGrokOAuthSlow):
			return "", grokDeviceOAuthStatus("authorization_pending", token), nil
		case errors.Is(err, errGrokOAuthDenied):
			token = grokDeviceToken{Kind: grokDeviceOAuthKey}
		default:
			return "", nil, err
		}
	}
	device, err := grokOAuthStartDevice(ctx, client)
	if err != nil {
		return "", nil, err
	}
	pending := grokDeviceToken{
		Kind:                    grokDeviceOAuthKey,
		DeviceCode:              device.DeviceCode,
		UserCode:                device.UserCode,
		VerificationURI:         device.VerificationURI,
		VerificationURIComplete: device.VerificationURIComplete,
		DeviceExpiresAt:         time.Now().UTC().Add(time.Duration(device.ExpiresIn) * time.Second).Format(time.RFC3339),
		Interval:                device.Interval,
	}
	if err := a.saveGrokDeviceOAuthToken(provider.ID, pending); err != nil {
		return "", nil, err
	}
	return "", grokDeviceOAuthStatus("authorization_required", pending), nil
}

func (a *App) ensureGrokDeviceOAuthToken(ctx context.Context, provider ProviderKey, client *http.Client) (grokDeviceToken, bool, error) {
	secret := strings.TrimSpace(provider.APIKey)
	if secret == "" || strings.Contains(secret, "...") {
		var err error
		secret, err = a.decryptProviderKey(provider)
		if err != nil {
			return grokDeviceToken{}, false, err
		}
	}
	token := parseGrokDeviceToken(secret)
	if token.AccessToken != "" && token.ValidAt(time.Now().Add(2*time.Minute)) {
		return token, false, nil
	}
	if token.RefreshToken != "" {
		refreshed, err := grokOAuthRefresh(ctx, client, token.RefreshToken)
		if err == nil {
			refreshed.Kind = grokDeviceOAuthKey
			if refreshed.RefreshToken == "" {
				refreshed.RefreshToken = token.RefreshToken
			}
			if err := a.saveGrokDeviceOAuthToken(provider.ID, refreshed); err != nil {
				return grokDeviceToken{}, false, err
			}
			return refreshed, false, nil
		}
	}
	authorized, err := a.runGrokDeviceOAuthFlow(ctx, client)
	if err != nil {
		return grokDeviceToken{}, false, err
	}
	if err := a.saveGrokDeviceOAuthToken(provider.ID, authorized); err != nil {
		return grokDeviceToken{}, false, err
	}
	return authorized, true, nil
}

func parseGrokDeviceToken(secret string) grokDeviceToken {
	var token grokDeviceToken
	if json.Unmarshal([]byte(strings.TrimSpace(secret)), &token) != nil {
		return grokDeviceToken{Kind: grokDeviceOAuthKey}
	}
	if token.Kind != grokDeviceOAuthKey {
		token.Kind = grokDeviceOAuthKey
	}
	return token
}

func (t grokDeviceToken) ValidAt(when time.Time) bool {
	if t.AccessToken == "" || t.ExpiresAt == "" {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339, t.ExpiresAt)
	return err == nil && expiresAt.After(when)
}

func (t grokDeviceToken) PendingValidAt(when time.Time) bool {
	if t.DeviceCode == "" || t.DeviceExpiresAt == "" {
		return false
	}
	expiresAt, err := time.Parse(time.RFC3339, t.DeviceExpiresAt)
	return err == nil && expiresAt.After(when)
}

func grokDeviceOAuthStatus(status string, token grokDeviceToken) map[string]any {
	authURL := strings.TrimSpace(token.VerificationURI)
	if authURL == "" {
		authURL = "https://accounts.x.ai/oauth2/device"
	}
	return map[string]any{
		"status":                    status,
		"error":                     "",
		"stage":                     "device_oauth",
		"authorization_url":         authURL,
		"user_code":                 token.UserCode,
		"verification_uri":          authURL,
		"verification_uri_complete": token.VerificationURIComplete,
		"message":                   "Open the authorization URL, enter the user code, approve access, then click test again.",
	}
}

func (a *App) saveGrokDeviceOAuthToken(providerID string, token grokDeviceToken) error {
	if strings.TrimSpace(providerID) == "" {
		return nil
	}
	token.Kind = grokDeviceOAuthKey
	payload, err := json.Marshal(token)
	if err != nil {
		return err
	}
	enc, err := a.encryptText(string(payload))
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`UPDATE provider_keys SET api_key_enc=?,updated_at=? WHERE id=?`, enc, now(), providerID)
	return err
}

func (a *App) runGrokDeviceOAuthFlow(ctx context.Context, client *http.Client) (grokDeviceToken, error) {
	device, err := grokOAuthStartDevice(ctx, client)
	if err != nil {
		return grokDeviceToken{}, err
	}
	authURL := strings.TrimSpace(device.VerificationURI)
	if authURL != "" {
		go openBrowser(authURL)
	}
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiresIn := time.Duration(device.ExpiresIn) * time.Second
	if expiresIn <= 0 {
		expiresIn = 30 * time.Minute
	}
	deadline := time.NewTimer(expiresIn)
	defer deadline.Stop()
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return grokDeviceToken{}, ctx.Err()
		case <-deadline.C:
			return grokDeviceToken{}, fmt.Errorf("xAI Device OAuth authorization timed out; open %s and enter code %s", device.VerificationURI, device.UserCode)
		case <-timer.C:
			token, err := grokOAuthPollDevice(ctx, client, device.DeviceCode)
			switch {
			case err == nil:
				return token, nil
			case errors.Is(err, errGrokOAuthPending):
			case errors.Is(err, errGrokOAuthSlow):
				interval += 5 * time.Second
			case errors.Is(err, errGrokOAuthDenied):
				return grokDeviceToken{}, fmt.Errorf("xAI Device OAuth authorization was denied or expired")
			default:
				return grokDeviceToken{}, err
			}
			timer.Reset(interval)
		}
	}
}

func grokOAuthStartDevice(ctx context.Context, client *http.Client) (grokDeviceAuthorization, error) {
	form := url.Values{"client_id": {grokOAuthClientID}, "scope": {grokOAuthScope}}
	var device grokDeviceAuthorization
	if err := grokOAuthPostForm(ctx, client, grokOAuthDeviceURL, form, &device); err != nil {
		return device, err
	}
	if device.DeviceCode == "" || device.UserCode == "" || device.VerificationURI == "" {
		return device, fmt.Errorf("xAI Device OAuth response is missing required fields")
	}
	if device.Interval <= 0 {
		device.Interval = 5
	}
	if device.ExpiresIn <= 0 {
		device.ExpiresIn = 1800
	}
	return device, nil
}

func grokOAuthPollDevice(ctx context.Context, client *http.Client, deviceCode string) (grokDeviceToken, error) {
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "client_id": {grokOAuthClientID}, "device_code": {deviceCode}}
	return grokOAuthExchange(ctx, client, form, "")
}

func grokOAuthRefresh(ctx context.Context, client *http.Client, refreshToken string) (grokDeviceToken, error) {
	form := url.Values{"grant_type": {"refresh_token"}, "client_id": {grokOAuthClientID}, "refresh_token": {refreshToken}}
	return grokOAuthExchange(ctx, client, form, refreshToken)
}

func grokOAuthExchange(ctx context.Context, client *http.Client, form url.Values, fallbackRefresh string) (grokDeviceToken, error) {
	var value grokOAuthTokenResponse
	status, err := grokOAuthPostFormStatus(ctx, client, grokOAuthTokenURL, form, &value)
	if err != nil {
		return grokDeviceToken{}, err
	}
	if status < 200 || status >= 300 {
		switch value.Error {
		case "authorization_pending":
			return grokDeviceToken{}, errGrokOAuthPending
		case "slow_down":
			return grokDeviceToken{}, errGrokOAuthSlow
		case "access_denied", "expired_token":
			return grokDeviceToken{}, errGrokOAuthDenied
		default:
			return grokDeviceToken{}, fmt.Errorf("xAI OAuth returned %d: %s", status, firstNonEmpty(value.ErrorDescription, value.Error, http.StatusText(status)))
		}
	}
	if value.AccessToken == "" {
		return grokDeviceToken{}, fmt.Errorf("xAI OAuth response is missing access_token")
	}
	if value.ExpiresIn <= 0 {
		value.ExpiresIn = 3600
	}
	return grokDeviceToken{
		Kind:         grokDeviceOAuthKey,
		AccessToken:  value.AccessToken,
		RefreshToken: firstNonEmpty(value.RefreshToken, fallbackRefresh),
		IDToken:      value.IDToken,
		ExpiresAt:    time.Now().UTC().Add(time.Duration(value.ExpiresIn) * time.Second).Format(time.RFC3339),
	}, nil
}

func grokOAuthPostForm(ctx context.Context, client *http.Client, endpoint string, form url.Values, output any) error {
	status, err := grokOAuthPostFormStatus(ctx, client, endpoint, form, output)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("xAI OAuth returned %d", status)
	}
	return nil
}

func grokOAuthPostFormStatus(ctx context.Context, client *http.Client, endpoint string, form url.Values, output any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if len(body) > 0 && output != nil {
		if err := json.Unmarshal(body, output); err != nil {
			return resp.StatusCode, fmt.Errorf("decode xAI OAuth response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

func (a *App) applyProviderAuthHeaders(req *http.Request, provider ProviderKey, bearer string) {
	req.Header.Set("Authorization", "Bearer "+bearer)
	if !isGrokDeviceOAuthSecret(provider.APIKey) {
		return
	}
	req.Header.Set("X-XAI-Token-Auth", grokBuildTokenAuth)
	req.Header.Set("x-grok-client-version", grokBuildClientVersion)
	req.Header.Set("x-grok-client-identifier", grokBuildClientIdentifier)
	req.Header.Set("x-grok-client-mode", "headless")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", grokBuildUserAgent)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func parseOAuthRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if parsed, err := http.ParseTime(value); err == nil && parsed.After(time.Now()) {
		return time.Until(parsed)
	}
	return 0
}
