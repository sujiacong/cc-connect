package core

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// UploadedFilesBlock renders the system-side note injected into the agent
// input for a message that carries Message.UploadedFiles. The platform layer
// (currently lanxin) populates that field from the session's per-session
// upload marker, so the agent is told exactly which files the CURRENT session
// uploaded — without scanning the uploads/ directory or another session's
// files.
//
//   - files with entries → render a timeline (sorted by UploadedAt ascending)
//     with each file's original name, absolute path and upload time, forbid
//     anything else in uploads/, and tell the agent to correlate against the
//     conversation (AskUserQuestion when ambiguous).
//   - files empty (non-nil) → the session exists but has no upload; forbid
//     using any uploads/ file and tell the user to upload first.
//   - pending=true with files empty → the download is STILL RUNNING. The agent
//     must stop early and ask the user to retry, never act as if no file was
//     ever sent (the two are different statements, see Message.UploadPending).
//
// Callers only invoke this when Message.UploadedFiles != nil (nil means the
// platform does not track uploads, e.g. other IM platforms).
func UploadedFilesBlock(files []UploadedFile, pending bool) string {
	if len(files) == 0 {
		if pending {
			return "【本次上传文件】用户刚发送的文件尚未就绪（下载未完成或已失败），因此本轮没有任何可用文件。" +
				"请立即结束本轮：不要检索知识库、不要猜测文件内容、不得使用 uploads/ 目录中任何文件，" +
				"只回复一句话——文件尚未接收完成，请稍后回复「分析」，或重新上传后再试。"
		}
		return "【本次上传文件】本会话未从蓝信上传过文件，不得使用 uploads/ 目录中任何文件。"
	}

	// Sort by upload time ascending so the timeline reads oldest → newest
	// ("刚才的" is always the last line). RFC3339 timestamps are parsed so the
	// comparison is true chronological order (nanosecond precision, mixed UTC
	// offsets); unparseable entries degrade to string comparison.
	sorted := append([]UploadedFile(nil), files...)
	sort.SliceStable(sorted, func(i, j int) bool {
		ti, erri := time.Parse(time.RFC3339, sorted[i].UploadedAt)
		tj, errj := time.Parse(time.RFC3339, sorted[j].UploadedAt)
		if erri != nil || errj != nil {
			return sorted[i].UploadedAt < sorted[j].UploadedAt
		}
		return ti.Before(tj)
	})

	var b strings.Builder
	b.WriteString("【本次上传文件（本会话 cc-connect 注入，按上传时间排序；仅可分析以下文件，uploads/ 目录其他文件不得使用）】\n")
	for i, f := range sorted {
		name := f.OriginalName
		if name == "" {
			name = f.Path
		}
		ts := uploadTimeHuman(f.UploadedAt)
		fmt.Fprintf(&b, "%d) %s — %s — 上传于 %s\n", i+1, name, f.Path, ts)
	}
	b.WriteString("规则：结合对话上下文判断用户本次指哪个文件（\"刚才的\"/\"之前那个\"/\"最新的\"）；无法确定时用 AskUserQuestion 向用户确认。")
	return b.String()
}

// uploadTimeHuman formats an RFC3339 upload timestamp as local "15:04:05".
// An unparseable timestamp degrades to the raw string rather than failing the
// whole block.
func uploadTimeHuman(uploadedAt string) string {
	t, err := time.Parse(time.RFC3339, uploadedAt)
	if err != nil {
		return uploadedAt
	}
	return t.Format("15:04:05")
}
