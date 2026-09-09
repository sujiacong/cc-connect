package lanxin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTestFile creates dir/name with the given content and returns its path.
func writeTestFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

// decodeJSONBody unmarshals a recorded request body into a map.
func decodeJSONBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("decode body %q: %v", body, err)
	}
	return m
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func TestExtractFileMarkers(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantTxt string
		want    []string
	}{
		{
			name:    "no markers",
			in:      "hello world",
			wantTxt: "hello world",
		},
		{
			name:    "single marker",
			in:      "工具发你\n{{file:/tmp/tool.zip}}",
			wantTxt: "工具发你",
			want:    []string{"/tmp/tool.zip"},
		},
		{
			name:    "multiple markers kept in order",
			in:      "{{file:/tmp/a.bin}} and {{file:/tmp/b.bin}}",
			wantTxt: "and",
			want:    []string{"/tmp/a.bin", "/tmp/b.bin"},
		},
		{
			name:    "duplicate markers deduped",
			in:      "{{file:/tmp/a.bin}} {{file:/tmp/a.bin}}",
			wantTxt: "",
			want:    []string{"/tmp/a.bin"},
		},
		{
			name:    "inner spaces trimmed",
			in:      "{{file:  /tmp/a.bin  }}",
			wantTxt: "",
			want:    []string{"/tmp/a.bin"},
		},
		{
			name:    "blank lines collapsed after strip",
			in:      "line1\n\n\n\n{{file:/tmp/a.bin}}\n\n\n\nline2",
			wantTxt: "line1\n\nline2",
			want:    []string{"/tmp/a.bin"},
		},
		{
			name:    "marker only",
			in:      "{{file:/tmp/a.bin}}",
			wantTxt: "",
			want:    []string{"/tmp/a.bin"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTxt, got := extractFileMarkers(tt.in)
			if gotTxt != tt.wantTxt {
				t.Errorf("text = %q, want %q", gotTxt, tt.wantTxt)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("paths = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("paths[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestReplyWithFiles_P2P(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	dir := t.TempDir()
	p.fileSendAllowDirs = append(p.fileSendAllowDirs, dir)
	tool := writeTestFile(t, dir, "tool.zip", "fake-zip-bytes")

	rc := replyContext{chatType: "p2p", target: "staff-1"}
	if err := p.replyWithFiles(rc, "工具发你\n{{file:"+tool+"}}"); err != nil {
		t.Fatalf("replyWithFiles: %v", err)
	}

	if got := rec.uploadedNames(); !containsStr(got, "tool.zip") {
		t.Errorf("uploaded names = %v, want tool.zip", got)
	}
	bodies := rec.botBodies()
	if len(bodies) != 2 {
		t.Fatalf("bot bodies = %d, want 2 (text + file): %v", len(bodies), bodies)
	}
	textMsg := decodeJSONBody(t, bodies[0])
	// The leading text message goes out as formatText (formatType=1, rendered
	// as Markdown by the client) and must not carry file mediaIds — only the
	// follow-up file message does (msgData.text.mediaIds).
	fmtText, ok := textMsg["msgData"].(map[string]any)["formatText"].(map[string]any)
	if !ok {
		t.Errorf("first message should be formatText (markdown-rendered), got: %v", bodies[0])
	} else {
		if ft, _ := fmtText["formatType"].(float64); ft != 1 {
			t.Errorf("formatType = %v, want 1", fmtText["formatType"])
		}
		if _, has := fmtText["mediaIds"]; has {
			t.Errorf("text message should not carry mediaIds: %v", bodies[0])
		}
	}
	fileMsg := decodeJSONBody(t, bodies[1])
	text := fileMsg["msgData"].(map[string]any)["text"].(map[string]any)
	mediaIds, ok := text["mediaIds"].([]any)
	if !ok || len(mediaIds) != 1 || mediaIds[0] != "m-123" {
		t.Errorf("file message mediaIds = %v, want [m-123]", text["mediaIds"])
	}
	if mt, _ := text["mediaType"].(float64); mt != float64(msgMediaTypeFile) {
		t.Errorf("file message mediaType = %v, want %d", text["mediaType"], msgMediaTypeFile)
	}
}

func TestReplyWithFiles_Group(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	dir := t.TempDir()
	p.fileSendAllowDirs = append(p.fileSendAllowDirs, dir)
	tool := writeTestFile(t, dir, "tool.zip", "fake-zip-bytes")

	rc := replyContext{chatType: "group", target: "grp-1", fromUserID: "user-open-1"}
	if err := p.replyWithFiles(rc, "工具发你 {{file:"+tool+"}}"); err != nil {
		t.Fatalf("replyWithFiles: %v", err)
	}

	if got := rec.uploadedNames(); !containsStr(got, "tool.zip") {
		t.Errorf("uploaded names = %v, want tool.zip", got)
	}
	bodies := rec.groupBodies()
	if len(bodies) != 2 {
		t.Fatalf("group bodies = %d, want 2 (text + file): %v", len(bodies), bodies)
	}
	textMsg := decodeJSONBody(t, bodies[0])
	// sendGroupText @-mentions the asker via formatText.reminder.userIds.
	textData, _ := textMsg["msgData"].(map[string]any)["formatText"].(map[string]any)
	rem, ok := textData["reminder"].(map[string]any)
	if !ok {
		t.Errorf("text message should carry reminder to @asker: %v", bodies[0])
	} else {
		ids, _ := rem["userIds"].([]any)
		found := false
		for _, id := range ids {
			if s, _ := id.(string); s == "user-open-1" {
				found = true
			}
		}
		if !found {
			t.Errorf("reminder.userIds = %v, want user-open-1", rem["userIds"])
		}
	}
	fileMsg := decodeJSONBody(t, bodies[1])
	// File messages must not duplicate the @-mention reminder.
	fileText, _ := fileMsg["msgData"].(map[string]any)["text"].(map[string]any)
	if _, has := fileText["reminder"]; has {
		t.Errorf("file message should not carry reminder, got %v", bodies[1])
	}
	if gid, _ := fileMsg["groupId"].(string); gid != "grp-1" {
		t.Errorf("file message groupId = %v, want grp-1", fileMsg["groupId"])
	}
}

func TestReplyWithFiles_PrevalidationNotes(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	dir := t.TempDir()
	p.fileSendAllowDirs = append(p.fileSendAllowDirs, dir)
	big := writeTestFile(t, dir, "big.bin", strings.Repeat("x", maxAppMediaFileBytes+1))

	rc := replyContext{chatType: "p2p", target: "staff-1"}
	err := p.replyWithFiles(rc, "见下方说明：\n{{file:"+filepath.Join(dir, "missing.zip")+"}}\n{{file:"+big+"}}")
	if err != nil {
		t.Fatalf("replyWithFiles: %v", err)
	}

	if got := rec.uploadedNames(); len(got) != 0 {
		t.Errorf("no upload expected, got %v", got)
	}
	bodies := rec.botBodies()
	if len(bodies) != 1 {
		t.Fatalf("bot bodies = %d, want 1 (text with notes): %v", len(bodies), bodies)
	}
	if !strings.Contains(bodies[0], "文件不存在") || !strings.Contains(bodies[0], "missing.zip") {
		t.Errorf("note should mention missing file, got %q", bodies[0])
	}
	if !strings.Contains(bodies[0], "10MB") || !strings.Contains(bodies[0], "big.bin") {
		t.Errorf("note should mention oversize file, got %q", bodies[0])
	}
}

func TestReplyWithFiles_UploadFailureNote(t *testing.T) {
	dir := t.TempDir()
	tool := writeTestFile(t, dir, "tool.zip", "fake-zip-bytes")

	rec := &reqRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case tokenPath:
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{"appToken":"tk","expiresIn":3600}}`))
		case appMediaUploadPath:
			w.Write([]byte(`{"errCode":500,"errMsg":"upload quota exceeded"}`))
		case botMsgPath:
			body, _ := io.ReadAll(r.Body)
			rec.mu.Lock()
			rec.bots = append(rec.bots, string(body))
			rec.mu.Unlock()
			w.Write([]byte(`{"errCode":0,"errMsg":"ok","data":{}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	p := &Platform{
		appID:             "app",
		appSecret:         "sec",
		apiBase:           srv.URL,
		httpClient:        srv.Client(),
		fileSendAllowDirs: []string{dir},
	}

	rc := replyContext{chatType: "p2p", target: "staff-1"}
	if err := p.replyWithFiles(rc, "工具发你 {{file:"+tool+"}}"); err != nil {
		t.Fatalf("replyWithFiles: %v", err)
	}

	bodies := rec.botBodies()
	if len(bodies) != 2 {
		t.Fatalf("bot bodies = %d, want 2 (text + failure note): %v", len(bodies), bodies)
	}
	if !strings.Contains(bodies[1], "上传失败") || !strings.Contains(bodies[1], "tool.zip") {
		t.Errorf("failure note should mention upload failure, got %q", bodies[1])
	}
}

func TestSendCardText_HonorsFileMarkers(t *testing.T) {
	p, rec := newTestPlatformWithServer(t, "")
	dir := t.TempDir()
	p.fileSendAllowDirs = append(p.fileSendAllowDirs, dir)
	tool := writeTestFile(t, dir, "tool.zip", "fake-zip-bytes")

	// sendCardText (the SendCard/ReplyCard text-render path) must route
	// through replyWithFiles so card replies can also carry files.
	if err := p.sendCardText(replyContext{chatType: "p2p", target: "staff-1"}, "**定位工具**\n——————————\n工具发你\n[ 查看用法 ]\n{{file:"+tool+"}}"); err != nil {
		t.Fatalf("sendCardText: %v", err)
	}

	if got := rec.uploadedNames(); !containsStr(got, "tool.zip") {
		t.Errorf("uploaded names = %v, want tool.zip", got)
	}
	bodies := rec.botBodies()
	if len(bodies) != 2 {
		t.Fatalf("bot bodies = %d, want 2 (text + file): %v", len(bodies), bodies)
	}
	if strings.Contains(bodies[0], "{{file:") {
		t.Errorf("marker should be stripped from text, got %q", bodies[0])
	}
}

func TestFileSendAllowed(t *testing.T) {
	downloadDir := t.TempDir()
	extraDir := t.TempDir()
	// symlink escape: a link inside the allowed dir pointing outside it
	outsideDir := t.TempDir()
	link := filepath.Join(downloadDir, "escape")
	if err := os.Symlink(outsideDir, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	p := &Platform{
		downloadDir:       downloadDir,
		fileSendAllowDirs: []string{downloadDir, extraDir},
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"download dir file", filepath.Join(downloadDir, "m1.log"), true},
		{"download dir subdir file", filepath.Join(downloadDir, "sub", "a.log"), true},
		{"download dir root itself", downloadDir, true},
		{"configured extra dir", filepath.Join(extraDir, "report.pdf"), true},
		{"outside all", "/etc/shadow", false},
		{"sibling prefix trick", downloadDir + "-other" + "/x.log", false},
		{"symlink escape", filepath.Join(link, "secret"), false},
		{"missing file inside allowed dir", filepath.Join(downloadDir, "later.log"), true},
		{"missing file outside", "/nonexistent-root-xyz/a.log", false},
	}
	for _, c := range cases {
		if got := p.fileSendAllowed(c.path); got != c.want {
			t.Errorf("%s: fileSendAllowed(%q) = %v, want %v", c.name, c.path, got, c.want)
		}
	}
}

func TestParseAllowDirs(t *testing.T) {
	downloadDir := t.TempDir()

	// empty option → download dir only
	dirs := parseAllowDirs("", downloadDir)
	if len(dirs) != 1 || dirs[0] != downloadDir {
		t.Errorf("parseAllowDirs(empty) = %v, want [%s]", dirs, downloadDir)
	}

	// configured extras are resolved and appended
	extra := t.TempDir()
	dirs = parseAllowDirs("  " + extra + " , " + filepath.Join(extra, "workspace") + " ", downloadDir)
	if len(dirs) != 3 || dirs[1] != extra {
		t.Errorf("parseAllowDirs(extra) = %v, want [%s %s <ws>]", dirs, downloadDir, extra)
	}

	// non-existent entry resolves via deepest existing ancestor
	nonExistent := filepath.Join(extra, "made-later", "deep")
	dirs = parseAllowDirs(nonExistent, downloadDir)
	if len(dirs) != 2 || dirs[1] != nonExistent {
		t.Errorf("parseAllowDirs(nonexistent) = %v, want [%s %s] (deepest-ancestor resolved)", dirs, downloadDir, nonExistent)
	}

	// /dev/null-style entry (existing but not a resolvable dir chain) is skipped
	dirs = parseAllowDirs("/proc/1/nonexistent-subdir-xyz", downloadDir)
	if len(dirs) != 2 {
		t.Errorf("parseAllowDirs(unresolvable) = %v, want only downloadDir+1, got extra entries", dirs)
	}
}
