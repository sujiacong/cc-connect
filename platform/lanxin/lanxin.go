package lanxin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
)

const platformName = "lanxin"

const (
	tokenPath      = "/v1/apptoken/create"
	botMsgPath     = "/v1/bot/messages/create"
	groupMsgPath   = "/v1/messages/group/create"
	wsEndpointPath = "/v1/ws/endpoint/create"
)

const defaultAPIBase = "https://apigw.lx.qianxin.com"

// defaultDownloadDir is where received file/media messages are saved
// (configurable via file_download_dir platform option).
const defaultDownloadDir = "/root/icg-ai-bot/uploads"

// Platform implements core.Platform for the Lanxin personal bot via WebSocket
// long-connection (outbound, no public URL needed).
type Platform struct {
	appID                 string
	appSecret             string
	apiBase               string
	allowFrom             string
	downloadDir           string
	handler               core.MessageHandler
	dedup                 core.MessageDedup
	httpClient            *http.Client
	tokenMu               sync.Mutex
	appToken              string
	tokenExpiry           time.Time
	ctx                   context.Context
	cancel                context.CancelFunc
	connMu                sync.Mutex
	conn                  *websocket.Conn
	shareSessionInChannel bool
}

// replyContext holds the context needed to reply to a specific message or to
// reconstruct a reply target from a session key (cron/proactive scenarios).
type replyContext struct {
	chatType   string // "p2p" or "group"
	target     string // staffId for p2p, groupId for group
	senderName string
	msgID      string
	proactive  bool
}

func init() {
	core.RegisterPlatform(platformName, New)
}

// New creates a Lanxin platform from config options.
func New(opts map[string]any) (core.Platform, error) {
	appID, _ := opts["app_id"].(string)
	appSecret, _ := opts["app_secret"].(string)
	if appID == "" || appSecret == "" {
		return nil, fmt.Errorf("lanxin: app_id and app_secret are required")
	}
	apiBase, _ := opts["api_base"].(string)
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	allowFrom, _ := opts["allow_from"].(string)
	core.CheckAllowFrom(platformName, allowFrom)

	downloadDir, _ := opts["file_download_dir"].(string)
	if downloadDir == "" {
		downloadDir = defaultDownloadDir
	}

	shareSession, _ := opts["share_session_in_channel"].(bool)

	slog.Warn("lanxin: personal bot mode — private chat only receives creator messages")

	return &Platform{
		appID:                 appID,
		appSecret:             appSecret,
		apiBase:               apiBase,
		allowFrom:             allowFrom,
		downloadDir:           downloadDir,
		httpClient:            &http.Client{Timeout: 30 * time.Second},
		shareSessionInChannel: shareSession,
	}, nil
}

// Name returns the platform identifier.
func (p *Platform) Name() string { return platformName }

// Start launches the WebSocket connection loop.
func (p *Platform) Start(handler core.MessageHandler) error {
	p.handler = handler
	p.dedup = core.MessageDedup{}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	go p.connectLoop()
	return nil
}

// Reply sends a message back to the originating chat.
func (p *Platform) Reply(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("lanxin: invalid reply context type %T", rctx)
	}
	if content == "" {
		return nil
	}

	var err error
	switch rc.chatType {
	case "p2p":
		err = p.sendBotText(rc.target, content)
	case "group":
		err = p.sendGroupText(rc.target, content)
	default:
		return fmt.Errorf("lanxin: unknown chatType %q", rc.chatType)
	}

	if err != nil {
		return fmt.Errorf("lanxin: reply %s: %w", rc.chatType, err)
	}
	slog.Debug("lanxin: reply sent", "chatType", rc.chatType, "target", rc.target, "len", len(content))
	return nil
}

// Send sends a proactive message. For lanxin this is identical to Reply since
// both go through the HTTP API (not the WebSocket). The replyCtx for Send is
// reconstructed by ReconstructReplyCtx.
func (p *Platform) Send(ctx context.Context, rctx any, content string) error {
	return p.Reply(ctx, rctx, content)
}

// ReconstructReplyCtx rebuilds a reply context from a session key.
// Session key formats:
//   - Private: "lanxin:p2p:<from>"
//   - Group shared: "lanxin:group:<groupId>"
//   - Group user-isolated: "lanxin:group:<groupId>:<from>"
//
// For group, target is always the groupId (sending only needs groupId).
// Aligned with wecom/websocket.go:540-547 and dingtalk/dingtalk.go:1599-1635.
func (p *Platform) ReconstructReplyCtx(sessionKey string) (any, error) {
	if !strings.HasPrefix(sessionKey, platformName+":") {
		return nil, fmt.Errorf("lanxin: not a lanxin session key: %q", sessionKey)
	}

	stripped := strings.TrimPrefix(sessionKey, platformName+":")
	parts := strings.SplitN(stripped, ":", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("lanxin: invalid session key format: %q", sessionKey)
	}

	chatType := parts[0]
	if chatType != "p2p" && chatType != "group" {
		return nil, fmt.Errorf("lanxin: invalid chatType %q in session key: %q", chatType, sessionKey)
	}

	target := parts[1]
	if target == "" {
		return nil, fmt.Errorf("lanxin: empty target in session key: %q", sessionKey)
	}

	return replyContext{
		chatType:  chatType,
		target:    target,
		proactive: true,
	}, nil
}

// Stop cancels the context and closes the active WebSocket connection.
func (p *Platform) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	p.connMu.Lock()
	conn := p.conn
	p.conn = nil
	p.connMu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	return nil
}
