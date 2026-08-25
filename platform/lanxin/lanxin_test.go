package lanxin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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
	err := p.sendBotText("user-123", "hello")
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
	err := p.sendBotText("user-123", "hello")
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
	err := p.sendGroupText("group-456", "hello group")
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

// reqRecorder captures request paths and the last group-message POST body
// served by the httptest server. Thread-safe because handleFileMessage runs
// in a goroutine and may hit the server concurrently with the test.
type reqRecorder struct {
	mu    sync.Mutex
	paths []string
	group string
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

// newTestPlatformWithServer wires a Platform to an httptest server that
// answers token/bot/group/media endpoints and records what was requested.
func newTestPlatformWithServer(t *testing.T, allowFrom string) (*Platform, *reqRecorder) {
	t.Helper()
	rec := &reqRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.add(r.URL.Path)
		switch r.URL.Path {
		case tokenPath:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{"appToken":"tok","expiresIn":7200}}`))
		case botMsgPath:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{}}`))
		case groupMsgPath:
			b, _ := io.ReadAll(r.Body)
			rec.mu.Lock()
			rec.group = string(b)
			rec.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{}}`))
		default:
			if strings.HasPrefix(r.URL.Path, "/v1/medias/") {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Write([]byte("file-content"))
				return
			}
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"errCode":404,"errMsg":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)

	p := &Platform{
		appID:       "test-app",
		appSecret:   "test-secret",
		apiBase:     srv.URL,
		allowFrom:   allowFrom,
		httpClient:  srv.Client(),
		downloadDir: t.TempDir(),
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
