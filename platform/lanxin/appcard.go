package lanxin

import (
	"fmt"
)

// Lanxin appCard endpoints: a dynamic card (isDynamic) can have its header
// status updated via /v1/messages/dynamic/update, and any message this bot
// sent can be withdrawn via /v1/messages/revoke.
const (
	dynamicUpdatePath = "/v1/messages/dynamic/update"
	revokeMessagePath = "/v1/messages/revoke"
)

// Progress-card status colours (status dot).
const (
	cardColourWorking = "#FFB116" // amber — still working
	cardColourDone    = "#198754" // green
	cardColourFailed  = "#dc3545" // red
)

// cardStatusHTML wraps header-status text in the div-style colour the lanxin
// client renders for an appCard's headStatusInfo.description.
//
// Verified against a live tenant: the client honours `color` here but ignores
// font-weight / <b> (bold is not available for header status), so colour is the
// only styling that takes effect — the separate `colour` field only colours the
// status DOT, not the text.
func cardStatusHTML(text, colour string) string {
	if colour == "" {
		return text
	}
	return `<div style="color:` + colour + `">` + text + `</div>`
}

// sendProgressCard posts a dynamic appCard and returns its msgId. The card
// carries a title/body plus the header status (short text + dot colour) that
// later updateProgressCard calls can change; body content itself is immutable
// in the lanxin API, so updates only touch the status/links.
func (p *Platform) sendProgressCard(rc replyContext, headTitle, bodyTitle, bodyContent, statusText, statusColour string) (string, error) {
	token, err := p.getAppToken()
	if err != nil {
		return "", fmt.Errorf("lanxin: sendProgressCard: get token: %w", err)
	}

	card := map[string]any{
		"headTitle":   headTitle,
		"bodyTitle":   bodyTitle,
		"bodyContent": bodyContent,
		"isDynamic":   true,
		"headStatusInfo": map[string]any{
			"description": cardStatusHTML(statusText, statusColour),
			"colour":      statusColour,
		},
	}
	payload := map[string]any{
		"msgType": "appCard",
		"msgData": map[string]any{"appCard": card},
	}
	path := botMsgPath
	if rc.chatType == "group" {
		path = groupMsgPath
		payload["groupId"] = rc.target
	} else {
		payload["userIdList"] = []string{rc.target}
	}
	return p.postAPIRaw(path, token, payload, "sendProgressCard")
}

// updateProgressCard updates a dynamic card's header status; last marks the
// card as final (the client stops showing it as in-progress).
func (p *Platform) updateProgressCard(msgID, statusText, statusColour string, last bool) error {
	if msgID == "" {
		return nil
	}
	token, err := p.getAppToken()
	if err != nil {
		return fmt.Errorf("lanxin: updateProgressCard: get token: %w", err)
	}

	update := map[string]any{}
	if last {
		update["isLastUpdate"] = true
	}
	if statusText != "" {
		update["headStatusInfo"] = map[string]any{
			"description": cardStatusHTML(statusText, statusColour),
			"colour":      statusColour,
		}
	}
	payload := map[string]any{
		"msgId":   msgID,
		"msgType": "appCard",
		"msgData": map[string]any{"appCardUpdateMsg": update},
	}
	return p.postAPI(dynamicUpdatePath, token, payload, "updateProgressCard")
}

// revokeMessages withdraws the given messages this bot sent in rc's chat.
// Group withdrawals additionally require the sender id (bot_sender_id option).
//
// NOTE: currently unused by the progress card on purpose — lanxin renders a
// visible "message recalled" hint to everyone in the chat, which reads as
// noise. Kept for callers that genuinely need to remove a message.

func (p *Platform) revokeMessages(rc replyContext, msgIDs []string) error {
	if len(msgIDs) == 0 {
		return nil
	}
	token, err := p.getAppToken()
	if err != nil {
		return fmt.Errorf("lanxin: revokeMessages: get token: %w", err)
	}

	payload := map[string]any{
		"messageIds": msgIDs,
		"chatType":   "bot",
	}
	if rc.chatType == "group" {
		payload["chatType"] = "group"
		if p.botSenderID != "" {
			payload["senderId"] = p.botSenderID
		}
	}
	return p.postAPI(revokeMessagePath, token, payload, "revokeMessages")
}
