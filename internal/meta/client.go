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
	errBadSession  = helper.Errorf(helper.CodeBadSession, "Messenger no longer accepts this login. Sign in to Messenger in the browser again.")
	errNeedsPerson = helper.Errorf(helper.CodeNeedsPerson, "Messenger wants the person to confirm something. Open Messenger in a browser, finish what it asks, then log in again.")
	errUnreachable = helper.Errorf(helper.CodeNotConnected, "Messenger could not be reached. Check the internet connection and try again.")
)

type Client struct {
	log      zerolog.Logger
	emit     func(helper.Event)
	cookies  *cookies.Cookies
	platform string
	keys     *keyStore
	dir      *directory
	self     helper.User
	selfID   int64

	ctx    context.Context
	cancel context.CancelFunc

	mu           sync.Mutex
	fb           *messagix.Client
	wa           *whatsmeow.Client
	device       *store.Device
	metaUp       bool
	waUp         bool
	waFailed     bool
	everUp       bool
	encrypting   bool
	restarting   bool
	restarts     int
	stopped      bool
	lastRestart  time.Time
	state        helper.State
	lastSession  [sha256.Size]byte
	unread       map[waTypes.JID]map[waTypes.JID][]waTypes.MessageID
	metaReady    chan struct{}
	waReady      chan struct{}
	sessionDirty atomic.Bool
	background   sync.WaitGroup
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

func platformOf(session helper.Session) (types.Platform, error) {
	switch session.Platform {
	case "", "messenger":
		return types.Messenger, nil
	case "facebook":
		return types.Facebook, nil
	default:
		return types.Unset, helper.Errorf(helper.CodeBadSession, "The login's platform must be messenger or facebook.")
	}
}

func connect(ctx context.Context, logOut io.Writer, session helper.Session, emit func(helper.Event)) (*Client, error) {
	platform, err := platformOf(session)
	if err != nil {
		return nil, err
	}
	jar := &cookies.Cookies{Platform: platform}
	values := make(map[cookies.MetaCookieName]string, len(session.Cookies))
	for name, value := range session.Cookies {
		values[cookies.MetaCookieName(name)] = value
	}
	jar.UpdateValues(values)
	if missing := jar.GetMissingCookieNames(); len(missing) > 0 {
		return nil, helper.Errorf(helper.CodeBadSession, "The login is missing the cookie %q from the Messenger browser session.", missing[0])
	}

	c := &Client{
		emit:      emit,
		cookies:   jar,
		platform:  platform.String(),
		unread:    map[waTypes.JID]map[waTypes.JID][]waTypes.MessageID{},
		metaReady: make(chan struct{}),
		waReady:   make(chan struct{}),
	}
	c.log = zerolog.New(newQuietLog(logOut, c.secrets)).Level(zerolog.InfoLevel).With().Timestamp().Logger()
	c.ctx, c.cancel = context.WithCancel(context.Background())

	c.keys, err = openKeyStore(ctx, session.Encryption, c.log.With().Str("component", "keys").Logger())
	if err != nil {
		c.log.Error().Err(err).Msg("Could not open the encryption keys")
		return nil, helper.Errorf(helper.CodeBadSession, "The saved encryption keys could not be read. Log in with only the cookies to register as a new device.")
	}

	c.setState(helper.State{State: helper.StateConnecting})
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
	known := classify(err)
	switch known {
	case errBadSession:
		c.setState(helper.State{State: helper.StateLoggedOut, Code: known.Code, Message: known.Message})
	case errNeedsPerson:
		c.setState(helper.State{State: helper.StateNeedsPerson, Code: known.Code, Message: known.Message})
	default:
		return errUnreachable
	}
	return known
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
	}
	c.fb = fb
	c.mu.Unlock()
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
	c.sessionDirty.Store(true)
	switch evt := raw.(type) {
	case *table.LSTable:
		c.publish(c.dir.ingest(evt))
		from.PostHandlePublishResponse(evt)
	case *messagix.ConnectedEvent, *messagix.ReconnectedEvent:
		c.metaConnected(from)
	case *messagix.TransientDisconnectEvent:
		c.log.Warn().Err(evt.Err).Msg("Lost the connection to Messenger, reconnecting")
		c.update(func() { c.metaUp = false })
	case *messagix.PermanentErrorEvent:
		c.log.Error().Err(evt.Err).Msg("Messenger closed the connection for good")
		if websocket.CloseStatus(evt.Err) == dgw.CloseStatusUnauthorized {
			c.update(func() { c.metaUp = false })
			c.setState(helper.State{State: helper.StateLoggedOut, Code: errBadSession.Code, Message: errBadSession.Message})
			return
		}
		c.restart()
	}
}

func (c *Client) metaConnected(from *messagix.Client) {
	c.update(func() {
		c.metaUp = true
		signal(c.metaReady)
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wa == nil && !c.encrypting && c.fb == from && !c.stopped {
		c.encrypting = true
		c.background.Go(func() { c.connectEncryption(from) })
	}
}

func signal(ready chan struct{}) {
	select {
	case <-ready:
	default:
		close(ready)
	}
}

func (c *Client) update(change func()) {
	c.mu.Lock()
	change()
	next := c.currentState()
	c.mu.Unlock()
	c.setState(next)
}

func (c *Client) currentState() helper.State {
	waiting := helper.StateConnecting
	if c.everUp {
		waiting = helper.StateReconnecting
	}
	switch {
	case !c.metaUp:
		return helper.State{State: waiting}
	case c.waFailed:
		c.everUp = true
		return helper.State{State: helper.StateConnected, Code: helper.CodeNotAvailable, Message: "Connected, but encrypted chats are not working right now. The helper keeps trying."}
	case !c.waUp:
		return helper.State{State: waiting}
	default:
		c.everUp = true
		c.restarts = 0
		return helper.State{State: helper.StateConnected}
	}
}

func (c *Client) setState(next helper.State) {
	c.mu.Lock()
	changed := next != c.state && !c.stopped
	if changed {
		c.state = next
	}
	c.mu.Unlock()
	if changed {
		c.emit(helper.StateEvent(next))
	}
}

func (c *Client) restart() {
	c.mu.Lock()
	if c.restarting || c.stopped {
		c.mu.Unlock()
		return
	}
	c.restarting = true
	wait := time.Until(c.lastRestart.Add(min(minRestartInterval<<min(c.restarts, 6), maxRestartInterval)))
	c.restarts++
	defer c.mu.Unlock()
	c.background.Go(func() {
		defer c.update(func() { c.restarting = false })
		backoff := max(wait, 0)
		for {
			select {
			case <-time.After(backoff):
			case <-c.ctx.Done():
				return
			}
			c.teardown()
			c.update(func() { c.metaUp, c.waUp, c.waFailed = false, false, false })
			err := c.start(c.ctx)
			c.mu.Lock()
			c.lastRestart = time.Now()
			c.mu.Unlock()
			if err == nil {
				return
			}
			if c.reportLoginFailure(err) != errUnreachable {
				return
			}
			backoff = min(max(backoff*2, 10*time.Second), maxRestartBackoff)
		}
	})
}

func (c *Client) teardown() {
	c.mu.Lock()
	fb, wa := c.fb, c.wa
	c.fb, c.wa, c.encrypting = nil, nil, false
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
	return helper.Session{Platform: c.platform, Cookies: values, Encryption: encryption}, nil
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
	c.emit(helper.StateEvent(helper.State{State: helper.StateLoggedOut}))
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
