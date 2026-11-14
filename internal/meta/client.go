package meta

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-meta/pkg/messagix"
	"go.mau.fi/mautrix-meta/pkg/messagix/cookies"
	"go.mau.fi/mautrix-meta/pkg/messagix/dgw"
	"go.mau.fi/mautrix-meta/pkg/messagix/httpclient"
	"go.mau.fi/mautrix-meta/pkg/messagix/table"
	"go.mau.fi/mautrix-meta/pkg/messagix/types"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	waTypes "go.mau.fi/whatsmeow/types"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

const (
	sessionFlushInterval = 5 * time.Second
	minRestartInterval   = time.Minute
	maxRestartInterval   = time.Hour
	maxRestartBackoff    = 5 * time.Minute
	connectWait          = 20 * time.Second
)

var (
	errBadSession  = helper.Errorf(helper.CodeBadSession, "Messenger no longer accepts this login. Sign in at facebook.com in the browser again.")
	errNeedsPerson = helper.Errorf(helper.CodeNeedsPerson, "Messenger wants the person to confirm something. Open facebook.com in a browser, finish what it asks, then log in again.")
	errUnreachable = helper.Errorf(helper.CodeNotConnected, "Messenger could not be reached. Check the internet connection and try again.")
)

type Client struct {
	log     zerolog.Logger
	emit    func(helper.Event)
	cookies *cookies.Cookies
	keys    *keyStore
	dir     *directory
	self    helper.User
	selfID  int64

	ctx    context.Context
	cancel context.CancelFunc

	mu                sync.Mutex
	fb                *messagix.Client
	wa                *whatsmeow.Client
	device            *store.Device
	conn              connection
	published         helper.State
	encryptionRun     int
	lastEncryptionRun int
	restarting        bool
	stopped           bool
	restarts          int
	lastRestart       time.Time
	lastSession       [sha256.Size]byte
	unread            map[waTypes.JID]map[waTypes.JID][]waTypes.MessageID
	metaReady         chan struct{}
	waReady           chan struct{}
	historyLocks      map[int64]*sync.Mutex
	sessionDirty      atomic.Bool
	background        sync.WaitGroup
}

func Connector(logOut io.Writer) helper.Connect {
	return func(ctx context.Context, session helper.Session, emit func(helper.Event)) (helper.Messenger, error) {
		client, err := connect(ctx, logOut, session, emit)
		if err != nil {
			return nil, err
		}
		return client, nil
	}
}

func cookieJar(session helper.Session) (*cookies.Cookies, error) {
	jar := &cookies.Cookies{Platform: types.Facebook}
	values := make(map[cookies.MetaCookieName]string, len(session.Cookies))
	for name, value := range session.Cookies {
		values[cookies.MetaCookieName(name)] = value
	}
	jar.UpdateValues(values)
	if missing := jar.GetMissingCookieNames(); len(missing) > 0 {
		return nil, helper.Errorf(helper.CodeBadSession, "The login is missing the cookie %q from the facebook.com browser session.", missing[0])
	}
	return jar, nil
}

func connect(ctx context.Context, logOut io.Writer, session helper.Session, emit func(helper.Event)) (*Client, error) {
	jar, err := cookieJar(session)
	if err != nil {
		return nil, err
	}

	c := &Client{
		emit:         emit,
		cookies:      jar,
		unread:       map[waTypes.JID]map[waTypes.JID][]waTypes.MessageID{},
		historyLocks: map[int64]*sync.Mutex{},
		metaReady:    make(chan struct{}),
		waReady:      make(chan struct{}),
	}
	c.log = zerolog.New(newQuietLog(logOut, c.secrets)).Level(zerolog.InfoLevel).With().Timestamp().Logger()
	c.ctx, c.cancel = context.WithCancel(context.Background())

	c.keys, err = openKeyStore(ctx, session.Encryption, c.log.With().Str("component", "keys").Logger())
	if err != nil {
		c.log.Error().Err(err).Msg("Could not open the encryption keys")
		return nil, helper.Errorf(helper.CodeBadSession, "The saved encryption keys could not be read. Log in with only the cookies to register as a new device.")
	}

	c.transition(func(*connection) {})
	if err := c.start(ctx); err != nil {
		c.keys.close()
		c.cancel()
		return nil, c.reportLoginFailure(err)
	}
	c.background.Go(c.flushSessions)
	return c, nil
}

func (c *Client) secrets() []string {
	all := c.cookies.GetAll()
	out := make([]string, 0, len(all))
	for _, value := range all {
		out = append(out, value)
	}
	return out
}

func classify(err error) *helper.Error {
	switch {
	case errors.Is(err, httpclient.ErrTokenInvalidated):
		return errBadSession
	case errors.Is(err, httpclient.ErrChallengeRequired),
		errors.Is(err, httpclient.ErrCheckpointRequired),
		errors.Is(err, httpclient.ErrConsentRequired),
		errors.Is(err, httpclient.ErrAccountSuspended):
		return errNeedsPerson
	default:
		return nil
	}
}

func (c *Client) reportLoginFailure(err error) error {
	c.log.Error().Err(err).Msg("Could not load Messenger")
	switch known := classify(err); known {
	case errBadSession:
		c.transition(func(conn *connection) { conn.ended = endedWith(helper.StateLoggedOut, known) })
		return known
	case errNeedsPerson:
		c.transition(func(conn *connection) { conn.ended = endedWith(helper.StateNeedsPerson, known) })
		return known
	default:
		return errUnreachable
	}
}

func (c *Client) start(ctx context.Context) error {
	fb := messagix.NewClient(c.cookies, c.log.With().Str("component", "messagix").Logger(), &messagix.Config{})
	user, initial, err := fb.LoadMessagesPage(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.dir == nil {
		c.selfID = user.GetFBID()
		c.self = helper.User{ID: userID(c.selfID), Name: user.GetName(), AvatarURL: user.GetAvatarURL()}
		c.dir = newDirectory(c.selfID)
	} else {
		c.dir.forgetRecent()
	}
	c.fb = fb
	c.mu.Unlock()
	c.sessionDirty.Store(true)
	fb.SetEventHandler(func(_ context.Context, evt any) { c.handleMeta(fb, evt) })
	c.publish(c.dir.ingest(initial))
	return fb.Connect(c.ctx)
}

func (c *Client) publish(events []helper.Event) {
	for _, evt := range events {
		c.emit(evt)
	}
}

func (c *Client) handleMeta(from *messagix.Client, raw any) {
	switch evt := raw.(type) {
	case *table.LSTable:
		c.publish(c.dir.ingest(evt))
		from.PostHandlePublishResponse(evt)
	case *messagix.ConnectedEvent, *messagix.ReconnectedEvent:
		c.messengerConnected(from)
	case *messagix.TransientDisconnectEvent:
		c.log.Warn().Err(evt.Err).Msg("Lost the connection to Messenger, reconnecting")
		c.transition(func(conn *connection) { conn.messenger = linkDown })
	case *messagix.PermanentErrorEvent:
		c.log.Error().Err(evt.Err).Msg("Messenger closed the connection for good")
		if websocket.CloseStatus(evt.Err) == dgw.CloseStatusUnauthorized {
			c.transition(func(conn *connection) {
				conn.messenger = linkBroken
				conn.ended = endedWith(helper.StateLoggedOut, errBadSession)
			})
			return
		}
		c.transition(func(conn *connection) { conn.messenger = linkBroken })
		c.restart()
	}
}

func (c *Client) messengerConnected(from *messagix.Client) {
	c.transition(func(conn *connection) { conn.messenger = linkUp })
	c.mu.Lock()
	defer c.mu.Unlock()
	signal(c.metaReady)
	if c.wa == nil && c.fb == from {
		c.spawnEncryption(from, false)
	}
}

func signal(ready chan struct{}) {
	select {
	case <-ready:
	default:
		close(ready)
	}
}

func (c *Client) transition(change func(*connection)) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	change(&c.conn)
	next := c.conn.public()
	if next.State == helper.StateConnected {
		c.conn.wasConnected = true
	}
	if c.conn.healthy() {
		c.restarts = 0
	}
	changed := next != c.published
	c.published = next
	c.mu.Unlock()
	if changed {
		c.emit(helper.StateEvent(next))
	}
}

func (c *Client) restart() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.restarting || c.stopped || c.conn.ended != nil {
		return
	}
	c.restarting = true
	wait := time.Until(c.lastRestart.Add(min(minRestartInterval<<min(c.restarts, 6), maxRestartInterval)))
	c.restarts++
	c.background.Go(func() {
		defer func() {
			c.mu.Lock()
			c.restarting = false
			c.mu.Unlock()
		}()
		backoff := max(wait, 0)
		for {
			select {
			case <-time.After(backoff):
			case <-c.ctx.Done():
				return
			}
			c.teardown()
			c.transition(func(conn *connection) { conn.messenger, conn.encryption = linkDown, linkDown })
			err := c.start(c.ctx)
			c.mu.Lock()
			c.lastRestart = time.Now()
			c.mu.Unlock()
			if err == nil || c.reportLoginFailure(err) != errUnreachable {
				return
			}
			backoff = min(max(backoff*2, 10*time.Second), maxRestartBackoff)
		}
	})
}

func (c *Client) teardown() {
	c.mu.Lock()
	fb, wa := c.fb, c.wa
	c.fb, c.wa, c.encryptionRun = nil, nil, 0
	c.metaReady, c.waReady = make(chan struct{}), make(chan struct{})
	c.mu.Unlock()
	if fb != nil {
		fb.SetEventHandler(nil)
		fb.Disconnect()
	}
	if wa != nil {
		wa.Disconnect()
	}
}

func (c *Client) flushSessions() {
	ticker := time.NewTicker(sessionFlushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if c.sessionDirty.Swap(false) {
				c.emitSession(false)
			}
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Client) session() (helper.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	encryption, err := c.keys.snapshot(ctx)
	if err != nil {
		return helper.Session{}, err
	}
	all := c.cookies.GetAll()
	values := make(map[string]string, len(all))
	for name, value := range all {
		values[string(name)] = value
	}
	return helper.Session{Cookies: values, Encryption: encryption}, nil
}

func (c *Client) emitSession(always bool) {
	session, err := c.session()
	if err != nil {
		c.log.Error().Err(err).Msg("Could not pack the session")
		return
	}
	raw, _ := json.Marshal(session)
	sum := sha256.Sum256(raw)
	c.mu.Lock()
	unchanged := sum == c.lastSession
	c.lastSession = sum
	c.mu.Unlock()
	if always || !unchanged {
		c.emit(helper.SessionEvent(session))
	}
}

func (c *Client) Self() helper.User { return c.self }

func (c *Client) Close(context.Context) error {
	c.mu.Lock()
	already := c.stopped
	c.stopped = true
	c.mu.Unlock()
	if already {
		return nil
	}
	c.emitSession(true)
	c.cancel()
	c.teardown()
	c.background.Wait()
	c.emitSession(false)
	c.keys.close()
	return nil
}

func (c *Client) Logout(ctx context.Context) error {
	c.mu.Lock()
	device := c.device
	c.mu.Unlock()
	c.teardown()
	if device != nil && device.ID != nil {
		if err := device.Delete(ctx); err != nil {
			c.log.Error().Err(err).Msg("Could not delete the encryption device")
		}
	}
	c.transition(func(conn *connection) { conn.ended = &helper.State{State: helper.StateLoggedOut} })
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.cancel()
	c.background.Wait()
	c.keys.close()
	return nil
}

func (c *Client) waitFor(ctx context.Context, pick func() chan struct{}) error {
	c.mu.Lock()
	ready := pick()
	c.mu.Unlock()
	select {
	case <-ready:
		return nil
	case <-time.After(connectWait):
		return helper.Errorf(helper.CodeNotConnected, "The helper is not connected to Messenger right now. Try again in a moment.")
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Client) plainClient(ctx context.Context) (*messagix.Client, error) {
	if err := c.waitFor(ctx, func() chan struct{} { return c.metaReady }); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fb == nil {
		return nil, helper.Errorf(helper.CodeNotConnected, "The helper is not connected to Messenger right now. Try again in a moment.")
	}
	return c.fb, nil
}

func (c *Client) encryptedClient(ctx context.Context) (*whatsmeow.Client, error) {
	if err := c.waitFor(ctx, func() chan struct{} { return c.waReady }); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wa == nil {
		return nil, helper.Errorf(helper.CodeNotConnected, "Encrypted chats are not connected right now. Try again in a moment.")
	}
	return c.wa, nil
}
