package core

import (
	"strings"
	"testing"
)

func TestUploadedFilesBlock_TimelineWithFiles(t *testing.T) {
	// Input deliberately out of chronological order: the block must sort by
	// UploadedAt ascending so "刚才的" is always the last line.
	block := UploadedFilesBlock([]UploadedFile{
		{Path: "/root/icg-ai-bot/uploads/b.log", OriginalName: "second.log", UploadedAt: "2026-08-30T20:30:00+08:00"},
		{Path: "/root/icg-ai-bot/uploads/a.zip", OriginalName: "icg-locating-ICGBUG-123.zip", UploadedAt: "2026-08-30T20:00:00+08:00"},
	})

	if !strings.Contains(block, "【本次上传文件（本会话 cc-connect 注入，按上传时间排序；仅可分析以下文件，uploads/ 目录其他文件不得使用）】") {
		t.Errorf("block missing timeline header: %q", block)
	}
	if !strings.Contains(block, "1) icg-locating-ICGBUG-123.zip — /root/icg-ai-bot/uploads/a.zip — 上传于 20:00:00") {
		t.Errorf("block missing file 1 (oldest first): %q", block)
	}
	if !strings.Contains(block, "2) second.log — /root/icg-ai-bot/uploads/b.log — 上传于 20:30:00") {
		t.Errorf("block missing file 2 (newest last): %q", block)
	}
	if !strings.Contains(block, "规则：结合对话上下文判断用户本次指哪个文件") {
		t.Errorf("block missing correlation rule: %q", block)
	}
	if !strings.Contains(block, "AskUserQuestion") {
		t.Errorf("block missing AskUserQuestion fallback: %q", block)
	}
	// ordering must be chronological regardless of input order
	if strings.Index(block, "icg-locating-ICGBUG-123.zip") > strings.Index(block, "second.log") {
		t.Errorf("block not sorted by UploadedAt ascending: %q", block)
	}
}

func TestUploadedFilesBlock_OriginalNameFallbackToPath(t *testing.T) {
	// An UploadedFile without an original name still renders something useful.
	block := UploadedFilesBlock([]UploadedFile{
		{Path: "/root/icg-ai-bot/uploads/m1.log", UploadedAt: "2026-08-30T20:00:00+08:00"},
	})
	if !strings.Contains(block, "1) /root/icg-ai-bot/uploads/m1.log — /root/icg-ai-bot/uploads/m1.log — 上传于 20:00:00") {
		t.Errorf("block missing fallback entry: %q", block)
	}
}

func TestUploadedFilesBlock_UnparseableTimestamp(t *testing.T) {
	// A broken UploadedAt must not fail the whole block; it degrades to the raw value.
	block := UploadedFilesBlock([]UploadedFile{
		{Path: "/x/y.log", OriginalName: "y.log", UploadedAt: "not-a-time"},
	})
	if !strings.Contains(block, "上传于 not-a-time") {
		t.Errorf("block should degrade to raw timestamp: %q", block)
	}
	if !strings.Contains(block, "1) y.log — /x/y.log —") {
		t.Errorf("block missing entry with raw timestamp: %q", block)
	}
}

func TestUploadedFilesBlock_Empty(t *testing.T) {
	block := UploadedFilesBlock([]UploadedFile{})
	if !strings.Contains(block, "本会话未从蓝信上传过文件") {
		t.Errorf("empty block should state no upload: %q", block)
	}
	if !strings.Contains(block, "不得使用 uploads/ 目录中任何文件") {
		t.Errorf("empty block should forbid uploads/ files: %q", block)
	}
}
