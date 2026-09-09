package lanxin

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// quotedMediaDir is the subdirectory under downloadDir where media referenced
// by quoted (引用) messages is downloaded. Quoted media is NOT the user's own
// upload — it stays out of the per-session upload markers/timeline — but a
// user pointing at a message explicitly authorizes reading its content.
const quotedMediaDir = ".quoted"

// quotedMessage is a parsed referenceMsg: the message a user quoted right
// before sending their own. Lanxin embeds the full quoted message (sender,
// type, timestamp and msgData) under the WS event's referenceMsg field, and
// a quoted message may itself quote an earlier one (nested references).
//
// Without this context the agent only sees the user's new text/media and, in
// multi-image conversations, cannot tell which picture a "这张/刚才那张/引用的图"
// refers to.
type quotedMessage struct {
	MsgType    string         // "text", "formatText", "image", "file", ...
	From       string         // sender staffId of the quoted message
	SenderName string         // display name of the quoted sender
	SendTime   string         // lanxin microsecond timestamp (string)
	Content    string         // quoted text / formatText / media caption
	MediaKind  string         // "file" or "image" when the quoted message carries media
	MediaIDs   []string       // media ids of the quoted media message
	Quoted     *quotedMessage // nested reference (quote-of-a-quote)
}

const maxQuoteDepth = 3

// parseQuotedMessage parses a raw referenceMsg JSON object into a quotedMessage.
// Returns nil when raw is not an object. Media ids live under the carrier key
// matching the quoted msgType (msgData.file / msgData.image), same layout as
// regular messages.
func parseQuotedMessage(raw any) *quotedMessage {
	return parseQuotedMessageDepth(raw, maxQuoteDepth)
}

func parseQuotedMessageDepth(raw any, depth int) *quotedMessage {
	m, ok := raw.(map[string]any)
	if !ok || m == nil || depth <= 0 {
		return nil
	}
	q := &quotedMessage{
		MsgType:    getString(m, "msgType"),
		From:       getString(m, "from"),
		SenderName: getString(m, "senderName"),
		SendTime:   getString(m, "sendTime"),
	}
	if md, ok := m["msgData"].(map[string]any); ok {
		switch q.MsgType {
		case "text":
			if t, ok := md["text"].(map[string]any); ok {
				q.Content, _ = t["content"].(string)
			}
		case "formatText":
			if t, ok := md["formatText"].(map[string]any); ok {
				q.Content, _ = t["text"].(string)
			}
		case "file", "image":
			if c, ok := md[q.MsgType].(map[string]any); ok {
				q.Content, _ = c["content"].(string)
				if ids, ok := c["mediaIds"].([]any); ok {
					for _, id := range ids {
						if s, ok := id.(string); ok && s != "" {
							q.MediaIDs = append(q.MediaIDs, s)
						}
					}
				}
				q.MediaKind = q.MsgType
			}
		}
	}
	if nested := parseQuotedMessageDepth(m["referenceMsg"], depth-1); nested != nil {
		q.Quoted = nested
	}
	return q
}

// attachReference prefixes a rendered description of the message the user
// quoted (if any) to content, so the agent understands what "这张图/刚才那条"
// refers to. Quoted media is downloaded on demand into downloadDir/.quoted/
// when it is not already cached (a quoted image may have been sent by someone
// else and never passed through this bot), and resolved to a local path the
// agent can actually read.
func (p *Platform) attachReference(ev lanxinEvent, content string) string {
	if ev.Quoted == nil {
		return content
	}
	p.ensureQuotedMedia(ev.Quoted)
	block := p.renderQuoted(ev.Quoted, 0)
	if block == "" {
		return content
	}
	if strings.TrimSpace(content) == "" {
		return block
	}
	return block + "\n" + content
}

// ensureQuotedMedia downloads (recursively through nested quotes) any quoted
// media id that is not already cached under downloadDir or downloadDir/.quoted/.
// Failures are logged and tolerated: the renderer then falls back to showing
// the bare media id. Runs in a worker goroutine (never the WS read loop).
func (p *Platform) ensureQuotedMedia(q *quotedMessage) {
	if q == nil {
		return
	}
	for _, id := range q.MediaIDs {
		if p.findQuotedMedia(id) != "" {
			continue
		}
		data, cd, err := p.downloadMedia(id)
		if err != nil {
			slog.Warn("lanxin: download quoted media failed", "media_id", id, "error", err)
			continue
		}
		ext := mediaExtFromDisposition(cd)
		if ext == "" {
			ext = detectMediaExt(data)
		}
		dir := filepath.Join(p.downloadDir, quotedMediaDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			slog.Warn("lanxin: quoted media dir", "error", err)
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, id+ext), data, 0o644); err != nil {
			slog.Warn("lanxin: save quoted media failed", "media_id", id, "error", err)
		}
	}
	p.ensureQuotedMedia(q.Quoted)
}

// findQuotedMedia returns a local path for a quoted media id if it is cached —
// under downloadDir/.quoted/ (downloaded on demand) or directly under
// downloadDir (an image the user sent earlier this session). Returns "" when
// the media is not available locally.
func (p *Platform) findQuotedMedia(id string) string {
	if id == "" {
		return ""
	}
	qdir := filepath.Join(p.downloadDir, quotedMediaDir)
	if m, _ := filepath.Glob(filepath.Join(qdir, id+".*")); len(m) > 0 {
		return m[0]
	}
	if st, err := os.Stat(filepath.Join(qdir, id)); err == nil && !st.IsDir() {
		return filepath.Join(qdir, id)
	}
	if m, _ := filepath.Glob(filepath.Join(p.downloadDir, id+".*")); len(m) > 0 {
		return m[0]
	}
	return ""
}

// renderQuoted renders one level of a quoted message chain into a readable
// text block, e.g.:
//
//	[引用消息] 张三: 看下这个报错
//	[引用的图片: /root/icg-ai-bot/uploads/xxx.png]
//	  └ 其引用的消息: ...
func (p *Platform) renderQuoted(q *quotedMessage, depth int) string {
	if q == nil || depth >= maxQuoteDepth {
		return ""
	}

	sender := q.SenderName
	if sender == "" {
		sender = q.From
	}
	if sender == "" {
		sender = "对方"
	}

	var parts []string
	head := "[引用消息] " + sender
	if q.SendTime != "" {
		head += " @" + q.SendTime
	}
	head += ":"
	parts = append(parts, head)

	switch {
	case q.MediaKind != "" && len(q.MediaIDs) > 0:
		// Point the agent at the local file when the media is cached (either
		// downloaded on demand into .quoted/, or sent earlier this session).
		// Quoting a message is an explicit user reference, so the media is
		// authorized content the agent may read even though it is not part of
		// this session's own upload timeline.
		kind := "文件"
		if q.MediaKind == "image" {
			kind = "图片"
		}
		var local []string
		for _, id := range q.MediaIDs {
			if lp := p.findQuotedMedia(id); lp != "" {
				local = append(local, lp)
			}
		}
		if len(local) > 0 {
			parts = append(parts, "引用的"+kind+"（用户引用指定，可直接读取查看内容）: "+strings.Join(local, " ; "))
		} else {
			parts = append(parts, "引用的"+kind+": mediaID="+strings.Join(q.MediaIDs, ",")+"（本地不可得，未能读取内容）")
		}
		if strings.TrimSpace(q.Content) != "" {
			parts = append(parts, "配文: "+q.Content)
		}
	default:
		if strings.TrimSpace(q.Content) != "" {
			parts = append(parts, q.Content)
		} else {
			parts = append(parts, "[非文本消息: "+q.MsgType+"]")
		}
	}

	if nested := p.renderQuoted(q.Quoted, depth+1); nested != "" {
		parts = append(parts, "其引用的消息: "+nested)
	}
	return strings.Join(parts, "\n")
}
