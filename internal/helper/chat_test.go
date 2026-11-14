package helper_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

func TestConversationsArePagedNewestFirst(t *testing.T) {
	a := startHelper(t)
	a.login()

	first := a.ok("conversations", map[string]any{"limit": 1})
	second := a.ok("conversations", map[string]any{"limit": 1, "before": first["next"]})

	one := first["conversations"].([]any)[0].(map[string]any)
	two := second["conversations"].([]any)[0].(map[string]any)
	if one["id"] != encryptedChatID || one["encrypted"] != true || one["kind"] != "direct" || one["last_activity_at"] != "2026-11-14T09:00:00.000Z" {
		t.Fatalf("first conversation %v", one)
	}
	if two["id"] != plainChatID || two["kind"] != "group" {
		t.Fatalf("second conversation %v", two)
	}
	if _, more := second["next"]; more {
		t.Fatalf("last page still has next: %v", second)
	}
}

func TestMessagesArePagedBackwardsThroughHistory(t *testing.T) {
	a := startHelper(t)
	a.login()

	newest := a.ok("messages", map[string]any{"conversation_id": plainChatID, "limit": 2})
	older := a.ok("messages", map[string]any{"conversation_id": plainChatID, "limit": 2, "before": newest["next"]})

	ids := func(page map[string]any) (out []string) {
		for _, m := range page["messages"].([]any) {
			out = append(out, m.(map[string]any)["id"].(string))
		}
		return
	}
	if got := ids(newest); got[0] != "m3" || got[1] != "m4" {
		t.Fatalf("newest page %v", got)
	}
	if got := ids(older); got[0] != "m1" || got[1] != "m2" {
		t.Fatalf("older page %v", got)
	}
	message := newest["messages"].([]any)[1].(map[string]any)
	if message["sent_at"] != "2026-11-14T08:04:00.000Z" || message["sender_id"] != fakeFriendID || message["text"] != "message 4" {
		t.Fatalf("message %v", message)
	}
}

func TestHistoryOfAnEncryptedChatIsNotAvailable(t *testing.T) {
	a := startHelper(t)
	a.login()

	a.fails("messages", map[string]any{"conversation_id": encryptedChatID}, "not_available")
}

func TestMessagesNeedAConversation(t *testing.T) {
	a := startHelper(t)
	a.login()

	a.fails("messages", map[string]any{}, "bad_request")
}

func TestSendDeliversTextAsAReplyAndAnswersTheSentMessage(t *testing.T) {
	a := startHelper(t)
	a.login()

	got := a.ok("send", map[string]any{"conversation_id": encryptedChatID, "text": "hi Ada", "reply_to_id": "m1"})

	message := got["messages"].([]any)[0].(map[string]any)
	if message["id"] != "sent-1" || message["from_me"] != true || message["encrypted"] != true || message["reply_to_id"] != "m1" {
		t.Fatalf("answer %v", message)
	}
	if sent := a.meta.sent[0]; sent.Text != "hi Ada" || sent.ReplyToID != "m1" || sent.ConversationID != encryptedChatID {
		t.Fatalf("Meta got %+v", sent)
	}
}

func TestSendDeliversFilesFromTheHelpersFolder(t *testing.T) {
	a := startHelper(t)
	a.login()
	os.WriteFile(filepath.Join(a.folder, "cat.jpg"), []byte("cat pixels"), 0o600)

	a.ok("send", map[string]any{"conversation_id": plainChatID, "files": []any{map[string]any{"path": "cat.jpg"}}})

	if len(a.meta.sentFiles) != 1 {
		t.Fatalf("Meta got %d files", len(a.meta.sentFiles))
	}
	for _, content := range a.meta.sentFiles {
		if string(content) != "cat pixels" {
			t.Fatalf("Meta got file content %q", content)
		}
	}
}

func TestSendRefusesFilesOutsideTheHelpersFolder(t *testing.T) {
	a := startHelper(t)
	a.login()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o600)
	os.Symlink(outside, filepath.Join(a.folder, "link.txt"))

	for _, path := range []string{outside, "../" + filepath.Base(filepath.Dir(outside)) + "/secret.txt", "link.txt"} {
		a.fails("send", map[string]any{"conversation_id": plainChatID, "files": []any{map[string]any{"path": path}}}, "outside_folder")
	}
	if len(a.meta.sent) != 0 {
		t.Fatalf("Meta got %d messages", len(a.meta.sent))
	}
}

func TestSendRefusesAMissingFileAndAnEmptyMessage(t *testing.T) {
	a := startHelper(t)
	a.login()

	a.fails("send", map[string]any{"conversation_id": plainChatID, "files": []any{map[string]any{"path": "gone.jpg"}}}, "file_problem")
	a.fails("send", map[string]any{"conversation_id": plainChatID}, "bad_request")
}

func TestPingIsAnsweredWhileASendIsStillRunning(t *testing.T) {
	a := startHelper(t)
	a.meta.sendDelay = make(chan struct{})
	a.login()
	a.writeLine(`{"jsonrpc":"2.0","id":"slow","method":"send","params":{"conversation_id":"` + plainChatID + `","text":"x"}}`)

	a.ok("ping", nil)

	close(a.meta.sendDelay)
	for {
		if l := a.read(); string(l.ID) == `"slow"` {
			break
		}
	}
}

func TestReactAddsAndRemovesAReaction(t *testing.T) {
	a := startHelper(t)
	a.login()

	a.ok("react", map[string]any{"conversation_id": plainChatID, "message_id": "m1", "emoji": "👍"})
	a.ok("react", map[string]any{"conversation_id": plainChatID, "message_id": "m1", "emoji": ""})

	if got := a.meta.reactions; got[0].Emoji != "👍" || got[0].MessageID != "m1" || got[1].Emoji != "" {
		t.Fatalf("Meta got %+v", got)
	}
}

func TestMarkReadTellsMetaUpToWhen(t *testing.T) {
	a := startHelper(t)
	a.login()

	a.ok("mark_read", map[string]any{"conversation_id": plainChatID, "read_at": "2026-11-14T09:00:00+01:00"})

	read := a.meta.reads[0]
	if read.ConversationID != plainChatID || !time.Time(*read.ReadAt).Equal(fakeEpoch) {
		t.Fatalf("Meta got %+v", read)
	}
}

func TestAttachmentIsDownloadedIntoTheHelpersFolder(t *testing.T) {
	a := startHelper(t)
	a.login()

	got := a.ok("attachment", map[string]any{"ref": "ref-photo"})

	path := got["path"].(string)
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "\x89PNG\r\n\x1a\npixels" {
		t.Fatalf("file content %q, %v", content, err)
	}
	resolvedFolder, _ := filepath.EvalSymlinks(a.folder)
	if filepath.Dir(path) != resolvedFolder {
		t.Fatalf("path %q is not in the folder %q", path, resolvedFolder)
	}
	if got["size"] != float64(len(content)) || got["type"] != "image/png" || got["name"] != "photo.png" {
		t.Fatalf("answer %v", got)
	}
}

func TestAttachmentNameFromTheAppCannotEscapeTheFolder(t *testing.T) {
	a := startHelper(t)
	a.login()

	got := a.ok("attachment", map[string]any{"ref": "ref-photo", "name": "../../evil.png"})

	resolvedFolder, _ := filepath.EvalSymlinks(a.folder)
	if filepath.Dir(got["path"].(string)) != resolvedFolder || got["name"] != "evil.png" {
		t.Fatalf("answer %v", got)
	}
}

func TestFailedAttachmentLeavesNothingInTheFolder(t *testing.T) {
	a := startHelper(t)
	a.login()

	a.fails("attachment", map[string]any{"ref": "gone"}, "not_found")

	if entries, _ := os.ReadDir(a.folder); len(entries) != 0 {
		t.Fatalf("folder has %d entries", len(entries))
	}
}

func TestEventsFromMetaReachTheAppAsNotifications(t *testing.T) {
	a := startHelper(t)
	a.login()
	at := helper.Time(fakeEpoch)

	a.meta.push(helper.MessageEvent(helper.Message{ID: "in-1", ConversationID: encryptedChatID, SenderID: fakeFriendID, SentAt: at, Text: "psst", Encrypted: true,
		Attachments: []helper.Attachment{{ID: "att-1", Kind: "image", Type: "image/png", Name: "photo.png", Size: 14, Ref: "ref-photo"}}}))
	a.meta.push(helper.ReactionEvent(helper.Reaction{ConversationID: encryptedChatID, MessageID: "in-1", SenderID: fakeFriendID, Emoji: "❤️", At: at}))
	a.meta.push(helper.ReadEvent(helper.Read{ConversationID: encryptedChatID, ReaderID: fakeFriendID, ReadAt: at}))
	a.meta.push(helper.TypingEvent(helper.Typing{ConversationID: encryptedChatID, SenderID: fakeFriendID, Typing: true}))
	a.meta.push(helper.ConversationEvent(helper.Conversation{ID: "200003", Name: "New chat", Kind: helper.KindDirect, LastActivityAt: at}))
	a.meta.push(helper.StateEvent(helper.State{State: helper.StateReconnecting}))

	message := a.event("message")
	attachment := message["attachments"].([]any)[0].(map[string]any)
	if message["id"] != "in-1" || message["text"] != "psst" || message["sent_at"] != "2026-11-14T08:00:00.000Z" || attachment["ref"] != "ref-photo" || attachment["size"] != float64(14) {
		t.Fatalf("message %v", message)
	}
	if got := a.event("reaction"); got["emoji"] != "❤️" || got["message_id"] != "in-1" || got["at"] != "2026-11-14T08:00:00.000Z" {
		t.Fatalf("reaction %v", got)
	}
	if got := a.event("read"); got["reader_id"] != fakeFriendID || got["read_at"] != "2026-11-14T08:00:00.000Z" {
		t.Fatalf("read %v", got)
	}
	if got := a.event("typing"); got["typing"] != true || got["sender_id"] != fakeFriendID {
		t.Fatalf("typing %v", got)
	}
	if got := a.event("conversation"); got["id"] != "200003" || got["name"] != "New chat" {
		t.Fatalf("conversation %v", got)
	}
	for a.event("state")["state"] != "reconnecting" {
	}
}

func TestRestartWithTheStoredSessionConnectsAgain(t *testing.T) {
	first := startHelper(t)
	first.login()
	first.stdin.Close()
	stored := first.event("session")
	for {
		if next := first.eventOrEnd("session"); next != nil {
			stored = next
			continue
		}
		break
	}

	second := startHelper(t)
	second.hello()
	second.ok("login", stored)

	if got := second.meta.sessions[0].Encryption; got != "at-close" {
		t.Fatalf("second run logged in with encryption %q", got)
	}
	for second.event("state")["state"] != "connected" {
	}
}
