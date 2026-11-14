package meta

import (
	"fmt"
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

func TestSyncedMessagesAreKeptAsTheRecentWindowWithoutMessageEvents(t *testing.T) {
	d, _ := syncedDirectory()
	tbl := history("mid.$a", "mid.$b", "mid.$c")
	tbl.LSUpsertReaction = []*table.LSUpsertReaction{{ThreadKey: group, MessageId: "mid.$b", ActorId: grace, Reaction: "👍"}}

	events := d.ingest(tbl)

	if len(events) != 0 {
		t.Fatalf("history produced events %+v", events)
	}
	recent, olderExist := d.recentBefore(group, nil)
	if len(recent) != 3 || recent[0].ID != "fb:mid.$a" || recent[2].ID != "fb:mid.$c" || !olderExist {
		t.Fatalf("recent %+v, older exist %v", recent, olderExist)
	}
	if got := recent[1].Reactions; len(got) != 1 || got[0].Emoji != "👍" || got[0].SenderID != "100003" {
		t.Fatalf("reactions %+v", got)
	}
	position := messageCursor(recent[1])
	if older, _ := d.recentBefore(group, &position); len(older) != 1 || older[0].ID != "fb:mid.$a" {
		t.Fatalf("before the middle message: %+v", older)
	}
}

func TestLaterReactionsUpdateTheRecentWindow(t *testing.T) {
	d, _ := syncedDirectory()
	d.ingest(history("mid.$a"))

	d.ingest(&table.LSTable{LSUpsertReaction: []*table.LSUpsertReaction{{ThreadKey: group, MessageId: "mid.$a", ActorId: ada, Reaction: "👍"}}})
	d.ingest(&table.LSTable{LSUpsertReaction: []*table.LSUpsertReaction{{ThreadKey: group, MessageId: "mid.$a", ActorId: ada, Reaction: "❤️"}}})
	recent, _ := d.recentBefore(group, nil)
	changed := recent[0].Reactions
	d.ingest(&table.LSTable{LSDeleteReaction: []*table.LSDeleteReaction{{ThreadKey: group, MessageId: "mid.$a", ActorId: ada}}})
	recent, _ = d.recentBefore(group, nil)

	if len(changed) != 1 || changed[0].Emoji != "❤️" {
		t.Fatalf("after changing the reaction: %+v", changed)
	}
	if len(recent[0].Reactions) != 0 {
		t.Fatalf("after removing the reaction: %+v", recent[0].Reactions)
	}
}

func TestFetchedHistoryGoesToWhoeverAskedAndNotIntoTheRecentWindow(t *testing.T) {
	d, _ := syncedDirectory()
	d.ingest(history("mid.$recent"))
	batches, done := d.collectHistory(group)

	d.ingest(history("mid.$old1", "mid.$old2"))
	batch := <-batches
	done()

	if len(batch.messages) != 2 || batch.messages[0].ID != "fb:mid.$old1" || batch.oldest.ID != "mid.$old1" || !batch.hasMoreBefore {
		t.Fatalf("batch %+v", batch)
	}
	if recent, _ := d.recentBefore(group, nil); len(recent) != 1 || recent[0].ID != "fb:mid.$recent" {
		t.Fatalf("recent window %+v", recent)
	}
}

func TestEndOfHistoryReachesWhoeverAsked(t *testing.T) {
	d, _ := syncedDirectory()
	batches, done := d.collectHistory(group)
	defer done()

	d.ingest(&table.LSTable{LSUpdateExistingMessageRange: []*table.LSUpdateExistingMessageRange{{ThreadKey: group, TimestampMS: sentMs, UnknownBool2: true}}})

	if batch := <-batches; len(batch.messages) != 0 || batch.hasMoreBefore {
		t.Fatalf("batch %+v", batch)
	}
}

func TestRecentWindowIsCappedAndThenAdmitsOlderMessagesExist(t *testing.T) {
	d, _ := syncedDirectory()
	tbl := &table.LSTable{LSInsertNewMessageRange: []*table.LSInsertNewMessageRange{{ThreadKey: group}}}
	for i := range maxRecentMessagesPerThread + 5 {
		tbl.LSUpsertMessage = append(tbl.LSUpsertMessage, &table.LSUpsertMessage{ThreadKey: group, MessageId: fmt.Sprintf("mid.$%04d", i), SenderId: ada, TimestampMs: sentMs + int64(i)})
	}

	d.ingest(tbl)

	recent, olderExist := d.recentBefore(group, nil)
	if len(recent) != maxRecentMessagesPerThread || recent[0].ID != "fb:mid.$0005" || !olderExist {
		t.Fatalf("%d recent, oldest %s, older exist %v", len(recent), recent[0].ID, olderExist)
	}
}

func TestMergingFetchedHistoryKeepsOrderDropsDuplicatesAndRespectsThePosition(t *testing.T) {
	message := func(id string, offset int64) helper.Message {
		return helper.Message{ID: id, SentAt: millis(sentMs + offset)}
	}
	position := cursor{TimestampMs: sentMs + 30, ID: "fb:c"}

	merged := mergeOlder(
		[]helper.Message{message("fb:a", 10), message("fb:b", 20), message("fb:c", 30), message("fb:d", 40)},
		[]helper.Message{message("fb:b", 20)},
		&position,
	)

	if len(merged) != 2 || merged[0].ID != "fb:a" || merged[1].ID != "fb:b" {
		t.Fatalf("merged %+v", merged)
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

func TestConversationsWithTheSameActivityAreNotSkippedAcrossPages(t *testing.T) {
	d := newDirectory(me)
	d.ingest(&table.LSTable{LSDeleteThenInsertThread: []*table.LSDeleteThenInsertThread{
		{ThreadKey: 9, ThreadType: table.GROUP_THREAD, LastActivityTimestampMs: sentMs},
		{ThreadKey: 10, ThreadType: table.GROUP_THREAD, LastActivityTimestampMs: sentMs},
	}})

	first := d.conversationsBefore(nil, 1)
	position := conversationCursor(first[0])
	second := d.conversationsBefore(&position, 1)

	if len(first) != 1 || len(second) != 1 || first[0].ID == second[0].ID {
		t.Fatalf("pages %+v then %+v", first, second)
	}
}

func TestEncryptedChatIsUnreadUntilReadOrAnswered(t *testing.T) {
	d := newDirectory(me)

	arrived, isNew := d.noteEncryptedChat(ada, false, sentMs, 0)
	_, reactionChanged := d.noteEncryptedChat(ada, false, 0, 0)
	read, readChanged := d.noteEncryptedChat(ada, false, 0, sentMs)
	answered, _ := d.noteEncryptedChat(ada, false, sentMs+1000, sentMs+1000)

	if !isNew || !arrived.Unread || !arrived.Encrypted || arrived.Kind != helper.KindDirect {
		t.Fatalf("after a message arrived: %+v (new %v)", arrived, isNew)
	}
	if reactionChanged {
		t.Fatal("a reaction changed the conversation")
	}
	if !readChanged || read.Unread {
		t.Fatalf("after reading: %+v (changed %v)", read, readChanged)
	}
	if answered.Unread {
		t.Fatalf("after my own message: %+v", answered)
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
