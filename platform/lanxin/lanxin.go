package lanxin

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
	uploadMarkerTTL       time.Duration // validity of per-session upload markers in <downloadDir>/.sessions/
	// markerMu serializes read-modify-write of session upload markers
	// (appendSessionUploads / sessionUploadedFiles / expireUploadMarkerIfStale)
	// so concurrent uploads to the same session cannot lose updates.
	markerMu sync.Mutex
	// uploadMu serializes the in-flight upload tracker map (inflightUploads):
	// beginSessionUpload / finishSessionUpload / waitSessionUploads. Distinct
	// from markerMu, which guards the on-disk session markers — a text command
	// first waits on the in-flight tracker (uploadMu) and only then reads the
	// marker (markerMu).
	uploadMu        sync.Mutex
	inflightUploads map[string]*uploadInflight
	// fileSendAllowDirs is the symlink-resolved allow-list of directories
	// outgoing {{file:...}} files may come from (downloadDir always included).
	// Checked by replyWithFiles before any upload so a prompt injection cannot
	// exfiltrate arbitrary host files into the chat.
	fileSendAllowDirs []string
	// milestonesEnabled toggles coarse progress feedback while the agent works
	// (core.ToolActivityListener); milestones holds per-chat throttle/card
	// state. milestonesStyle is "text" (sparse stage lines) or "card" (one
	// dynamic appCard withdrawn at turn end).
	milestonesEnabled bool
	milestonesStyle   string
	milestones        *milestoneTracker
	// botSenderID is this bot's id inside a group, required by the message
	// revoke API for group chats (bot_sender_id platform option).
	botSenderID string
	// acceptBotMessages opts in to processing messages sent by other bots
	// (default false: such messages are ignored so two bots cannot ping-pong).
	acceptBotMessages bool
	// quoteReply makes outgoing replies quote the message being answered
	// (refMsgId); enabled by default, disable with reply_quote = false.
	quoteReply bool
}

// quoteID returns the message id a reply should quote, or "" when quoting is
// disabled (reply_quote = false) or the reply context carries no message id.
// Progress helpers deliberately pass "" instead so only real answers (and
// receipts) carry the quoted original.
func (p *Platform) quoteID(rc replyContext) string {
	if !p.quoteReply {
		return ""
	}
	return rc.msgID
}

// replyContext holds the context needed to reply to a specific message or to
// reconstruct a reply target from a session key (cron/proactive scenarios).
type replyContext struct {
	chatType   string // "p2p" or "group"
	target     string // staffId for p2p, groupId for group
	senderName string
	msgID      string
	proactive  bool
	fromUserID string // sender openId; used to @-mention in group replies
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

	// file_send_allow_dirs: extra directories the agent may deliver files from
	// via {{file:...}} reply markers. The download dir is always allowed; an
	// empty option (the default) restricts outgoing files to it, so a prompt
	// injection cannot make cc-connect upload arbitrary host files (e.g.
	// /etc/shadow or ~/.ssh keys) into the chat.
	allowDirs := parseAllowDirs(opts["file_send_allow_dirs"], downloadDir)

	// upload_marker_ttl_minutes: how long per-session upload markers stay valid
	// (default 60). Each session's marker lives at <downloadDir>/.sessions/<sessionKey>.json
	// and is swept by the background goroutine once stale, so a previous-session
	// upload can never be reported as a current-session one. Explicit 0 (or
	// negative) disables the expiry sweep (markers live until the next upload).
	const markerTTLUnset = -1
	uploadMarkerTTL := defaultUploadMarkerTTL
	switch ttlMin := optionInt(opts["upload_marker_ttl_minutes"], markerTTLUnset); {
	case ttlMin == markerTTLUnset:
		// option absent → default
	case ttlMin > 0:
		uploadMarkerTTL = time.Duration(ttlMin) * time.Minute
	default:
		// explicit 0/negative → disable expiry sweep
		uploadMarkerTTL = 0
	}

	slog.Warn("lanxin: personal bot mode — private chat only receives creator messages")

	milestones, _ := opts["milestones"].(bool)
	milestonesStyle, _ := opts["milestones_style"].(string)
	if milestonesStyle == "" {
		milestonesStyle = "text"
	}
	botSenderID, _ := opts["bot_sender_id"].(string)
	acceptBotMessages, _ := opts["accept_bot_messages"].(bool)
	// reply_quote defaults to true (unset → enabled)
	quoteReply := true
	if v, ok := opts["reply_quote"].(bool); ok {
		quoteReply = v
	}

	return &Platform{
		appID:                 appID,
		appSecret:             appSecret,
		apiBase:               apiBase,
		allowFrom:             allowFrom,
		downloadDir:           downloadDir,
		httpClient:            &http.Client{Timeout: 30 * time.Second},
		shareSessionInChannel: shareSession,
		uploadMarkerTTL:       uploadMarkerTTL,
		fileSendAllowDirs:     allowDirs,
		milestonesEnabled:     milestones,
		milestonesStyle:       milestonesStyle,
		milestones:            &milestoneTracker{state: map[string]*milestoneState{}},
		botSenderID:           botSenderID,
		acceptBotMessages:     acceptBotMessages,
		quoteReply:            quoteReply,
	}, nil
}

// parseAllowDirs builds the normalized allow-list of directories outgoing
// {{file:...}} files may come from: the (symlink-resolved) download dir plus
// every (symlink-resolved) entry of the comma-separated option. Bad entries
// (unresolvable symlinks) are skipped with a warning — they cannot match any
// real file path, so dropping them keeps the allow-list honest.
func parseAllowDirs(raw any, downloadDir string) []string {
	dirs := make([]string, 0, 4)
	add := func(dir string) {
		if dir == "" {
			return
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			slog.Warn("lanxin: file_send_allow_dirs entry not absolute-able, skipped", "dir", dir, "error", err)
			return
		}
		resolved, err := filepath.EvalSymlinks(abs)
		if err != nil {
			// Non-existent dirs are common during setup; resolve the deepest
			// existing ancestor so the dir still matches once created.
			resolved = resolveDeepestExisting(abs)
			if resolved == "" {
				slog.Warn("lanxin: file_send_allow_dirs entry unresolvable, skipped", "dir", dir, "error", err)
				return
			}
		}
		dirs = append(dirs, resolved)
	}
	add(downloadDir)
	s, _ := raw.(string)
	for _, dir := range strings.Split(s, ",") {
		add(strings.TrimSpace(dir))
	}
	return dirs
}

// resolveDeepestExisting resolves the deepest existing ancestor of dir and
// appends the remaining relative segments, so allow-lists keep working across
// a "configure first, create later" setup without weakening the boundary (the
// symlink resolution still happens on a real directory).
func resolveDeepestExisting(dir string) string {
	tail := []string{}
	cur := dir
	for cur != "/" {
		if _, err := os.Stat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return ""
			}
			return filepath.Join(append([]string{resolved}, tail...)...)
		}
		tail = append([]string{filepath.Base(cur)}, tail...)
		cur = filepath.Dir(cur)
	}
	return ""
}

// optionInt returns v as an int when it is a numeric config value, else def.
// TOML decoders may hand us int, int64 or float64 depending on the table
// layout, so all three are accepted (aligned with pickInt in other platforms).
func optionInt(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	default:
		return def
	}
}

// Name returns the platform identifier.
func (p *Platform) Name() string { return platformName }

// Start launches the WebSocket connection loop.
func (p *Platform) Start(handler core.MessageHandler) error {
	p.handler = handler
	p.dedup = core.MessageDedup{}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	go p.connectLoop()
	if p.uploadMarkerTTL > 0 {
		go p.watchUploadMarker()
	}
	if p.milestonesEnabled {
		go p.watchMilestones()
	}
	return nil
}

// watchUploadMarker periodically expires stale upload markers so a file
// uploaded in a previous session is never reported as a current-session one.
// Stops when the platform context is cancelled.
func (p *Platform) watchUploadMarker() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.expireUploadMarkerIfStale()
		}
	}
}

// Reply sends a message back to the originating chat. {{file:...}} markers in
// the content are handled by replyWithFiles: markers are stripped from the
// text and each referenced file is uploaded and sent as a file message.
func (p *Platform) Reply(ctx context.Context, rctx any, content string) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("lanxin: invalid reply context type %T", rctx)
	}
	if content == "" {
		return nil
	}
	return p.replyWithFiles(rc, content)
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
