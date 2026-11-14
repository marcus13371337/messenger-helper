package helper

import (
	"context"
	"encoding/json"
)

const (
	defaultConversationLimit = 20
	maxConversationLimit     = 100
	defaultMessageLimit      = 50
	maxMessageLimit          = 200
)

func clampLimit(limit, fallback, ceiling int) int {
	if limit <= 0 {
		return fallback
	}
	return min(limit, ceiling)
}

func needConversation(id string) error {
	if id == "" {
		return Errorf(CodeBadRequest, "The request must say which conversation it is about.")
	}
	return nil
}

func conversations(ctx context.Context, c *call) (any, error) {
	var q ConversationsQuery
	if err := c.decode(&q); err != nil {
		return nil, err
	}
	q.Limit = clampLimit(q.Limit, defaultConversationLimit, maxConversationLimit)
	page, err := c.messenger.Conversations(ctx, q)
	if err != nil {
		return nil, err
	}
	if page.Conversations == nil {
		page.Conversations = []Conversation{}
	}
	return page, nil
}

func messages(ctx context.Context, c *call) (any, error) {
	var q MessagesQuery
	if err := c.decode(&q); err != nil {
		return nil, err
	}
	if err := needConversation(q.ConversationID); err != nil {
		return nil, err
	}
	q.Limit = clampLimit(q.Limit, defaultMessageLimit, maxMessageLimit)
	page, err := c.messenger.Messages(ctx, q)
	if err != nil {
		return nil, err
	}
	if page.Messages == nil {
		page.Messages = []Message{}
	}
	return page, nil
}

type sendResult struct {
	Messages []Message    `json:"messages"`
	Problem  *sendProblem `json:"problem,omitempty"`
}

type sendProblem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func conversationLane(params json.RawMessage) string {
	var target struct {
		ConversationID string `json:"conversation_id"`
	}
	_ = json.Unmarshal(params, &target)
	return target.ConversationID
}

func send(ctx context.Context, c *call) (any, error) {
	var out Outgoing
	if err := c.decode(&out); err != nil {
		return nil, err
	}
	if err := needConversation(out.ConversationID); err != nil {
		return nil, err
	}
	if out.Text == "" && len(out.Files) == 0 {
		return nil, Errorf(CodeBadRequest, "A message needs some text or at least one file.")
	}
	for i, file := range out.Files {
		resolved, err := resolveInFolder(c.folder, file.Path)
		if err != nil {
			return nil, err
		}
		out.Files[i].Path = resolved
	}
	sent, err := c.messenger.Send(ctx, out)
	if err != nil && len(sent) == 0 {
		return nil, err
	}
	result := sendResult{Messages: sent}
	if err != nil {
		plain := c.server.plain(err)
		result.Problem = &sendProblem{Code: plain.Code, Message: plain.Message}
	}
	return result, nil
}

func react(ctx context.Context, c *call) (any, error) {
	var r OutgoingReaction
	if err := c.decode(&r); err != nil {
		return nil, err
	}
	if err := needConversation(r.ConversationID); err != nil {
		return nil, err
	}
	if r.MessageID == "" {
		return nil, Errorf(CodeBadRequest, "The request must say which message to react to.")
	}
	return nil, c.messenger.React(ctx, r)
}

func markRead(ctx context.Context, c *call) (any, error) {
	var r ReadMark
	if err := c.decode(&r); err != nil {
		return nil, err
	}
	if err := needConversation(r.ConversationID); err != nil {
		return nil, err
	}
	return nil, c.messenger.MarkRead(ctx, r)
}
