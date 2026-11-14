package helper_test

import (
	"context"
	"io"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

const (
	fakeUserID       = "100001"
	fakeFriendID     = "100002"
	plainChatID      = "200001"
	encryptedChatID  = "200002"
	validCookieValue = "valid-xs-cookie-value"
)

var fakeEpoch = time.Date(2026, 11, 14, 9, 0, 0, 0, time.FixedZone("CET", 3600))

func validSession() map[string]any {
	return map[string]any{"cookies": map[string]any{"c_user": fakeUserID, "xs": validCookieValue}, "encryption": "ZW5jcnlwdGlvbi1zdGF0ZQ=="}
}

type fakeMeta struct {
	mu            sync.Mutex
	emit          func(helper.Event)
	sessions      []helper.Session
	conversations []helper.Conversation
	history       map[string][]helper.Message
	sent          []helper.Outgoing
	sentFiles     map[string][]byte
	reactions     []helper.OutgoingReaction
	reads         []helper.ReadMark
	files         map[string][]byte
	checkpoint    bool
	sessionBytes  int
	sendDelay     chan struct{}
	loggedOut     bool
	closed        bool
}

func newFakeMeta() *fakeMeta {
	friend := helper.User{ID: fakeFriendID, Name: "Ada"}
	f := &fakeMeta{
		history:   map[string][]helper.Message{},
		sentFiles: map[string][]byte{},
		files:     map[string][]byte{"ref-photo": []byte("\x89PNG\r\n\x1a\npixels")},
		conversations: []helper.Conversation{
			{ID: encryptedChatID, Name: "Ada", Kind: helper.KindDirect, Encrypted: true, Participants: []helper.User{friend}, LastActivityAt: helper.Time(fakeEpoch.Add(time.Hour))},
			{ID: plainChatID, Name: "Book club", Kind: helper.KindGroup, Participants: []helper.User{friend}, LastActivityAt: helper.Time(fakeEpoch)},
		},
	}
	for i := range 5 {
		f.history[plainChatID] = append(f.history[plainChatID], helper.Message{
			ID: "m" + strconv.Itoa(i), ConversationID: plainChatID, SenderID: fakeFriendID,
			SentAt: helper.Time(fakeEpoch.Add(time.Duration(i) * time.Minute)), Text: "message " + strconv.Itoa(i),
			Attachments: []helper.Attachment{},
		})
	}
	return f
}

func (f *fakeMeta) connect(_ context.Context, session helper.Session, emit func(helper.Event)) (helper.Messenger, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions = append(f.sessions, session)
	if session.Cookies["xs"] != validCookieValue {
		return nil, helper.Errorf(helper.CodeBadSession, "Messenger no longer accepts this login. Log in again in the browser.")
	}
	if f.checkpoint {
		emit(helper.StateEvent(helper.State{State: helper.StateNeedsPerson, Code: helper.CodeNeedsPerson, Message: "Messenger wants you to confirm it is you. Open Messenger in a browser."}))
		return nil, helper.Errorf(helper.CodeNeedsPerson, "Messenger wants you to confirm it is you. Open Messenger in a browser.")
	}
	f.emit = emit
	f.loggedOut, f.closed = false, false
	emit(helper.StateEvent(helper.State{State: helper.StateConnecting}))
	emit(helper.StateEvent(helper.State{State: helper.StateConnected}))
	emit(helper.SessionEvent(f.refreshedSession("after-connect")))
	return f, nil
}

func (f *fakeMeta) refreshedSession(encryption string) helper.Session {
	if f.sessionBytes > 0 {
		encryption = string(make([]byte, f.sessionBytes))
	}
	return helper.Session{Cookies: map[string]string{"c_user": fakeUserID, "xs": validCookieValue}, Encryption: encryption}
}

func (f *fakeMeta) push(evt helper.Event) {
	f.mu.Lock()
	emit := f.emit
	f.mu.Unlock()
	emit(evt)
}

func (f *fakeMeta) Self() helper.User { return helper.User{ID: fakeUserID, Name: "Marcus"} }

func (f *fakeMeta) Conversations(_ context.Context, q helper.ConversationsQuery) (helper.ConversationPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := 0
	if q.Before != "" {
		start, _ = strconv.Atoi(q.Before)
	}
	end := min(start+q.Limit, len(f.conversations))
	page := helper.ConversationPage{Conversations: f.conversations[start:end]}
	if end < len(f.conversations) {
		page.Next = strconv.Itoa(end)
	}
	return page, nil
}

func (f *fakeMeta) conversation(id string) (helper.Conversation, error) {
	for _, c := range f.conversations {
		if c.ID == id {
			return c, nil
		}
	}
	return helper.Conversation{}, helper.Errorf(helper.CodeNotFound, "That conversation does not exist.")
}

func (f *fakeMeta) Messages(_ context.Context, q helper.MessagesQuery) (helper.MessagePage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	conversation, err := f.conversation(q.ConversationID)
	if err != nil {
		return helper.MessagePage{}, err
	}
	if conversation.Encrypted {
		return helper.MessagePage{}, helper.Errorf(helper.CodeNotAvailable, "Messenger does not keep a copy of encrypted chats, so older messages cannot be fetched.")
	}
	all := f.history[q.ConversationID]
	end := len(all)
	if q.Before != "" {
		end, _ = strconv.Atoi(q.Before)
	}
	start := max(end-q.Limit, 0)
	page := helper.MessagePage{Messages: all[start:end]}
	if start > 0 {
		page.Next = strconv.Itoa(start)
	}
	return page, nil
}

func (f *fakeMeta) Send(ctx context.Context, out helper.Outgoing) (helper.Message, error) {
	if f.sendDelay != nil {
		<-f.sendDelay
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	conversation, err := f.conversation(out.ConversationID)
	if err != nil {
		return helper.Message{}, err
	}
	for _, file := range out.Files {
		content, err := os.ReadFile(file.Path)
		if err != nil {
			return helper.Message{}, err
		}
		f.sentFiles[file.Path] = content
	}
	f.sent = append(f.sent, out)
	return helper.Message{
		ID: "sent-" + strconv.Itoa(len(f.sent)), ConversationID: out.ConversationID, SenderID: fakeUserID, FromMe: true,
		SentAt: helper.Time(fakeEpoch), Text: out.Text, ReplyToID: out.ReplyToID, Attachments: []helper.Attachment{}, Encrypted: conversation.Encrypted,
	}, nil
}

func (f *fakeMeta) React(_ context.Context, r helper.OutgoingReaction) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reactions = append(f.reactions, r)
	return nil
}

func (f *fakeMeta) MarkRead(_ context.Context, r helper.ReadMark) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, r)
	return nil
}

func (f *fakeMeta) Download(_ context.Context, ref string, into io.Writer) (helper.Downloaded, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	content, ok := f.files[ref]
	if !ok {
		return helper.Downloaded{}, helper.Errorf(helper.CodeNotFound, "That attachment is no longer available.")
	}
	_, err := into.Write(content)
	return helper.Downloaded{Name: "photo.png"}, err
}

func (f *fakeMeta) Logout(context.Context) error {
	f.mu.Lock()
	f.loggedOut = true
	emit := f.emit
	f.mu.Unlock()
	emit(helper.StateEvent(helper.State{State: helper.StateLoggedOut}))
	return nil
}

func (f *fakeMeta) Close(context.Context) error {
	f.mu.Lock()
	f.closed = true
	emit, session := f.emit, f.refreshedSession("at-close")
	f.mu.Unlock()
	emit(helper.SessionEvent(session))
	return nil
}
