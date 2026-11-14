package meta

import (
	"fmt"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

func chatKey(chat types.JID) (int64, bool) {
	key, err := strconv.ParseInt(chat.User, 10, 64)
	return key, err == nil && key != 0
}

func chatJID(t knownThread) types.JID {
	server := types.MessengerServer
	if !t.kind.IsOneToOne() {
		server = types.GroupServer
	}
	return types.JID{User: userID(t.key), Server: server}
}

func isGroupChat(chat types.JID) bool {
	return chat.Server != types.MessengerServer && chat.Server != types.DefaultUserServer
}

func messageKey(id encryptedMessageID, self int64) *waCommon.MessageKey {
	key := &waCommon.MessageKey{RemoteJID: new(id.Chat.String()), ID: new(id.ID)}
	if id.Sender.User == userID(self) {
		key.FromMe = new(true)
	}
	if isGroupChat(id.Chat) {
		key.Participant = new(id.Sender.String())
	}
	return key
}

func keyedMessageID(chat, sender types.JID, self int64, key *waCommon.MessageKey) string {
	author := sender.ToNonAD()
	if !key.GetFromMe() {
		if participant, err := types.ParseJID(key.GetParticipant()); err == nil && key.GetParticipant() != "" {
			author = participant.ToNonAD()
		} else if !isGroupChat(chat) {
			if author.User == userID(self) {
				author = chat
			} else {
				author = types.JID{User: userID(self), Server: types.MessengerServer}
			}
		}
	}
	return encryptedMessageID{Chat: chat, Sender: author, ID: key.GetID()}.String()
}

type encryptedContent struct {
	message  *helper.Message
	reaction *helper.Reaction
}

func convertEncrypted(evt *events.FBMessage, self int64) (encryptedContent, bool) {
	key, ok := chatKey(evt.Info.Chat)
	if !ok {
		return encryptedContent{}, false
	}
	conversationID := userID(key)
	senderID := evt.Info.Sender.User
	consumer, isConsumer := evt.Message.(*waConsumerApplication.ConsumerApplication)
	if isConsumer {
		if _, isData := consumer.GetPayload().GetPayload().(*waConsumerApplication.ConsumerApplication_Payload_ApplicationData); isData {
			return encryptedContent{}, false
		}
		switch content := consumer.GetPayload().GetContent().GetContent().(type) {
		case *waConsumerApplication.ConsumerApplication_Content_ReactionMessage:
			return encryptedContent{reaction: &helper.Reaction{
				ConversationID: conversationID,
				MessageID:      keyedMessageID(evt.Info.Chat, evt.Info.Sender, self, content.ReactionMessage.GetKey()),
				SenderID:       senderID,
				Emoji:          content.ReactionMessage.GetText(),
				At:             helper.Time(evt.Info.Timestamp),
			}}, true
		case *waConsumerApplication.ConsumerApplication_Content_EditMessage:
			return encryptedContent{}, false
		}
	}
	if evt.Message == nil {
		return encryptedContent{}, false
	}
	message := &helper.Message{
		ID:             encryptedMessageID{Chat: evt.Info.Chat, Sender: evt.Info.Sender, ID: evt.Info.ID}.String(),
		ConversationID: conversationID,
		SenderID:       senderID,
		FromMe:         senderID == userID(self),
		SentAt:         helper.Time(evt.Info.Timestamp),
		Attachments:    []helper.Attachment{},
		Encrypted:      true,
	}
	if quoted := evt.FBApplication.GetMetadata().GetQuotedMessage(); quoted != nil {
		if participant, err := types.ParseJID(quoted.GetParticipant()); err == nil {
			message.ReplyToID = encryptedMessageID{Chat: evt.Info.Chat, Sender: participant, ID: quoted.GetStanzaID()}.String()
		}
	}
	if !isConsumer || !fillFromConsumer(message, consumer.GetPayload().GetContent()) {
		message.Unsupported = true
	}
	return encryptedContent{message: message}, true
}

type mediaTransport interface {
	GetTransport() *waMediaTransport.WAMediaTransport
}

func fillFromConsumer(message *helper.Message, content *waConsumerApplication.ConsumerApplication_Content) bool {
	switch typed := content.GetContent().(type) {
	case *waConsumerApplication.ConsumerApplication_Content_MessageText:
		message.Text = typed.MessageText.GetText()
	case *waConsumerApplication.ConsumerApplication_Content_ExtendedTextMessage:
		message.Text = typed.ExtendedTextMessage.GetText().GetText()
	case *waConsumerApplication.ConsumerApplication_Content_ImageMessage:
		return fillImage(message, typed.ImageMessage)
	case *waConsumerApplication.ConsumerApplication_Content_VideoMessage:
		return fillVideo(message, typed.VideoMessage)
	case *waConsumerApplication.ConsumerApplication_Content_ViewOnceMessage:
		switch once := typed.ViewOnceMessage.GetViewOnceContent().(type) {
		case *waConsumerApplication.ConsumerApplication_ViewOnceMessage_ImageMessage:
			return fillImage(message, once.ImageMessage)
		case *waConsumerApplication.ConsumerApplication_ViewOnceMessage_VideoMessage:
			return fillVideo(message, once.VideoMessage)
		default:
			return false
		}
	case *waConsumerApplication.ConsumerApplication_Content_AudioMessage:
		transport, err := typed.AudioMessage.Decode()
		return err == nil && addMedia(message, transport.GetIntegral(), whatsmeow.MediaAudio, "audio", "")
	case *waConsumerApplication.ConsumerApplication_Content_DocumentMessage:
		transport, err := typed.DocumentMessage.Decode()
		return err == nil && addMedia(message, transport.GetIntegral(), whatsmeow.MediaDocument, "file", typed.DocumentMessage.GetFileName())
	case *waConsumerApplication.ConsumerApplication_Content_StickerMessage:
		transport, err := typed.StickerMessage.Decode()
		return err == nil && addMedia(message, transport.GetIntegral(), whatsmeow.MediaImage, "sticker", "")
	case *waConsumerApplication.ConsumerApplication_Content_LocationMessage:
		location := typed.LocationMessage.GetLocation()
		message.Text = strings.Join(nonEmpty(
			location.GetName(),
			typed.LocationMessage.GetAddress(),
			fmt.Sprintf("geo:%f,%f", location.GetDegreesLatitude(), location.GetDegreesLongitude()),
		), "\n")
	default:
		return false
	}
	return true
}

func fillImage(message *helper.Message, image *waConsumerApplication.ConsumerApplication_ImageMessage) bool {
	transport, err := image.Decode()
	message.Text = image.GetCaption().GetText()
	return err == nil && addMedia(message, transport.GetIntegral(), whatsmeow.MediaImage, "image", "")
}

func fillVideo(message *helper.Message, video *waConsumerApplication.ConsumerApplication_VideoMessage) bool {
	transport, err := video.Decode()
	message.Text = video.GetCaption().GetText()
	return err == nil && addMedia(message, transport.GetIntegral(), whatsmeow.MediaVideo, "video", "")
}

func addMedia(message *helper.Message, container mediaTransport, mediaType whatsmeow.MediaType, kind, name string) bool {
	transport := container.GetTransport()
	integral, ancillary := transport.GetIntegral(), transport.GetAncillary()
	if integral.GetDirectPath() == "" || len(integral.GetMediaKey()) == 0 {
		return false
	}
	message.Attachments = append(message.Attachments, helper.Attachment{
		ID:   firstNonEmpty(ancillary.GetObjectID(), message.ID),
		Kind: kind,
		Type: ancillary.GetMimetype(),
		Name: name,
		Size: int64(ancillary.GetFileLength()),
		Ref: attachmentRef{
			Type:          ancillary.GetMimetype(),
			Name:          name,
			DirectPath:    integral.GetDirectPath(),
			MediaKey:      integral.GetMediaKey(),
			FileSHA256:    integral.GetFileSHA256(),
			FileEncSHA256: integral.GetFileEncSHA256(),
			MediaType:     string(mediaType),
		}.encode(),
	})
	return true
}

func (c *Client) selfJID() types.JID {
	return types.JID{User: userID(c.selfID), Server: types.MessengerServer}
}
