package lanxin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chenhg5/cc-connect/core"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// sendBotText sends a rich-text message to a private chat. The payload uses
// msgType=formatText with formatType=1, which the lanxin client renders as
// Markdown (aligned with the official lansenger channel plugin, whose every
// AI reply goes through sendFormatText with the same {formatType:1,text}
// body). Plain text with no Markdown characters renders unchanged.
func (p *Platform) sendBotText(target string, content string) error {
	token, err := p.getAppToken()
	if err != nil {
		return fmt.Errorf("lanxin: sendBotText: get token: %w", err)
	}

	payload := map[string]any{
		"msgType": "formatText",
		"msgData": map[string]any{
			"formatText": map[string]any{"formatType": 1, "text": content},
		},
		"userIdList": []string{target},
	}
	return p.postAPI(botMsgPath, token, payload, "sendBotText")
}

// sendGroupText sends a rich-text (formatText, Markdown-rendered) message to a
// group, @-mentioning mentionUserID when set.
func (p *Platform) sendGroupText(target string, content string, mentionUserID string) error {
	token, err := p.getAppToken()
	if err != nil {
		return fmt.Errorf("lanxin: sendGroupText: get token: %w", err)
	}

	text := map[string]any{"formatType": 1, "text": content}
	if mentionUserID != "" {
		// @-mention the asking user so they get notified in group chat
		text["reminder"] = map[string]any{"all": false, "userIds": []string{mentionUserID}}
	}

	payload := map[string]any{
		"groupId": target,
		"msgType": "formatText",
		"msgData": map[string]any{
			"formatText": text,
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

// downloadMedia fetches a media file by mediaId (GET /v1/medias/{id}/fetch).
// Returns the raw bytes plus the Content-Disposition header (for filename).
func (p *Platform) downloadMedia(mediaID string) ([]byte, string, error) {
	token, err := p.getAppToken()
	if err != nil {
		return nil, "", fmt.Errorf("lanxin: downloadMedia: get token: %w", err)
	}

	u := p.apiBase + "/v1/medias/" + mediaID + "/fetch?app_token=" + token
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", fmt.Errorf("lanxin: downloadMedia: create request: %w", err)
	}

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("lanxin: downloadMedia: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("lanxin: downloadMedia: status %d: %s",
			resp.StatusCode, core.RedactToken(string(body), token))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("lanxin: downloadMedia: read body: %w", err)
	}
	return data, resp.Header.Get("Content-Disposition"), nil
}

// mediaExtFromDisposition extracts a file extension (e.g. ".pdf") from a
// Content-Disposition header like `attachment;filename=DNSxxx.pdf`, or ""
// when none can be inferred. The raw header is often GBK-encoded, so we only
// trust the extension (ASCII) and use the mediaId for the base name.
func mediaExtFromDisposition(cd string) string {
	idx := strings.LastIndex(cd, ".")
	if idx < 0 {
		return ""
	}
	ext := strings.TrimRight(cd[idx:], "\"'; \t\r\n")
	if ext == "" || strings.ContainsAny(ext, "=; \t\r\n\"'") {
		return ""
	}
	return ext
}

// detectMediaExt sniffs common binary signatures to pick a file extension for
// media downloads whose Content-Disposition carries no filename (lanxin image
// messages, for example). Mirrors the official lansenger channel plugin's
// detectExtFromBytes for the formats this bot actually consumes; returns ""
// when nothing matches so the caller keeps the bare mediaID file name.
func detectMediaExt(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	if b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff {
		return ".jpg"
	}
	png := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a}
	if len(b) >= len(png) && bytes.Equal(b[:len(png)], png) {
		return ".png"
	}
	if len(b) >= 6 {
		if h := string(b[:6]); h == "GIF87a" || h == "GIF89a" {
			return ".gif"
		}
	}
	if b[0] == 0x25 && b[1] == 0x50 && b[2] == 0x44 && b[3] == 0x46 {
		return ".pdf"
	}
	if string(b[:4]) == "PK\x03\x04" {
		return ".zip"
	}
	return ""
}

// contentDispositionFilename extracts the original file name from a
// Content-Disposition header, or "" when none can be found. It understands
// both a plain filename="..." (often GBK-encoded for Chinese names) and the
// RFC 5987 percent-encoded form (filename*=UTF-8 with a url-encoded value),
// and decodes GBK to UTF-8 so the resulting name is what the user actually
// called the file.
func contentDispositionFilename(cd string) string {
	if cd == "" {
		return ""
	}
	// RFC 5987 form wins when present: filename*=UTF-8''%E5%8E%9F...zip
	for _, part := range splitDispositionParams(cd) {
		if rest, ok := strings.CutPrefix(part, "filename*="); ok {
			val := strings.Trim(rest, `"`)
			if i := strings.LastIndex(val, "'"); i >= 0 {
				val = val[i+1:]
			}
			if dec, err := url.PathUnescape(val); err == nil {
				val = dec
			}
			return sanitizeOriginalName(decodeMaybeGBK(val))
		}
	}
	// plain filename="..." / filename=...
	for _, part := range splitDispositionParams(cd) {
		if rest, ok := strings.CutPrefix(part, "filename="); ok {
			val := strings.Trim(rest, `"`)
			return sanitizeOriginalName(decodeMaybeGBK(val))
		}
	}
	return ""
}

// splitDispositionParams splits a Content-Disposition value on ';' and trims
// each segment, matching the parameter shapes `filename=...`, `filename*=...`.
func splitDispositionParams(cd string) []string {
	var parts []string
	for _, seg := range strings.Split(cd, ";") {
		seg = strings.TrimSpace(seg)
		lower := strings.ToLower(seg)
		if strings.HasPrefix(lower, "filename*=") {
			parts = append(parts, "filename*="+seg[len("filename*="):])
			continue
		}
		if strings.HasPrefix(lower, "filename=") {
			parts = append(parts, "filename="+seg[len("filename="):])
			continue
		}
	}
	return parts
}

// decodeMaybeGBK converts a possibly-GBK-encoded file name to UTF-8. ASCII and
// already-valid UTF-8 are returned unchanged; bytes that are not valid UTF-8
// are decoded as GBK (the encoding lanxin's server commonly emits for Chinese
// file names). If that also fails the raw string is returned untouched.
func decodeMaybeGBK(s string) string {
	if s == "" || utf8.ValidString(s) {
		return s
	}
	decoded, err := simplifiedchinese.GBK.NewDecoder().String(s)
	if err != nil {
		return s
	}
	return decoded
}

// sanitizeOriginalName reduces a user-supplied original file name to a single
// basename with control characters removed. The name is used for DISPLAY ONLY
// (never joined into a path), so this is defense in depth against prompt
// injection / path confusion, not the path-safety boundary.
func sanitizeOriginalName(name string) string {
	name = strings.TrimSpace(name)
	name = filepath.ToSlash(name)
	// filepath.ToSlash only maps the OS separator, so backslashes from a
	// Windows-style name survive on Linux — normalize them explicitly so
	// filepath.Base strips both separator styles on every host.
	name = strings.ReplaceAll(name, "\\", "/")
	name = filepath.Base(name)
	name = strings.Trim(name, `"' `+"\t\r\n")
	var b strings.Builder
	for _, r := range name {
		if r < 0x20 && r != '\t' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// sessionMarkerDir is the per-session upload marker directory. It lives under
// the download directory with a dot prefix so it is hidden from log collectors
// that scan uploads/ for uploaded logs, and so uploads/ stays untouched apart
// from the media files themselves.
const sessionMarkerDir = ".sessions"

// defaultUploadMarkerTTL is how long a per-session upload marker stays valid.
// After this duration the background sweeper removes the marker, so a stale
// marker can never be mistaken for a fresh same-session upload. Configurable
// via the upload_marker_ttl_minutes platform option (<=0 disables the expiry
// sweep).
const defaultUploadMarkerTTL = 60 * time.Minute

// maxSessionUploadFiles caps how many files one session's marker retains. Older
// uploads beyond this count are dropped so a marker cannot grow without bound.
const maxSessionUploadFiles = 50

// uploadWaitTimeout is how long a text command waits for a same-session media
// download that is still in flight before proceeding. A large file (e.g. a 92MB
// log package) can take several seconds to download — the download itself has a
// 60s timeout — so 30s is a generous bound for it to finish. On timeout the
// text command proceeds with whatever the session marker already contains: the
// download keeps running and its eventual marker write stays visible to later
// turns, so nothing is lost beyond that turn's injection.
const uploadWaitTimeout = 30 * time.Second

// uploadInflight tracks one in-flight media download for a chat session. It is
// shared by all concurrent file messages of the same session (reference
// counted, refs guarded by uploadMu) and by the text-command waiters: a text
// command sent immediately after a large file upload waits on done, which the
// last finisher closes AFTER the per-session marker has been written, so the
// waiter always reads the completed marker.
type uploadInflight struct {
	done chan struct{} // closed by the last finisher, releasing every waiter
	once sync.Once     // guarantees done is closed exactly once
	refs int           // beginSessionUpload calls not yet finished; guarded by uploadMu
}

// beginSessionUpload registers an in-flight media download for sessionKey and
// returns the shared tracker. A second file message for the same session while
// one is already downloading reuses the same tracker (refcounted), so a text
// command waits for ALL of them. The map is lazily created so platforms built
// without New (e.g. tests) are safe. Must be paired with exactly one
// finishSessionUpload.
func (p *Platform) beginSessionUpload(sessionKey string) *uploadInflight {
	p.uploadMu.Lock()
	defer p.uploadMu.Unlock()
	if p.inflightUploads == nil {
		p.inflightUploads = make(map[string]*uploadInflight)
	}
	if uf := p.inflightUploads[sessionKey]; uf != nil {
		uf.refs++
		return uf
	}
	uf := &uploadInflight{done: make(chan struct{})}
	uf.refs = 1
	p.inflightUploads[sessionKey] = uf
	return uf
}

// finishSessionUpload releases one reference held by beginSessionUpload. When
// the last reference is released the tracker is removed from the map and its
// done channel is closed, releasing every waiter. done is closed AFTER the
// session-marker write (saveMedia → appendSessionUploads runs before the
// deferred finish), so a waiter woken by the close is guaranteed to read the
// completed marker.
func (p *Platform) finishSessionUpload(sessionKey string, uf *uploadInflight) {
	closeDone := false
	p.uploadMu.Lock()
	if cur := p.inflightUploads[sessionKey]; cur == uf {
		uf.refs--
		if uf.refs <= 0 {
			closeDone = true
			delete(p.inflightUploads, sessionKey)
		}
	}
	p.uploadMu.Unlock()
	if closeDone {
		uf.once.Do(func() { close(uf.done) })
	}
}

// waitSessionUploads blocks until every in-flight media download of sessionKey
// has finished (true), or until timeout elapses (false). It never blocks the WS
// read loop — callers must run it from a worker goroutine (the handler
// goroutine for text messages). A timeout returns false and the caller proceeds
// with the current marker state; the download keeps running and its marker
// write stays visible to later turns.
func (p *Platform) waitSessionUploads(sessionKey string, timeout time.Duration) bool {
	p.uploadMu.Lock()
	uf := p.inflightUploads[sessionKey]
	p.uploadMu.Unlock()
	if uf == nil {
		return true // nothing in flight → nothing to wait for
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-uf.done:
		return true
	case <-timer.C:
		return false
	}
}

// sessionUploadFile is one file recorded in a session upload marker.
type sessionUploadFile struct {
	Path         string `json:"path"`          // absolute path of the saved media file
	MediaID      string `json:"media_id"`      // lanxin mediaId of the uploaded file
	OriginalName string `json:"original_name"` // original file name (GBK-decoded), falls back to mediaID
	UploadedAt   string `json:"uploaded_at"`   // RFC3339, e.g. 2026-08-30T20:00:00+08:00
}

// sessionUploadMarker records all files uploaded by one chat session that are
// still within the TTL (identified by the same session key used for text-message
// routing). Concurrent sessions each get their own marker file, so one session's
// upload can never clobber another session's. Written by saveMedia (cumulative:
// new uploads are appended, historical batches are kept until the cap) and read
// by the platform when a text command arrives to populate Message.UploadedFiles.
type sessionUploadMarker struct {
	SessionKey string              `json:"session_key"` // e.g. lanxin:p2p:<from>
	Files      []sessionUploadFile `json:"files"`       // all non-expired uploads, oldest first
	UpdatedAt  string              `json:"updated_at"`  // RFC3339 marker write time
}

// safeSessionKeyName converts a session key into a filesystem-safe base name
// for the per-session marker file. ':' and '/' (and '\' for safety on any host)
// are replaced with '_'; the result is used only as a file name.
func safeSessionKeyName(sessionKey string) string {
	replacer := strings.NewReplacer(":", "_", "/", "_", "\\", "_")
	return replacer.Replace(sessionKey)
}

// sessionMarkerPath returns the per-session marker file path for sessionKey.
func sessionMarkerPath(downloadDir, sessionKey string) string {
	return filepath.Join(downloadDir, sessionMarkerDir, safeSessionKeyName(sessionKey)+".json")
}

// readSessionUploadMarker reads the per-session marker file at pth. Returns
// (nil, nil) when the file does not exist; an error for corrupt content so
// callers can treat it as "no upload" rather than trusting partial data.
func readSessionUploadMarkerFile(pth string) (*sessionUploadMarker, error) {
	data, err := os.ReadFile(pth)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var m sessionUploadMarker
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// readSessionUploadMarker is the per-session variant used by the platform when
// routing a text command. Returns nil when the marker is missing or corrupt.
func (p *Platform) readSessionUploadMarker(sessionKey string) (*sessionUploadMarker, error) {
	return readSessionUploadMarkerFile(sessionMarkerPath(p.downloadDir, sessionKey))
}

// saveMedia downloads a media file and writes it into the platform download
// directory, returning the local absolute path. On success it also appends the
// file to the per-session upload marker (cumulative: earlier uploads of the
// same session are kept) so the platform can later inject the session's full
// upload timeline into the agent input.
func (p *Platform) saveMedia(mediaID, sessionKey, uploadedAt string) (string, error) {
	data, cd, err := p.downloadMedia(mediaID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(p.downloadDir, 0o755); err != nil {
		return "", fmt.Errorf("lanxin: saveMedia: mkdir: %w", err)
	}
	ext := mediaExtFromDisposition(cd)
	if ext == "" {
		// No filename in Content-Disposition — common for lanxin image
		// messages — so sniff the bytes for a usable extension.
		ext = detectMediaExt(data)
	}
	path := filepath.Join(p.downloadDir, mediaID+ext)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("lanxin: saveMedia: write: %w", err)
	}
	originalName := contentDispositionFilename(cd)
	if originalName == "" {
		originalName = mediaID
	}
	if err := p.recordSessionUpload(path, mediaID, originalName, sessionKey, uploadedAt); err != nil {
		// The media file itself is already saved; a marker failure must not
		// fail the whole intake but has to be visible in logs.
		slog.Warn("lanxin: saveMedia: recordSessionUpload failed", "media_id", mediaID, "session_key", sessionKey, "error", err)
	}
	return path, nil
}

// recordSessionUpload appends one file to the per-session upload marker. Called
// after every successful saveMedia; earlier uploads of the session are kept.
// uploadedAt is the message's arrival timestamp (taken by the WS read loop in
// message order) — NOT the download-finish time — so the marker's timeline
// reflects the order the user sent the files in, even when downloads of
// different messages complete out of order.
func (p *Platform) recordSessionUpload(path, mediaID, originalName, sessionKey, uploadedAt string) error {
	return p.appendSessionUploads(sessionKey, sessionUploadFile{
		Path:         path,
		MediaID:      mediaID,
		OriginalName: originalName,
		UploadedAt:   uploadedAt,
	})
}

// appendSessionUploads appends files to the per-session upload marker for
// sessionKey, keeping the session's earlier uploads (cumulative semantics).
// Exact duplicates — the same mediaID uploaded at the same second — are merged
// so one message's batch is not recorded twice. The marker keeps at most
// maxSessionUploadFiles files, dropping the oldest beyond the cap. The whole
// read-modify-write runs under markerMu so concurrent uploads to the same
// session cannot lose updates.
func (p *Platform) appendSessionUploads(sessionKey string, files ...sessionUploadFile) error {
	if sessionKey == "" {
		return fmt.Errorf("lanxin: appendSessionUploads: empty session key")
	}
	if len(files) == 0 {
		return nil
	}

	p.markerMu.Lock()
	defer p.markerMu.Unlock()

	// Start from the current marker when it is still fresh; a stale or missing
	// marker means the session starts over (TTL semantics unchanged).
	existing := []sessionUploadFile{}
	if m, err := p.readSessionUploadMarker(sessionKey); err == nil && m != nil && p.markerFresh(m.UpdatedAt) {
		existing = m.Files
	}

	for _, f := range files {
		if f.Path == "" {
			continue
		}
		dup := false
		for _, e := range existing {
			if e.MediaID == f.MediaID && e.UploadedAt == f.UploadedAt {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		existing = append(existing, f)
	}
	if len(existing) > maxSessionUploadFiles {
		existing = existing[len(existing)-maxSessionUploadFiles:]
	}

	marker := sessionUploadMarker{
		SessionKey: sessionKey,
		Files:      existing,
		UpdatedAt:  time.Now().Format(time.RFC3339),
	}
	data, err := json.Marshal(marker)
	if err != nil {
		return fmt.Errorf("lanxin: appendSessionUploads: marshal: %w", err)
	}
	dir := filepath.Join(p.downloadDir, sessionMarkerDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("lanxin: appendSessionUploads: mkdir: %w", err)
	}
	markerPath := sessionMarkerPath(p.downloadDir, sessionKey)
	if err := os.WriteFile(markerPath, data, 0o644); err != nil {
		return fmt.Errorf("lanxin: appendSessionUploads: write: %w", err)
	}
	return nil
}

// sessionUploadedFiles returns the files recorded in the per-session upload
// marker for sessionKey, or an empty slice when the session has no (or an
// expired) upload. Used by the platform when a text command arrives so the
// engine can inject the session's upload timeline into the agent input. The
// list is sorted by upload time ascending (oldest → newest) — the raw marker
// order is concurrent-download completion order, not the user's sending
// order. Old markers without an original_name fall back to the mediaID for
// display.
func (p *Platform) sessionUploadedFiles(sessionKey string) []core.UploadedFile {
	p.markerMu.Lock()
	defer p.markerMu.Unlock()
	m, err := p.readSessionUploadMarker(sessionKey)
	if err != nil || m == nil {
		// Missing or corrupt marker → no upload. A corrupt marker is swept by
		// expireUploadMarkerIfStale on the next pass.
		return []core.UploadedFile{}
	}
	if !p.markerFresh(m.UpdatedAt) {
		return []core.UploadedFile{}
	}
	files := make([]core.UploadedFile, 0, len(m.Files))
	for _, f := range m.Files {
		if f.Path == "" {
			continue
		}
		originalName := f.OriginalName
		if originalName == "" {
			originalName = f.MediaID
		}
		files = append(files, core.UploadedFile{
			Path:         f.Path,
			OriginalName: originalName,
			UploadedAt:   f.UploadedAt,
		})
	}
	sort.SliceStable(files, func(i, j int) bool {
		ti, erri := time.Parse(time.RFC3339Nano, files[i].UploadedAt)
		tj, errj := time.Parse(time.RFC3339Nano, files[j].UploadedAt)
		if erri != nil || errj != nil {
			return files[i].UploadedAt < files[j].UploadedAt
		}
		return ti.Before(tj)
	})
	return files
}

// markerFresh reports whether a marker whose updated_at RFC3339 string is t is
// still within the TTL window. A missing/invalid timestamp counts as stale
// (treat as "no current upload"). TTL <= 0 disables expiry.
func (p *Platform) markerFresh(updatedAt string) bool {
	if p.uploadMarkerTTL <= 0 {
		return true
	}
	t, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return false
	}
	return time.Since(t) <= p.uploadMarkerTTL
}

// expireUploadMarkerIfStale removes per-session upload markers once they have
// been valid for longer than uploadMarkerTTL, so a stale marker cannot be
// treated as a current-session upload. Corrupt markers are removed too. No-op
// when there are no markers or the TTL is disabled. Runs under markerMu so it
// cannot remove a marker that appendSessionUploads is concurrently refreshing.
func (p *Platform) expireUploadMarkerIfStale() {
	if p.uploadMarkerTTL <= 0 {
		return
	}
	p.markerMu.Lock()
	defer p.markerMu.Unlock()
	dir := filepath.Join(p.downloadDir, sessionMarkerDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return // no marker directory (or unreadable) — nothing to expire
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		markerPath := filepath.Join(dir, e.Name())
		m, err := readSessionUploadMarkerFile(markerPath)
		if err != nil {
			// Corrupt marker: expire it so it can never be reported as a
			// current-session upload.
			_ = os.Remove(markerPath)
			slog.Warn("lanxin: removed corrupt session upload marker", "path", markerPath, "error", err)
			continue
		}
		if m == nil || p.markerFresh(m.UpdatedAt) {
			continue
		}
		if err := os.Remove(markerPath); err != nil && !os.IsNotExist(err) {
			slog.Warn("lanxin: remove expired session upload marker failed", "path", markerPath, "error", err)
			continue
		}
		slog.Info("lanxin: expired session upload marker removed", "path", markerPath)
	}
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
