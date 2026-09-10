package lanxin

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Milestone progress lines: the platform implements core.ToolActivityListener
// to surface coarse "正在查询 Jira… / 正在检索知识库…" progress while the agent
// works. Core broadcasts tool start/finish BEFORE its display-mode gating, so
// a quiet session still gets lightweight progress without re-enabling the raw
// thinking/tool text the display settings intentionally hide.

const (
	// milestoneStageIdleReset: when no stage line was sent for this long, the
	// next tool start is treated as a fresh stage even if the label matches the
	// last one — otherwise the first milestone of a later turn (same stage as
	// the previous turn) would be wrongly suppressed.
	milestoneStageIdleReset = 90 * time.Second
	milestoneFailedCooldown = 30 * time.Second
	milestonePersistentTimeout = 5 * time.Minute // drop idle tracker state
)

// milestoneState tracks the currently announced stage per reply target. The
// rule is "announce on stage change": a tool whose coarse stage label equals
// the last announced one is NOT re-announced (so 5 consecutive Bash calls send
// one line, not five), and only a stage switch produces a new line.
type milestoneState struct {
	lastLabel string // last announced stage label; "" = none yet
	lastAt    time.Time
	failedAt  time.Time
	updatedAt time.Time
}

// milestoneTracker holds per-target throttling state for milestone messages.
type milestoneTracker struct {
	mu    sync.Mutex
	state map[string]*milestoneState
}

// OnToolActivity implements core.ToolActivityListener: coarse tool lifecycle
// events become short, throttled milestone messages, so the user sees the
// agent making progress instead of silence until the final answer. Tool names
// are mapped to friendly stage labels; fine-grained tools are skipped and
// repeated stages never flood the chat.
func (p *Platform) OnToolActivity(ctx context.Context, replyCtx any, activity core.ToolActivity) error {
	rc, ok := replyCtx.(replyContext)
	if !ok || rc.target == "" || !p.milestonesEnabled {
		return nil
	}

	key := rc.chatType + ":" + rc.target

	p.milestones.mu.Lock()
	ts := p.milestones.state[key]
	if ts == nil {
		ts = &milestoneState{}
		p.milestones.state[key] = ts
	}
	now := time.Now()
	ts.updatedAt = now

	send := func(text string) {
		p.milestones.mu.Unlock()
		// Send on a goroutine so the engine event loop is never blocked by
		// lanxin HTTP latency; failures are only logged.
		go func() {
			if err := p.sendReplyText(rc, text); err != nil {
				slog.Debug("lanxin: milestone send failed", "target", key, "error", err)
			}
		}()
	}

	if activity.Starting {
		label := milestoneLabel(activity)
		if label == "" {
			p.milestones.mu.Unlock()
			return nil // tool is not worth broadcasting
		}
		// Stage-change rule: only announce when the coarse stage differs from
		// the last announced one (or the previous announcement is stale).
		sameStage := label == ts.lastLabel
		stale := now.Sub(ts.lastAt) > milestoneStageIdleReset
		if sameStage && !stale {
			p.milestones.mu.Unlock()
			return nil
		}
		ts.lastLabel = label
		ts.lastAt = now
		send(label)
		return nil
	}

	// Finish: only a failure is worth a line (short, throttled).
	if activity.Status != "failed" {
		p.milestones.mu.Unlock()
		return nil
	}
	if now.Sub(ts.failedAt) < milestoneFailedCooldown {
		p.milestones.mu.Unlock()
		return nil
	}
	ts.failedAt = now
	label := milestoneLabel(core.ToolActivity{Starting: true, ToolName: activity.ToolName})
	if label == "" {
		label = "该步骤"
	}
	send("⚠️ " + label + " 未成功，正在继续尝试…")
	return nil
}

// sweepMilestones drops tracker state idle for a while so the map does not
// grow without bound across sessions.
func (p *Platform) sweepMilestones() {
	cutoff := time.Now().Add(-milestonePersistentTimeout)
	p.milestones.mu.Lock()
	defer p.milestones.mu.Unlock()
	for k, ts := range p.milestones.state {
		if ts.updatedAt.Before(cutoff) {
			delete(p.milestones.state, k)
		}
	}
}

// watchMilestones periodically prunes milestone throttle state.
func (p *Platform) watchMilestones() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.sweepMilestones()
		}
	}
}

// milestoneLabel maps a tool name to a user-facing coarse stage, or "" when
// the tool is not worth broadcasting (fine-grained reads, private details).
func milestoneLabel(a core.ToolActivity) string {
	if !a.Starting {
		return ""
	}
	switch {
	case strings.HasPrefix(a.ToolName, "mcp__jira-qax__"):
		return "📋 正在查询 Jira…"
	case strings.HasPrefix(a.ToolName, "mcp__wiki-qax__"):
		return "📚 正在检索知识库文档…"
	case a.ToolName == "Bash":
		return "💻 正在执行分析命令…"
	case a.ToolName == "WebSearch":
		return "🌐 正在联网检索…"
	case a.ToolName == "Write" || a.ToolName == "Edit":
		return "✍️ 正在整理分析结果…"
	case a.ToolName == "Read" || a.ToolName == "Grep" || a.ToolName == "Glob":
		return "🔎 正在检索资料…"
	}
	return ""
}
