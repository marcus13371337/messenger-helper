package meta

import (
	"bytes"
	"cmp"
	"context"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/methods"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	_ "golang.org/x/image/webp"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

const (
	sendAttempts     = 5
	sendReadyTimeout = 15 * time.Second
)

var errRefused = helper.Errorf(helper.CodeMetaRefused, "Messenger did not accept the message. Try again in a moment.")

type outgoingFile struct {
	name        string
	contentType string
	data        []byte
}

func readFiles(files []helper.OutgoingFile) ([]outgoingFile, error) {
	out := make([]outgoingFile, 0, len(files))
	for _, file := range files {
		data, err := os.ReadFile(file.Path)
		if err != nil {
			return nil, helper.Errorf(helper.CodeFileProblem, "The file %q could not be read.", filepath.Base(file.Path))
		}
		name := cmp.Or(file.Name, filepath.Base(file.Path))
		out = append(out, outgoingFile{name: name, contentType: contentTypeOf(name, data), data: data})
	}
	return out, nil
}

func contentTypeOf(name string, data []byte) string {
	sniffed := http.DetectContentType(data)
	if sniffed != "application/octet-stream" && !strings.HasPrefix(sniffed, "text/plain") {
		return strings.Split(sniffed, ";")[0]
	}
	if byName := mime.TypeByExtension(filepath.Ext(name)); byName != "" {
		return strings.Split(byName, ";")[0]
	}
	return strings.Split(sniffed, ";")[0]
}

func (c *Client) Send(ctx context.Context, out helper.Outgoing) ([]helper.Message, error) {
	t, err := c.findThread(ctx, out.ConversationID)
	if err != nil {
		return nil, err
	}
	files, err := readFiles(out.Files)
	if err != nil {
		return nil, err
	}
	if t.encrypted() {
		return c.sendEncrypted(ctx, t, out, files)
	}
	message, err := c.sendPlain(ctx, t, out, files)
	if err != nil {
		return nil, err
	}
	return []helper.Message{message}, nil
}

func (c *Client) sendPlain(ctx context.Context, t knownThread, out helper.Outgoing, files []outgoingFile) (helper.Message, error) {
	fb, err := c.plainClient(ctx)
	if err != nil {
		return helper.Message{}, err
	}
	otid := methods.GenerateEpochID()
	task := &socket.SendMessageTask{
		ThreadId:         t.key,
		Otid:             otid,
		Source:           table.MESSENGER_INBOX_IN_THREAD,
		InitiatingSource: table.FACEBOOK_INBOX,
		SendType:         table.TEXT,
		SyncGroup:        inboxSyncGroup,
		Text:             out.Text,
	}
	if out.ReplyToID != "" {
		replyTo, isPlain := strings.CutPrefix(out.ReplyToID, plainMessagePrefix)
		if !isPlain {
			return helper.Message{}, helper.Errorf(helper.CodeBadRequest, "The message being replied to is not from this conversation.")
		}
		task.ReplyMetaData = &socket.ReplyMetaData{ReplyMessageId: replyTo, ReplySourceType: 1}
	}
	attachments := []helper.Attachment{}
	for _, file := range files {
		uploaded, err := fb.GetHTTP().SendMercuryUploadRequest(ctx, t.key, &httpclient.MercuryUploadMedia{
			Filename: file.name, MimeType: file.contentType, MediaData: file.data,
		})
		if err != nil {
			return helper.Message{}, c.unexpected(err, "Could not upload a file")
		}
		attachmentID := uploaded.Payload.RealMetadata.GetFbId()
		if attachmentID == 0 {
			return helper.Message{}, helper.Errorf(helper.CodeMetaRefused, "Messenger did not accept the file %q.", file.name)
		}
		c.sessionDirty.Store(true)
		kind, _ := mediaKind(file.contentType)
		task.SendType = table.MEDIA
		task.AttachmentFBIds = append(task.AttachmentFBIds, attachmentID)
		attachments = append(attachments, helper.Attachment{
			ID: strconv.FormatInt(attachmentID, 10), Kind: kind, Type: file.contentType, Name: file.name, Size: int64(len(file.data)),
		})
	}
	resp, err := c.executeSend(ctx, fb, task, &socket.ThreadMarkReadTask{
		ThreadId: t.key, SyncGroup: inboxSyncGroup, LastReadWatermarkTs: time.Now().UnixMilli(),
	})
	if err != nil {
		return helper.Message{}, err
	}
	messageID := ""
	for _, replaced := range resp.LSReplaceOptimsiticMessage {
		if replaced.OfflineThreadingId == strconv.FormatInt(otid, 10) {
			messageID = replaced.MessageId
		}
	}
	if messageID == "" {
		c.log.Error().Int("failed_messages", len(resp.LSMarkOptimisticMessageFailed)).Int("failed_tasks", len(resp.LSHandleFailedTask)).Msg("Messenger did not confirm the message")
		return helper.Message{}, errRefused
	}
	sentAt := time.Now()
	if ms, err := methods.ParseMessageID(messageID); err == nil && time.Since(time.UnixMilli(ms)).Abs() < time.Hour {
		sentAt = time.UnixMilli(ms)
	}
	return helper.Message{
		ID:             plainMessageID(messageID),
		ConversationID: userID(t.key),
		SenderID:       userID(c.selfID),
		FromMe:         true,
		SentAt:         helper.Time(sentAt),
		Text:           out.Text,
		ReplyToID:      out.ReplyToID,
		Attachments:    attachments,
	}, nil
}

func (c *Client) executeSend(ctx context.Context, fb *messagix.Client, tasks ...socket.Task) (*table.LSTable, error) {
	var lastErr error
	for attempt := 0; attempt < sendAttempts && ctx.Err() == nil; attempt++ {
		if lastErr = fb.WaitUntilCanSendMessages(ctx, sendReadyTimeout); lastErr != nil {
			continue
		}
		resp, err := fb.ExecuteTasks(ctx, tasks...)
		if err == nil && resp != nil {
			return resp, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		return nil, errRefused
	}
	return nil, c.unexpected(lastErr, "Could not send the message")
}

func consumerMessage(content *waConsumerApplication.ConsumerApplication_Content) *waConsumerApplication.ConsumerApplication {
	return &waConsumerApplication.ConsumerApplication{Payload: &waConsumerApplication.ConsumerApplication_Payload{
		Payload: &waConsumerApplication.ConsumerApplication_Payload_Content{Content: content},
	}}
}

func textContent(text string) *waConsumerApplication.ConsumerApplication_Content {
	return &waConsumerApplication.ConsumerApplication_Content{
		Content: &waConsumerApplication.ConsumerApplication_Content_MessageText{MessageText: &waCommon.MessageText{Text: &text}},
	}
}

func reactionMessage(key *waCommon.MessageKey, emoji string) *waConsumerApplication.ConsumerApplication {
	return consumerMessage(&waConsumerApplication.ConsumerApplication_Content{
		Content: &waConsumerApplication.ConsumerApplication_Content_ReactionMessage{
			ReactionMessage: &waConsumerApplication.ConsumerApplication_ReactionMessage{
				Key: key, Text: &emoji, SenderTimestampMS: new(time.Now().UnixMilli()),
			},
		},
	})
}

type encryptedPart struct {
	content     *waConsumerApplication.ConsumerApplication_Content
	text        string
	attachments []helper.Attachment
}

func mediaKind(contentType string) (string, whatsmeow.MediaType) {
	switch {
	case strings.HasPrefix(contentType, "image/") && contentType != "image/gif":
		return "image", whatsmeow.MediaImage
	case strings.HasPrefix(contentType, "video/"):
		return "video", whatsmeow.MediaVideo
	case strings.HasPrefix(contentType, "audio/"):
		return "audio", whatsmeow.MediaAudio
	default:
		return "file", whatsmeow.MediaDocument
	}
}

func takesCaption(contentType string) bool {
	kind, _ := mediaKind(contentType)
	return kind == "image" || kind == "video"
}

func (c *Client) sendEncrypted(ctx context.Context, t knownThread, out helper.Outgoing, files []outgoingFile) ([]helper.Message, error) {
	wa, err := c.encryptedClient(ctx)
	if err != nil {
		return nil, err
	}
	metadata := &waMsgApplication.MessageApplication_Metadata{}
	if out.ReplyToID != "" {
		replyTo, ok := parseEncryptedMessageID(out.ReplyToID)
		if !ok {
			return nil, helper.Errorf(helper.CodeBadRequest, "The message being replied to is not from this conversation.")
		}
		metadata.QuotedMessage = &waMsgApplication.MessageApplication_Metadata_QuotedMessage{
			StanzaID: &replyTo.ID, Participant: new(replyTo.Sender.String()),
		}
	}
	caption := out.Text
	captionGoesOnFile := false
	for _, file := range files {
		captionGoesOnFile = captionGoesOnFile || takesCaption(file.contentType)
	}
	var parts []encryptedPart
	if caption != "" && !captionGoesOnFile {
		parts = append(parts, encryptedPart{content: textContent(caption), text: caption})
		caption = ""
	}
	for _, file := range files {
		fileCaption := ""
		if takesCaption(file.contentType) {
			fileCaption, caption = caption, ""
		}
		part, err := c.uploadEncrypted(ctx, wa, file, fileCaption)
		if err != nil {
			return nil, err
		}
		parts = append(parts, part)
	}
	chat := chatJID(t)
	sender := c.selfJID()
	sent := make([]helper.Message, 0, len(parts))
	for _, part := range parts {
		resp, err := wa.SendFBMessage(ctx, chat, consumerMessage(part.content), metadata)
		if err != nil {
			return sent, c.unexpected(err, "Could not send the encrypted message")
		}
		c.sessionDirty.Store(true)
		sent = append(sent, helper.Message{
			ID:             encryptedMessageID{Chat: chat, Sender: sender, ID: resp.ID}.String(),
			ConversationID: userID(t.key),
			SenderID:       userID(c.selfID),
			FromMe:         true,
			SentAt:         helper.Time(resp.Timestamp),
			Text:           part.text,
			ReplyToID:      out.ReplyToID,
			Attachments:    append([]helper.Attachment{}, part.attachments...),
			Encrypted:      true,
		})
	}
	return sent, nil
}

func (c *Client) uploadEncrypted(ctx context.Context, wa *whatsmeow.Client, file outgoingFile, caption string) (encryptedPart, error) {
	kind, mediaType := mediaKind(file.contentType)
	uploaded, err := wa.Upload(ctx, file.data, mediaType)
	if err != nil {
		return encryptedPart{}, c.unexpected(err, "Could not upload a file for the encrypted chat")
	}
	transport := &waMediaTransport.WAMediaTransport{
		Integral: &waMediaTransport.WAMediaTransport_Integral{
			FileSHA256:        uploaded.FileSHA256,
			MediaKey:          uploaded.MediaKey,
			FileEncSHA256:     uploaded.FileEncSHA256,
			DirectPath:        &uploaded.DirectPath,
			MediaKeyTimestamp: new(time.Now().Unix()),
		},
		Ancillary: &waMediaTransport.WAMediaTransport_Ancillary{
			FileLength: new(uint64(len(file.data))),
			Mimetype:   &file.contentType,
			ObjectID:   &uploaded.ObjectID,
		},
	}
	content := &waConsumerApplication.ConsumerApplication_Content{}
	switch kind {
	case "image":
		size, _, _ := image.DecodeConfig(bytes.NewReader(file.data))
		message := &waConsumerApplication.ConsumerApplication_ImageMessage{Caption: &waCommon.MessageText{Text: &caption}}
		err = message.Set(&waMediaTransport.ImageTransport{
			Integral:  &waMediaTransport.ImageTransport_Integral{Transport: transport},
			Ancillary: &waMediaTransport.ImageTransport_Ancillary{Height: new(uint32(size.Height)), Width: new(uint32(size.Width))},
		})
		content.Content = &waConsumerApplication.ConsumerApplication_Content_ImageMessage{ImageMessage: message}
	case "video":
		message := &waConsumerApplication.ConsumerApplication_VideoMessage{Caption: &waCommon.MessageText{Text: &caption}}
		err = message.Set(&waMediaTransport.VideoTransport{
			Integral:  &waMediaTransport.VideoTransport_Integral{Transport: transport},
			Ancillary: &waMediaTransport.VideoTransport_Ancillary{},
		})
		content.Content = &waConsumerApplication.ConsumerApplication_Content_VideoMessage{VideoMessage: message}
	case "audio":
		message := &waConsumerApplication.ConsumerApplication_AudioMessage{PTT: new(false)}
		err = message.Set(&waMediaTransport.AudioTransport{
			Integral:  &waMediaTransport.AudioTransport_Integral{Transport: transport},
			Ancillary: &waMediaTransport.AudioTransport_Ancillary{},
		})
		content.Content = &waConsumerApplication.ConsumerApplication_Content_AudioMessage{AudioMessage: message}
	default:
		message := &waConsumerApplication.ConsumerApplication_DocumentMessage{FileName: &file.name}
		err = message.Set(&waMediaTransport.DocumentTransport{
			Integral:  &waMediaTransport.DocumentTransport_Integral{Transport: transport},
			Ancillary: &waMediaTransport.DocumentTransport_Ancillary{},
		})
		content.Content = &waConsumerApplication.ConsumerApplication_Content_DocumentMessage{DocumentMessage: message}
	}
	if err != nil {
		return encryptedPart{}, err
	}
	return encryptedPart{
		content: content,
		text:    caption,
		attachments: []helper.Attachment{{
			ID:   uploaded.ObjectID,
			Kind: kind,
			Type: file.contentType,
			Name: file.name,
			Size: int64(len(file.data)),
			Ref: attachmentRef{
				Type: file.contentType, Name: file.name, DirectPath: uploaded.DirectPath, MediaKey: uploaded.MediaKey,
				FileSHA256: uploaded.FileSHA256, FileEncSHA256: uploaded.FileEncSHA256, MediaType: string(mediaType),
			}.encode(),
		}},
	}, nil
}
