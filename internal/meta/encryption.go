package meta

import (
	"context"
	"errors"
	"time"

	"go.mau.fi/mautrix-meta/pkg/messagix"
	metaTypes "go.mau.fi/mautrix-meta/pkg/messagix/types"
	waTypes "go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

func (c *Client) connectEncryption(fb *messagix.Client) {
	if err := c.startEncryption(fb); err != nil {
		c.log.Error().Err(err).Msg("Could not connect encrypted chats")
		c.update(func() { c.waFailed = true })
		c.restart()
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

func (c *Client) handleEncrypted(raw any) bool {
	c.sessionDirty.Store(true)
	switch evt := raw.(type) {
	case *events.FBMessage:
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
		c.update(func() {
			c.waUp, c.waFailed = true, false
			signal(c.waReady)
		})
	case *events.Disconnected:
		c.update(func() { c.waUp = false })
	case *events.CATRefreshError:
		c.log.Error().Err(evt.Error).Msg("Could not refresh the encrypted chat token")
		c.encryptionBroke(errors.Is(evt.Error, metaTypes.ErrPleaseReloadPage))
	case *events.LoggedOut:
		c.log.Error().Msg("Messenger logged the encryption device out")
		c.encryptionBroke(true)
	case *events.ConnectFailure:
		c.log.Error().Int("reason", int(evt.Reason)).Msg("Messenger refused the encrypted chat connection")
		c.encryptionBroke(evt.Reason == events.ConnectFailureNotFound || evt.Reason == events.ConnectFailureClientUnknown)
	case events.PermanentDisconnect:
		c.log.Error().Msg("Encrypted chats were disconnected for good")
		c.encryptionBroke(false)
	}
	return true
}

func (c *Client) encryptionBroke(registerAgain bool) {
	c.mu.Lock()
	device := c.device
	if registerAgain {
		c.device = nil
	}
	c.mu.Unlock()
	if registerAgain && device != nil {
		ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		if err := device.Delete(ctx); err != nil {
			c.log.Error().Err(err).Msg("Could not delete the encryption device")
		}
		cancel()
	}
	c.update(func() { c.waUp, c.waFailed = false, true })
	c.restart()
}

func (c *Client) encryptedMessage(evt *events.FBMessage) {
	content, ok := convertEncrypted(evt, c.selfID)
	if !ok {
		return
	}
	key, _ := chatKey(evt.Info.Chat)
	conversation, isNew := c.dir.noteEncryptedChat(key, isGroupChat(evt.Info.Chat), evt.Info.Timestamp.UnixMilli())
	if isNew {
		c.emit(helper.ConversationEvent(conversation))
	}
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
	}
	c.emit(helper.ReadEvent(helper.Read{
		ConversationID: userID(key),
		ReaderID:       evt.Sender.User,
		ReadAt:         helper.Time(evt.Timestamp),
	}))
}
