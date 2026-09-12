package lanxin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"golang.org/x/text/encoding/simplifiedchinese"
)

func TestNew_MissingCredentials(t *testing.T) {
	_, err := New(map[string]any{})
	if err == nil {
		t.Fatal("expected error when app_id and app_secret are missing")
	}
}

func TestNew_MissingSecret(t *testing.T) {
	_, err := New(map[string]any{"app_id": "test-app-id"})
	if err == nil {
		t.Fatal("expected error when app_secret is missing")
	}
}

func TestNew_ValidConfig(t *testing.T) {
	p, err := New(map[string]any{
		"app_id":     "test-app-id",
		"app_secret": "test-secret",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.Name() != "lanxin" {
		t.Fatalf("expected name 'lanxin', got %q", p.Name())
	}
}

func TestNew_DefaultAPIBase(t *testing.T) {
	p, _ := New(map[string]any{
		"app_id":     "test",
		"app_secret": "test",
	})
	plat := p.(*Platform)
	if plat.apiBase != defaultAPIBase {
		t.Fatalf("expected default apiBase %q, got %q", defaultAPIBase, plat.apiBase)
	}
}

func TestNew_CustomAPIBase(t *testing.T) {
	p, _ := New(map[string]any{
		"app_id":     "test",
		"app_secret": "test",
		"api_base":   "https://openapi.lanxin.cn",
	})
	plat := p.(*Platform)
	if plat.apiBase != "https://openapi.lanxin.cn" {
		t.Fatalf("expected custom apiBase, got %q", plat.apiBase)
	}
}

func TestGetAppToken_CacheHit(t *testing.T) {
	p := &Platform{
		appID:       "test-app",
		appSecret:   "test-secret",
		apiBase:     "http://localhost",
		httpClient:  &http.Client{},
		appToken:    "cached-token",
		tokenExpiry: time.Now().Add(1 * time.Hour), // not expired
	}
	token, err := p.getAppToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "cached-token" {
		t.Fatalf("expected cached token, got %q", token)
	}
}

func TestGetAppToken_RefreshOnExpiry(t *testing.T) {
	// mock lanxin appToken API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != tokenPath {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.URL.Query().Get("appid") != "test-app" {
			t.Errorf("unexpected appid %q", r.URL.Query().Get("appid"))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{"appToken":"new-token-123","expiresIn":7200}}`))
	}))
	defer server.Close()

	p := &Platform{
		appID:      "test-app",
		appSecret:  "test-secret",
		apiBase:    server.URL,
		httpClient: server.Client(),
		appToken:   "", // no cache
	}
	token, err := p.getAppToken()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "new-token-123" {
		t.Fatalf("expected 'new-token-123', got %q", token)
	}
	// verify cache updated
	if p.appToken != "new-token-123" {
		t.Fatalf("token not cached")
	}
	// verify 5-minute early refresh (expiresIn=7200, minus 300 = 6900s)
	expectedExpiry := time.Now().Add(6900 * time.Second)
	if p.tokenExpiry.Before(expectedExpiry.Add(-10*time.Second)) ||
		p.tokenExpiry.After(expectedExpiry.Add(10*time.Second)) {
		t.Fatalf("expiry out of range: got %v, expected ~%v", p.tokenExpiry, expectedExpiry)
	}
}

func TestGetAppToken_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"errCode":40001,"errMsg":"invalid appid"}`))
	}))
	defer server.Close()

	p := &Platform{
		appID:      "test-app",
		appSecret:  "test-secret",
		apiBase:    server.URL,
		httpClient: server.Client(),
	}
	_, err := p.getAppToken()
	if err == nil {
		t.Fatal("expected error for API errCode != 0")
	}
	// error message must not leak appSecret
	if strings.Contains(err.Error(), "test-secret") {
		t.Fatalf("error message leaks secret: %v", err)
	}
}

func TestSendBotText_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != botMsgPath {
			t.Errorf("unexpected path %q, want %q", r.URL.Path, botMsgPath)
		}
		if r.URL.Query().Get("app_token") == "" {
			t.Error("missing app_token query param")
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{}}`))
	}))
	defer server.Close()

	p := &Platform{
		appID:       "test",
		appSecret:   "test",
		apiBase:     server.URL,
		httpClient:  server.Client(),
		appToken:    "fake-token",
		tokenExpiry: time.Now().Add(1 * time.Hour),
	}
	err := p.sendBotText("user-123", "hello", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSendBotText_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"errCode":40014,"errMsg":"invalid app_token"}`))
	}))
	defer server.Close()

	p := &Platform{
		appID:       "test",
		appSecret:   "test",
		apiBase:     server.URL,
		httpClient:  server.Client(),
		appToken:    "fake-token",
		tokenExpiry: time.Now().Add(1 * time.Hour),
	}
	err := p.sendBotText("user-123", "hello", "")
	if err == nil {
		t.Fatal("expected error for errCode != 0")
	}
}

func TestSendGroupText_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != groupMsgPath {
			t.Errorf("unexpected path %q, want %q", r.URL.Path, groupMsgPath)
		}
		w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{}}`))
	}))
	defer server.Close()

	p := &Platform{
		appID:       "test",
		appSecret:   "test",
		apiBase:     server.URL,
		httpClient:  server.Client(),
		appToken:    "fake-token",
		tokenExpiry: time.Now().Add(1 * time.Hour),
	}
	err := p.sendGroupText("group-456", "hello group", "", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestGetWSEndpoint_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != wsEndpointPath {
			t.Errorf("unexpected path %q, want %q", r.URL.Path, wsEndpointPath)
		}
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{"wsEndpoint":"wss://ws.example.com/connect"}}`))
	}))
	defer server.Close()

	p := &Platform{
		appID:      "test",
		appSecret:  "test",
		apiBase:    server.URL,
		httpClient: server.Client(),
	}
	endpoint, err := p.getWSEndpoint()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if endpoint != "wss://ws.example.com/connect" {
		t.Fatalf("expected wss endpoint, got %q", endpoint)
	}
}

func TestParseWSMessage_EventsFormat(t *testing.T) {
	raw := `{"events":[{"type":"bot_private_message","data":{"from":"user-001","chatType":"p2p","msgType":"text","msgData":{"text":{"content":"hello"}},"msgId":"msg-123","senderName":"Alice"}}]}`
	events := parseWSMessage([]byte(raw))
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.EventType != "bot_private_message" {
		t.Errorf("expected event type bot_private_message, got %q", ev.EventType)
	}
	if ev.From != "user-001" {
		t.Errorf("expected from user-001, got %q", ev.From)
	}
	if ev.ChatType != "p2p" {
		t.Errorf("expected chatType p2p, got %q", ev.ChatType)
	}
	if ev.Content != "hello" {
		t.Errorf("expected content 'hello', got %q", ev.Content)
	}
	if ev.MsgID != "msg-123" {
		t.Errorf("expected msgId msg-123, got %q", ev.MsgID)
	}
	if ev.SenderName != "Alice" {
		t.Errorf("expected senderName Alice, got %q", ev.SenderName)
	}
}

func TestParseWSMessage_SingleEventFormat(t *testing.T) {
	// approve callback etc.: top-level is {type,data}
	raw := `{"type":"approve_card_callback","data":{"staffId":"user-002","eventData":"some-data"}}`
	events := parseWSMessage([]byte(raw))
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].EventType != "approve_card_callback" {
		t.Errorf("expected approve_card_callback, got %q", events[0].EventType)
	}
}

func TestParseWSMessage_GroupMessage(t *testing.T) {
	raw := `{"events":[{"type":"bot_group_message","data":{"from":"user-003","chatType":"group","groupId":"group-456","msgType":"text","msgData":{"text":{"content":"@bot hi"}},"msgId":"msg-789","reminder":{"isAtMe":true},"senderName":"Bob"}}]}`
	events := parseWSMessage([]byte(raw))
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev := events[0]
	if ev.ChatType != "group" {
		t.Errorf("expected group, got %q", ev.ChatType)
	}
	if ev.GroupID != "group-456" {
		t.Errorf("expected groupId group-456, got %q", ev.GroupID)
	}
	if !ev.IsAtMe {
		t.Error("expected isAtMe=true")
	}
}

func TestParseWSMessage_InvalidJSON(t *testing.T) {
	events := parseWSMessage([]byte("not json"))
	if len(events) != 0 {
		t.Fatalf("expected 0 events for invalid JSON, got %d", len(events))
	}
}

func TestParseWSMessage_UnknownStructure(t *testing.T) {
	events := parseWSMessage([]byte(`{"foo":"bar"}`))
	if len(events) != 0 {
		t.Fatalf("expected 0 events for unknown structure, got %d", len(events))
	}
}

func TestParseWSMessage_EmptyEvents(t *testing.T) {
	events := parseWSMessage([]byte(`{"events":[]}`))
	if len(events) != 0 {
		t.Fatalf("expected 0 events, got %d", len(events))
	}
}

func TestReconstructReplyCtx_PrivateChat(t *testing.T) {
	p := &Platform{}
	rctx, err := p.ReconstructReplyCtx("lanxin:p2p:user-001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rc, ok := rctx.(replyContext)
	if !ok {
		t.Fatalf("expected replyContext, got %T", rctx)
	}
	if rc.chatType != "p2p" {
		t.Errorf("expected chatType p2p, got %q", rc.chatType)
	}
	if rc.target != "user-001" {
		t.Errorf("expected target user-001, got %q", rc.target)
	}
	if !rc.proactive {
		t.Error("expected proactive=true")
	}
}

func TestReconstructReplyCtx_GroupSharedSession(t *testing.T) {
	p := &Platform{}
	rctx, err := p.ReconstructReplyCtx("lanxin:group:group-456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rc := rctx.(replyContext)
	if rc.chatType != "group" {
		t.Errorf("expected group, got %q", rc.chatType)
	}
	if rc.target != "group-456" {
		t.Errorf("expected target group-456, got %q", rc.target)
	}
}

func TestReconstructReplyCtx_GroupUserIsolated(t *testing.T) {
	// three-part: lanxin:group:<groupId>:<from>
	// target is groupId (sending only needs groupId)
	p := &Platform{}
	rctx, err := p.ReconstructReplyCtx("lanxin:group:group-456:user-789")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	rc := rctx.(replyContext)
	if rc.chatType != "group" {
		t.Errorf("expected group, got %q", rc.chatType)
	}
	if rc.target != "group-456" {
		t.Errorf("expected target group-456, got %q", rc.target)
	}
}

func TestReconstructReplyCtx_InvalidPrefix(t *testing.T) {
	p := &Platform{}
	_, err := p.ReconstructReplyCtx("wecom:p2p:user-001")
	if err == nil {
		t.Fatal("expected error for non-lanxin prefix")
	}
}

func TestReconstructReplyCtx_TooFewParts(t *testing.T) {
	p := &Platform{}
	_, err := p.ReconstructReplyCtx("lanxin:p2p")
	if err == nil {
		t.Fatal("expected error for session key with too few parts")
	}
}

// reqRecorder captures request paths, the last group-message POST body, and
// all bot/group POST bodies and uploaded filenames served by the httptest
// server. Thread-safe because handleFileMessage runs in a goroutine and may
// hit the server concurrently with the test.
type reqRecorder struct {
	mu          sync.Mutex
	paths       []string
	group       string
	bots        []string
	groups      []string
	uploadNames []string
	cardUpdates []string // /v1/messages/dynamic/update bodies
	revokes     []string // /v1/messages/revoke bodies
}

func (r *reqRecorder) add(p string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, p)
}

func (r *reqRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.paths...)
}

func (r *reqRecorder) groupBody() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.group
}

func (r *reqRecorder) botBodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.bots...)
}

func (r *reqRecorder) groupBodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.groups...)
}

func (r *reqRecorder) uploadedNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.uploadNames...)
}

func (r *reqRecorder) cardUpdateBodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.cardUpdates...)
}

func (r *reqRecorder) revokeBodies() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.revokes...)
}

// newTestPlatformWithServer wires a Platform to an httptest server that
// answers token/bot/group/media endpoints and records what was requested. The
// media response carries no Content-Disposition, so saveMedia falls back to the
// mediaID as the original name.
func newTestPlatformWithServer(t *testing.T, allowFrom string) (*Platform, *reqRecorder) {
	t.Helper()
	return newTestPlatformWithServerDelay(t, allowFrom, "", 0)
}

// newTestPlatformWithServerCD is newTestPlatformWithServer plus a Content-
// Disposition header (or "") served for every /v1/medias/ download.
func newTestPlatformWithServerCD(t *testing.T, allowFrom string, contentDisposition string) (*Platform, *reqRecorder) {
	t.Helper()
	return newTestPlatformWithServerDelay(t, allowFrom, contentDisposition, 0)
}

// newTestPlatformWithServerDelay is newTestPlatformWithServerCD plus an optional
// per-download delay (mediaDelay) so tests can keep a media download "in
// flight" while a text command is processed — the exact upload-download race.
func newTestPlatformWithServerDelay(t *testing.T, allowFrom string, contentDisposition string, mediaDelay time.Duration) (*Platform, *reqRecorder) {
	t.Helper()
	rec := &reqRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		switch r.URL.Path {
		case tokenPath:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{"appToken":"tok","expiresIn":7200}}`))
		case botMsgPath:
			b, _ := io.ReadAll(r.Body)
			rec.mu.Lock()
			rec.bots = append(rec.bots, string(b))
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{"msgId":"m-card"}}`))
		case groupMsgPath:
			b, _ := io.ReadAll(r.Body)
			rec.mu.Lock()
			rec.group = string(b)
			rec.groups = append(rec.groups, string(b))
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{"msgId":"m-card"}}`))
		case dynamicUpdatePath:
			b, _ := io.ReadAll(r.Body)
			rec.mu.Lock()
			rec.cardUpdates = append(rec.cardUpdates, string(b))
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{}}`))
		case revokeMessagePath:
			b, _ := io.ReadAll(r.Body)
			rec.mu.Lock()
			rec.revokes = append(rec.revokes, string(b))
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{}}`))
		case appMediaUploadPath:
			// Self-built-app media upload (filesend): multipart field "media",
			// type=file query (param name "type" per SDK WithMediaTypeString).
			// Records the uploaded filename and serves a fixed mediaId.
			if r.URL.Query().Get("type") != "file" {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"errCode":400,"errMsg":"bad type"}`))
				return
			}
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"errCode":400,"errMsg":"bad multipart"}`))
				return
			}
			f, hdr, err := r.FormFile("media")
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(`{"errCode":400,"errMsg":"missing media field"}`))
				return
			}
			defer f.Close()
			_, _ = io.Copy(io.Discard, f)
			rec.mu.Lock()
			rec.uploadNames = append(rec.uploadNames, hdr.Filename)
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{"mediaId":"m-123"}}`))
		default:
			if strings.HasPrefix(r.URL.Path, "/v1/medias/") {
				if mediaDelay > 0 {
					time.Sleep(mediaDelay)
				}
				if contentDisposition != "" {
					w.Header().Set("Content-Disposition", contentDisposition)
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Write([]byte("file-content"))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errCode":404,"errMsg":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	downloadDir := t.TempDir()

	p := &Platform{
		appID:             "test-app",
		appSecret:         "test-secret",
		apiBase:           srv.URL,
		allowFrom:         allowFrom,
		httpClient:        srv.Client(),
		downloadDir:       downloadDir,
		fileSendAllowDirs: []string{downloadDir},
		quoteReply:        true, // mirrors the New() default (reply_quote unset → on)
	}
	return p, rec
}

func waitForGroupReply(t *testing.T, rec *reqRecorder, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if b := rec.groupBody(); b != "" {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for group message reply; recorded paths: %v", rec.snapshot())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHandleWSMessage_UnauthorizedMediaNotSaved verifies the allow_from gate
// runs before the media branch: a media message from an unauthorized sender
// must NOT trigger downloadMedia/saveMedia.
func TestHandleWSMessage_UnauthorizedMediaNotSaved(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "alice")

	raw := `{"events":[{"type":"bot_private_message","data":{"from":"bob","chatType":"p2p","msgType":"file","msgData":{"file":{"mediaIds":["m1"]}},"msgId":"m1"}}]}`
	p.handleWSMessage([]byte(raw))

	for _, path := range rec.snapshot() {
		if strings.Contains(path, "/v1/medias/") {
			t.Fatalf("media fetch requested for unauthorized user: %s", path)
		}
	}
	entries, err := os.ReadDir(p.downloadDir)
	if err != nil {
		t.Fatalf("read downloadDir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("media file saved for unauthorized user: %v", entries)
	}
}

// TestHandleWSMessage_GroupFileReplyTargetsGroup verifies handleFileMessage
// addresses the group-file acknowledgement to the groupId, not the sender's
// staffId (regression for the ev.From-vs-ev.GroupID mix-up).
func TestHandleWSMessage_GroupFileReplyTargetsGroup(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "bob")

	raw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"file","msgData":{"file":{"mediaIds":["m1"]}},"msgId":"gm1","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(raw))

	body := waitForGroupReply(t, rec, 2*time.Second)

	var reply struct {
		GroupID string `json:"groupId"`
	}
	if err := json.Unmarshal([]byte(body), &reply); err != nil {
		t.Fatalf("decode group reply body: %v", err)
	}
	if reply.GroupID != "group-456" {
		t.Fatalf("group reply targeted %q, want group-456 (sender staffId must not be used as groupId)", reply.GroupID)
	}

	// positive control: the download did happen, so the negative test above
	// (UnauthorizedMediaNotSaved) is not passing vacuously.
	if _, err := os.Stat(filepath.Join(p.downloadDir, "m1")); err != nil {
		t.Fatalf("expected media file to be saved: %v", err)
	}
}

func TestNew_UploadMarkerTTL(t *testing.T) {
	p, _ := New(map[string]any{"app_id": "a", "app_secret": "s"})
	plat := p.(*Platform)
	if plat.uploadMarkerTTL != defaultUploadMarkerTTL {
		t.Fatalf("expected default TTL %v, got %v", defaultUploadMarkerTTL, plat.uploadMarkerTTL)
	}

	p2, _ := New(map[string]any{"app_id": "a", "app_secret": "s", "upload_marker_ttl_minutes": 120})
	plat2 := p2.(*Platform)
	if plat2.uploadMarkerTTL != 120*time.Minute {
		t.Fatalf("expected TTL 120m, got %v", plat2.uploadMarkerTTL)
	}

	p3, _ := New(map[string]any{"app_id": "a", "app_secret": "s", "upload_marker_ttl_minutes": 0})
	plat3 := p3.(*Platform)
	if plat3.uploadMarkerTTL != 0 {
		t.Fatalf("expected TTL 0 (expiry sweep disabled), got %v", plat3.uploadMarkerTTL)
	}
}

func TestSafeSessionKeyName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"lanxin:p2p:user-001", "lanxin_p2p_user-001"},
		{"lanxin:group:group-456:user-789", "lanxin_group_group-456_user-789"},
		{"lanxin:group:g/1", "lanxin_group_g_1"},
		{"lanxin:group:g\\1", "lanxin_group_g_1"},
	}
	for _, c := range cases {
		if got := safeSessionKeyName(c.in); got != c.want {
			t.Errorf("safeSessionKeyName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSessionKeyForEvent(t *testing.T) {
	// private chat → lanxin:p2p:<from>
	p := &Platform{}
	ev := lanxinEvent{ChatType: "p2p", From: "u1"}
	if got := p.sessionKeyForEvent(ev); got != "lanxin:p2p:u1" {
		t.Errorf("p2p session key = %q, want lanxin:p2p:u1", got)
	}

	// group + share_session_in_channel → lanxin:group:<groupId>
	shared := &Platform{shareSessionInChannel: true}
	evg := lanxinEvent{ChatType: "group", GroupID: "g1", From: "u1"}
	if got := shared.sessionKeyForEvent(evg); got != "lanxin:group:g1" {
		t.Errorf("shared group session key = %q, want lanxin:group:g1", got)
	}

	// group + per-user sessions → lanxin:group:<groupId>:<from>
	isolated := &Platform{shareSessionInChannel: false}
	if got := isolated.sessionKeyForEvent(evg); got != "lanxin:group:g1:u1" {
		t.Errorf("isolated group session key = %q, want lanxin:group:g1:u1", got)
	}
}

func TestSaveMedia_WritesPerSessionMarker(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "bob")
	sessionKey := "lanxin:p2p:alice"

	path, err := p.saveMedia("m-marker-1", sessionKey, time.Now().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("saveMedia: %v", err)
	}
	if path == "" {
		t.Fatal("expected a saved path")
	}

	markerPath := sessionMarkerPath(p.downloadDir, sessionKey)
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m sessionUploadMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	if m.SessionKey != sessionKey {
		t.Errorf("marker session_key = %q, want %q", m.SessionKey, sessionKey)
	}
	if len(m.Files) != 1 {
		t.Fatalf("marker files = %d, want 1", len(m.Files))
	}
	if m.Files[0].Path != path {
		t.Errorf("marker path = %q, want %q", m.Files[0].Path, path)
	}
	if m.Files[0].MediaID != "m-marker-1" {
		t.Errorf("marker media_id = %q, want m-marker-1", m.Files[0].MediaID)
	}
	// No Content-Disposition header from the test server → original name falls
	// back to the mediaID (backward compatible with old markers).
	if m.Files[0].OriginalName != "m-marker-1" {
		t.Errorf("marker original_name = %q, want fallback to mediaID m-marker-1", m.Files[0].OriginalName)
	}
	if _, err := time.Parse(time.RFC3339, m.Files[0].UploadedAt); err != nil {
		t.Errorf("marker uploaded_at is not RFC3339: %q (%v)", m.Files[0].UploadedAt, err)
	}
	if _, err := time.Parse(time.RFC3339, m.UpdatedAt); err != nil {
		t.Errorf("marker updated_at is not RFC3339: %q (%v)", m.UpdatedAt, err)
	}
	// the marker must not collide with the saved media file itself
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected media file to exist: %v", err)
	}
}

func TestSaveMedia_MarkerCumulativeAppend(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "bob")
	sessionKey := "lanxin:p2p:alice"

	if _, err := p.saveMedia("m-first", sessionKey, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("saveMedia first: %v", err)
	}
	if _, err := p.saveMedia("m-second", sessionKey, time.Now().Add(time.Second).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("saveMedia second: %v", err)
	}

	data, err := os.ReadFile(sessionMarkerPath(p.downloadDir, sessionKey))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m sessionUploadMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	// Cumulative: BOTH uploads are retained (append, not overwrite).
	if len(m.Files) != 2 {
		t.Fatalf("marker files = %d, want 2 (cumulative append)", len(m.Files))
	}
	if m.Files[0].MediaID != "m-first" || m.Files[1].MediaID != "m-second" {
		t.Errorf("marker media_ids = [%s %s], want [m-first m-second]", m.Files[0].MediaID, m.Files[1].MediaID)
	}
	if m.Files[0].Path != filepath.Join(p.downloadDir, "m-first") {
		t.Errorf("marker path 0 = %q, want %q", m.Files[0].Path, filepath.Join(p.downloadDir, "m-first"))
	}
	if m.Files[1].Path != filepath.Join(p.downloadDir, "m-second") {
		t.Errorf("marker path 1 = %q, want %q", m.Files[1].Path, filepath.Join(p.downloadDir, "m-second"))
	}
}

func TestAppendSessionUploads_KeepsBatchAndCumulative(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "bob")
	sessionKey := "lanxin:p2p:alice"

	at1 := time.Now().Add(-time.Minute).Format(time.RFC3339)
	files := []sessionUploadFile{
		{Path: filepath.Join(p.downloadDir, "a.log"), MediaID: "ma", OriginalName: "a.log", UploadedAt: at1},
		{Path: filepath.Join(p.downloadDir, "b.log"), MediaID: "mb", OriginalName: "b.log", UploadedAt: at1},
	}
	if err := p.appendSessionUploads(sessionKey, files...); err != nil {
		t.Fatalf("appendSessionUploads: %v", err)
	}

	// a second batch must accumulate on top of the first
	at2 := time.Now().Format(time.RFC3339)
	if err := p.appendSessionUploads(sessionKey, sessionUploadFile{Path: filepath.Join(p.downloadDir, "c.zip"), MediaID: "mc", OriginalName: "c.zip", UploadedAt: at2}); err != nil {
		t.Fatalf("appendSessionUploads second batch: %v", err)
	}

	data, err := os.ReadFile(sessionMarkerPath(p.downloadDir, sessionKey))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m sessionUploadMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	if len(m.Files) != 3 {
		t.Fatalf("marker files = %d, want 3 (cumulative across batches)", len(m.Files))
	}
	if m.Files[0].MediaID != "ma" || m.Files[1].MediaID != "mb" || m.Files[2].MediaID != "mc" {
		t.Errorf("marker files order = [%s %s %s], want [ma mb mc]", m.Files[0].MediaID, m.Files[1].MediaID, m.Files[2].MediaID)
	}
}

func TestAppendSessionUploads_DedupSameBatch(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "bob")
	sessionKey := "lanxin:p2p:alice"

	at := time.Now().Format(time.RFC3339)
	// the same media uploaded twice at the same second (same batch) must not
	// produce two records
	dup := []sessionUploadFile{
		{Path: "/x/m1.log", MediaID: "m1", OriginalName: "m1.log", UploadedAt: at},
		{Path: "/x/m1.log", MediaID: "m1", OriginalName: "m1.log", UploadedAt: at},
	}
	if err := p.appendSessionUploads(sessionKey, dup...); err != nil {
		t.Fatalf("appendSessionUploads: %v", err)
	}
	// a genuine re-upload later (different timestamp) IS a new record
	later := time.Now().Add(time.Second).Format(time.RFC3339)
	if err := p.appendSessionUploads(sessionKey, sessionUploadFile{Path: "/x/m1.log", MediaID: "m1", OriginalName: "m1.log", UploadedAt: later}); err != nil {
		t.Fatalf("appendSessionUploads re-upload: %v", err)
	}

	data, err := os.ReadFile(sessionMarkerPath(p.downloadDir, sessionKey))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m sessionUploadMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	if len(m.Files) != 2 {
		t.Fatalf("marker files = %d, want 2 (same-batch dedup + one later re-upload)", len(m.Files))
	}
}

func TestAppendSessionUploads_CapKeepsNewest(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "bob")
	sessionKey := "lanxin:p2p:alice"

	total := maxSessionUploadFiles + 10
	base := time.Now().Add(-time.Duration(total) * time.Minute)
	for i := 0; i < total; i++ {
		at := base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
		if err := p.appendSessionUploads(sessionKey, sessionUploadFile{
			Path:       filepath.Join(p.downloadDir, fmt.Sprintf("f%d.log", i)),
			MediaID:    fmt.Sprintf("m%d", i),
			UploadedAt: at,
		}); err != nil {
			t.Fatalf("appendSessionUploads %d: %v", i, err)
		}
	}

	data, err := os.ReadFile(sessionMarkerPath(p.downloadDir, sessionKey))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m sessionUploadMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	if len(m.Files) != maxSessionUploadFiles {
		t.Fatalf("marker files = %d, want capped at %d", len(m.Files), maxSessionUploadFiles)
	}
	// the 10 oldest are dropped; the first retained is m10 (the 11th upload)
	if m.Files[0].MediaID != fmt.Sprintf("m%d", total-maxSessionUploadFiles) {
		t.Errorf("oldest retained = %q, want m%d (oldest dropped)", m.Files[0].MediaID, total-maxSessionUploadFiles)
	}
	if m.Files[len(m.Files)-1].MediaID != fmt.Sprintf("m%d", total-1) {
		t.Errorf("newest retained = %q, want m%d", m.Files[len(m.Files)-1].MediaID, total-1)
	}
}

func TestAppendSessionUploads_RejectsEmptySessionKey(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "bob")
	if err := p.appendSessionUploads("", sessionUploadFile{Path: "/x"}); err == nil {
		t.Fatal("expected error for empty session key")
	}
}

func TestSessionUploadedFiles_Isolation(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "bob")
	skA := "lanxin:p2p:alice"
	skB := "lanxin:p2p:bob"

	if _, err := p.saveMedia("m-a", skA, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("saveMedia A: %v", err)
	}
	if _, err := p.saveMedia("m-b", skB, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("saveMedia B: %v", err)
	}

	filesA := p.sessionUploadedFiles(skA)
	filesB := p.sessionUploadedFiles(skB)
	if len(filesA) != 1 || !strings.Contains(filesA[0].Path, "m-a") {
		t.Errorf("session A files = %+v, want exactly [<...>/m-a]", filesA)
	}
	if len(filesB) != 1 || !strings.Contains(filesB[0].Path, "m-b") {
		t.Errorf("session B files = %+v, want exactly [<...>/m-b]", filesB)
	}
}

func TestSessionUploadedFiles_OriginalNameFallback(t *testing.T) {
	// Old marker without original_name (backward compat) → fall back to mediaID.
	p := &Platform{downloadDir: t.TempDir()}
	sessionKey := "lanxin:p2p:alice"
	now := time.Now().Format(time.RFC3339)
	marker := sessionUploadMarker{
		SessionKey: sessionKey,
		Files:      []sessionUploadFile{{Path: "/x/y.log", MediaID: "m1", UploadedAt: now}},
		UpdatedAt:  now,
	}
	data, _ := json.Marshal(marker)
	os.MkdirAll(filepath.Join(p.downloadDir, sessionMarkerDir), 0o755)
	if err := os.WriteFile(sessionMarkerPath(p.downloadDir, sessionKey), data, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	files := p.sessionUploadedFiles(sessionKey)
	if len(files) != 1 {
		t.Fatalf("files = %+v, want 1", files)
	}
	if files[0].OriginalName != "m1" {
		t.Errorf("OriginalName = %q, want fallback to mediaID m1", files[0].OriginalName)
	}
}

func TestSessionUploadedFiles_MissingMarker(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "bob")
	files := p.sessionUploadedFiles("lanxin:p2p:ghost")
	if files == nil {
		t.Fatal("expected non-nil empty slice for lanxin session with no marker")
	}
	if len(files) != 0 {
		t.Fatalf("expected no files, got %v", files)
	}
}

func TestSessionUploadedFiles_ExpiredMarker(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "bob")
	p.uploadMarkerTTL = time.Minute
	sessionKey := "lanxin:p2p:alice"

	old := time.Now().Add(-2 * time.Minute).Format(time.RFC3339)
	marker := sessionUploadMarker{
		SessionKey: sessionKey,
		Files:      []sessionUploadFile{{Path: "/x/y.log", MediaID: "m1", UploadedAt: old}},
		UpdatedAt:  old,
	}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(p.downloadDir, sessionMarkerDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(sessionMarkerPath(p.downloadDir, sessionKey), data, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	if files := p.sessionUploadedFiles(sessionKey); len(files) != 0 {
		t.Fatalf("expected expired marker to yield no files, got %v", files)
	}
}

func TestExpireUploadMarkerIfStale_RemovesExpired(t *testing.T) {
	p := &Platform{downloadDir: t.TempDir(), uploadMarkerTTL: time.Minute}
	sessionKey := "lanxin:p2p:alice"
	markerPath := sessionMarkerPath(p.downloadDir, sessionKey)

	old := time.Now().Add(-2 * time.Minute).Format(time.RFC3339)
	marker := sessionUploadMarker{
		SessionKey: sessionKey,
		Files:      []sessionUploadFile{{Path: "/x", MediaID: "m", UploadedAt: old}},
		UpdatedAt:  old,
	}
	data, _ := json.Marshal(marker)
	if err := os.MkdirAll(filepath.Join(p.downloadDir, sessionMarkerDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(markerPath, data, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	p.expireUploadMarkerIfStale()

	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale marker to be removed, stat err = %v", err)
	}
}

func TestExpireUploadMarkerIfStale_KeepsFresh(t *testing.T) {
	p := &Platform{downloadDir: t.TempDir(), uploadMarkerTTL: time.Minute}
	sessionKey := "lanxin:p2p:alice"
	markerPath := sessionMarkerPath(p.downloadDir, sessionKey)

	now := time.Now().Format(time.RFC3339)
	marker := sessionUploadMarker{
		SessionKey: sessionKey,
		Files:      []sessionUploadFile{{Path: "/x", MediaID: "m", UploadedAt: now}},
		UpdatedAt:  now,
	}
	data, _ := json.Marshal(marker)
	if err := os.MkdirAll(filepath.Join(p.downloadDir, sessionMarkerDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(markerPath, data, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	p.expireUploadMarkerIfStale()

	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("expected fresh marker to be kept: %v", err)
	}
}

func TestExpireUploadMarkerIfStale_OnlyRemovesExpired(t *testing.T) {
	p := &Platform{downloadDir: t.TempDir(), uploadMarkerTTL: time.Minute}
	skA := "lanxin:p2p:alice"
	skB := "lanxin:p2p:bob"

	old := time.Now().Add(-2 * time.Minute).Format(time.RFC3339)
	now := time.Now().Format(time.RFC3339)
	dir := filepath.Join(p.downloadDir, sessionMarkerDir)
	os.MkdirAll(dir, 0o755)
	for _, c := range []struct {
		key string
		at  string
	}{{skA, old}, {skB, now}} {
		m := sessionUploadMarker{SessionKey: c.key, Files: []sessionUploadFile{{Path: "/x", MediaID: "m", UploadedAt: c.at}}, UpdatedAt: c.at}
		data, _ := json.Marshal(m)
		if err := os.WriteFile(sessionMarkerPath(p.downloadDir, c.key), data, 0o644); err != nil {
			t.Fatalf("write marker: %v", err)
		}
	}

	p.expireUploadMarkerIfStale()

	if _, err := os.Stat(sessionMarkerPath(p.downloadDir, skA)); !os.IsNotExist(err) {
		t.Errorf("expected stale marker %s to be removed", skA)
	}
	if _, err := os.Stat(sessionMarkerPath(p.downloadDir, skB)); err != nil {
		t.Errorf("expected fresh marker %s to be kept: %v", skB, err)
	}
}

// waitForMarker polls until the per-session marker file for sessionKey exists.
func waitForMarker(t *testing.T, p *Platform, sessionKey string) {
	t.Helper()
	pth := sessionMarkerPath(p.downloadDir, sessionKey)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(pth); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for marker %s", pth)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitForMarkerFileCount polls until the session marker for sessionKey records
// at least want files (used when a single reply channel cannot distinguish
// consecutive upload messages).
func waitForMarkerFileCount(t *testing.T, p *Platform, sessionKey string, want int) {
	t.Helper()
	pth := sessionMarkerPath(p.downloadDir, sessionKey)
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, err := os.ReadFile(pth)
		if err == nil {
			var m sessionUploadMarker
			if json.Unmarshal(data, &m) == nil && len(m.Files) >= want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for marker %s to have %d files", pth, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHandleWSMessage_FileMessageWritesPerSessionMarker verifies the end-to-end
// flow: a file message from the WS read loop flows through handleFileMessage →
// saveMedia → the per-session marker under uploads/.sessions/ keyed by the
// same session key the sender's text commands use.
func TestHandleWSMessage_FileMessageWritesPerSessionMarker(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "bob")

	raw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"file","msgData":{"file":{"mediaIds":["m-marker"]}},"msgId":"gm-marker","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(raw))

	// handleFileMessage runs in a goroutine; the reply is sent last, so once it
	// arrives the marker has already been written.
	waitForGroupReply(t, rec, 2*time.Second)

	sessionKey := "lanxin:group:group-456:bob" // shareSessionInChannel=false default → per-user session
	data, err := os.ReadFile(sessionMarkerPath(p.downloadDir, sessionKey))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m sessionUploadMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	if m.SessionKey != sessionKey {
		t.Errorf("marker session_key = %q, want %q", m.SessionKey, sessionKey)
	}
	if len(m.Files) != 1 || m.Files[0].MediaID != "m-marker" {
		t.Fatalf("marker files = %+v, want [m-marker]", m.Files)
	}
	if m.Files[0].Path != filepath.Join(p.downloadDir, "m-marker") {
		t.Errorf("marker path = %q, want %q", m.Files[0].Path, filepath.Join(p.downloadDir, "m-marker"))
	}
}

// TestHandleWSMessage_FileMessageMultiMedia verifies a single file message with
// several mediaIds records ALL of them in the session marker (batch overwrite).
func TestHandleWSMessage_FileMessageMultiMedia(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "bob")

	raw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"file","msgData":{"file":{"mediaIds":["m1","m2"]}},"msgId":"gm-multi","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(raw))
	waitForGroupReply(t, rec, 2*time.Second)

	sessionKey := "lanxin:group:group-456:bob"
	data, err := os.ReadFile(sessionMarkerPath(p.downloadDir, sessionKey))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m sessionUploadMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	if len(m.Files) != 2 {
		t.Fatalf("marker files = %d, want 2 (all mediaIds of the message)", len(m.Files))
	}
	if m.Files[0].MediaID != "m1" || m.Files[1].MediaID != "m2" {
		t.Errorf("marker files = [%s %s], want [m1 m2]", m.Files[0].MediaID, m.Files[1].MediaID)
	}
}

// TestHandleWSMessage_GroupSharedSessionMarker verifies the shared-session
// group mode (share_session_in_channel=true) keys the marker by groupId only.
func TestHandleWSMessage_GroupSharedSessionMarker(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "bob")
	p.shareSessionInChannel = true

	raw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"file","msgData":{"file":{"mediaIds":["m1"]}},"msgId":"gm-shared","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(raw))
	waitForGroupReply(t, rec, 2*time.Second)

	sessionKey := "lanxin:group:group-456"
	data, err := os.ReadFile(sessionMarkerPath(p.downloadDir, sessionKey))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m sessionUploadMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	if m.SessionKey != sessionKey {
		t.Errorf("marker session_key = %q, want %q", m.SessionKey, sessionKey)
	}
}

// newTestPlatformWithCapture wires a Platform to the httptest server plus a
// handler that records every message routed to the engine (for verifying
// UploadedFiles on text commands).
func newTestPlatformWithCapture(t *testing.T, allowFrom string) (*Platform, *reqRecorder, chan *core.Message) {
	t.Helper()
	return newTestPlatformWithCaptureDelay(t, allowFrom, 0)
}

// newTestPlatformWithCaptureDelay is newTestPlatformWithCapture plus a
// per-download delay so a media download stays in flight while the text
// command is processed (the upload-then-command race).
func newTestPlatformWithCaptureDelay(t *testing.T, allowFrom string, mediaDelay time.Duration) (*Platform, *reqRecorder, chan *core.Message) {
	t.Helper()
	p, rec := newTestPlatformWithServerDelay(t, allowFrom, "", mediaDelay)
	msgs := make(chan *core.Message, 8)
	p.handler = func(_ core.Platform, m *core.Message) { msgs <- m }
	return p, rec, msgs
}

// TestHandleWSMessage_TextMessageCarriesUploadedFiles verifies that after a
// file upload, the sender's next text command carries the session's uploaded
// file paths in Message.UploadedFiles (engine-injection source).
func TestHandleWSMessage_TextMessageCarriesUploadedFiles(t *testing.T) {
	p, rec, msgs := newTestPlatformWithCapture(t, "bob")

	// 1) group file message from bob → per-user session marker
	fileRaw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"file","msgData":{"file":{"mediaIds":["m1"]}},"msgId":"gm1","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(fileRaw))
	waitForGroupReply(t, rec, 2*time.Second)

	// 2) text command from the same sender in the same group
	textRaw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"text","msgData":{"text":{"content":"分析"}},"msgId":"gm2","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(textRaw))

	select {
	case m := <-msgs:
		wantKey := "lanxin:group:group-456:bob"
		if m.SessionKey != wantKey {
			t.Errorf("session key = %q, want %q", m.SessionKey, wantKey)
		}
		if m.UploadedFiles == nil {
			t.Fatal("expected non-nil UploadedFiles on text message")
		}
		if len(m.UploadedFiles) != 1 || !strings.Contains(m.UploadedFiles[0].Path, "m1") {
			t.Errorf("UploadedFiles = %+v, want exactly [<...>/m1]", m.UploadedFiles)
		}
		// no Content-Disposition header → original name falls back to mediaID
		if m.UploadedFiles[0].OriginalName != "m1" {
			t.Errorf("OriginalName = %q, want fallback to mediaID m1", m.UploadedFiles[0].OriginalName)
		}
		if _, err := time.Parse(time.RFC3339, m.UploadedFiles[0].UploadedAt); err != nil {
			t.Errorf("UploadedAt not RFC3339: %q (%v)", m.UploadedFiles[0].UploadedAt, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for text message routing")
	}
}

// --- in-flight upload tracking (upload-then-command race fix) ---

// waitForInflight polls until sessionKey has a registered in-flight upload
// (the file-handler goroutine has run beginSessionUpload). Deterministic
// replacement for a bare sleep when a test must observe the race window.
func waitForInflight(t *testing.T, p *Platform, sessionKey string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.uploadMu.Lock()
		uf := p.inflightUploads[sessionKey]
		p.uploadMu.Unlock()
		if uf != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for in-flight upload registration for %s", sessionKey)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestInflightUploads_BeginFinishDelete verifies beginSessionUpload registers a
// tracker, reuses it for a second concurrent upload of the same session, keeps
// the session "in flight" while any reference remains, and removes it only when
// the last reference is finished.
func TestInflightUploads_BeginFinishDelete(t *testing.T) {
	p := &Platform{}
	sk := "lanxin:p2p:alice"

	uf1 := p.beginSessionUpload(sk)
	if uf1 == nil {
		t.Fatal("expected a non-nil tracker")
	}
	// a second concurrent upload of the same session shares the tracker
	uf2 := p.beginSessionUpload(sk)
	if uf2 != uf1 {
		t.Fatal("expected the same tracker for a second concurrent upload to the same session")
	}

	// still in flight → wait must not return true (short timeout proves the wait blocks)
	if p.waitSessionUploads(sk, 10*time.Millisecond) {
		t.Fatal("wait returned true while an upload is still in flight")
	}

	p.finishSessionUpload(sk, uf1)
	// uf2's reference is still held → still in flight
	if p.waitSessionUploads(sk, 10*time.Millisecond) {
		t.Fatal("wait returned true while a second upload reference is still in flight")
	}

	p.finishSessionUpload(sk, uf2)
	if !p.waitSessionUploads(sk, 10*time.Millisecond) {
		t.Fatal("wait returned false after all uploads finished")
	}
	// tracker removed from the map
	p.uploadMu.Lock()
	_, ok := p.inflightUploads[sk]
	p.uploadMu.Unlock()
	if ok {
		t.Fatal("expected the finished tracker to be removed from the in-flight map")
	}
}

// TestInflightUploads_WaitReturnsAfterDownloadCompletes verifies the core race
// fix: a waiter blocks while the download is in flight, and is released only
// after the session marker has been written (finishSessionUpload closes done
// AFTER the marker write, via the deferred call after saveMedia).
func TestInflightUploads_WaitReturnsAfterDownloadCompletes(t *testing.T) {
	p := &Platform{downloadDir: t.TempDir()}
	sk := "lanxin:p2p:alice"
	uf := p.beginSessionUpload(sk)

	got := make(chan bool, 1)
	go func() { got <- p.waitSessionUploads(sk, 2*time.Second) }()

	// let the waiter block on done, then simulate the download completing:
	// marker write first (as saveMedia does), finish second (as the deferred
	// call in handleFileMessage does).
	time.Sleep(50 * time.Millisecond)
	if err := p.appendSessionUploads(sk, sessionUploadFile{Path: "/x/m1.log", MediaID: "m1", UploadedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("appendSessionUploads: %v", err)
	}
	p.finishSessionUpload(sk, uf)

	select {
	case ok := <-got:
		if !ok {
			t.Fatal("wait returned false after the download finished")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("wait did not return after the download finished")
	}
	// the marker written before finish is now readable (as the text handler
	// reads it after the wait returns)
	files := p.sessionUploadedFiles(sk)
	if len(files) != 1 || !strings.Contains(files[0].Path, "m1") {
		t.Errorf("sessionUploadedFiles = %+v, want exactly [<...>/m1]", files)
	}
}

// TestInflightUploads_WaitTimeout verifies a waiter is released by the timeout
// when a download never finishes (no hang; caller proceeds with current state).
func TestInflightUploads_WaitTimeout(t *testing.T) {
	p := &Platform{}
	sk := "lanxin:p2p:alice"
	_ = p.beginSessionUpload(sk) // never finished

	start := time.Now()
	if p.waitSessionUploads(sk, 100*time.Millisecond) {
		t.Fatal("wait returned true on timeout with a still-in-flight upload")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("wait timeout took %v, want ~100ms", elapsed)
	}
}

// TestInflightUploads_NoInflightReturnsImmediately verifies the fast path:
// a session with no in-flight download returns true with no waiting.
func TestInflightUploads_NoInflightReturnsImmediately(t *testing.T) {
	p := &Platform{}
	if !p.waitSessionUploads("lanxin:p2p:ghost", 100*time.Millisecond) {
		t.Fatal("wait returned false with no in-flight upload")
	}
}

// TestInflightUploads_ConcurrentWaitersAndSessions verifies that several text
// commands of the same session waiting concurrently are all released when the
// session's download finishes, and that finishing one session does not release
// a waiter of another session.
func TestInflightUploads_ConcurrentWaitersAndSessions(t *testing.T) {
	p := &Platform{}
	skA := "lanxin:p2p:alice"
	skB := "lanxin:p2p:bob"

	ufA := p.beginSessionUpload(skA)
	ufB := p.beginSessionUpload(skB)

	// session A: several text commands waiting concurrently; all must be released
	const n = 5
	var wg sync.WaitGroup
	resultsA := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resultsA[i] = p.waitSessionUploads(skA, 2*time.Second)
		}(i)
	}

	// session B: an independent waiter must only be released by finishing B
	bRes := make(chan bool, 1)
	go func() { bRes <- p.waitSessionUploads(skB, 2*time.Second) }()

	time.Sleep(50 * time.Millisecond) // let all waiters block on their done

	p.finishSessionUpload(skB, ufB)
	select {
	case ok := <-bRes:
		if !ok {
			t.Fatal("session B wait returned false after B finished")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("session B wait not released after B finished")
	}

	// finishing B must NOT release A's waiters — A is still in flight
	if p.waitSessionUploads(skA, 20*time.Millisecond) {
		t.Fatal("session A wait returned true even though A is still in flight (B's finish leaked)")
	}

	p.finishSessionUpload(skA, ufA)
	wg.Wait()
	for i, ok := range resultsA {
		if !ok {
			t.Errorf("waiter %d of session A returned false", i)
		}
	}
}

// TestHandleWSMessage_TextRightAfterFileWaitsForInflight is the regression test
// for the upload-download race: a large file download takes seconds, so a text
// command sent immediately after the file message used to read an empty
// per-session marker and tell the agent "no files uploaded". The in-flight
// tracker makes the text command wait for the download (and its marker write)
// before populating UploadedFiles.
func TestHandleWSMessage_TextRightAfterFileWaitsForInflight(t *testing.T) {
	p, _, msgs := newTestPlatformWithCaptureDelay(t, "bob", 300*time.Millisecond)

	// group file message from bob; the download takes 300ms, so the marker is
	// NOT yet written when the text command arrives moments later.
	fileRaw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"file","msgData":{"file":{"mediaIds":["m-race"]}},"msgId":"gm-race","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(fileRaw))

	// wait until the download is registered as in-flight, then send the text
	// command — exactly the "upload then immediately type a command" flow.
	sessionKey := "lanxin:group:group-456:bob"
	waitForInflight(t, p, sessionKey)

	textRaw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"text","msgData":{"text":{"content":"分析"}},"msgId":"gm-race-2","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(textRaw))

	select {
	case m := <-msgs:
		if len(m.UploadedFiles) != 1 || !strings.Contains(m.UploadedFiles[0].Path, "m-race") {
			t.Errorf("UploadedFiles = %+v, want exactly [<...>/m-race] (text after file must wait for the download)", m.UploadedFiles)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for text message routing")
	}
}

// TestHandleWSMessage_TextMessageNoUpload verifies a lanxin text message
// without any prior upload carries a non-nil EMPTY UploadedFiles (so the
// engine injects the "no upload" note, not nothing).
func TestHandleWSMessage_TextMessageNoUpload(t *testing.T) {
	p, _, msgs := newTestPlatformWithCapture(t, "bob")

	raw := `{"events":[{"type":"bot_private_message","data":{"from":"bob","chatType":"p2p","msgType":"text","msgData":{"text":{"content":"你好"}},"msgId":"pm1","senderName":"Bob"}}]}`
	p.handleWSMessage([]byte(raw))

	select {
	case m := <-msgs:
		if m.UploadedFiles == nil {
			t.Fatal("expected non-nil empty UploadedFiles for lanxin text message")
		}
		if len(m.UploadedFiles) != 0 {
			t.Fatalf("expected no uploaded files, got %v", m.UploadedFiles)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for text message routing")
	}
}

// TestConcurrentSessions_FileIsolation simulates two different chat sessions
// (same group, different senders → per-user session keys) uploading files and
// then sending text commands to the SAME platform concurrently. It verifies:
//  1. each session's per-session marker records only its own file, and
//  2. each session's text message carries only its own uploaded file list
//     (no cross-session contamination through the upload markers).
func TestConcurrentSessions_FileIsolation(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "alice,bob")

	var mu sync.Mutex
	routed := map[string][]string{}
	routedOnce := map[string]bool{}
	p.handler = func(_ core.Platform, m *core.Message) {
		mu.Lock()
		defer mu.Unlock()
		paths := make([]string, 0, len(m.UploadedFiles))
		for _, f := range m.UploadedFiles {
			paths = append(paths, f.Path)
		}
		routed[m.SessionKey] = paths
		routedOnce[m.SessionKey] = true
	}

	skA := "lanxin:group:g1:alice"
	skB := "lanxin:group:g1:bob"

	// Two concurrent file messages (each spawns its own download goroutine).
	fileA := `{"events":[{"type":"bot_group_message","data":{"from":"alice","chatType":"group","groupId":"g1","msgType":"file","msgData":{"file":{"mediaIds":["m-a"]}},"msgId":"ga1","reminder":{"isAtMe":true}}}]}`
	fileB := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"g1","msgType":"file","msgData":{"file":{"mediaIds":["m-b"]}},"msgId":"gb1","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(fileA))
	p.handleWSMessage([]byte(fileB))

	// Wait for BOTH session markers to be written.
	waitForMarker(t, p, skA)
	waitForMarker(t, p, skB)

	// Markers are independent files with only their own media.
	dataA, err := os.ReadFile(sessionMarkerPath(p.downloadDir, skA))
	if err != nil {
		t.Fatalf("read marker A: %v", err)
	}
	dataB, err := os.ReadFile(sessionMarkerPath(p.downloadDir, skB))
	if err != nil {
		t.Fatalf("read marker B: %v", err)
	}
	var mA, mB sessionUploadMarker
	if err := json.Unmarshal(dataA, &mA); err != nil {
		t.Fatalf("unmarshal marker A: %v", err)
	}
	if err := json.Unmarshal(dataB, &mB); err != nil {
		t.Fatalf("unmarshal marker B: %v", err)
	}
	if len(mA.Files) != 1 || mA.Files[0].MediaID != "m-a" {
		t.Errorf("marker A files = %+v, want exactly [m-a]", mA.Files)
	}
	if len(mB.Files) != 1 || mB.Files[0].MediaID != "m-b" {
		t.Errorf("marker B files = %+v, want exactly [m-b]", mB.Files)
	}

	// Both sessions now send text commands.
	textA := `{"events":[{"type":"bot_group_message","data":{"from":"alice","chatType":"group","groupId":"g1","msgType":"text","msgData":{"text":{"content":"分析A"}},"msgId":"ga2","reminder":{"isAtMe":true}}}]}`
	textB := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"g1","msgType":"text","msgData":{"text":{"content":"分析B"}},"msgId":"gb2","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(textA))
	p.handleWSMessage([]byte(textB))

	// Wait until BOTH text messages were routed to the handler.
	deadline := time.Now().Add(3 * time.Second)
	for {
		mu.Lock()
		done := routedOnce[skA] && routedOnce[skB]
		mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for both sessions to route text; routed=%v", routed)
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	filesA := routed[skA]
	filesB := routed[skB]
	if len(filesA) != 1 || !strings.Contains(filesA[0], "m-a") {
		t.Errorf("session A UploadedFiles = %v, want exactly [<...>/m-a]", filesA)
	}
	if len(filesB) != 1 || !strings.Contains(filesB[0], "m-b") {
		t.Errorf("session B UploadedFiles = %v, want exactly [<...>/m-b]", filesB)
	}
	if strings.Contains(strings.Join(filesA, " "), "m-b") {
		t.Errorf("session A saw session B's file: %v (cross-session contamination)", filesA)
	}
	if strings.Contains(strings.Join(filesB, " "), "m-a") {
		t.Errorf("session B saw session A's file: %v (cross-session contamination)", filesB)
	}
}

// TestContentDispositionFilename covers plain / quoted / RFC 5987 / missing
// filename forms plus the path-traversal defenses (display-only name must never
// carry directory components).
func TestContentDispositionFilename(t *testing.T) {
	cases := []struct {
		name, cd, want string
	}{
		{"plain", `attachment;filename=DNSxxx.pdf`, "DNSxxx.pdf"},
		{"quoted", `attachment; filename="report v2.log"`, "report v2.log"},
		{"rfc5987", `attachment; filename*=UTF-8''%E6%97%A5%E5%BF%97.zip`, "日志.zip"},
		{"rfc5987-precedence", `attachment; filename="fallback.pdf"; filename*=UTF-8''%E6%97%A5%E5%BF%97.zip`, "日志.zip"},
		{"empty", "", ""},
		{"no-filename", `attachment`, ""},
		{"traversal", `attachment; filename="../../etc/passwd"`, "passwd"},
		{"win-path", `attachment; filename="C:\\Users\\x\\a.txt"`, "a.txt"},
	}
	for _, c := range cases {
		if got := contentDispositionFilename(c.cd); got != c.want {
			t.Errorf("%s: contentDispositionFilename(%q) = %q, want %q", c.name, c.cd, got, c.want)
		}
	}
}

// TestContentDispositionFilename_GBK verifies GBK-encoded (non-UTF-8) Chinese
// file names in a plain filename="..." header are decoded to UTF-8.
func TestContentDispositionFilename_GBK(t *testing.T) {
	gbk, err := simplifiedchinese.GBK.NewEncoder().String("日志分析.zip")
	if err != nil {
		t.Fatalf("gbk encode: %v", err)
	}
	cd := `attachment; filename="` + gbk + `"`
	if got := contentDispositionFilename(cd); got != "日志分析.zip" {
		t.Errorf("contentDispositionFilename(%q) = %q, want %q", cd, got, "日志分析.zip")
	}
}

// TestContentDispositionFilename_ASCIIKept verifies an already-UTF-8 (ASCII)
// name passes through unchanged (no false GBK decoding).
func TestContentDispositionFilename_ASCIIKept(t *testing.T) {
	cd := `attachment; filename="DNSxxx.pdf"`
	if got := contentDispositionFilename(cd); got != "DNSxxx.pdf" {
		t.Errorf("contentDispositionFilename(%q) = %q, want DNSxxx.pdf", cd, got)
	}
}

// TestSaveMedia_RecordsOriginalNameFromDisposition verifies saveMedia stores the
// Content-Disposition original name in the marker while the saved path stays
// mediaID-based (the original name is display-only, never joined into a path).
func TestSaveMedia_RecordsOriginalNameFromDisposition(t *testing.T) {
	p, _ := newTestPlatformWithServerCD(t, "bob", `attachment; filename="icg-locating-ICGBUG-123.zip"`)
	sessionKey := "lanxin:p2p:alice"

	if _, err := p.saveMedia("m-orig", sessionKey, time.Now().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("saveMedia: %v", err)
	}

	data, err := os.ReadFile(sessionMarkerPath(p.downloadDir, sessionKey))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m sessionUploadMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	if len(m.Files) != 1 {
		t.Fatalf("marker files = %d, want 1", len(m.Files))
	}
	if m.Files[0].OriginalName != "icg-locating-ICGBUG-123.zip" {
		t.Errorf("marker original_name = %q, want icg-locating-ICGBUG-123.zip", m.Files[0].OriginalName)
	}
	// saved path still uses the mediaID as base (plus the ASCII extension) —
	// the original name must not leak into the filesystem path.
	if m.Files[0].Path != filepath.Join(p.downloadDir, "m-orig.zip") {
		t.Errorf("marker path = %q, want %q (original name not used in path)", m.Files[0].Path, filepath.Join(p.downloadDir, "m-orig.zip"))
	}
}

// TestAppendSessionUploads_StaleMarkerStartsFresh verifies the TTL semantics:
// appending to a marker that already expired must NOT inherit its old files
// (the marker starts over from the new upload).
func TestAppendSessionUploads_StaleMarkerStartsFresh(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "bob")
	p.uploadMarkerTTL = time.Minute
	sessionKey := "lanxin:p2p:alice"

	old := time.Now().Add(-2 * time.Minute).Format(time.RFC3339)
	marker := sessionUploadMarker{
		SessionKey: sessionKey,
		Files:      []sessionUploadFile{{Path: "/x/m-old", MediaID: "m-old", UploadedAt: old}},
		UpdatedAt:  old,
	}
	data, _ := json.Marshal(marker)
	os.MkdirAll(filepath.Join(p.downloadDir, sessionMarkerDir), 0o755)
	if err := os.WriteFile(sessionMarkerPath(p.downloadDir, sessionKey), data, 0o644); err != nil {
		t.Fatalf("write stale marker: %v", err)
	}

	if err := p.appendSessionUploads(sessionKey, sessionUploadFile{Path: "/y/m-new", MediaID: "m-new", UploadedAt: time.Now().Format(time.RFC3339)}); err != nil {
		t.Fatalf("appendSessionUploads: %v", err)
	}

	files := p.sessionUploadedFiles(sessionKey)
	if len(files) != 1 || files[0].OriginalName != "m-new" {
		t.Errorf("files = %+v, want exactly the new upload (stale marker starts fresh)", files)
	}
}

// TestConcurrentSaveMedia_SameSession_NoLostUpdate runs N uploads to the SAME
// session concurrently and verifies all N are recorded (run with -race). The
// marker read-modify-write is serialized by markerMu, so no update is lost.
func TestConcurrentSaveMedia_SameSession_NoLostUpdate(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "alice")
	sessionKey := "lanxin:p2p:alice"

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			mid := fmt.Sprintf("m-conc-%d", i)
			if _, err := p.saveMedia(mid, sessionKey, time.Now().Format(time.RFC3339Nano)); err != nil {
				t.Errorf("saveMedia %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	files := p.sessionUploadedFiles(sessionKey)
	if len(files) != n {
		t.Fatalf("session files = %d, want %d (concurrent appends lost updates)", len(files), n)
	}
	seen := map[string]bool{}
	for _, f := range files {
		if f.Path == "" {
			t.Errorf("empty path in %+v", f)
		}
		if seen[f.Path] {
			t.Errorf("duplicate path recorded: %s", f.Path)
		}
		seen[f.Path] = true
	}
}

// TestHandleWSMessage_TwoUploadsCumulative verifies the end-to-end flow: two
// separate file messages to the same session produce a marker with BOTH files,
// and a later text command carries both in Message.UploadedFiles (timeline).
func TestHandleWSMessage_TwoUploadsCumulative(t *testing.T) {
	p, _, msgs := newTestPlatformWithCapture(t, "bob")

	file1 := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"file","msgData":{"file":{"mediaIds":["m1"]}},"msgId":"gm1","reminder":{"isAtMe":true}}}]}`
	file2 := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"file","msgData":{"file":{"mediaIds":["m2"]}},"msgId":"gm2","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(file1))
	p.handleWSMessage([]byte(file2))
	// handleFileMessage runs in a goroutine per message; the marker write
	// happens inside saveMedia before each reply. waitForGroupReply cannot
	// distinguish the two replies, so poll the marker until BOTH uploads are
	// recorded instead.
	sessionKey := "lanxin:group:group-456:bob"
	waitForMarkerFileCount(t, p, sessionKey, 2)

	data, err := os.ReadFile(sessionMarkerPath(p.downloadDir, sessionKey))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	var m sessionUploadMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal marker: %v", err)
	}
	// The raw marker file order is goroutine completion order (concurrent
	// downloads append out of order), so assert it as a set.
	if len(m.Files) != 2 {
		t.Fatalf("marker files = %+v, want 2 (two uploads accumulated)", m.Files)
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		seen[f.MediaID] = true
	}
	if !seen["m1"] || !seen["m2"] {
		t.Fatalf("marker files = %+v, want {m1, m2}", m.Files)
	}

	// text command now carries BOTH files
	textRaw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"text","msgData":{"text":{"content":"分析"}},"msgId":"gm3","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(textRaw))
	select {
	case msg := <-msgs:
		if msg.UploadedFiles == nil {
			t.Fatal("expected non-nil UploadedFiles")
		}
		if len(msg.UploadedFiles) != 2 {
			t.Fatalf("UploadedFiles = %+v, want 2 entries", msg.UploadedFiles)
		}
		// The injected timeline IS order-guaranteed: the WS read loop stamps
		// each message's arrival in read order, so m1 (sent first) sorts first
		// regardless of which download finished first.
		if msg.UploadedFiles[0].OriginalName != "m1" || msg.UploadedFiles[1].OriginalName != "m2" {
			t.Errorf("OriginalNames = [%s %s], want [m1 m2] (arrival-order timeline)", msg.UploadedFiles[0].OriginalName, msg.UploadedFiles[1].OriginalName)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for text message routing")
	}
}

// --- image message support (media carrier extraction, caption routing) ---

// TestParseWSMessage_ImageAndVoiceCarriers verifies media ids and caption are
// extracted from the msgType-matching carrier: msgData.image.mediaIds +
// msgData.image.content for images (aligned with the official lansenger
// channel plugin). Voice messages are parsed for msgType recognition but NOT
// ingested — no ids are extracted, so handleWSMessage's no-content skip drops
// them instead of downloading audio nobody can analyze.
func TestParseWSMessage_ImageAndVoiceCarriers(t *testing.T) {
	imgRaw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","groupId":"g1","msgType":"image","msgData":{"image":{"mediaIds":["img-1","img-2"],"content":"看下这个报错"}},"msgId":"i1","reminder":{"isAtMe":true}}}]}`
	evs := parseWSMessage([]byte(imgRaw))
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.MsgType != "image" {
		t.Errorf("MsgType = %q, want image", ev.MsgType)
	}
	if len(ev.MediaIDs) != 2 || ev.MediaIDs[0] != "img-1" || ev.MediaIDs[1] != "img-2" {
		t.Errorf("image MediaIDs = %v, want [img-1 img-2]", ev.MediaIDs)
	}
	if ev.Content != "看下这个报错" {
		t.Errorf("image caption = %q, want 看下这个报错", ev.Content)
	}

	voiceRaw := `{"events":[{"type":"bot_private_message","data":{"from":"bob","msgType":"voice","msgData":{"voice":{"mediaIds":["v1"]}},"msgId":"v1"}}]}`
	evs = parseWSMessage([]byte(voiceRaw))
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	if evs[0].MsgType != "voice" {
		t.Errorf("voice MsgType = %q, want voice", evs[0].MsgType)
	}
	if len(evs[0].MediaIDs) != 0 {
		t.Errorf("voice MediaIDs = %v, want none (voice not ingested)", evs[0].MediaIDs)
	}
}

// TestHandleWSMessage_ImageWithCaptionRoutesToAgent verifies an image message
// that carries a caption is downloaded, its files injected as UploadedFiles,
// and the (mention-prefix-stripped) caption routed to the agent — the bot can
// answer about the picture immediately.
func TestHandleWSMessage_ImageWithCaptionRoutesToAgent(t *testing.T) {
	p, _, msgs := newTestPlatformWithCapture(t, "bob")

	raw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"image","msgData":{"image":{"mediaIds":["img-1"],"content":"@ICG智能助手 看下这个报错"}},"msgId":"gi1","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(raw))

	select {
	case m := <-msgs:
		if m.Content != "看下这个报错" {
			t.Errorf("agent content = %q, want stripped caption 看下这个报错", m.Content)
		}
		if m.SessionKey != "lanxin:group:group-456:bob" {
			t.Errorf("session key = %q, want lanxin:group:group-456:bob", m.SessionKey)
		}
		if len(m.UploadedFiles) != 1 || !strings.Contains(m.UploadedFiles[0].Path, "img-1") {
			t.Errorf("UploadedFiles = %+v, want exactly [<...>/img-1]", m.UploadedFiles)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for image message routing to agent")
	}
}

// TestHandleWSMessage_ImageNoCaptionAcksImage verifies a caption-less image is
// downloaded and acknowledged with an image-specific reply (not "文件"), like
// the caption-less file intake path.
func TestHandleWSMessage_ImageNoCaptionAcksImage(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "bob")

	raw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"image","msgData":{"image":{"mediaIds":["img-1"]}},"msgId":"gi2","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(raw))

	reply := waitForGroupReply(t, rec, 2*time.Second)
	if !strings.Contains(reply, "已收到图片") {
		t.Errorf("reply = %q, want 已收到图片 ack", reply)
	}
}

// TestSaveMedia_SniffsImageExtension verifies a media download whose response
// has no Content-Disposition filename (the norm for lanxin image messages)
// still lands on disk with a sniffed extension, so agents/tools can read it.
func TestSaveMedia_SniffsImageExtension(t *testing.T) {
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x03}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tokenPath:
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{"appToken":"tok","expiresIn":7200}}`))
		case "/v1/medias/m-img/fetch":
			// No Content-Disposition header: saveMedia must sniff .png.
			w.Write(png)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	p := &Platform{appID: "a", appSecret: "s", apiBase: srv.URL, httpClient: srv.Client(), downloadDir: t.TempDir()}
	path, err := p.saveMedia("m-img", "lanxin:p2p:bob", time.Now().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("saveMedia: %v", err)
	}
	if !strings.HasSuffix(path, ".png") {
		t.Errorf("saved path = %q, want .png extension (sniffed)", path)
	}
}

// --- quoted message (referenceMsg) support ---

// TestParseQuotedMessage extracts the quoted message fields and recurses into
// a nested quote.
func TestParseQuotedMessage(t *testing.T) {
	raw := map[string]any{
		"msgType": "image",
		"from":    "alice",
		"msgData": map[string]any{
			"image": map[string]any{"mediaIds": []any{"img-old"}, "content": "这张图"},
		},
		"referenceMsg": map[string]any{
			"msgType": "text",
			"msgData": map[string]any{"text": map[string]any{"content": "再往上看"}},
		},
	}
	q := parseQuotedMessage(raw)
	if q == nil {
		t.Fatal("parseQuotedMessage returned nil")
	}
	if q.MsgType != "image" || len(q.MediaIDs) != 1 || q.MediaIDs[0] != "img-old" {
		t.Errorf("quoted image = %+v, want kind image + [img-old]", q)
	}
	if q.Content != "这张图" {
		t.Errorf("quoted caption = %q, want 这张图", q.Content)
	}
	if q.Quoted == nil || q.Quoted.MsgType != "text" || q.Quoted.Content != "再往上看" {
		t.Errorf("nested quote = %+v, want text 再往上看", q.Quoted)
	}
}

// TestAttachReference_QuotedTextAndImage verifies attachReference prepends the
// quoted message context, and that a quoted media id already saved under the
// download dir resolves to a local path the agent can actually read.
func TestAttachReference_QuotedTextAndImage(t *testing.T) {
	p, _, msgs := newTestPlatformWithCapture(t, "bob")
	// Simulate an image the user sent earlier this session: already downloaded
	// under downloadDir with the mediaID-based file name.
	if err := os.WriteFile(filepath.Join(p.downloadDir, "img-old.png"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed quoted image: %v", err)
	}

	// Text message quoting an earlier image (mediaId img-old).
	raw := `{"events":[{"type":"bot_private_message","data":{"from":"bob","msgType":"text","msgData":{"text":{"content":"这张图是什么意思"}},"msgId":"m2","referenceMsg":{"from":"alice","senderName":"Alice","msgType":"image","msgData":{"image":{"mediaIds":["img-old"]}},"msgId":"m1"}}}]}`
	p.handleWSMessage([]byte(raw))
	select {
	case m := <-msgs:
		if !strings.Contains(m.Content, "[引用消息] Alice") {
			t.Errorf("content should carry quoted header, got %q", m.Content)
		}
		if !strings.Contains(m.Content, "引用的图片") || !strings.Contains(m.Content, filepath.Join(p.downloadDir, "img-old.png")) {
			t.Errorf("content should resolve quoted image to local path, got %q", m.Content)
		}
		if !strings.Contains(m.Content, "这张图是什么意思") {
			t.Errorf("content should keep the new message, got %q", m.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for quoted text routing")
	}
}

// TestHandleWSMessage_QuotedImageWithNewImage routes a new image that quotes an
// earlier image to the agent (the quoted reference alone is enough context even
// without a caption) and keeps both available.
func TestHandleWSMessage_QuotedImageWithNewImage(t *testing.T) {
	p, _, msgs := newTestPlatformWithCapture(t, "bob")
	if err := os.WriteFile(filepath.Join(p.downloadDir, "img-old.png"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed quoted image: %v", err)
	}

	raw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"image","msgData":{"image":{"mediaIds":["img-new"]}},"msgId":"gi1","reminder":{"isAtMe":true},"referenceMsg":{"from":"alice","senderName":"Alice","msgType":"image","msgData":{"image":{"mediaIds":["img-old"]}},"msgId":"m1"}}}]}`
	p.handleWSMessage([]byte(raw))

	select {
	case m := <-msgs:
		// New image is downloaded and injected; quoted image resolved locally.
		if len(m.UploadedFiles) != 1 || !strings.Contains(m.UploadedFiles[0].Path, "img-new") {
			t.Errorf("UploadedFiles = %+v, want the new image img-new", m.UploadedFiles)
		}
		if !strings.Contains(m.Content, "引用的图片") || !strings.Contains(m.Content, filepath.Join(p.downloadDir, "img-old.png")) {
			t.Errorf("content should reference the quoted image path, got %q", m.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for quoted image routing")
	}
}

// TestHandleWSMessage_QuotedImageDownloadedOnDemand verifies that a quoted
// image NOT already cached (e.g. sent earlier in the group by someone else,
// never passed through this bot) is fetched from the media API into
// downloadDir/.quoted/ so the agent can actually read it.
func TestHandleWSMessage_QuotedImageDownloadedOnDemand(t *testing.T) {
	p, _, msgs := newTestPlatformWithCapture(t, "bob")

	raw := `{"events":[{"type":"bot_group_message","data":{"from":"bob","chatType":"group","groupId":"group-456","msgType":"image","msgData":{"image":{"mediaIds":["img-new"]}},"msgId":"gi1","reminder":{"isAtMe":true},"referenceMsg":{"from":"alice","senderName":"Alice","msgType":"image","msgData":{"image":{"mediaIds":["img-ref"]}},"msgId":"m1"}}}]}`
	p.handleWSMessage([]byte(raw))

	select {
	case m := <-msgs:
		if !strings.Contains(m.Content, "引用的图片") || !strings.Contains(m.Content, filepath.Join(p.downloadDir, quotedMediaDir)) {
			t.Errorf("content should resolve the uncached quoted image into .quoted/, got %q", m.Content)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for quoted image routing")
	}

	matches, _ := filepath.Glob(filepath.Join(p.downloadDir, quotedMediaDir, "img-ref*"))
	if len(matches) != 1 {
		t.Fatalf(".quoted files = %v, want exactly the downloaded img-ref", matches)
	}
}

// --- milestone progress (core.ToolActivityListener) ---

func TestMilestoneLabel_Map(t *testing.T) {
	cases := []struct {
		tool string
		want string
	}{
		{"mcp__jira-qax__jira_search", "📋 正在查询 Jira…"},
		{"mcp__jira-qax__jira_get_issue", "📋 正在查询 Jira…"},
		{"mcp__wiki-qax__confluence_search", "📚 正在检索知识库文档…"},
		{"Bash", "💻 正在执行分析命令…"},
		{"Read", "🔎 正在检索资料…"},
		{"Grep", "🔎 正在检索资料…"},
		{"WebSearch", "🌐 正在联网检索…"},
		{"Write", "✍️ 正在整理分析结果…"},
		{"mcp__codebase-memory-mcp__query_graph", ""},
		{"TaskList", ""},
	}
	for _, c := range cases {
		got := milestoneLabel(core.ToolActivity{Starting: true, ToolName: c.tool})
		if got != c.want {
			t.Errorf("milestoneLabel(%q) = %q, want %q", c.tool, got, c.want)
		}
	}
	// Finish events never map to a start label.
	if got := milestoneLabel(core.ToolActivity{Starting: false, ToolName: "Bash"}); got != "" {
		t.Errorf("finish event label = %q, want empty", got)
	}
}

// waitForBotBodies polls until rec has at least n recorded bot (p2p) bodies.
func waitForBotBodies(t *testing.T, rec *reqRecorder, n int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if b := rec.botBodies(); len(b) >= n {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d bot bodies; have %d: %v", n, len(rec.botBodies()), rec.botBodies())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// enableMilestones turns on the milestone listener plumbing on a test platform.
func enableMilestones(p *Platform) {
	p.milestonesEnabled = true
	p.milestones = &milestoneTracker{state: map[string]*milestoneState{}}
}

// TestOnToolActivity_ThrottledLines verifies tool start/finish events become
// sparse milestone messages: the same stage inside the cooldown is dropped,
// a different stage is sent, and a failed tool finish yields a warning line.
func TestOnToolActivity_ThrottledLines(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	enableMilestones(p)
	rc := replyContext{chatType: "p2p", target: "staff-1"}
	ctx := context.Background()

	act := func(starting bool, tool, status string) {
		t.Helper()
		if err := p.OnToolActivity(ctx, rc, core.ToolActivity{Starting: starting, ToolName: tool, Status: status}); err != nil {
			t.Fatalf("OnToolActivity: %v", err)
		}
	}

	// Same jira stage twice inside the cooldown → only one line.
	act(true, "mcp__jira-qax__jira_search", "")
	act(true, "mcp__jira-qax__jira_get_issue", "")
	// A different coarse stage → a second line.
	act(true, "Bash", "")
	// A failed finish of an unlabelled/uncooldowned tool → warning line.
	act(false, "mcp__jira-qax__jira_search", "failed")

	bodies := waitForBotBodies(t, rec, 3, 2*time.Second)
	joined := strings.Join(bodies, "\n")
	// The start line ends with the stage text; the warning line has an extra
	// "… 未成功" suffix, so a quote-terminated match counts only start lines.
	if strings.Count(joined, `正在查询 Jira…"`) != 1 {
		t.Errorf("jira milestone start lines = %d, want 1 (dedup in cooldown)\n%v", strings.Count(joined, `正在查询 Jira…"`), joined)
	}
	if !strings.Contains(joined, "正在执行分析命令") {
		t.Errorf("expected Bash milestone line, got %v", joined)
	}
	if !strings.Contains(joined, "未成功") {
		t.Errorf("expected failed-tool warning line, got %v", joined)
	}
}

// TestOnToolActivity_DisabledSkips verifies milestones are off by default and
// nothing is sent when the platform option is not enabled.
func TestOnToolActivity_DisabledSkips(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	rc := replyContext{chatType: "p2p", target: "staff-1"}
	if err := p.OnToolActivity(context.Background(), rc, core.ToolActivity{Starting: true, ToolName: "mcp__jira-qax__jira_search"}); err != nil {
		t.Fatalf("OnToolActivity: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if got := rec.botBodies(); len(got) != 0 {
		t.Errorf("milestones should be disabled by default, got %d bodies: %v", len(got), got)
	}
}

// TestSendReplyText_TrimsBlankEdges verifies intermediate text is sent without
// stray leading/trailing blank lines (a common agent-stream artifact).
func TestSendReplyText_TrimsBlankEdges(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	rc := replyContext{chatType: "p2p", target: "staff-1"}
	if err := p.sendReplyText(rc, "\n结论\n\n\n   "); err != nil {
		t.Fatalf("sendReplyText: %v", err)
	}
	bodies := rec.botBodies()
	if len(bodies) != 1 {
		t.Fatalf("bot bodies = %d, want 1: %v", len(bodies), bodies)
	}
	m := decodeJSONBody(t, bodies[0])
	ft, ok := m["msgData"].(map[string]any)["formatText"].(map[string]any)
	if !ok {
		t.Fatalf("message should be formatText, got %v", bodies[0])
	}
	if got, _ := ft["text"].(string); got != "结论" {
		t.Errorf("text = %q, want 结论 (blank edges trimmed)", got)
	}
}

// --- progress card (dynamic appCard) style ---

func TestMilestoneCardLabel_Map(t *testing.T) {
	cases := []struct{ tool, want string }{
		{"mcp__jira-qax__jira_search", "正在查询 Jira…"},
		{"mcp__wiki-qax__confluence_search", "正在检索知识库…"},
		{"Bash", "正在执行分析…"},
		{"Read", "正在检索资料…"},
		{"TaskList", ""},
	}
	for _, c := range cases {
		if got := milestoneCardLabel(core.ToolActivity{ToolName: c.tool}); got != c.want {
			t.Errorf("milestoneCardLabel(%q) = %q, want %q", c.tool, got, c.want)
		}
		if len(c.want) > 30 {
			t.Errorf("card label %q exceeds the 30-byte status budget", c.want)
		}
	}
}

// waitForCond polls until cond is true or the timeout expires.
func waitForCond(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestProgressCard_LifecycleCreatesUpdatesFinalizes covers the card style end
// to end: first stage creates a dynamic appCard, a stage change updates its
// header status, and turn end finalises it (isLastUpdate) WITHOUT withdrawing
// it — a recall shows a visible hint to everyone in the chat.
func TestProgressCard_LifecycleCreatesUpdatesFinalizes(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	p.milestonesEnabled = true
	p.milestonesStyle = "card"
	p.milestones = &milestoneTracker{state: map[string]*milestoneState{}}
	rc := replyContext{chatType: "p2p", target: "staff-1"}
	ctx := context.Background()

	// 1) first stage → a dynamic appCard is created
	if err := p.OnToolActivity(ctx, rc, core.ToolActivity{Starting: true, ToolName: "mcp__jira-qax__jira_search"}); err != nil {
		t.Fatalf("OnToolActivity: %v", err)
	}
	waitForCond(t, 2*time.Second, "card create", func() bool { return len(rec.botBodies()) >= 1 })
	card := decodeJSONBody(t, rec.botBodies()[0])
	cardData, _ := card["msgData"].(map[string]any)["appCard"].(map[string]any)
	if isDyn, _ := cardData["isDynamic"].(bool); !isDyn {
		t.Errorf("card should be isDynamic, got %v", card["msgData"])
	}
	status, _ := cardData["headStatusInfo"].(map[string]any)
	statusDesc, _ := status["description"].(string)
	if !strings.Contains(statusDesc, "正在查询 Jira") {
		t.Errorf("card status = %v, want it to contain 正在查询 Jira", statusDesc)
	}

	// 2) stage change → header status update
	if err := p.OnToolActivity(ctx, rc, core.ToolActivity{Starting: true, ToolName: "Bash"}); err != nil {
		t.Fatalf("OnToolActivity: %v", err)
	}
	waitForCond(t, 2*time.Second, "card update", func() bool { return len(rec.cardUpdateBodies()) >= 1 })
	upd := decodeJSONBody(t, rec.cardUpdateBodies()[0])
	if upd["msgId"] != "m-card" {
		t.Errorf("update msgId = %v, want m-card", upd["msgId"])
	}
	updData, _ := upd["msgData"].(map[string]any)["appCardUpdateMsg"].(map[string]any)
	updStatus, _ := updData["headStatusInfo"].(map[string]any)
	updDesc, _ := updStatus["description"].(string)
	if !strings.Contains(updDesc, "正在执行分析") {
		t.Errorf("updated status = %v, want it to contain 正在执行分析", updDesc)
	}

	// 3) turn end → finalize, and never revoke
	if err := p.OnTurnEnd(ctx, rc, true); err != nil {
		t.Fatalf("OnTurnEnd: %v", err)
	}
	waitForCond(t, 2*time.Second, "card finalize", func() bool {
		ups := rec.cardUpdateBodies()
		if len(ups) < 2 {
			return false
		}
		last := decodeJSONBody(t, ups[len(ups)-1])
		d, _ := last["msgData"].(map[string]any)["appCardUpdateMsg"].(map[string]any)
		b, _ := d["isLastUpdate"].(bool)
		return b
	})
	// The card stays in the chat: withdrawing it would broadcast a recall hint.
	time.Sleep(120 * time.Millisecond)
	if got := rec.revokeBodies(); len(got) != 0 {
		t.Errorf("progress card must not be revoked, got %v", got)
	}
}

// TestOnTurnEnd_NoCardDoesNothing verifies a turn that never created a card
// (e.g. a short answer with no tool calls) sends no request at all.
func TestOnTurnEnd_NoCardDoesNothing(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	p.milestonesEnabled = true
	p.milestonesStyle = "card"
	p.milestones = &milestoneTracker{state: map[string]*milestoneState{}}
	rc := replyContext{chatType: "p2p", target: "staff-1"}
	if err := p.OnTurnEnd(context.Background(), rc, true); err != nil {
		t.Fatalf("OnTurnEnd: %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if got := rec.revokeBodies(); len(got) != 0 {
		t.Errorf("no card -> no revoke, got %v", got)
	}
}

// TestProgressCard_TurnStartCreatesCardImmediately verifies the card is created
// at turn start (as fast as the old instant reply) instead of waiting for the
// first tool call, and that a following tool event updates that same card
// rather than creating a second one.
func TestProgressCard_TurnStartCreatesCardImmediately(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	p.milestonesEnabled = true
	p.milestonesStyle = "card"
	p.milestones = &milestoneTracker{state: map[string]*milestoneState{}}
	rc := replyContext{chatType: "p2p", target: "staff-1"}
	ctx := context.Background()

	if err := p.OnTurnStart(ctx, rc); err != nil {
		t.Fatalf("OnTurnStart: %v", err)
	}
	waitForCond(t, 2*time.Second, "card create at turn start", func() bool { return len(rec.botBodies()) >= 1 })
	card := decodeJSONBody(t, rec.botBodies()[0])
	cardData, _ := card["msgData"].(map[string]any)["appCard"].(map[string]any)
	status, _ := cardData["headStatusInfo"].(map[string]any)
	statusDesc, _ := status["description"].(string)
	if !strings.Contains(statusDesc, progressCardStartingText) {
		t.Errorf("initial status = %v, want it to contain %v", statusDesc, progressCardStartingText)
	}

	// A tool event right after must update that card, not create another.
	if err := p.OnToolActivity(ctx, rc, core.ToolActivity{Starting: true, ToolName: "mcp__jira-qax__jira_search"}); err != nil {
		t.Fatalf("OnToolActivity: %v", err)
	}
	waitForCond(t, 2*time.Second, "card update", func() bool { return len(rec.cardUpdateBodies()) >= 1 })
	time.Sleep(120 * time.Millisecond)
	if got := rec.botBodies(); len(got) != 1 {
		t.Errorf("exactly one card should be created, got %d", len(got))
	}
}

// --- group callback extras (sender type / bot id / mention lists) ---

func TestParseWSMessage_GroupExtras(t *testing.T) {
	raw := `{"events":[{"type":"bot_group_message","data":{"from":"u1","fromType":"bot","botId":"bot-9","botCreator":"creator-1","groupId":"g1","msgType":"text","msgData":{"text":{"content":"@ICG hi"}},"msgId":"m1","reminder":{"isAtMe":true,"isAtAll":true,"staffs":[{"staffId":"u2"},{"openId":"u3"}],"bots":[{"botId":"bot-7"}]}}}]}`
	evs := parseWSMessage([]byte(raw))
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.SenderType != "bot" || ev.BotID != "bot-9" || ev.BotCreator != "creator-1" {
		t.Errorf("sender extras = %q/%q/%q, want bot/bot-9/creator-1", ev.SenderType, ev.BotID, ev.BotCreator)
	}
	if !ev.IsAtMe || !ev.IsAtAll {
		t.Errorf("isAtMe/isAtAll = %v/%v, want true/true", ev.IsAtMe, ev.IsAtAll)
	}
	if len(ev.AtUserIDs) != 2 || ev.AtUserIDs[0] != "u2" || ev.AtUserIDs[1] != "u3" {
		t.Errorf("AtUserIDs = %v, want [u2 u3] (from staffId/openId shapes)", ev.AtUserIDs)
	}
	if len(ev.AtBotIDs) != 1 || ev.AtBotIDs[0] != "bot-7" {
		t.Errorf("AtBotIDs = %v, want [bot-7]", ev.AtBotIDs)
	}
}

// TestParseWSMessage_GroupExtrasAbsent verifies older servers that omit the new
// fields keep the previous behaviour (zero values, no panic).
func TestParseWSMessage_GroupExtrasAbsent(t *testing.T) {
	raw := `{"events":[{"type":"bot_group_message","data":{"from":"u1","groupId":"g1","msgType":"text","msgData":{"text":{"content":"hi"}},"msgId":"m1","reminder":{"isAtMe":true}}}]}`
	evs := parseWSMessage([]byte(raw))
	if len(evs) != 1 {
		t.Fatalf("events = %d, want 1", len(evs))
	}
	ev := evs[0]
	if ev.SenderType != "" || ev.BotID != "" || ev.IsAtAll || len(ev.AtUserIDs) != 0 || len(ev.AtBotIDs) != 0 {
		t.Errorf("absent extras should stay zero-valued, got %+v", ev)
	}
	if !ev.IsAtMe {
		t.Error("isAtMe should still parse without the new fields")
	}
}

// TestHandleWSMessage_IgnoresBotSenderByDefault verifies messages sent by other
// bots are dropped unless accept_bot_messages opts in.
func TestHandleWSMessage_IgnoresBotSenderByDefault(t *testing.T) {
	p, _, msgs := newTestPlatformWithCapture(t, "")
	raw := `{"events":[{"type":"bot_group_message","data":{"from":"other-bot","fromType":"bot","botId":"bot-2","groupId":"g1","msgType":"text","msgData":{"text":{"content":"@ICG ping"}},"msgId":"m1","reminder":{"isAtMe":true}}}]}`
	p.handleWSMessage([]byte(raw))
	select {
	case m := <-msgs:
		t.Errorf("bot-sent message should be ignored by default, got %q", m.Content)
	case <-time.After(300 * time.Millisecond):
	}

	p.acceptBotMessages = true
	raw2 := strings.Replace(raw, `"msgId":"m1"`, `"msgId":"m2"`, 1)
	p.handleWSMessage([]byte(raw2))
	select {
	case <-msgs:
	case <-time.After(2 * time.Second):
		t.Error("accept_bot_messages=true should let bot-sent messages through")
	}
}

// --- reply quoting (refMsgId) ---

func TestSendReplyText_QuotesOriginalMessage(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	if !p.quoteReply {
		t.Fatal("quoteReply should default to true")
	}
	if err := p.sendReplyText(replyContext{chatType: "p2p", target: "u1", msgID: "orig-1"}, "结论"); err != nil {
		t.Fatalf("sendReplyText: %v", err)
	}
	body := decodeJSONBody(t, rec.botBodies()[0])
	if body["refMsgId"] != "orig-1" {
		t.Errorf("refMsgId = %v, want orig-1", body["refMsgId"])
	}
}

func TestSendReplyText_QuoteDisabledAndProgressStripped(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	rc := replyContext{chatType: "p2p", target: "u1", msgID: "orig-1"}

	// Progress lines never quote, even with quoting enabled.
	if err := p.sendReplyText(progressReplyContext(rc), "📋 正在查询 Jira…"); err != nil {
		t.Fatalf("sendReplyText(progress): %v", err)
	}
	if body := decodeJSONBody(t, rec.botBodies()[0]); body["refMsgId"] != nil {
		t.Errorf("progress line should not quote, got %v", body["refMsgId"])
	}

	// reply_quote=false turns quoting off globally.
	p.quoteReply = false
	if err := p.sendReplyText(rc, "结论"); err != nil {
		t.Fatalf("sendReplyText: %v", err)
	}
	if body := decodeJSONBody(t, rec.botBodies()[1]); body["refMsgId"] != nil {
		t.Errorf("reply_quote=false should drop refMsgId, got %v", body["refMsgId"])
	}
}

// TestHandleApproveCardCallback_NoopWithoutPayload guards the approval-card
// callback skeleton: it must accept events with and without a payload.
func TestHandleApproveCardCallback_NoopWithoutPayload(t *testing.T) {
	p, _ := newTestPlatformWithServer(t, "")
	p.handleApproveCardCallback(lanxinEvent{EventType: "approve_card_callback", From: "u1"})
	p.handleApproveCardCallback(lanxinEvent{EventType: "approve_card_callback", From: "u1", CallbackData: `{"action":"approve"}`})
}
