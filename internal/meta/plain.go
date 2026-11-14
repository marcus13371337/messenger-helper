package meta

import (
	"cmp"
	"strings"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

func (d *directory) plainMessage(t *thread, wrapped *table.WrappedMessage) helper.Message {
	message := helper.Message{
		ID:             plainMessageID(wrapped.MessageId),
		ConversationID: userID(t.key),
		SenderID:       userID(wrapped.SenderId),
		FromMe:         wrapped.SenderId == d.self,
		SentAt:         millis(wrapped.TimestampMs),
		Text:           wrapped.Text,
		Attachments:    []helper.Attachment{},
	}
	if wrapped.ReplySourceId != "" {
		message.ReplyToID = plainMessageID(wrapped.ReplySourceId)
	}
	for _, blob := range wrapped.BlobAttachments {
		message.Attachments = append(message.Attachments, plainAttachment(
			blob.AttachmentFbid, blob.AttachmentType, blob.Filename, blob.Filesize,
			blob.PlayableUrl, cmp.Or(blob.PlayableUrlMimeType, blob.AttachmentMimeType),
			blob.PreviewUrl, blob.PreviewUrlMimeType,
		))
	}
	for _, legacy := range wrapped.Attachments {
		message.Attachments = append(message.Attachments, plainAttachment(
			legacy.AttachmentFbid, legacy.AttachmentType, legacy.Filename, legacy.Filesize,
			legacy.PlayableUrl, cmp.Or(legacy.PlayableUrlMimeType, legacy.AttachmentMimeType),
			legacy.PreviewUrl, legacy.PreviewUrlMimeType,
		))
	}
	for _, sticker := range wrapped.Stickers {
		message.Attachments = append(message.Attachments, plainAttachment(
			sticker.AttachmentFbid, table.AttachmentTypeSticker, sticker.AccessibilitySummaryText, 0,
			sticker.PlayableUrl, sticker.PlayableUrlMimeType,
			sticker.PreviewUrl, sticker.PreviewUrlMimeType,
		))
	}
	for _, xma := range wrapped.XMAAttachments {
		message.Text = appendLine(message.Text, sharedLink(xma))
	}
	for _, reaction := range wrapped.Reactions {
		message.Reactions = append(message.Reactions, helper.Reacted{SenderID: userID(reaction.ActorId), Emoji: reaction.Reaction})
	}
	return message
}

func plainAttachment(id string, kind table.AttachmentType, name string, size int64, playableURL, playableType, previewURL, previewType string) helper.Attachment {
	url, contentType := playableURL, playableType
	if url == "" {
		url, contentType = previewURL, previewType
	}
	return helper.Attachment{
		ID:   id,
		Kind: attachmentKind(kind),
		Type: contentType,
		Name: name,
		Size: size,
		Ref:  attachmentRef{URL: url, Type: contentType, Name: name}.encode(),
	}
}

func attachmentKind(kind table.AttachmentType) string {
	switch kind {
	case table.AttachmentTypeImage, table.AttachmentTypeAnimatedImage, table.AttachmentTypeEphemeralImage:
		return "image"
	case table.AttachmentTypeVideo, table.AttachmentTypeEphemeralVideo:
		return "video"
	case table.AttachmentTypeAudio, table.AttachmentTypeSoundBite:
		return "audio"
	case table.AttachmentTypeSticker, table.AttachmentTypeSelfieSticker, table.AttachmentTypeThirdPartySticker:
		return "sticker"
	default:
		return "file"
	}
}

func sharedLink(xma *table.WrappedXMA) string {
	return strings.TrimSpace(strings.Join(nonEmpty(xma.TitleText, xma.ActionUrl), "\n"))
}

func appendLine(text, line string) string {
	if line == "" || strings.Contains(text, line) {
		return text
	}
	if text == "" {
		return line
	}
	return text + "\n" + line
}

func nonEmpty(values ...string) []string {
	out := values[:0]
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}
