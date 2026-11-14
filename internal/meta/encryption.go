package meta

import (
	"context"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

const (
	firstEncryptionRetry = 10 * time.Second
	maxEncryptionRetry   = 5 * time.Minute
)

func (c *Client) spawnEncryption(fb *messagix.Client, registerAgain bool) {
	if c.encrypting || c.stopped || fb == nil {
		return
	}
	c.encrypting = true
	previous, device := c.wa, c.device
	c.wa = nil
	c.waReady = make(chan struct{})
	c.background.Go(func() {
		defer func() {
			c.mu.Lock()
			c.encrypting = false
			c.mu.Unlock()
		}()
		if previous != nil {
			previous.Disconnect()
		}
		if registerAgain && device != nil {
			if err := device.Delete(c.ctx); err != nil {
				c.log.Error().Err(err).Msg("Could not delete the encryption device")
			}
		}
		c.connectEncryption(fb)
	})
}

func (c *Client) connectEncryption(fb *messagix.Client) {
	retryIn := firstEncryptionRetry
	for {
		err := c.startEncryption(fb)
		if err == nil {
			return
		}
		c.log.Error().Err(err).Msg("Could not connect encrypted chats")
		c.transition(func(conn *connection) { conn.encryption = linkBroken })
		select {
		case <-time.After(retryIn):
		case <-c.ctx.Done():
			return
		}
		retryIn = min(retryIn*2, maxEncryptionRetry)
	}
}

func (c *Client) startEncryption(fb *messagix.Client) error {
	device, err := c.keys.device(c.ctx)
	if err != nil {
		return err
	}
	fb.SetDevice(device)
	if device.ID == nil {
		c.log.Info().Msg("Registering this helper as a new device for encrypted chats")
		if err := fb.RegisterE2EE(c.ctx, c.selfID); err != nil {
			return err
		}
		if err := device.Save(c.ctx); err != nil {
			return err
		}
		c.sessionDirty.Store(true)
	}
	wa, err := fb.PrepareE2EEClient()
	if err != nil {
		return err
	}
	wa.AddEventHandlerWithSuccessStatus(c.handleEncrypted)
	c.mu.Lock()
	stale := c.fb != fb || c.stopped
	if !stale {
		c.wa, c.device = wa, device
	}
	c.mu.Unlock()
	if stale {
		return nil
	}
	return wa.Connect()
}

func (c *Client) retryEncryption(registerAgain bool) {
	c.transition(func(conn *connection) { conn.encryption = linkBroken })
	c.mu.Lock()
	defer c.mu.Unlock()
	c.spawnEncryption(c.fb, registerAgain)
}

func (c *Client) reloadForEncryption(registerAgain bool) {
	c.mu.Lock()
	device := c.device
	c.mu.Unlock()
	if registerAgain && device != nil {
		ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		if err := device.Delete(ctx); err != nil {
			c.log.Error().Err(err).Msg("Could not delete the encryption device")
		}
		cancel()
	}
	c.transition(func(conn *connection) { conn.encryption = linkBroken })
	c.restart()
}

func (c *Client) handleEncrypted(raw any) bool {
	switch evt := raw.(type) {
	case *events.FBMessage:
		c.sessionDirty.Store(true)
		c.encryptedMessage(evt)
	case *events.Receipt:
		c.encryptedReceipt(evt)
	case *events.ChatPresence:
		if key, ok := chatKey(evt.Chat); ok {
			c.emit(helper.TypingEvent(helper.Typing{
				ConversationID: userID(key),
				SenderID:       evt.Sender.User,
				Typing:         evt.State == waTypes.ChatPresenceComposing,
			}))
		}
	case *events.Connected:
		c.sessionDirty.Store(true)
		c.transition(func(conn *connection) { conn.encryption = linkUp })
		c.mu.Lock()
		signal(c.waReady)
		c.mu.Unlock()
	case *events.Disconnected:
		c.transition(func(conn *connection) { conn.encryption = linkDown })
	case *events.CATRefreshError:
		c.log.Error().Err(evt.Error).Msg("Could not refresh the encrypted chat token")
		c.reloadForEncryption(false)
	case *events.LoggedOut:
		c.log.Error().Msg("Messenger logged the encryption device out")
		c.reloadForEncryption(true)
	case *events.ConnectFailure:
		c.log.Error().Int("reason", int(evt.Reason)).Msg("Messenger refused the encrypted chat connection")
		switch evt.Reason {
		case events.ConnectFailureNotFound:
			c.retryEncryption(true)
		case events.ConnectFailureClientUnknown:
			c.reloadForEncryption(true)
		case events.ConnectFailureGeneric:
			c.reloadForEncryption(false)
		default:
			c.retryEncryption(false)
		}
	case events.PermanentDisconnect:
		c.log.Error().Msg("Encrypted chats were disconnected for good")
		c.retryEncryption(false)
	}
	return true
}

func (c *Client) encryptedMessage(evt *events.FBMessage) {
	content, ok := convertEncrypted(evt, c.selfID)
	if !ok {
		return
	}
	sentMs, readMs := int64(0), int64(0)
	if content.message != nil {
		sentMs = evt.Info.Timestamp.UnixMilli()
		if content.message.FromMe {
			readMs = sentMs
		}
	}
	c.noteEncrypted(evt.Info.Chat, sentMs, readMs)
	if content.reaction != nil {
		c.emit(helper.ReactionEvent(*content.reaction))
		return
	}
	if !content.message.FromMe {
		c.rememberUnread(evt)
	}
	c.emit(helper.MessageEvent(*content.message))
}

func (c *Client) rememberUnread(evt *events.FBMessage) {
	var sender waTypes.JID
	if isGroupChat(evt.Info.Chat) {
		sender = evt.Info.Sender.ToNonAD()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	bySender, ok := c.unread[evt.Info.Chat]
	if !ok {
		bySender = map[waTypes.JID][]waTypes.MessageID{}
		c.unread[evt.Info.Chat] = bySender
	}
	bySender[sender] = append(bySender[sender], evt.Info.ID)
}

func (c *Client) noteEncrypted(chat waTypes.JID, activityMs, readMs int64) {
	key, ok := chatKey(chat)
	if !ok {
		return
	}
	if conversation, changed := c.dir.noteEncryptedChat(key, isGroupChat(chat), activityMs, readMs); changed {
		c.emit(helper.ConversationEvent(conversation))
	}
}

func (c *Client) putBackUnread(chat waTypes.JID, unread map[waTypes.JID][]waTypes.MessageID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	bySender, ok := c.unread[chat]
	if !ok {
		bySender = map[waTypes.JID][]waTypes.MessageID{}
		c.unread[chat] = bySender
	}
	for sender, ids := range unread {
		bySender[sender] = append(ids, bySender[sender]...)
	}
}

func (c *Client) takeUnread(chat waTypes.JID) map[waTypes.JID][]waTypes.MessageID {
	c.mu.Lock()
	defer c.mu.Unlock()
	unread := c.unread[chat]
	delete(c.unread, chat)
	return unread
}

func (c *Client) encryptedReceipt(evt *events.Receipt) {
	if evt.Type != waTypes.ReceiptTypeRead && evt.Type != waTypes.ReceiptTypeReadSelf {
		return
	}
	key, ok := chatKey(evt.Chat)
	if !ok {
		return
	}
	if evt.Type == waTypes.ReceiptTypeReadSelf {
		c.takeUnread(evt.Chat)
		c.noteEncrypted(evt.Chat, 0, evt.Timestamp.UnixMilli())
	}
	c.emit(helper.ReadEvent(helper.Read{
		ConversationID: userID(key),
		ReaderID:       evt.Sender.User,
		ReadAt:         helper.Time(evt.Timestamp),
	}))
}
