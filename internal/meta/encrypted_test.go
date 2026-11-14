package meta

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waArmadilloApplication"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waConsumerApplication"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"
	"go.mau.fi/whatsmeow/proto/waMsgApplication"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

var (
	adaJID  = types.JID{User: "100002", Server: types.MessengerServer}
	myJID   = types.JID{User: "100001", Server: types.MessengerServer}
	clubJID = types.JID{User: "900002", Server: types.GroupServer}
	sentAt  = time.UnixMilli(sentMs)
)

func consumer(content *waConsumerApplication.ConsumerApplication_Content) *waConsumerApplication.ConsumerApplication {
	return &waConsumerApplication.ConsumerApplication{Payload: &waConsumerApplication.ConsumerApplication_Payload{
		Payload: &waConsumerApplication.ConsumerApplication_Payload_Content{Content: content},
	}}
}

func fromAda(message *waConsumerApplication.ConsumerApplication) *events.FBMessage {
	return &events.FBMessage{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: adaJID, Sender: adaJID.ToNonAD()}, ID: "3EB0AA", Timestamp: sentAt},
		Message: message,
	}
}

func TestEncryptedTextBecomesAMessage(t *testing.T) {
	evt := fromAda(consumer(&waConsumerApplication.ConsumerApplication_Content{
		Content: &waConsumerApplication.ConsumerApplication_Content_MessageText{MessageText: &waCommon.MessageText{Text: new("psst")}},
	}))
	evt.FBApplication = &waMsgApplication.MessageApplication{Metadata: &waMsgApplication.MessageApplication_Metadata{
		QuotedMessage: &waMsgApplication.MessageApplication_Metadata_QuotedMessage{StanzaID: new("3EB099"), Participant: new(myJID.String())},
	}}

	got, ok := convertEncrypted(evt, me)

	message := got.message
	if !ok || message == nil || message.ID != "wa:100002@msgr:100002@msgr:3EB0AA" || message.ConversationID != "100002" || message.SenderID != "100002" {
		t.Fatalf("message %+v", message)
	}
	if message.Text != "psst" || !message.Encrypted || message.FromMe || message.Unsupported || !time.Time(message.SentAt).Equal(sentAt) {
		t.Fatalf("message %+v", message)
	}
	if message.ReplyToID != "wa:100002@msgr:100001@msgr:3EB099" {
		t.Fatalf("reply to %q", message.ReplyToID)
	}
}

func TestEncryptedImageCarriesWhatIsNeededToDownloadIt(t *testing.T) {
	image := &waConsumerApplication.ConsumerApplication_ImageMessage{Caption: &waCommon.MessageText{Text: new("our cat")}}
	err := image.Set(&waMediaTransport.ImageTransport{Integral: &waMediaTransport.ImageTransport_Integral{Transport: &waMediaTransport.WAMediaTransport{
		Integral:  &waMediaTransport.WAMediaTransport_Integral{DirectPath: new("/v/t62/cat"), MediaKey: []byte("media-key"), FileSHA256: []byte("sha"), FileEncSHA256: []byte("enc-sha")},
		Ancillary: &waMediaTransport.WAMediaTransport_Ancillary{Mimetype: new("image/jpeg"), FileLength: new(uint64(4321)), ObjectID: new("object-1")},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	got, _ := convertEncrypted(fromAda(consumer(&waConsumerApplication.ConsumerApplication_Content{
		Content: &waConsumerApplication.ConsumerApplication_Content_ImageMessage{ImageMessage: image},
	})), me)

	message := got.message
	if message.Text != "our cat" || len(message.Attachments) != 1 || message.Unsupported {
		t.Fatalf("message %+v", message)
	}
	attachment := message.Attachments[0]
	if attachment.ID != "object-1" || attachment.Kind != "image" || attachment.Type != "image/jpeg" || attachment.Size != 4321 {
		t.Fatalf("attachment %+v", attachment)
	}
	ref, err := decodeRef(attachment.Ref)
	if err != nil || ref.DirectPath != "/v/t62/cat" || string(ref.MediaKey) != "media-key" || string(ref.FileEncSHA256) != "enc-sha" || ref.MediaType != string(whatsmeow.MediaImage) {
		t.Fatalf("ref %+v %v", ref, err)
	}
}

func reactionTo(key *waCommon.MessageKey, emoji string) *waConsumerApplication.ConsumerApplication {
	return consumer(&waConsumerApplication.ConsumerApplication_Content{
		Content: &waConsumerApplication.ConsumerApplication_Content_ReactionMessage{
			ReactionMessage: &waConsumerApplication.ConsumerApplication_ReactionMessage{Key: key, Text: &emoji},
		},
	})
}

func TestEncryptedReactionPointsAtTheRightAuthor(t *testing.T) {
	toMine, _ := convertEncrypted(fromAda(reactionTo(&waCommon.MessageKey{ID: new("3EB0MINE")}, "❤️")), me)
	toOwn, _ := convertEncrypted(fromAda(reactionTo(&waCommon.MessageKey{ID: new("3EB0HERS"), FromMe: new(true)}, "😂")), me)
	groupEvt := fromAda(reactionTo(&waCommon.MessageKey{ID: new("3EB0GRACE"), Participant: new("100003@msgr")}, ""))
	groupEvt.Info.Chat = clubJID
	inGroup, _ := convertEncrypted(groupEvt, me)

	if r := toMine.reaction; r == nil || r.MessageID != "wa:100002@msgr:100001@msgr:3EB0MINE" || r.Emoji != "❤️" || r.SenderID != "100002" || r.ConversationID != "100002" {
		t.Fatalf("reaction to my message %+v", r)
	}
	if r := toOwn.reaction; r == nil || r.MessageID != "wa:100002@msgr:100002@msgr:3EB0HERS" {
		t.Fatalf("reaction to her own message %+v", r)
	}
	if r := inGroup.reaction; r == nil || r.MessageID != "wa:900002@g.us:100003@msgr:3EB0GRACE" || r.Emoji != "" || r.ConversationID != "900002" {
		t.Fatalf("removed group reaction %+v", r)
	}
}

func TestEncryptedContentTheProtocolCannotCarryIsMarkedUnsupported(t *testing.T) {
	evt := fromAda(nil)
	evt.Message = &waArmadilloApplication.Armadillo{}

	got, ok := convertEncrypted(evt, me)

	if !ok || got.message == nil || !got.message.Unsupported || got.message.Text != "" {
		t.Fatalf("got %+v", got.message)
	}
}

func TestEncryptedEditsAndRevokesAreSkipped(t *testing.T) {
	edit := fromAda(consumer(&waConsumerApplication.ConsumerApplication_Content{
		Content: &waConsumerApplication.ConsumerApplication_Content_EditMessage{EditMessage: &waConsumerApplication.ConsumerApplication_EditMessage{}},
	}))
	revoke := fromAda(&waConsumerApplication.ConsumerApplication{Payload: &waConsumerApplication.ConsumerApplication_Payload{
		Payload: &waConsumerApplication.ConsumerApplication_Payload_ApplicationData{},
	}})

	for _, evt := range []*events.FBMessage{edit, revoke} {
		if _, ok := convertEncrypted(evt, me); ok {
			t.Fatalf("event %+v was not skipped", evt.Message)
		}
	}
}

func TestMessageKeyForAReactionWeSend(t *testing.T) {
	direct, _ := parseEncryptedMessageID("wa:100002@msgr:100001@msgr:3EB0MINE")
	inGroup, _ := parseEncryptedMessageID("wa:900002@g.us:100003@msgr:3EB0GRACE")

	mine := messageKey(direct, me)
	graces := messageKey(inGroup, me)

	if !mine.GetFromMe() || mine.GetParticipant() != "" || mine.GetRemoteJID() != "100002@msgr" || mine.GetID() != "3EB0MINE" {
		t.Fatalf("key for my message %+v", mine)
	}
	if graces.GetFromMe() || graces.GetParticipant() != "100003@msgr" || graces.GetRemoteJID() != "900002@g.us" {
		t.Fatalf("key for a group message %+v", graces)
	}
}
