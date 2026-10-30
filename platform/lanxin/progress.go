package lanxin

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

// Progress feedback while the agent works. Two styles, selected by the
// `milestones` platform option:
//
//	milestones = true                    → text style: sparse stage lines
//	milestones = true + style = "card"   → card style: one dynamic appCard whose
//	                                       header status follows the stage and is
//	                                       finalised (kept, not withdrawn) at the
//	                                       end of the turn
//
// Both are driven by core.ToolActivityListener (tool start/finish broadcast
// before display-mode gating) plus core.TurnLifecycleListener (turn end), so a
// quiet session still gets coarse progress without re-enabling thinking text.

const (
	// milestoneStageIdleReset: when no stage line was sent for this long, the
	// next tool start is treated as a fresh stage even if the label matches —
	// otherwise the first milestone of a later turn (same stage as the previous
	// turn) would be wrongly suppressed.
	milestoneStageIdleReset = 90 * time.Second
	milestoneFailedCooldown = 30 * time.Second
	// milestonePersistentTimeout must stay well ABOVE the longest plausible
	// turn: agent turns routinely run 20-30 minutes, and a card that goes quiet
	// that long (model thinking between tool calls) must NOT be mistaken for a
	// crashed turn — that made a successful run show "已中断" while the answer
	// was still being produced. This is only a backstop for a crashed process.
	milestonePersistentTimeout = 45 * time.Minute

	// progress card copy. bodyContent may contain simple HTML — <b> renders
	// (verified), while the header status field does not support bold.
	progressCardHeadTitle    = "ICG 智能助手"
	progressCardBodyTitle    = "处理进度"
	progressCardBody         = "<b>正在检索资料并分析</b>，请稍候…"
	progressCardDoneText     = "已完成"
	progressCardFailText     = "已中断"
	progressCardStartingText = "正在处理…"

	// progressCardHeartbeat keeps a live card moving while the agent is busy
	// without emitting tool events (the model can think for many minutes
	// between calls). Every interval with no stage update the header status is
	// refreshed to progressCardHeartbeatText so the turn never looks stalled.
	progressCardHeartbeat     = 3 * time.Minute
	progressCardHeartbeatText = "仍在分析…"

	// progressCardDelay holds the card back at turn start: a short turn (a
	// greeting answered in a few seconds) should not flash a card at all. The
	// card is created once this delay elapses with the turn still running, or
	// immediately when the first tool activity appears (which means real work).
	progressCardDelay = 20 * time.Second
)

// milestoneState tracks the current stage (and, in card style, the live card)
// for one reply target.
type milestoneState struct {
	lastLabel string // last announced stage label; "" = none yet
	lastAt    time.Time
	failedAt  time.Time
	updatedAt time.Time

	// card style state.
	cardMsgID   string       // msgId of the live progress card ("" = none)
	cardPending bool         // a create op is queued/running (msgId not known yet)
	cardRC      replyContext // reply target used when the card was created
	queue       []cardOp     // serialized card operations (protected by tracker mu)
	draining    bool         // a worker is draining the queue
	timer       *time.Timer  // delayed card creation (short turns never create one)
}

// cardOp is one queued progress-card operation; ops run strictly in order on a
// per-target worker so create→update→finalise never race.
type cardOp struct {
	kind  string // "create" | "update" | "finalize"
	label string
	ok    bool
}

// milestoneTracker holds per-target throttling/card state.
type milestoneTracker struct {
	mu    sync.Mutex
	state map[string]*milestoneState
}

// progressReplyContext strips the message id so progress lines are NOT sent as
// quoted replies — only real answers (and receipts) quote the user's message.
func progressReplyContext(rc replyContext) replyContext {
	rc.msgID = ""
	return rc
}

// OnTurnStart implements core.TurnStartListener: in card style the progress
// card is created the moment the turn begins, so it shows up as fast as an
// instant reply would — instead of waiting for the first tool call (which can
// be tens of seconds after the agent starts thinking).
func (p *Platform) OnTurnStart(ctx context.Context, replyCtx any) error {
	rc, ok := replyCtx.(replyContext)
	if !ok || rc.target == "" || !p.milestonesEnabled || p.milestonesStyle != "card" {
		return nil
	}
	key := rc.chatType + ":" + rc.target

	p.milestones.mu.Lock()
	defer p.milestones.mu.Unlock()
	ts := p.milestones.state[key]
	if ts == nil {
		ts = &milestoneState{}
		p.milestones.state[key] = ts
	}
	ts.updatedAt = time.Now()
	if ts.cardMsgID != "" || ts.cardPending {
		return nil // a card already exists (or is being created) for this turn
	}
	// Hold the card back briefly: a greeting answered in a few seconds should
	// not flash a card. It appears when the delay elapses with the turn still
	// running, or at once on the first tool activity.
	ts.cardRC = rc
	if ts.timer != nil {
		ts.timer.Stop()
	}
	ts.timer = time.AfterFunc(progressCardDelay, func() { p.startCardAfterDelay(key) })
	return nil
}

// startCardAfterDelay creates the progress card once progressCardDelay has
// passed since the turn began, provided the turn is still running and no card
// exists yet (tool activity may have created one earlier).
func (p *Platform) startCardAfterDelay(key string) {
	p.milestones.mu.Lock()
	defer p.milestones.mu.Unlock()
	ts := p.milestones.state[key]
	if ts == nil || ts.cardMsgID != "" || ts.cardPending {
		return
	}
	ts.timer = nil
	ts.updatedAt = time.Now()
	ts.queue = append(ts.queue, cardOp{kind: "create", label: progressCardStartingText})
	p.startCardDrainLocked(key, ts)
}

// OnToolActivity implements core.ToolActivityListener: it turns coarse tool
// lifecycle events into progress feedback (text stage lines, or a single
// dynamic appCard whose header follows the stage). Runs before the engine's
// display-mode gating, so a quiet session still sees progress.
func (p *Platform) OnToolActivity(ctx context.Context, replyCtx any, activity core.ToolActivity) error {
	rc, ok := replyCtx.(replyContext)
	if !ok || rc.target == "" || !p.milestonesEnabled {
		// 早退曾完全静默，卡片不出现时无从判断是哪一项不满足；保留一条
		// debug 作为可复用的判据（enabled=false → 配置未生效；reply_ctx_ok=false →
		// 引擎传了非 lanxin replyContext）。
		slog.Debug("lanxin: milestone skipped",
			"reply_ctx_ok", ok, "target", rc.target, "enabled", p.milestonesEnabled)
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

	if activity.Starting {
		label := milestoneLabel(activity)
		if label == "" {
			p.milestones.mu.Unlock()
			return nil // tool is not worth broadcasting
		}
		// Stage-change rule: only announce when the coarse stage differs from
		// the last announced one (or the previous announcement is stale), so
		// e.g. five consecutive Bash calls produce one line, not five.
		sameStage := label == ts.lastLabel
		stale := now.Sub(ts.lastAt) > milestoneStageIdleReset
		if sameStage && !stale {
			p.milestones.mu.Unlock()
			return nil
		}
		ts.lastLabel = label
		ts.lastAt = now

		if p.milestonesStyle == "card" {
			ts.cardRC = rc
			// Real work started: cancel the start delay and show the card now.
			if ts.timer != nil {
				ts.timer.Stop()
				ts.timer = nil
			}
			kind := "update"
			// Create only when no card exists yet; cardPending covers the window
			// where the create op is still queued/running (msgId unknown), so a
			// tool event racing the turn-start create cannot spawn a second card.
			if ts.cardMsgID == "" && !ts.cardPending {
				kind = "create"
				ts.cardPending = true
			}
			ts.queue = append(ts.queue, cardOp{kind: kind, label: milestoneCardLabel(activity)})
			p.startCardDrainLocked(key, ts)
			p.milestones.mu.Unlock()
			return nil
		}

		text := label
		p.milestones.mu.Unlock()
		go func() {
			if err := p.sendReplyText(progressReplyContext(rc), text); err != nil {
				slog.Debug("lanxin: milestone send failed", "target", key, "error", err)
			}
		}()
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

	if p.milestonesStyle == "card" {
		p.milestones.mu.Unlock()
		return nil // the card keeps its working state; failures surface at turn end
	}

	label := milestoneLabel(core.ToolActivity{Starting: true, ToolName: activity.ToolName})
	if label == "" {
		label = "该步骤"
	}
	p.milestones.mu.Unlock()
	go func() {
		if err := p.sendReplyText(progressReplyContext(rc), "⚠️ "+label+" 未成功，正在继续尝试…"); err != nil {
			slog.Debug("lanxin: milestone send failed", "target", key, "error", err)
		}
	}()
	return nil
}

// OnTurnEnd implements core.TurnLifecycleListener: in card style the live
// progress card is marked final (isLastUpdate) so it stops showing as
// in-progress. The card is kept — withdrawing it would broadcast a recall
// hint into the chat.
func (p *Platform) OnTurnEnd(ctx context.Context, replyCtx any, ok bool) error {
	rc, isRC := replyCtx.(replyContext)
	if !isRC || rc.target == "" || !p.milestonesEnabled || p.milestonesStyle != "card" {
		return nil
	}
	key := rc.chatType + ":" + rc.target

	p.milestones.mu.Lock()
	defer p.milestones.mu.Unlock()
	ts := p.milestones.state[key]
	if ts == nil {
		return nil
	}
	if ts.cardMsgID == "" && !ts.cardPending && len(ts.queue) == 0 {
		// Short turn that finished before the card was ever created: cancel the
		// pending delay so no card appears after the answer, and skip finalising.
		if ts.timer != nil {
			ts.timer.Stop()
			ts.timer = nil
		}
		return nil
	}
	ts.cardRC = rc
	ts.updatedAt = time.Now()
	ts.queue = append(ts.queue, cardOp{kind: "finalize", ok: ok})
	p.startCardDrainLocked(key, ts)
	return nil
}

// startCardDrainLocked launches the per-target card worker when it is idle.
// Callers must hold p.milestones.mu.
func (p *Platform) startCardDrainLocked(key string, ts *milestoneState) {
	if ts.draining {
		return
	}
	ts.draining = true
	go p.drainCardOps(key, ts)
}

// drainCardOps runs queued card operations strictly in order (create → update
// → finalize), so HTTP latency never blocks the engine's event loop and a
// finalize can never overtake the create it belongs to.
func (p *Platform) drainCardOps(key string, ts *milestoneState) {
	for {
		p.milestones.mu.Lock()
		if len(ts.queue) == 0 {
			ts.draining = false
			p.milestones.mu.Unlock()
			return
		}
		op := ts.queue[0]
		ts.queue = ts.queue[1:]
		rc := ts.cardRC
		cardID := ts.cardMsgID
		p.milestones.mu.Unlock()

		switch op.kind {
		case "create":
			id, err := p.sendProgressCard(rc, progressCardHeadTitle, progressCardBodyTitle, progressCardBody, op.label, cardColourWorking)
			p.milestones.mu.Lock()
			ts.cardPending = false
			if err == nil {
				ts.cardMsgID = id
			}
			p.milestones.mu.Unlock()
			if err != nil {
				slog.Debug("lanxin: progress card create failed", "target", key, "error", err)
				continue
			}
			slog.Debug("lanxin: progress card created", "target", key, "msg_id", id)
		case "update":
			if cardID == "" {
				continue
			}
			if err := p.updateProgressCard(cardID, op.label, cardColourWorking, false); err != nil {
				slog.Debug("lanxin: progress card update failed", "target", key, "error", err)
			}
		case "finalize":
			if cardID == "" {
				continue
			}
			text, colour := progressCardDoneText, cardColourDone
			if !op.ok {
				text, colour = progressCardFailText, cardColourFailed
			}
			if err := p.updateProgressCard(cardID, text, colour, true); err != nil {
				slog.Debug("lanxin: progress card finalize failed", "target", key, "error", err)
			}
			// The card is intentionally NOT withdrawn: lanxin shows a visible
			// "message recalled" hint to everyone in the chat, which reads as
			// noise. It stays as a completed status card instead.
			p.milestones.mu.Lock()
			ts.cardPending = false
			if ts.cardMsgID == cardID {
				ts.cardMsgID = ""
			}
			p.milestones.mu.Unlock()
		}
	}
}

// sweepMilestones drops tracker state idle for a while so the map does not
// grow without bound, and finalises any progress card left behind by a turn
// that never reported its end (crashed agent process, restart). The card is
// finalised rather than withdrawn — a recall shows a visible hint in lanxin.
func (p *Platform) sweepMilestones() {
	cutoff := time.Now().Add(-milestonePersistentTimeout)
	var stale []string

	p.milestones.mu.Lock()
	for k, ts := range p.milestones.state {
		if !ts.updatedAt.Before(cutoff) {
			continue
		}
		if ts.cardMsgID != "" {
			stale = append(stale, ts.cardMsgID)
			ts.cardMsgID = ""
		}
		ts.cardPending = false // a stuck create must not block the next turn
		if len(ts.queue) == 0 {
			if ts.timer != nil {
				ts.timer.Stop()
				ts.timer = nil
			}
			delete(p.milestones.state, k)
		}
	}
	p.milestones.mu.Unlock()

	for _, id := range stale {
		if err := p.updateProgressCard(id, progressCardFailText, cardColourFailed, true); err != nil {
			slog.Debug("lanxin: stale progress card finalize failed", "error", err)
		}
	}
}

// touchProgressCards refreshes any live progress card that has been silent for
// progressCardHeartbeat, so a long turn (the model thinking between tool calls
// can take many minutes) never looks stalled to the user. Refreshing also
// updates updatedAt, which keeps an actively-running turn's card away from the
// crash backstop in sweepMilestones.
func (p *Platform) touchProgressCards() {
	if !p.milestonesEnabled || p.milestonesStyle != "card" {
		return
	}
	now := time.Now()

	p.milestones.mu.Lock()
	defer p.milestones.mu.Unlock()
	for key, ts := range p.milestones.state {
		if ts.cardMsgID == "" || now.Sub(ts.updatedAt) < progressCardHeartbeat {
			continue
		}
		ts.updatedAt = now
		ts.queue = append(ts.queue, cardOp{kind: "update", label: progressCardHeartbeatText})
		p.startCardDrainLocked(key, ts)
	}
}

// watchMilestones periodically heartbeats live cards and prunes stale state.
func (p *Platform) watchMilestones() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-ticker.C:
			p.touchProgressCards()
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
	// qagent 通过 ACP 上报的工具名是 Shell/Process/WriteFile/LoadSkill（不是 Claude 的
	// Bash/Write/Read），旧白名单与它交集为空，导致卡片链路全程静默跳过（无日志）。
	case a.ToolName == "Bash" || a.ToolName == "Shell":
		return "💻 正在执行分析命令…"
	case a.ToolName == "WebSearch":
		return "🌐 正在联网检索…"
	case a.ToolName == "Write" || a.ToolName == "Edit" || a.ToolName == "WriteFile":
		return "✍️ 正在整理分析结果…"
	case a.ToolName == "Read" || a.ToolName == "Grep" || a.ToolName == "Glob":
		return "🔎 正在检索资料…"
	case a.ToolName == "Process":
		return "⏳ 正在等待后台任务…"
	case a.ToolName == "LoadSkill":
		return "📚 正在加载技能…"
	}
	// 兜底：未知工具也要出进度，否则换 agent 后会再次「静默无卡片」。
	return "🔎 正在处理…"
}

// milestoneCardLabel is the short card-header variant of milestoneLabel: the
// appCard status description is limited to ~30 bytes, so no emoji and no long
// phrasing.
func milestoneCardLabel(a core.ToolActivity) string {
	switch {
	case strings.HasPrefix(a.ToolName, "mcp__jira-qax__"):
		return "正在查询 Jira…"
	case strings.HasPrefix(a.ToolName, "mcp__wiki-qax__"):
		return "正在检索知识库…"
	case a.ToolName == "Bash" || a.ToolName == "Shell":
		return "正在执行分析…"
	case a.ToolName == "WebSearch":
		return "正在联网检索…"
	case a.ToolName == "Write" || a.ToolName == "Edit" || a.ToolName == "WriteFile":
		return "正在整理结果…"
	case a.ToolName == "Read" || a.ToolName == "Grep" || a.ToolName == "Glob":
		return "正在检索资料…"
	case a.ToolName == "Process":
		return "等待后台任务…"
	case a.ToolName == "LoadSkill":
		return "加载技能…"
	}
	return "正在处理…"
}
