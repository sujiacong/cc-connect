package lanxin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
	"github.com/gorilla/websocket"
)

const (
	wsPingInterval = 30 * time.Second
	wsPongTimeout  = 10 * time.Second
)

// reconnectBackoff is the fixed backoff sequence (aligned with ws_client.py:42).
var reconnectBackoff = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	10 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

// connectLoop establishes the WS connection and reconnects on failure with
// a fixed backoff sequence [2,5,10,30,60]s (aligned with lanxin-bot ws_client.py:42).
// Each reconnect re-fetches the WS endpoint (address may change).
func (p *Platform) connectLoop() {
	backoffIdx := 0
	for {
		select {
		case <-p.ctx.Done():
			return
		default:
		}

		start := time.Now()
		uri, err := p.getWSEndpoint()
		if err != nil {
			slog.Warn("lanxin: get WS endpoint failed", "error", err)
		} else {
			err = p.runConnection(uri)
		}
		if p.ctx.Err() != nil {
			return // shutting down
		}

		// Reset backoff if connection was alive for a meaningful period
		if time.Since(start) > 2*wsPingInterval {
			backoffIdx = 0
		}

		backoff := reconnectBackoff[backoffIdx%len(reconnectBackoff)]
		slog.Warn("lanxin: connection lost, reconnecting", "error", err, "backoff", backoff)
		select {
		case <-time.After(backoff):
		case <-p.ctx.Done():
			return
		}
		backoffIdx++
	}
}

// runConnection dials, sets up heartbeat, and processes messages until disconnection.
// Aligned with wecom/websocket.go:188-282 but without subscribe/ack (lanxin has none).
func (p *Platform) runConnection(uri string) error {
	slog.Info("lanxin: connecting", "endpoint", uri)

	conn, _, err := websocket.DefaultDialer.DialContext(p.ctx, uri, nil)
	if err != nil {
		return fmt.Errorf("lanxin: dial: %w", err)
	}

	p.connMu.Lock()
	p.conn = conn
	p.connMu.Unlock()

	defer func() {
		p.connMu.Lock()
		p.conn = nil
		p.connMu.Unlock()
		conn.Close()
	}()

	// Set initial read deadline (ping interval + pong timeout)
	conn.SetReadDeadline(time.Now().Add(wsPingInterval + wsPongTimeout))
	// Reset read deadline on each pong
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(wsPingInterval + wsPongTimeout))
		return nil
	})

	// Start heartbeat goroutine
	heartCtx, heartCancel := context.WithCancel(p.ctx)
	defer heartCancel()
	go p.heartbeat(heartCtx, conn)

	slog.Info("lanxin: connected")
	// Read loop
	for {
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		default:
		}

		_, raw, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("lanxin: read: %w", err)
		}

		p.handleWSMessage(raw)
	}
}

// heartbeat sends periodic WebSocket ping frames.
// If a ping write fails, the goroutine exits; the read loop's ReadDeadline
// will time out (no pong received) and trigger reconnect.
// Aligned with wecom/websocket.go:329-358 but uses native PingMessage.
func (p *Platform) heartbeat(ctx context.Context, conn *websocket.Conn) {
	ticker := time.NewTicker(wsPingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			deadline := time.Now().Add(wsPongTimeout)
			if err := conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
				slog.Warn("lanxin: ping failed", "error", err)
				return
			}
			slog.Debug("lanxin: ping sent")
		}
	}
}

// lanxinEvent is the parsed representation of a single WS event.
type lanxinEvent struct {
	EventType    string
	From         string
	ChatType     string // "p2p" or "group"
	GroupID      string
	MsgType      string
	Content      string
	MsgID        string
	IsAtMe       bool
	SenderName   string
	CallbackData string         // for approve_card_callback
	MediaIDs     []string       // for file/image/video/voice messages
	Quoted       *quotedMessage // referenceMsg: the message this one quotes, if any

	// Group-callback extras. Lanxin's group push now carries the sender kind,
	// the bot identity and the full mention lists; all of these are parsed
	// tolerantly — older servers simply omit them and the zero values keep the
	// previous behaviour intact.
	SenderType string   // fromType: "user" or "bot"
	BotID      string   // botId of the sending bot (when SenderType == "bot")
	BotCreator string   // botCreator: creator of that bot
	IsAtAll    bool     // reminder.isAtAll (@全体成员)
	AtUserIDs  []string // reminder.staffs: mentioned users
	AtBotIDs   []string // reminder.bots: mentioned bots
}

// extractIDs pulls string identifiers out of a lanxin mention list, whose
// entries are either plain id strings or objects carrying the id under one of
// a few known keys (staffId/openId/id/botId). Unknown shapes are skipped.
func extractIDs(raw any) []string {
	list, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, item := range list {
		switch v := item.(type) {
		case string:
			if v != "" {
				out = append(out, v)
			}
		case map[string]any:
			for _, k := range []string{"staffId", "openId", "id", "botId"} {
				if s, ok := v[k].(string); ok && s != "" {
					out = append(out, s)
					break
				}
			}
		}
	}
	return out
}

// parseWSMessage parses raw WS JSON, returning a list of events.
// Compatible with both {events:[...]} and {type,data} outer structures
// (aligned with ws_client.py:45-115).
func parseWSMessage(raw []byte) []lanxinEvent {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		slog.Warn("lanxin: invalid JSON in WS message", "error", err, "raw", truncateForLog(raw))
		return nil
	}

	var events []map[string]any
	if e, ok := payload["events"].([]any); ok {
		for _, item := range e {
			if m, ok := item.(map[string]any); ok {
				events = append(events, m)
			}
		}
	} else if _, ok := payload["type"].(string); ok {
		events = []map[string]any{payload}
	} else {
		slog.Debug("lanxin: unrecognized WS message structure", "raw", truncateForLog(raw))
		return nil
	}

	var result []lanxinEvent
	for _, ev := range events {
		etype, _ := ev["type"].(string)
		data, _ := ev["data"].(map[string]any)
		if data == nil {
			data = map[string]any{}
		}

		if etype == "approve_card_callback" {
			result = append(result, lanxinEvent{
				EventType:    etype,
				From:         getString(data, "staffId"),
				CallbackData: getString(data, "eventData"),
			})
			continue
		}

		if etype != "bot_private_message" && etype != "bot_group_message" {
			continue
		}

		// Lanxin distinguishes group vs private via the event TYPE (bot_group_message),
		// not a chatType field — the payload has groupId but no chatType.
		chatType := "p2p"
		if etype == "bot_group_message" {
			chatType = "group"
		}

		var isAtMe, isAtAll bool
		var atUserIDs, atBotIDs []string
		if reminder, ok := data["reminder"].(map[string]any); ok {
			isAtMe, _ = reminder["isAtMe"].(bool)
			isAtAll, _ = reminder["isAtAll"].(bool)
			atUserIDs = extractIDs(reminder["staffs"])
			atBotIDs = extractIDs(reminder["bots"])
		}

		// extract text content msgData.text.content
		content := ""
		var mediaIDs []string
		msgType := getString(data, "msgType")
		if msgData, ok := data["msgData"].(map[string]any); ok {
			if text, ok := msgData["text"].(map[string]any); ok {
				content, _ = text["content"].(string)
			}
			// Extract media ids from the carrier key matching the msgType:
			// msgData.file / msgData.image / msgData.video / msgData.voice
			// (aligned with the official lansenger channel plugin). Only
			// "file" was read before, so image messages were silently
			// downgraded to empty text. A media carrier may also carry a text
			// caption (msgData.<kind>.content), which becomes the message
			// content the agent sees.
			// Only file/image are ingested: voice/video carriers are parsed for
			// msgType recognition but carry no ids here, so such messages fall
			// through to the no-content skip below instead of being downloaded.
			switch msgType {
			case "file", "image":
				if carrier, ok := msgData[msgType].(map[string]any); ok {
					if ids, ok := carrier["mediaIds"].([]any); ok {
						for _, id := range ids {
							if s, ok := id.(string); ok && s != "" {
								mediaIDs = append(mediaIDs, s)
							}
						}
					}
					if caption, ok := carrier["content"].(string); ok && content == "" {
						content = strings.TrimSpace(caption)
					}
				}
			}
		}

		result = append(result, lanxinEvent{
			EventType:  etype,
			From:       getString(data, "from"),
			ChatType:   chatType,
			GroupID:    getString(data, "groupId"),
			MsgType:    getString(data, "msgType"),
			Content:    content,
			MsgID:      getString(data, "msgId"),
			IsAtMe:     isAtMe,
			SenderName: getString(data, "senderName"),
			MediaIDs:   mediaIDs,
			Quoted:     parseQuotedMessage(data["referenceMsg"]),
			SenderType: getString(data, "fromType"),
			BotID:      getString(data, "botId"),
			BotCreator: getString(data, "botCreator"),
			IsAtAll:    isAtAll,
			AtUserIDs:  atUserIDs,
			AtBotIDs:   atBotIDs,
		})
	}
	return result
}

// getString returns the string value of m[key], or "" if missing/not a string.
func getString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

// truncateForLog returns a preview of raw for logging (max 200 bytes).
func truncateForLog(raw []byte) string {
	if len(raw) > 200 {
		return string(raw[:200])
	}
	return string(raw)
}

// handleWSMessage dispatches parsed events to the handler.
func (p *Platform) handleWSMessage(raw []byte) {
	events := parseWSMessage(raw)
	// TEMP-DEBUG: log raw event fields for group-vs-private diagnosis
	for _, ev := range events {
		c := ev.Content
		if len(c) > 40 {
			c = c[:40]
		}
		slog.Info("lanxin: ws event", "type", ev.EventType, "chatType", ev.ChatType, "groupId", ev.GroupID, "from", ev.From, "msgType", ev.MsgType, "content", c, "content_len", len(ev.Content),
			"senderType", ev.SenderType, "botId", ev.BotID, "isAtMe", ev.IsAtMe, "isAtAll", ev.IsAtAll,
			"atUsers", len(ev.AtUserIDs), "atBots", len(ev.AtBotIDs))
	}
	for _, ev := range events {
		switch ev.EventType {
		case "approve_card_callback":
			p.handleApproveCardCallback(ev)
			continue
		case "bot_private_message", "bot_group_message":
			// proceed
		default:
			slog.Debug("lanxin: unknown event type", "type", ev.EventType)
			continue
		}

		// Messages sent by another bot are ignored by default so two bots in
		// the same group cannot ping-pong each other; set accept_bot_messages
		// to opt in. Older servers omit fromType, in which case this is a no-op.
		if ev.SenderType == "bot" && !p.acceptBotMessages {
			slog.Debug("lanxin: message from a bot, skipping", "bot_id", ev.BotID, "bot_creator", ev.BotCreator)
			continue
		}

		// group @bot filter (aligned with ws_client.py:174-176). isAtAll means
		// the whole group was mentioned — the bot is included, so it still counts
		// as being addressed (kept for backwards compatibility with servers that
		// only send isAtMe).
		if ev.ChatType == "group" && !ev.IsAtMe {
			slog.Debug("lanxin: group message not @bot, skipping", "group", ev.GroupID, "isAtAll", ev.IsAtAll)
			continue
		}

		// msgId dedup
		if p.dedup.IsDuplicate(ev.MsgID) {
			slog.Debug("lanxin: skipping duplicate message", "msg_id", ev.MsgID)
			continue
		}

		// allow_from permission — checked before any media download so
		// unauthorized senders cannot trigger ingestion or leak local paths
		if !core.AllowList(p.allowFrom, ev.From) {
			slog.Debug("lanxin: message from unauthorized user", "user", ev.From)
			rc := replyContext{chatType: ev.ChatType, target: replyTarget(ev), senderName: ev.SenderName, fromUserID: ev.From}
			if err := p.Reply(context.Background(), rc, core.UnauthorizedAccessMessage); err != nil {
				slog.Warn("lanxin: unauthorized reply failed", "error", err)
			}
			continue
		}

		// File/media messages: download asynchronously (do not block the WS
		// read loop, whose 40s read deadline would otherwise expire mid-download),
		// then reply with saved paths; never route the empty text to the agent.
		// arrivedAt is taken HERE — in WS read order — so the session marker's
		// upload timeline reflects the order the user sent the files in, even
		// when concurrent downloads complete out of order.
		if len(ev.MediaIDs) > 0 {
			slog.Info("lanxin: media message received", "user", ev.From, "chatType", ev.ChatType, "msgType", ev.MsgType, "media_ids", ev.MediaIDs, "has_caption", ev.Content != "")
			arrivedAt := time.Now().Format(time.RFC3339Nano)
			// Media with a text caption or a quoted reference (e.g. an image
			// captioned "看下这个报错", or quoting an earlier picture) goes to the
			// agent right away with the files injected; caption-less media is
			// saved and acknowledged, staying available to the session's later
			// commands (bot-log-analysis intake style).
			if (ev.Content != "" || ev.Quoted != nil) && (ev.MsgType == "image" || ev.MsgType == "file") {
				go p.handleMediaWithCaption(ev, arrivedAt)
			} else {
				go p.handleFileMessage(ev, arrivedAt)
			}
			continue
		}

		// build session key and replyContext
		target := replyTarget(ev)
		sessionKey := p.sessionKeyForEvent(ev)

		chatName := ""
		if ev.ChatType == "group" {
			chatName = ev.GroupID
		}

		content := ev.Content
		if ev.ChatType == "group" {
			// Lanxin group messages carry a "@botname " mention prefix (e.g.
			// "@ICG智能助手 /help"). Strip it so slash commands are recognized.
			content = stripMentionPrefix(content)
		}
		// If the user quoted an earlier message (referenceMsg), prepend its
		// context so the agent understands what "这张图/刚才那条/这个报错" refers to.
		content = p.attachReference(ev, content)

		// Non-text message kinds without any ingestible payload (stickers,
		// positions, cards, ...) have neither content nor mediaIds; skip them
		// instead of routing a pointless empty message to the agent.
		if content == "" && (ev.MsgType == "" || (ev.MsgType != "text" && ev.MsgType != "formatText")) {
			slog.Debug("lanxin: message kind without content, skipping", "type", ev.MsgType)
			continue
		}

		msg := &core.Message{
			SessionKey: sessionKey,
			Platform:   platformName,
			MessageID:  ev.MsgID,
			UserID:     ev.From,
			UserName:   ev.SenderName,
			ChatName:   chatName,
			Content:    content,
			// UploadedFiles is populated inside the handler goroutine below,
			// AFTER waiting for any same-session media download that is still
			// in flight, so a text command sent immediately after a large file
			// upload still carries that file.
			ReplyCtx: replyContext{
				chatType:   ev.ChatType,
				target:     target,
				senderName: ev.SenderName,
				msgID:      ev.MsgID,
				proactive:  false,
				fromUserID: ev.From,
			},
		}

		slog.Debug("lanxin: text received", "user", ev.From, "chatType", ev.ChatType, "len", len(ev.Content))
		go func() {
			// A file message sent moments ago may still be downloading (large
			// files take seconds; the download itself has a 60s timeout). Wait
			// for it before reading the session marker. Runs in this worker
			// goroutine so the WS read loop is never blocked. On timeout the
			// message proceeds with the current marker state — the download
			// keeps running and its marker write stays visible to later turns.
			p.waitSessionUploads(sessionKey, uploadWaitTimeout)
			// Inject this session's full upload timeline (if any) so the agent
			// analyzes exactly the files the current session uploaded, without
			// scanning uploads/ history or another session's files.
			msg.UploadedFiles = p.sessionUploadedFiles(sessionKey)
			p.handler(p, msg)
		}()
	}
}

// replyTarget returns the destination for a reply to this event: the groupId
// for group chat, or the sender's staffId for private chat.
func replyTarget(ev lanxinEvent) string {
	if ev.ChatType == "group" {
		return ev.GroupID
	}
	return ev.From
}

// sessionKeyForEvent builds the same session key used for text-message routing
// (see handleWSMessage) so file uploads are attributed to the session whose
// commands they belong to:
//
//	lanxin:p2p:<from>                  — private chat
//	lanxin:group:<groupId>             — group with share_session_in_channel
//	lanxin:group:<groupId>:<from>      — group with per-user sessions
func (p *Platform) sessionKeyForEvent(ev lanxinEvent) string {
	if ev.ChatType == "group" && !p.shareSessionInChannel {
		return fmt.Sprintf("lanxin:%s:%s:%s", ev.ChatType, ev.GroupID, ev.From)
	}
	return fmt.Sprintf("lanxin:%s:%s", ev.ChatType, replyTarget(ev))
}

// downloadSessionMedia downloads every media id of ev and appends each saved
// file to the sender session's per-session upload marker (cumulative: earlier
// uploads of the session are kept). Returns the saved local paths. Each
// saveMedia appends its own record, so a multi-file message contributes all of
// its files. arrivedAt is the message's WS-arrival timestamp (RFC3339Nano): it
// timestamps every file of the batch so the marker timeline matches the user's
// sending order. Runs in a worker goroutine (never the WS read loop).
func (p *Platform) downloadSessionMedia(ev lanxinEvent, arrivedAt string) []string {
	sessionKey := p.sessionKeyForEvent(ev)
	// Register this download as in-flight so a text command sent right after
	// the media message waits for the download (and its session-marker write)
	// before reading UploadedFiles. finish runs via defer AFTER saveMedia, so
	// the marker is always written before waiters are released.
	uf := p.beginSessionUpload(sessionKey)
	defer p.finishSessionUpload(sessionKey, uf)
	var saved []string
	for _, mid := range ev.MediaIDs {
		path, err := p.saveMedia(mid, sessionKey, arrivedAt)
		if err != nil {
			slog.Warn("lanxin: save media failed", "media_id", mid, "error", err)
			continue
		}
		saved = append(saved, path)
	}
	return saved
}

// handleFileMessage downloads all attached media (caption-less intake path),
// appends them to the session marker, and replies with the saved local paths
// (bot-log-analysis style log-package intake).
func (p *Platform) handleFileMessage(ev lanxinEvent, arrivedAt string) {
	saved := p.downloadSessionMedia(ev, arrivedAt)

	kindLabel := "文件"
	if ev.MsgType == "image" {
		kindLabel = "图片"
	}
	reply := "已收到" + kindLabel
	if len(saved) > 0 {
		reply += "，保存到：\n" + strings.Join(saved, "\n")
	} else {
		reply += "，但下载失败，请重试"
	}

	rc := replyContext{chatType: ev.ChatType, target: replyTarget(ev), senderName: ev.SenderName, fromUserID: ev.From}
	if err := p.Reply(context.Background(), rc, reply); err != nil {
		slog.Warn("lanxin: media reply failed", "error", err)
	}
}

// handleMediaWithCaption downloads media that carries a text caption (e.g. an
// image captioned "看下这个报错"), then routes the caption to the agent with the
// downloaded files injected as UploadedFiles — so the bot can answer about the
// media immediately, without needing a follow-up command. When nothing could
// be saved (or the caption is only a bare group mention) it falls back to the
// plain ack path, which reports the download failure.
func (p *Platform) handleMediaWithCaption(ev lanxinEvent, arrivedAt string) {
	content := ev.Content
	if ev.ChatType == "group" {
		content = stripMentionPrefix(content)
	}
	// If the user quoted an earlier message (referenceMsg), prepend its
	// context — e.g. quoting a previously sent picture while sending a new one.
	content = p.attachReference(ev, content)
	if content == "" {
		p.handleFileMessage(ev, arrivedAt)
		return
	}
	saved := p.downloadSessionMedia(ev, arrivedAt)
	if len(saved) == 0 {
		p.handleFileMessage(ev, arrivedAt) // acks the failure
		return
	}

	sessionKey := p.sessionKeyForEvent(ev)
	chatName := ""
	if ev.ChatType == "group" {
		chatName = ev.GroupID
	}
	msg := &core.Message{
		SessionKey: sessionKey,
		Platform:   platformName,
		MessageID:  ev.MsgID,
		UserID:     ev.From,
		UserName:   ev.SenderName,
		ChatName:   chatName,
		Content:    content,
		// Downloads finished synchronously above, so the session marker
		// already holds this batch — inject it so the agent sees the files.
		UploadedFiles: p.sessionUploadedFiles(sessionKey),
		ReplyCtx: replyContext{
			chatType:   ev.ChatType,
			target:     replyTarget(ev),
			senderName: ev.SenderName,
			msgID:      ev.MsgID,
			proactive:  false,
			fromUserID: ev.From,
		},
	}
	p.handler(p, msg)
}

// stripMentionPrefix removes a leading "@botname " mention segment from a
// group-chat message. Lanxin pushes group messages as "@botname <text>"; the
// mention is not part of the user's actual intent and would otherwise prevent
// slash-command detection (strings.HasPrefix(content, "/")).
func stripMentionPrefix(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "@") {
		return s
	}
	// Find the end of the mention: first whitespace after '@'.
	idx := strings.IndexByte(s, ' ')
	if idx < 0 {
		// Pure mention ("@botname") with nothing after — treat as empty intent.
		return ""
	}
	return strings.TrimSpace(s[idx+1:])
}

// handleApproveCardCallback handles an approval-card button press. Lanxin
// delivers the press as a dedicated event carrying the pressing staffId and
// the button payload (eventData); until now the platform silently dropped it.
//
// The sending side is intentionally not wired yet: the approval-card message
// type is still rolling out server-side and its field contract is unpublished,
// so this handler parses and logs the callback, keeping a single hook point
// where a press will later be routed back to the waiting agent interaction
// (e.g. a permission question or an AskUserQuestion choice).
func (p *Platform) handleApproveCardCallback(ev lanxinEvent) {
	payload := strings.TrimSpace(ev.CallbackData)
	if payload == "" {
		slog.Info("lanxin: approve card callback (empty payload)", "from", ev.From)
		return
	}
	slog.Info("lanxin: approve card callback", "from", ev.From, "payload_len", len(payload))
	// Payload shape is not documented yet — keep a truncated preview so the
	// real contract can be pinned down from traffic, then replace this with the
	// routing call (approval → pending interaction).
	slog.Debug("lanxin: approve card callback payload", "from", ev.From, "payload", truncateForLog([]byte(payload)))
}
