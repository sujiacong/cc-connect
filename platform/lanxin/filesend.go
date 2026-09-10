package lanxin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// appMediaUploadPath is the self-built-app media upload endpoint (doc 4.5.4:
// files up to 10MB, images/video up to 20MB). Unlike the JSON message APIs it
// takes a multipart/form body with a single "media" file field.
const appMediaUploadPath = "/v1/app/medias/create"

// maxAppMediaFileBytes is the documented 10MB file limit for app media upload.
const maxAppMediaFileBytes = 10 << 20

// msgMediaTypeFile is the message-side mediaType for generic files
// (lanxin AppToMsgMediaType maps "file" -> 3, sharing the audio slot value).
const msgMediaTypeFile = 3

// fileMarkerRe matches {{file:/absolute/path}} markers embedded by the agent
// in reply text. Each marker asks cc-connect to upload the local file via the
// app media API and deliver it as a file message in the same chat.
var fileMarkerRe = regexp.MustCompile(`\{\{file:\s*([^}]+?)\s*\}\}`)

// multipleBlankLines collapses runs of 3+ newlines left behind after marker
// stripping so the remaining text keeps normal paragraph spacing.
var multipleBlankLines = regexp.MustCompile(`\n{3,}`)

// extractFileMarkers splits content into cleaned text and the unique file
// paths referenced by {{file:...}} markers (in order of first appearance).
func extractFileMarkers(content string) (string, []string) {
	matches := fileMarkerRe.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return content, nil
	}
	seen := map[string]bool{}
	paths := make([]string, 0, len(matches))
	for _, m := range matches {
		p := strings.TrimSpace(m[1])
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	cleaned := strings.TrimSpace(multipleBlankLines.ReplaceAllString(fileMarkerRe.ReplaceAllString(content, ""), "\n\n"))
	return cleaned, paths
}

// replyWithFiles sends content to the reply target, honoring {{file:...}}
// markers: markers are stripped from the text, the text is sent first, then
// each referenced local file is uploaded (app media API) and delivered as a
// file message. Files are first checked against the send allow-list (see
// fileSendAllowed) so a prompt injection cannot exfiltrate arbitrary host
// files; files that fail pre-validation (not allowed / missing / over 10MB)
// are reported inline with the text; upload/send failures are reported in a
// follow-up text message.
func (p *Platform) replyWithFiles(rc replyContext, content string) error {
	text, files := extractFileMarkers(content)

	var notes []string
	valid := make([]string, 0, len(files))
	for _, fp := range files {
		if !p.fileSendAllowed(fp) {
			slog.Warn("lanxin: file send blocked outside allow-list", "path", fp, "allow_dirs", p.fileSendAllowDirs)
			notes = append(notes, fmt.Sprintf("⚠️ 文件不在允许发送的目录内，未发送：%s（可用 file_send_allow_dirs 扩展）", fp))
			continue
		}
		st, err := os.Stat(fp)
		if err != nil {
			notes = append(notes, fmt.Sprintf("⚠️ 文件不存在，未发送：%s", fp))
			continue
		}
		if st.Size() > maxAppMediaFileBytes {
			notes = append(notes, fmt.Sprintf("⚠️ 文件超过蓝信 10MB 上限，未发送：%s（%.1fMB）", filepath.Base(fp), float64(st.Size())/(1<<20)))
			continue
		}
		valid = append(valid, fp)
	}

	if text == "" && len(valid) == 0 && len(notes) == 0 {
		return nil
	}

	out := text
	if len(notes) > 0 {
		if out != "" {
			out += "\n\n"
		}
		out += strings.Join(notes, "\n")
	}
	if out != "" {
		if err := p.sendReplyText(rc, out); err != nil {
			return fmt.Errorf("lanxin: reply %s: %w", rc.chatType, err)
		}
	}

	var failures []string
	for _, fp := range valid {
		mediaID, err := p.uploadAppMedia(fp)
		if err != nil {
			slog.Warn("lanxin: file upload failed", "path", fp, "err", err)
			failures = append(failures, fmt.Sprintf("⚠️ 文件上传失败：%s（%v）", filepath.Base(fp), err))
			continue
		}
		if rc.chatType == "group" {
			err = p.sendGroupFile(rc.target, mediaID)
		} else {
			err = p.sendBotFile(rc.target, mediaID)
		}
		if err != nil {
			slog.Warn("lanxin: file message failed", "path", fp, "err", err)
			failures = append(failures, fmt.Sprintf("⚠️ 文件发送失败：%s（%v）", filepath.Base(fp), err))
		}
	}
	if len(failures) > 0 {
		if err := p.sendReplyText(rc, strings.Join(failures, "\n")); err != nil {
			return fmt.Errorf("lanxin: reply file failure note: %w", err)
		}
	}

	slog.Debug("lanxin: reply sent", "chatType", rc.chatType, "target", rc.target, "len", len(text), "files", len(valid))
	return nil
}

// fileSendAllowed reports whether fp may be uploaded to the chat. The allow
// boundary is fileSendAllowDirs (the download dir plus configured extras,
// all symlink-resolved at construction). fp is symlink-resolved too, so a
// link inside an allowed directory pointing outside it does NOT escape the
// boundary. A not-yet-existing fp is judged by its deepest existing ancestor
// (mirroring parseAllowDirs), so the "file does not exist" pre-validation
// note stays accurate instead of being masked by an allow-list refusal.
func (p *Platform) fileSendAllowed(fp string) bool {
	abs, err := filepath.Abs(fp)
	if err != nil {
		return false
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = resolveDeepestExisting(abs)
		if resolved == "" {
			return false
		}
	}
	for _, dir := range p.fileSendAllowDirs {
		if resolved == dir || strings.HasPrefix(resolved, dir+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// sendReplyText sends plain text to the reply target (@-mentioning the asking
// user in group chats, same as the normal reply path). Leading/trailing blank
// lines are trimmed so an intermediate segment ending in stray newlines (a
// common agent-stream artifact) does not render as a tall empty bubble.
func (p *Platform) sendReplyText(rc replyContext, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	if rc.chatType == "group" {
		return p.sendGroupText(rc.target, content, rc.fromUserID)
	}
	return p.sendBotText(rc.target, content)
}

// uploadAppMedia uploads a local file via the self-built-app media API and
// returns the mediaID. The uploaded filename (basename) is what the receiving
// client displays, so callers should stage distinctly-named copies when the
// same basename would be ambiguous.
func (p *Platform) uploadAppMedia(filePath string) (string, error) {
	token, err := p.getAppToken()
	if err != nil {
		return "", fmt.Errorf("lanxin: uploadAppMedia: get token: %w", err)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("lanxin: uploadAppMedia: open: %w", err)
	}
	defer func() { _ = f.Close() }()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("media", filepath.Base(filePath))
	if err != nil {
		return "", fmt.Errorf("lanxin: uploadAppMedia: create form file: %w", err)
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", fmt.Errorf("lanxin: uploadAppMedia: copy: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("lanxin: uploadAppMedia: close writer: %w", err)
	}

	// Query params match the SDK exactly: the media-type param is named
	// "type" (url_helpers.go WithMediaTypeString), NOT "mediaType" — the
	// server rejects the latter with errCode 50052 缺少上传media类型.
	u := p.apiBase + appMediaUploadPath + "?app_token=" + token + "&type=file&width=0&height=0&duration=0"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &buf)
	if err != nil {
		return "", fmt.Errorf("lanxin: uploadAppMedia: create request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("lanxin: uploadAppMedia: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("lanxin: uploadAppMedia: read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("lanxin: uploadAppMedia: status %d: %s", resp.StatusCode, core.RedactToken(string(body), token))
	}

	var result struct {
		ErrCode int    `json:"errCode"`
		ErrMsg  string `json:"errMsg"`
		Data    struct {
			MediaID string `json:"mediaId"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("lanxin: uploadAppMedia: decode: %w", err)
	}
	if result.ErrCode != 0 {
		slog.Warn("lanxin: uploadAppMedia API error", "errCode", result.ErrCode, "errMsg", result.ErrMsg)
		return "", fmt.Errorf("lanxin: uploadAppMedia: errCode=%d errMsg=%s", result.ErrCode, result.ErrMsg)
	}
	if result.Data.MediaID == "" {
		return "", fmt.Errorf("lanxin: uploadAppMedia: empty mediaId in response")
	}
	return result.Data.MediaID, nil
}

// sendBotFile sends a file message to a p2p chat. Lanxin files travel as
// msgType "text" whose text payload carries mediaIds + mediaType (mirrors the
// official SDK SendFile construction).
func (p *Platform) sendBotFile(target, mediaID string) error {
	token, err := p.getAppToken()
	if err != nil {
		return fmt.Errorf("lanxin: sendBotFile: get token: %w", err)
	}
	payload := map[string]any{
		"msgType": "text",
		"msgData": map[string]any{
			"text": map[string]any{
				"mediaIds":  []string{mediaID},
				"mediaType": msgMediaTypeFile,
			},
		},
		"userIdList": []string{target},
	}
	return p.postAPI(botMsgPath, token, payload, "sendBotFile")
}

// sendGroupFile sends a file message to a group chat.
func (p *Platform) sendGroupFile(target, mediaID string) error {
	token, err := p.getAppToken()
	if err != nil {
		return fmt.Errorf("lanxin: sendGroupFile: get token: %w", err)
	}
	payload := map[string]any{
		"groupId": target,
		"msgType": "text",
		"msgData": map[string]any{
			"text": map[string]any{
				"mediaIds":  []string{mediaID},
				"mediaType": msgMediaTypeFile,
			},
		},
	}
	return p.postAPI(groupMsgPath, token, payload, "sendGroupFile")
}
