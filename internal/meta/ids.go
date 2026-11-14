package meta

import (
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/types"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

const (
	plainMessagePrefix     = "fb:"
	encryptedMessagePrefix = "wa:"
)

func userID(id int64) string { return strconv.FormatInt(id, 10) }

func plainMessageID(id string) string { return plainMessagePrefix + id }

type encryptedMessageID struct {
	Chat   types.JID
	Sender types.JID
	ID     string
}

func (m encryptedMessageID) String() string {
	return encryptedMessagePrefix + m.Chat.String() + ":" + m.Sender.ToNonAD().String() + ":" + m.ID
}

func parseEncryptedMessageID(id string) (encryptedMessageID, bool) {
	parts := strings.SplitN(strings.TrimPrefix(id, encryptedMessagePrefix), ":", 3)
	if !strings.HasPrefix(id, encryptedMessagePrefix) || len(parts) != 3 {
		return encryptedMessageID{}, false
	}
	chat, chatErr := types.ParseJID(parts[0])
	sender, senderErr := types.ParseJID(parts[1])
	if chatErr != nil || senderErr != nil || parts[2] == "" {
		return encryptedMessageID{}, false
	}
	return encryptedMessageID{Chat: chat, Sender: sender, ID: parts[2]}, true
}

func parseThreadKey(conversationID string) (int64, error) {
	key, err := strconv.ParseInt(conversationID, 10, 64)
	if err != nil || key == 0 {
		return 0, helper.Errorf(helper.CodeNotFound, "That conversation does not exist.")
	}
	return key, nil
}

type cursor struct {
	TimestampMs int64
	ID          string
}

func (c cursor) String() string {
	return strconv.FormatInt(c.TimestampMs, 10) + ":" + c.ID
}

func parseCursor(raw string) (cursor, error) {
	timestamp, id, ok := strings.Cut(raw, ":")
	ms, err := strconv.ParseInt(timestamp, 10, 64)
	if !ok || err != nil {
		return cursor{}, helper.Errorf(helper.CodeBadRequest, "That paging position did not come from an earlier answer.")
	}
	return cursor{TimestampMs: ms, ID: id}, nil
}
