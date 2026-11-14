package meta

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix/table"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

const maxRememberedMessagesPerThread = 1000

type thread struct {
	key            int64
	kind           table.ThreadType
	name           string
	lastActivityMs int64
	lastReadMs     int64
	members        map[int64]struct{}

	messages      map[string]helper.Message
	oldest        cursor
	oldestKnown   bool
	hasMoreBefore bool
}

func (t *thread) encrypted() bool { return t.kind.IsWhatsApp() }

type directory struct {
	mu          sync.Mutex
	self        int64
	threads     map[int64]*thread
	contacts    map[int64]helper.User
	hybridToJID map[int64]int64
	upserted    chan struct{}
}

func newDirectory(self int64) *directory {
	return &directory{
		self:        self,
		threads:     map[int64]*thread{},
		contacts:    map[int64]helper.User{},
		hybridToJID: map[int64]int64{},
		upserted:    make(chan struct{}),
	}
}

func (d *directory) canonical(key int64) int64 {
	if mapped, ok := d.hybridToJID[key]; ok {
		return mapped
	}
	return key
}

func (d *directory) ensure(key int64) *thread {
	t, ok := d.threads[key]
	if !ok {
		t = &thread{key: key, members: map[int64]struct{}{}, messages: map[string]helper.Message{}, hasMoreBefore: true}
		d.threads[key] = t
	}
	return t
}

func (d *directory) upsertThread(changed map[int64]struct{}, key int64, kind table.ThreadType, name string, lastActivityMs, lastReadMs int64) {
	t := d.ensure(d.canonical(key))
	if kind != table.UNKNOWN_THREAD_TYPE {
		t.kind = kind
	}
	if name != "" {
		t.name = name
	}
	t.lastActivityMs = max(t.lastActivityMs, lastActivityMs)
	t.lastReadMs = max(t.lastReadMs, lastReadMs)
	changed[t.key] = struct{}{}
}

func (d *directory) ingest(tbl *table.LSTable) []helper.Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	var events []helper.Event
	changed := map[int64]struct{}{}

	for _, m := range tbl.LSUpdateThreadAuthorityAndMappingWithOTIDFromJID {
		d.hybridToJID[m.ThreadKey] = m.ThreadJID
	}
	for _, m := range tbl.LSVerifyHybridThreadExists {
		d.hybridToJID[m.ThreadKey] = m.ThreadJID
	}
	for _, c := range tbl.LSDeleteThenInsertContact {
		d.contacts[c.Id] = helper.User{ID: userID(c.Id), Name: c.Name, AvatarURL: c.ProfilePictureUrl}
	}
	for _, c := range tbl.LSVerifyContactRowExists {
		d.contacts[c.ContactId] = helper.User{ID: userID(c.ContactId), Name: c.Name, AvatarURL: c.ProfilePictureUrl}
	}

	for _, t := range tbl.LSDeleteThenInsertThread {
		d.upsertThread(changed, t.ThreadKey, t.ThreadType, t.ThreadName, t.LastActivityTimestampMs, t.LastReadWatermarkTimestampMs)
	}
	for _, t := range tbl.LSUpdateOrInsertThread {
		d.upsertThread(changed, t.ThreadKey, t.ThreadType, t.ThreadName, t.LastActivityTimestampMs, t.LastReadWatermarkTimestampMs)
	}
	for _, t := range tbl.LSVerifyThreadExists {
		if _, known := d.threads[d.canonical(t.ThreadKey)]; !known {
			d.upsertThread(changed, t.ThreadKey, t.ThreadType, "", 0, 0)
		}
	}
	for _, p := range tbl.LSAddParticipantIdToGroupThread {
		t := d.ensure(d.canonical(p.ThreadKey))
		if _, already := t.members[p.ContactId]; !already {
			t.members[p.ContactId] = struct{}{}
			changed[t.key] = struct{}{}
		}
	}
	for _, p := range tbl.LSRemoveParticipantFromThread {
		t := d.ensure(d.canonical(p.ThreadKey))
		delete(t.members, p.ParticipantId)
		changed[t.key] = struct{}{}
	}
	for _, n := range tbl.LSSyncUpdateThreadName {
		t := d.ensure(d.canonical(n.ThreadKey))
		t.name = n.ThreadName
		changed[t.key] = struct{}{}
	}
	for _, m := range tbl.LSMoveThreadToE2EECutoverFolder {
		t := d.ensure(d.canonical(m.ThreadKey))
		if t.kind == table.ONE_TO_ONE || t.kind == table.UNKNOWN_THREAD_TYPE {
			t.kind = table.ENCRYPTED_OVER_WA_ONE_TO_ONE
			changed[t.key] = struct{}{}
		}
	}

	upserts, inserts := tbl.WrapMessages()
	historical := map[string]struct{}{}
	for _, upsert := range upserts {
		t := d.ensure(d.canonical(upsert.GetThreadKey()))
		for _, wrapped := range upsert.Messages {
			historical[wrapped.MessageId] = struct{}{}
			if wrapped.IsUnsent {
				continue
			}
			t.remember(d.plainMessage(t, wrapped))
		}
		if upsert.Range != nil {
			t.oldest = cursor{TimestampMs: upsert.Range.MinTimestampMs, ID: upsert.Range.MinMessageId}
			t.oldestKnown = true
			t.hasMoreBefore = upsert.Range.HasMoreBefore
		}
	}
	if len(upserts) > 0 {
		close(d.upserted)
		d.upserted = make(chan struct{})
	}
	for _, wrapped := range inserts {
		if wrapped.IsUnsent {
			continue
		}
		t := d.ensure(d.canonical(wrapped.ThreadKey))
		message := d.plainMessage(t, wrapped)
		t.remember(message)
		t.lastActivityMs = max(t.lastActivityMs, wrapped.TimestampMs)
		events = append(events, helper.MessageEvent(message))
	}

	for _, r := range tbl.LSUpsertReaction {
		if _, bundled := historical[r.MessageId]; bundled {
			continue
		}
		events = append(events, d.reaction(r.ThreadKey, r.MessageId, r.ActorId, r.Reaction, r.TimestampMs))
	}
	for _, r := range tbl.LSDeleteReaction {
		events = append(events, d.reaction(r.ThreadKey, r.MessageId, r.ActorId, "", time.Now().UnixMilli()))
	}
	for _, r := range tbl.LSUpdateReadReceipt {
		events = append(events, helper.ReadEvent(helper.Read{
			ConversationID: userID(d.canonical(r.ThreadKey)),
			ReaderID:       userID(r.ContactId),
			ReadAt:         millis(r.ReadWatermarkTimestampMs),
		}))
	}
	for _, r := range tbl.LSMarkThreadReadV2 {
		t := d.ensure(d.canonical(r.ThreadKey))
		t.lastReadMs = max(t.lastReadMs, r.LastReadWatermarkTimestampMs)
		changed[t.key] = struct{}{}
		events = append(events, helper.ReadEvent(helper.Read{
			ConversationID: userID(t.key),
			ReaderID:       userID(d.self),
			ReadAt:         millis(r.LastReadWatermarkTimestampMs),
		}))
	}
	for _, typing := range tbl.LSUpdateTypingIndicator {
		events = append(events, helper.TypingEvent(helper.Typing{
			ConversationID: userID(d.canonical(typing.ThreadKey)),
			SenderID:       userID(typing.SenderId),
			Typing:         typing.IsTyping,
		}))
	}

	keys := make([]int64, 0, len(changed))
	for key := range changed {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	conversations := make([]helper.Event, 0, len(keys))
	for _, key := range keys {
		conversations = append(conversations, helper.ConversationEvent(d.conversation(d.threads[key])))
	}
	return append(conversations, events...)
}

func (d *directory) reaction(threadKey int64, messageID string, actor int64, emoji string, atMs int64) helper.Event {
	return helper.ReactionEvent(helper.Reaction{
		ConversationID: userID(d.canonical(threadKey)),
		MessageID:      plainMessageID(messageID),
		SenderID:       userID(actor),
		Emoji:          emoji,
		At:             millis(atMs),
	})
}

func millis(ms int64) helper.Time { return helper.Time(time.UnixMilli(ms)) }

func (t *thread) remember(m helper.Message) {
	t.messages[m.ID] = m
	if len(t.messages) <= maxRememberedMessagesPerThread {
		return
	}
	sorted := t.sorted()
	for _, old := range sorted[:len(sorted)-maxRememberedMessagesPerThread] {
		delete(t.messages, old.ID)
	}
}

func (t *thread) sorted() []helper.Message {
	out := make([]helper.Message, 0, len(t.messages))
	for _, m := range t.messages {
		out = append(out, m)
	}
	slices.SortFunc(out, compareMessages)
	return out
}

func compareMessages(a, b helper.Message) int {
	return cmp.Or(time.Time(a.SentAt).Compare(time.Time(b.SentAt)), cmp.Compare(a.ID, b.ID))
}

func (d *directory) conversation(t *thread) helper.Conversation {
	kind := helper.KindGroup
	members := t.members
	if t.kind.IsOneToOne() {
		kind = helper.KindDirect
		members = map[int64]struct{}{t.key: {}}
	}
	ids := make([]int64, 0, len(members))
	for id := range members {
		if id != d.self {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	participants := make([]helper.User, 0, len(ids))
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		user, known := d.contacts[id]
		if !known {
			user = helper.User{ID: userID(id)}
		}
		participants = append(participants, user)
		if user.Name != "" {
			names = append(names, user.Name)
		}
	}
	name := t.name
	if name == "" {
		name = strings.Join(names, ", ")
	}
	return helper.Conversation{
		ID:             userID(t.key),
		Name:           name,
		Kind:           kind,
		Encrypted:      t.encrypted(),
		Participants:   participants,
		LastActivityAt: millis(t.lastActivityMs),
		Unread:         t.lastReadMs < t.lastActivityMs,
	}
}

type knownThread struct {
	key  int64
	kind table.ThreadType
}

func (t knownThread) encrypted() bool { return t.kind.IsWhatsApp() }

func (d *directory) lookup(conversationID string) (knownThread, error) {
	key, err := parseThreadKey(conversationID)
	if err != nil {
		return knownThread{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	t, ok := d.threads[key]
	if !ok || t.kind == table.UNKNOWN_THREAD_TYPE {
		return knownThread{}, helper.Errorf(helper.CodeNotFound, "The helper has not seen that conversation. List conversations until it shows up, then try again.")
	}
	return knownThread{key: t.key, kind: t.kind}, nil
}

func (d *directory) conversationsBefore(before *cursor, limit int) []helper.Conversation {
	d.mu.Lock()
	defer d.mu.Unlock()
	all := make([]*thread, 0, len(d.threads))
	for _, t := range d.threads {
		if t.kind == table.UNKNOWN_THREAD_TYPE || t.kind == table.FOLDER || t.lastActivityMs == 0 {
			continue
		}
		if before != nil && !threadCursor(t).before(*before) {
			continue
		}
		all = append(all, t)
	}
	slices.SortFunc(all, func(a, b *thread) int {
		return cmp.Or(cmp.Compare(b.lastActivityMs, a.lastActivityMs), cmp.Compare(b.key, a.key))
	})
	out := make([]helper.Conversation, 0, min(limit, len(all)))
	for _, t := range all[:min(limit, len(all))] {
		out = append(out, d.conversation(t))
	}
	return out
}

func threadCursor(t *thread) cursor {
	return cursor{TimestampMs: t.lastActivityMs, ID: userID(t.key)}
}

func conversationCursor(c helper.Conversation) cursor {
	return cursor{TimestampMs: time.Time(c.LastActivityAt).UnixMilli(), ID: c.ID}
}

func (c cursor) before(other cursor) bool {
	return cmp.Or(cmp.Compare(c.TimestampMs, other.TimestampMs), cmp.Compare(c.ID, other.ID)) < 0
}

type historyPage struct {
	messages      []helper.Message
	oldest        cursor
	oldestKnown   bool
	hasMoreBefore bool
	changed       <-chan struct{}
}

func (d *directory) messagesBefore(key int64, before *cursor, limit int) historyPage {
	d.mu.Lock()
	defer d.mu.Unlock()
	t := d.ensure(key)
	sorted := t.sorted()
	if before != nil {
		end, _ := slices.BinarySearchFunc(sorted, *before, func(m helper.Message, c cursor) int {
			if messageCursor(m).before(c) {
				return -1
			}
			return 1
		})
		sorted = sorted[:end]
	}
	return historyPage{
		messages:      sorted[max(len(sorted)-limit, 0):],
		oldest:        t.oldest,
		oldestKnown:   t.oldestKnown,
		hasMoreBefore: t.hasMoreBefore,
		changed:       d.upserted,
	}
}

func messageCursor(m helper.Message) cursor {
	return cursor{TimestampMs: time.Time(m.SentAt).UnixMilli(), ID: m.ID}
}

func (d *directory) noteEncryptedChat(key int64, group bool, activityMs int64) (helper.Conversation, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	t, known := d.threads[key]
	isNew := !known || !t.encrypted()
	t = d.ensure(key)
	if isNew {
		t.kind = table.ENCRYPTED_OVER_WA_ONE_TO_ONE
		if group {
			t.kind = table.ENCRYPTED_OVER_WA_GROUP
		}
	}
	t.lastActivityMs = max(t.lastActivityMs, activityMs)
	return d.conversation(t), isNew
}
