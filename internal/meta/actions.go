package meta

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/socket"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/mautrix-meta/pkg/messagix/useragent"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waMediaTransport"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

const (
	inboxSyncGroup       = 1
	maxThreadPagesToScan = 10
	historyWait          = 10 * time.Second
)

func (c *Client) loadMoreThreads(ctx context.Context, fb *messagix.Client) (bool, error) {
	_, tbl, err := fb.FetchMoreThreads(ctx, inboxSyncGroup)
	if err != nil {
		return false, c.unexpected(err, "Could not load more conversations")
	}
	if tbl == nil {
		return false, nil
	}
	c.publish(c.dir.ingest(tbl))
	return true, nil
}

func (c *Client) unexpected(err error, what string) error {
	if ctxErr := context.Cause(c.ctx); ctxErr != nil {
		return helper.Errorf(helper.CodeNotConnected, "The helper is shutting down.")
	}
	c.log.Error().Err(err).Msg(what)
	if known := classify(err); known != nil {
		return known
	}
	return helper.Errorf(helper.CodeMetaRefused, "Messenger did not accept that. Try again in a moment.")
}

func (c *Client) Conversations(ctx context.Context, q helper.ConversationsQuery) (helper.ConversationPage, error) {
	var before *cursor
	if q.Before != "" {
		parsed, err := parseCursor(q.Before)
		if err != nil {
			return helper.ConversationPage{}, err
		}
		before = &parsed
	}
	fb, err := c.plainClient(ctx)
	if err != nil {
		return helper.ConversationPage{}, err
	}
	found := c.dir.conversationsBefore(before, q.Limit+1)
	for scanned := 0; len(found) <= q.Limit && scanned < maxThreadPagesToScan; scanned++ {
		more, err := c.loadMoreThreads(ctx, fb)
		if err != nil {
			return helper.ConversationPage{}, err
		}
		if !more {
			break
		}
		found = c.dir.conversationsBefore(before, q.Limit+1)
	}
	page := helper.ConversationPage{Conversations: found}
	if len(found) > q.Limit {
		page.Conversations = found[:q.Limit]
		last := page.Conversations[q.Limit-1]
		page.Next = cursor{TimestampMs: time.Time(last.LastActivityAt).UnixMilli(), ID: last.ID}.String()
	}
	return page, nil
}

func (c *Client) findThread(ctx context.Context, conversationID string) (*thread, error) {
	_, t, err := c.dir.lookup(conversationID)
	if err == nil {
		return t, nil
	}
	if _, keyErr := parseThreadKey(conversationID); keyErr != nil {
		return nil, keyErr
	}
	fb, clientErr := c.plainClient(ctx)
	if clientErr != nil {
		return nil, clientErr
	}
	for scanned := 0; scanned < maxThreadPagesToScan; scanned++ {
		more, loadErr := c.loadMoreThreads(ctx, fb)
		if loadErr != nil {
			return nil, loadErr
		}
		if _, t, err = c.dir.lookup(conversationID); err == nil || !more {
			break
		}
	}
	return t, err
}

func (c *Client) Messages(ctx context.Context, q helper.MessagesQuery) (helper.MessagePage, error) {
	t, err := c.findThread(ctx, q.ConversationID)
	if err != nil {
		return helper.MessagePage{}, err
	}
	if t.encrypted() {
		return helper.MessagePage{}, helper.Errorf(helper.CodeNotAvailable, "Messenger does not keep a readable copy of encrypted chats, so older messages cannot be fetched. Keep the messages as they arrive.")
	}
	var before *cursor
	if q.Before != "" {
		parsed, err := parseCursor(q.Before)
		if err != nil {
			return helper.MessagePage{}, err
		}
		before = &parsed
	}
	page := c.dir.messagesBefore(t, before, q.Limit)
	if len(page.messages) < q.Limit && page.hasMoreBefore {
		if err := c.fetchOlder(ctx, t, olderThan(page, before)); err != nil {
			return helper.MessagePage{}, err
		}
		select {
		case <-page.changed:
		case <-time.After(historyWait):
		case <-ctx.Done():
			return helper.MessagePage{}, ctx.Err()
		}
		page = c.dir.messagesBefore(t, before, q.Limit)
	}
	out := helper.MessagePage{Messages: page.messages}
	if len(page.messages) == q.Limit {
		out.Next = messageCursor(page.messages[0]).String()
	}
	return out, nil
}

func olderThan(page historyPage, before *cursor) cursor {
	switch {
	case len(page.messages) > 0:
		oldest := messageCursor(page.messages[0])
		oldest.ID = strings.TrimPrefix(oldest.ID, plainMessagePrefix)
		return oldest
	case before != nil:
		return cursor{TimestampMs: before.TimestampMs, ID: strings.TrimPrefix(before.ID, plainMessagePrefix)}
	case page.oldestKnown:
		return page.oldest
	default:
		return cursor{TimestampMs: time.Now().UnixMilli()}
	}
}

func (c *Client) fetchOlder(ctx context.Context, t *thread, reference cursor) error {
	fb, err := c.plainClient(ctx)
	if err != nil {
		return err
	}
	resp, err := fb.ExecuteTasks(ctx, &socket.FetchMessagesTask{
		ThreadKey:            t.key,
		ReferenceTimestampMs: reference.TimestampMs,
		ReferenceMessageId:   reference.ID,
		SyncGroup:            inboxSyncGroup,
		Cursor:               fb.GetCursor(inboxSyncGroup),
	})
	if err != nil {
		return c.unexpected(err, "Could not fetch older messages")
	}
	if resp != nil {
		c.dir.ingest(resp)
	}
	return nil
}

func (c *Client) React(ctx context.Context, r helper.OutgoingReaction) error {
	if encrypted, ok := parseEncryptedMessageID(r.MessageID); ok {
		wa, err := c.encryptedClient(ctx)
		if err != nil {
			return err
		}
		_, err = wa.SendFBMessage(ctx, encrypted.Chat, reactionMessage(messageKey(encrypted, c.selfID), r.Emoji), nil)
		if err != nil {
			return c.unexpected(err, "Could not send the reaction")
		}
		c.sessionDirty.Store(true)
		return nil
	}
	messageID, isPlain := strings.CutPrefix(r.MessageID, plainMessagePrefix)
	if !isPlain {
		return helper.Errorf(helper.CodeBadRequest, "That message id did not come from the helper.")
	}
	t, err := c.findThread(ctx, r.ConversationID)
	if err != nil {
		return err
	}
	fb, err := c.plainClient(ctx)
	if err != nil {
		return err
	}
	_, err = fb.ExecuteTasks(ctx, &socket.SendReactionTask{
		ThreadKey:       t.key,
		TimestampMs:     time.Now().UnixMilli(),
		MessageID:       messageID,
		Reaction:        r.Emoji,
		ActorID:         c.selfID,
		SyncGroup:       inboxSyncGroup,
		SendAttribution: table.MESSENGER_INBOX_IN_THREAD,
	})
	if err != nil {
		return c.unexpected(err, "Could not send the reaction")
	}
	return nil
}

func (c *Client) MarkRead(ctx context.Context, r helper.ReadMark) error {
	t, err := c.findThread(ctx, r.ConversationID)
	if err != nil {
		return err
	}
	readAt := time.Now()
	if r.ReadAt != nil {
		readAt = time.Time(*r.ReadAt)
	}
	if t.encrypted() {
		wa, err := c.encryptedClient(ctx)
		if err != nil {
			return err
		}
		chat := chatJID(t)
		for sender, ids := range c.takeUnread(chat) {
			if err := wa.MarkRead(ctx, ids, readAt, chat, sender); err != nil {
				return c.unexpected(err, "Could not mark the encrypted chat as read")
			}
		}
		return nil
	}
	fb, err := c.plainClient(ctx)
	if err != nil {
		return err
	}
	_, err = fb.ExecuteTasks(ctx, &socket.ThreadMarkReadTask{ThreadId: t.key, LastReadWatermarkTs: readAt.UnixMilli(), SyncGroup: inboxSyncGroup})
	if err != nil {
		return c.unexpected(err, "Could not mark the chat as read")
	}
	return nil
}

var downloadHosts = []string{"fbcdn.net", "fbsbx.com", "facebook.com", "messenger.com", "cdninstagram.com"}

func checkDownloadURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Scheme == "https" {
		host := strings.ToLower(parsed.Hostname())
		for _, allowed := range downloadHosts {
			if host == allowed || strings.HasSuffix(host, "."+allowed) {
				return nil
			}
		}
	}
	return helper.Errorf(helper.CodeBadRequest, "That attachment ref did not come from a message.")
}

var errAttachmentGone = helper.Errorf(helper.CodeNotAvailable, "This attachment's link has run out. Fetch the conversation's messages again to get a fresh one.")

func (c *Client) Download(ctx context.Context, ref string, into io.Writer) (helper.Downloaded, error) {
	parsed, err := decodeRef(ref)
	if err != nil {
		return helper.Downloaded{}, err
	}
	described := helper.Downloaded{Type: parsed.Type, Name: parsed.Name}
	if parsed.DirectPath != "" {
		wa, err := c.encryptedClient(ctx)
		if err != nil {
			return helper.Downloaded{}, err
		}
		data, err := wa.DownloadFB(ctx, &waMediaTransport.WAMediaTransport_Integral{
			DirectPath:    &parsed.DirectPath,
			MediaKey:      parsed.MediaKey,
			FileSHA256:    parsed.FileSHA256,
			FileEncSHA256: parsed.FileEncSHA256,
		}, whatsmeow.MediaType(parsed.MediaType))
		if err != nil {
			c.log.Error().Err(err).Msg("Could not download the encrypted attachment")
			return helper.Downloaded{}, errAttachmentGone
		}
		_, err = into.Write(data)
		return described, err
	}
	if err := checkDownloadURL(parsed.URL); err != nil {
		return helper.Downloaded{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.URL, nil)
	if err != nil {
		return helper.Downloaded{}, err
	}
	req.Header.Set("User-Agent", useragent.UserAgent)
	req.Header.Set("Referer", "https://www.messenger.com/")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.log.Error().Msg("Could not reach the attachment's server")
		return helper.Downloaded{}, errUnreachable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return helper.Downloaded{}, errAttachmentGone
	}
	if described.Type == "" {
		described.Type = resp.Header.Get("Content-Type")
	}
	_, err = io.Copy(into, resp.Body)
	return described, err
}
