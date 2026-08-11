package lanxin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// getAppToken returns a cached appToken or fetches a new one.
// Lazy-load + mutex cache, refreshes 5 minutes before expiry (aligned with dingtalk).
func (p *Platform) getAppToken() (string, error) {
	p.tokenMu.Lock()
	defer p.tokenMu.Unlock()

	if p.appToken != "" && time.Now().Before(p.tokenExpiry) {
		return p.appToken, nil
	}

	// GET {apiBase}/v1/apptoken/create?grant_type=client_credential&appid=...&secret=...
	u := p.apiBase + tokenPath
	params := url.Values{
		"grant_type": {"client_credential"},
		"appid":      {p.appID},
		"secret":     {p.appSecret},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u+"?"+params.Encode(), nil)
	if err != nil {
		return "", fmt.Errorf("lanxin: get appToken: create request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("lanxin: get appToken: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("lanxin: get appToken: status %d: %s", resp.StatusCode, core.RedactToken(string(body), p.appSecret))
	}

	var result struct {
		ErrCode int    `json:"errCode"`
		ErrMsg  string `json:"errMsg"`
		Data    struct {
			AppToken  string `json:"appToken"`
			ExpiresIn int    `json:"expiresIn"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("lanxin: get appToken: decode: %w", err)
	}

	if result.ErrCode != 0 {
		return "", fmt.Errorf("lanxin: get appToken: errCode=%d errMsg=%s",
			result.ErrCode, core.RedactToken(result.ErrMsg, p.appSecret))
	}

	if result.Data.AppToken == "" {
		return "", fmt.Errorf("lanxin: get appToken: empty appToken in response")
	}

	expiresIn := result.Data.ExpiresIn
	if expiresIn <= 0 {
		slog.Warn("lanxin: missing/invalid expiresIn, defaulting to 7200s", "got", expiresIn)
		expiresIn = 7200
	}
	if expiresIn > 300 {
		expiresIn -= 300 // 5-minute early refresh
	}

	p.appToken = result.Data.AppToken
	p.tokenExpiry = time.Now().Add(time.Duration(expiresIn) * time.Second)

	slog.Debug("lanxin: appToken refreshed", "expires_at", p.tokenExpiry)
	return p.appToken, nil
}
