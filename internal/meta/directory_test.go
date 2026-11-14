package meta

import (
	"testing"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

const (
	me     = int64(100001)
	ada    = int64(100002)
	grace  = int64(100003)
	group  = int64(900001)
	sentMs = int64(1794643200000)
)

func syncedDirectory() (*directory, []helper.Event) {
	d := newDirectory(me)
	events := d.ingest(&table.LSTable{
		LSDeleteThenInsertContact: []*table.LSDeleteThenInsertContact{{Id: ada, Name: "Ada", ProfilePictureUrl: "https://example.test/ada.jpg"}},
		LSVerifyContactRowExists:  []*table.LSVerifyContactRowExists{{ContactId: grace, Name: "Grace"}},
		LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{
			{ThreadKey: ada, ThreadType: table.ENCRYPTED_OVER_WA_ONE_TO_ONE, LastActivityTimestampMs: sentMs, LastReadWatermarkTimestampMs: sentMs},
			{ThreadKey: group, ThreadType: table.GROUP_THREAD, ThreadName: "Book club", LastActivityTimestampMs: sentMs + 1000, LastReadWatermarkTimestampMs: sentMs},
		},
		LSAddParticipantIdToGroupThread: []*table.LSAddParticipantIdToGroupThread{
			{ThreadKey: group, ContactId: me}, {ThreadKey: group, ContactId: ada}, {ThreadKey: group, ContactId: grace},
		},
	})
	return d, events
}

func only[T any](t *testing.T, events []helper.Event, method string) T {
	t.Helper()
	var found []T
	for _, evt := range events {
		if evt.Method == method {
			found = append(found, evt.Params.(T))
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one %s event, got %d in %+v", method, len(found), events)
	}
	return found[0]
}

func TestThreadSyncDescribesEachConversation(t *testing.T) {
	_, events := syncedDirectory()

	if len(events) != 2 {
		t.Fatalf("want 2 conversation events, got %+v", events)
	}
	direct := events[0].Params.(helper.Conversation)
	club := events[1].Params.(helper.Conversation)
	if direct.ID != "100002" || direct.Name != "Ada" || direct.Kind != helper.KindDirect || !direct.Encrypted || direct.Unread {
		t.Fatalf("direct conversation %+v", direct)
	}
	if direct.Participants[0].AvatarURL != "https://example.test/ada.jpg" {
		t.Fatalf("direct participants %+v", direct.Participants)
	}
	if club.ID != "900001" || club.Name != "Book club" || club.Kind != helper.KindGroup || club.Encrypted || !club.Unread {
		t.Fatalf("group conversation %+v", club)
	}
	if len(club.Participants) != 2 || club.Participants[0].Name != "Ada" || club.Participants[1].Name != "Grace" {
		t.Fatalf("group participants %+v", club.Participants)
	}
}

func TestNewMessageBecomesAMessageEvent(t *testing.T) {
	d, _ := syncedDirectory()

	events := d.ingest(&table.LSTable{
		LSInsertMessage: []*table.LSInsertMessage{{
			ThreadKey: group, MessageId: "mid.$new", SenderId: ada, TimestampMs: sentMs + 5000, Text: "look at this", ReplySourceId: "mid.$old",
		}},
		LSInsertBlobAttachment: []*table.LSInsertBlobAttachment{{
			MessageId: "mid.$new", AttachmentFbid: "att1", AttachmentType: table.AttachmentTypeImage, Filename: "cat.jpg", Filesize: 1234,
			PreviewUrl: "https://example.test/cat.jpg", PreviewUrlMimeType: "image/jpeg",
		}},
	})

	message := only[helper.Message](t, events, "message")
	if message.ID != "fb:mid.$new" || message.ConversationID != "900001" || message.SenderID != "100002" || message.FromMe || message.Encrypted {
		t.Fatalf("message %+v", message)
	}
	if message.Text != "look at this" || message.ReplyToID != "fb:mid.$old" || !time.Time(message.SentAt).Equal(time.UnixMilli(sentMs+5000)) {
		t.Fatalf("message %+v", message)
	}
	attachment := message.Attachments[0]
	if attachment.ID != "att1" || attachment.Kind != "image" || attachment.Type != "image/jpeg" || attachment.Name != "cat.jpg" || attachment.Size != 1234 {
		t.Fatalf("attachment %+v", attachment)
	}
	ref, err := decodeRef(attachment.Ref)
	if err != nil || ref.URL != "https://example.test/cat.jpg" {
		t.Fatalf("ref %+v, %v", ref, err)
	}
}

func TestOwnAndUnsentMessages(t *testing.T) {
	d, _ := syncedDirectory()

	events := d.ingest(&table.LSTable{LSInsertMessage: []*table.LSInsertMessage{
		{ThreadKey: group, MessageId: "mid.$mine", SenderId: me, TimestampMs: sentMs, Text: "hi"},
		{ThreadKey: group, MessageId: "mid.$gone", SenderId: ada, TimestampMs: sentMs, IsUnsent: true},
	}})

	message := only[helper.Message](t, events, "message")
	if message.ID != "fb:mid.$mine" || !message.FromMe {
		t.Fatalf("message %+v", message)
	}
}

func history(ids ...string) *table.LSTable {
	tbl := &table.LSTable{LSInsertNewMessageRange: []*table.LSInsertNewMessageRange{{
		ThreadKey: group, MinTimestampMs: sentMs, MinMessageId: ids[0], HasMoreBefore: true,
	}}}
	for i, id := range ids {
		tbl.LSUpsertMessage = append(tbl.LSUpsertMessage, &table.LSUpsertMessage{
			ThreadKey: group, MessageId: id, SenderId: ada, TimestampMs: sentMs + int64(i)*1000, Text: id,
		})
	}
	return tbl
}

func TestHistoryIsRememberedWithoutMessageEvents(t *testing.T) {
	d, _ := syncedDirectory()
	tbl := history("mid.$a", "mid.$b", "mid.$c")
	tbl.LSUpsertReaction = []*table.LSUpsertReaction{{ThreadKey: group, MessageId: "mid.$b", ActorId: grace, Reaction: "👍"}}

	events := d.ingest(tbl)

	if len(events) != 0 {
		t.Fatalf("history produced events %+v", events)
	}
	newest := d.messagesBefore(group, nil, 2)
	if len(newest.messages) != 2 || newest.messages[0].ID != "fb:mid.$b" || newest.messages[1].ID != "fb:mid.$c" {
		t.Fatalf("newest page %+v", newest.messages)
	}
	if got := newest.messages[0].Reactions; len(got) != 1 || got[0].Emoji != "👍" || got[0].SenderID != "100003" {
		t.Fatalf("reactions %+v", got)
	}
	position := messageCursor(newest.messages[0])
	older := d.messagesBefore(group, &position, 2)
	if len(older.messages) != 1 || older.messages[0].ID != "fb:mid.$a" {
		t.Fatalf("older page %+v", older.messages)
	}
	if !older.hasMoreBefore || !older.oldestKnown || older.oldest.ID != "mid.$a" {
		t.Fatalf("range %+v", older)
	}
}

func TestReactionsReadsAndTypingBecomeEvents(t *testing.T) {
	d, _ := syncedDirectory()

	events := d.ingest(&table.LSTable{
		LSUpsertReaction:        []*table.LSUpsertReaction{{ThreadKey: group, MessageId: "mid.$a", ActorId: ada, Reaction: "❤️", TimestampMs: sentMs}},
		LSUpdateReadReceipt:     []*table.LSUpdateReadReceipt{{ThreadKey: group, ContactId: grace, ReadWatermarkTimestampMs: sentMs}},
		LSUpdateTypingIndicator: []*table.LSUpdateTypingIndicator{{ThreadKey: group, SenderId: ada, IsTyping: true}},
	})

	reaction := only[helper.Reaction](t, events, "reaction")
	if reaction.MessageID != "fb:mid.$a" || reaction.SenderID != "100002" || reaction.Emoji != "❤️" || reaction.ConversationID != "900001" {
		t.Fatalf("reaction %+v", reaction)
	}
	read := only[helper.Read](t, events, "read")
	if read.ReaderID != "100003" || !time.Time(read.ReadAt).Equal(time.UnixMilli(sentMs)) {
		t.Fatalf("read %+v", read)
	}
	typing := only[helper.Typing](t, events, "typing")
	if typing.SenderID != "100002" || !typing.Typing {
		t.Fatalf("typing %+v", typing)
	}
}

func TestRemovedReactionIsAnEmptyEmoji(t *testing.T) {
	d, _ := syncedDirectory()

	events := d.ingest(&table.LSTable{LSDeleteReaction: []*table.LSDeleteReaction{{ThreadKey: group, MessageId: "mid.$a", ActorId: ada}}})

	if reaction := only[helper.Reaction](t, events, "reaction"); reaction.Emoji != "" || reaction.MessageID != "fb:mid.$a" {
		t.Fatalf("reaction %+v", reaction)
	}
}

func TestReadingOnAnotherDeviceClearsUnread(t *testing.T) {
	d, _ := syncedDirectory()

	events := d.ingest(&table.LSTable{LSMarkThreadReadV2: []*table.LSMarkThreadReadV2{{ThreadKey: group, LastReadWatermarkTimestampMs: sentMs + 1000}}})

	if conversation := only[helper.Conversation](t, events, "conversation"); conversation.Unread {
		t.Fatalf("conversation still unread: %+v", conversation)
	}
	if read := only[helper.Read](t, events, "read"); read.ReaderID != "100001" {
		t.Fatalf("read %+v", read)
	}
}

func TestChatMovedToEncryptionIsReportedAsEncrypted(t *testing.T) {
	d := newDirectory(me)
	d.ingest(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{{ThreadKey: grace, ThreadType: table.ONE_TO_ONE, LastActivityTimestampMs: sentMs}}})

	events := d.ingest(&table.LSTable{LSMoveThreadToE2EECutoverFolder: []*table.LSMoveThreadToE2EECutoverFolder{{ThreadKey: grace}}})

	if conversation := only[helper.Conversation](t, events, "conversation"); !conversation.Encrypted {
		t.Fatalf("conversation %+v", conversation)
	}
}

func TestConversationsArePagedByLastActivity(t *testing.T) {
	d, _ := syncedDirectory()

	first := d.conversationsBefore(nil, 1)
	position := conversationCursor(first[0])
	second := d.conversationsBefore(&position, 5)

	if len(first) != 1 || first[0].ID != "900001" {
		t.Fatalf("first page %+v", first)
	}
	if len(second) != 1 || second[0].ID != "100002" {
		t.Fatalf("second page %+v", second)
	}
}

func TestUnknownConversationIsNotFound(t *testing.T) {
	d, _ := syncedDirectory()

	for _, id := range []string{"123", "not-a-number", ""} {
		if _, err := d.lookup(id); err == nil {
			t.Fatalf("lookup(%q) succeeded", id)
		}
	}
}

func TestEncryptedMessageIDRoundTrips(t *testing.T) {
	id, ok := parseEncryptedMessageID("wa:100002@msgr:100002@msgr:3EB0ABC")

	if !ok || id.Chat.User != "100002" || id.Sender.Server != "msgr" || id.ID != "3EB0ABC" || id.String() != "wa:100002@msgr:100002@msgr:3EB0ABC" {
		t.Fatalf("parsed %+v %v", id, ok)
	}
	if _, ok := parseEncryptedMessageID("fb:mid.$a"); ok {
		t.Fatal("plain id parsed as encrypted")
	}
}
