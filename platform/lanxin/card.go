package lanxin

import (
	"context"
	"fmt"
	"strings"

	"github.com/chenhg5/cc-connect/core"
)

// renderCardText converts a core.Card into a copyable, nicely formatted
// Markdown text. Lanxin receives plain text, so we render structural card
// elements into readable text lines (headers, bullets, notes, action hints).
func renderCardText(card *core.Card) string {
	var b strings.Builder

	if card.Header != nil && strings.TrimSpace(card.Header.Title) != "" {
		b.WriteString("**" + card.Header.Title + "**\n")
		b.WriteString("——————————\n")
	}

	for _, el := range card.Elements {
		switch e := el.(type) {
		case core.CardMarkdown:
			t := strings.TrimSpace(e.Content)
			if t != "" {
				b.WriteString(t)
				b.WriteString("\n")
			}
		case core.CardDivider:
			b.WriteString("——————————\n")
		case core.CardListItem:
			t := strings.TrimSpace(e.Text)
			if t != "" {
				b.WriteString("• " + t + "\n")
			}
		case core.CardNote:
			t := strings.TrimSpace(e.Text)
			if t != "" {
				b.WriteString("\n_" + t + "_")
			}
		case core.CardActions:
			var btnTexts []string
			for _, btn := range e.Buttons {
				if strings.TrimSpace(btn.Text) != "" {
					btnTexts = append(btnTexts, btn.Text)
				}
			}
			if len(btnTexts) > 0 {
				b.WriteString("[ " + strings.Join(btnTexts, " | ") + " ]\n")
			}
		case core.CardSelect:
			opts := make([]string, 0, len(e.Options))
			for _, o := range e.Options {
				opts = append(opts, o.Text)
			}
			if len(opts) > 0 {
				b.WriteString("可选: " + strings.Join(opts, " / ") + "\n")
			}
		}
	}

	return strings.TrimRight(b.String(), "\n")
}

// SendCard renders the card as copyable Markdown text and sends it.
func (p *Platform) SendCard(ctx context.Context, rctx any, card *core.Card) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("lanxin: SendCard: invalid reply context type %T", rctx)
	}
	return p.sendCardText(rc, renderCardText(card))
}

// ReplyCard renders the card as copyable Markdown text and replies with it.
func (p *Platform) ReplyCard(ctx context.Context, rctx any, card *core.Card) error {
	rc, ok := rctx.(replyContext)
	if !ok {
		return fmt.Errorf("lanxin: ReplyCard: invalid reply context type %T", rctx)
	}
	return p.sendCardText(rc, renderCardText(card))
}

// sendCardText sends a plain text message (copyable) to the reply target.
// {{file:...}} markers are honored like in Reply (see replyWithFiles).
func (p *Platform) sendCardText(rc replyContext, text string) error {
	if text == "" {
		return nil
	}
	return p.replyWithFiles(rc, text)
}
