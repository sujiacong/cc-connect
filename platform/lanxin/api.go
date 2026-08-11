package lanxin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// sendBotText sends a private text message to a user (aligned with client.py:141-153).
func (p *Platform) sendBotText(target string, content string) error {
	token, err := p.getAppToken()
	if err != nil {
		return fmt.Errorf("lanxin: sendBotText: get token: %w", err)
	}

	payload := map[string]any{
		"msgType": "text",
		"msgData": map[string]any{
			"text": map[string]string{"content": content},
		},
		"userIdList": []string{target},
	}
	return p.postAPI(botMsgPath, token, payload, "sendBotText")
}

// sendGroupText sends a text message to a group (aligned with client.py:155-165).
func (p *Platform) sendGroupText(target string, content string) error {
	token, err := p.getAppToken()
	if err != nil {
		return fmt.Errorf("lanxin: sendGroupText: get token: %w", err)
	}

	payload := map[string]any{
		"groupId": target,
		"msgType": "text",
		"msgData": map[string]any{
			"text": map[string]string{"content": content},
		},
	}
	return p.postAPI(groupMsgPath, token, payload, "sendGroupText")
}

// getWSEndpoint fetches the WebSocket endpoint URL (aligned with client.py:78-99).
// NOTE: this endpoint does NOT use appToken; it takes appId+secret in the body.
func (p *Platform) getWSEndpoint() (string, error) {
	u := p.apiBase + wsEndpointPath
	payload := map[string]string{
		"appId":  p.appID,
		"secret": p.appSecret,
	}
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("lanxin: getWSEndpoint: marshal: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("lanxin: getWSEndpoint: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("lanxin: getWSEndpoint: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("lanxin: getWSEndpoint: status %d: %s",
			resp.StatusCode, core.RedactToken(string(body), p.appSecret))
	}

	var result struct {
		ErrCode int    `json:"errCode"`
		ErrMsg  string `json:"errMsg"`
		Data    struct {
			WSEndpoint string `json:"wsEndpoint"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("lanxin: getWSEndpoint: decode: %w", err)
	}

	if result.ErrCode != 0 {
		return "", fmt.Errorf("lanxin: getWSEndpoint: errCode=%d errMsg=%s",
			result.ErrCode, core.RedactToken(result.ErrMsg, p.appSecret))
	}

	if result.Data.WSEndpoint == "" {
		return "", fmt.Errorf("lanxin: getWSEndpoint: empty wsEndpoint in response")
	}

	return result.Data.WSEndpoint, nil
}

// postAPI is the shared POST helper for sendBotText/sendGroupText.
func (p *Platform) postAPI(path string, token string, payload map[string]any, opName string) error {
	u := p.apiBase + path + "?app_token=" + token
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("lanxin: %s: marshal: %w", opName, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("lanxin: %s: create request: %w", opName, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("lanxin: %s: do request: %w", opName, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("lanxin: %s: status %d: %s", opName, resp.StatusCode, core.RedactToken(string(body), token))
	}

	var result struct {
		ErrCode int    `json:"errCode"`
		ErrMsg  string `json:"errMsg"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("lanxin: %s: decode: %w", opName, err)
	}

	if result.ErrCode != 0 {
		slog.Warn("lanxin: API error", "op", opName, "errCode", result.ErrCode, "errMsg", result.ErrMsg)
		return fmt.Errorf("lanxin: %s: errCode=%d errMsg=%s", opName, result.ErrCode, result.ErrMsg)
	}

	return nil
}
