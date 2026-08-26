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
	CallbackData string   // for approve_card_callback
	MediaIDs     []string // for file/image/video/voice messages
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

		var isAtMe bool
		if reminder, ok := data["reminder"].(map[string]any); ok {
			isAtMe, _ = reminder["isAtMe"].(bool)
		}

		// extract text content msgData.text.content
		content := ""
		var mediaIDs []string
		if msgData, ok := data["msgData"].(map[string]any); ok {
			if text, ok := msgData["text"].(map[string]any); ok {
				content, _ = text["content"].(string)
			}
			// extract mediaIds for file/image/video/voice messages
			if media, ok := msgData["file"].(map[string]any); ok {
				if ids, ok := media["mediaIds"].([]any); ok {
					for _, id := range ids {
						if s, ok := id.(string); ok && s != "" {
							mediaIDs = append(mediaIDs, s)
						}
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
		slog.Info("lanxin: ws event", "type", ev.EventType, "chatType", ev.ChatType, "groupId", ev.GroupID, "from", ev.From, "msgType", ev.MsgType, "content", c, "content_len", len(ev.Content))
	}
	for _, ev := range events {
		switch ev.EventType {
		case "approve_card_callback":
			slog.Debug("lanxin: approve_card_callback received (ignored)", "from", ev.From)
			continue
		case "bot_private_message", "bot_group_message":
			// proceed
		default:
			slog.Debug("lanxin: unknown event type", "type", ev.EventType)
			continue
		}

		// group @bot filter (aligned with ws_client.py:174-176)
		if ev.ChatType == "group" && !ev.IsAtMe {
			slog.Debug("lanxin: group message not @bot, skipping", "group", ev.GroupID)
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
		if len(ev.MediaIDs) > 0 {
			slog.Info("lanxin: file message received", "user", ev.From, "chatType", ev.ChatType, "msgType", ev.MsgType, "media_ids", ev.MediaIDs)
			go p.handleFileMessage(ev)
			continue
		}

		// build session key and replyContext
		target := replyTarget(ev)

		var sessionKey string
		if ev.ChatType == "group" && !p.shareSessionInChannel {
			sessionKey = fmt.Sprintf("lanxin:%s:%s:%s", ev.ChatType, ev.GroupID, ev.From)
		} else {
			sessionKey = fmt.Sprintf("lanxin:%s:%s", ev.ChatType, target)
		}

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

		msg := &core.Message{
			SessionKey: sessionKey,
			Platform:   platformName,
			MessageID:  ev.MsgID,
			UserID:     ev.From,
			UserName:   ev.SenderName,
			ChatName:   chatName,
			Content:    content,
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
		go p.handler(p, msg)
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

// handleFileMessage downloads all attached media files and replies with the
// saved local paths (bot-log-analysis style log-package intake).
func (p *Platform) handleFileMessage(ev lanxinEvent) {
	var saved []string
	for _, mid := range ev.MediaIDs {
		path, err := p.saveMedia(mid)
		if err != nil {
			slog.Warn("lanxin: save media failed", "media_id", mid, "error", err)
			continue
		}
		saved = append(saved, path)
	}

	reply := "已收到文件"
	if len(saved) > 0 {
		reply += "，保存到：\n" + strings.Join(saved, "\n")
	} else {
		reply += "，但下载失败，请重试"
	}

	rc := replyContext{chatType: ev.ChatType, target: replyTarget(ev), senderName: ev.SenderName, fromUserID: ev.From}
	if err := p.Reply(context.Background(), rc, reply); err != nil {
		slog.Warn("lanxin: file reply failed", "error", err)
	}
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
