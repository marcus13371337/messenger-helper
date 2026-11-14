package helper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

type Options struct {
	In       io.Reader
	Out      io.Writer
	Log      io.Writer
	Version  string
	Connect  Connect
	StateDir string
}

const shutdownGrace = 3 * time.Second

type server struct {
	opts Options
	out  *lineWriter
	log  zerolog.Logger

	greeted   bool
	folder    string
	messenger Messenger

	inFlight sync.WaitGroup
}

func Run(ctx context.Context, opts Options) error {
	s := &server{
		opts: opts,
		out:  &lineWriter{out: opts.Out},
		log:  zerolog.New(opts.Log).With().Timestamp().Logger(),
	}
	lines := make(chan readLine)
	go readLines(opts.In, lines)
	for running := true; running; {
		select {
		case <-ctx.Done():
			running = false
		case l, ok := <-lines:
			if !ok {
				running = false
				break
			}
			s.handleLine(ctx, l)
		}
	}
	return s.shutdown()
}

func (s *server) shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	waited := make(chan struct{})
	go func() {
		s.inFlight.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-ctx.Done():
	}
	if s.messenger == nil {
		return nil
	}
	return s.messenger.Close(ctx)
}

func (s *server) handleLine(ctx context.Context, l readLine) {
	if l.tooLong {
		s.answer(nil, nil, Errorf(CodeTooLarge, "That message was too big. A line can be at most %d MB.", MaxLineBytes>>20))
		return
	}
	if len(bytes.TrimSpace(l.raw)) == 0 {
		return
	}
	var req request
	if err := json.Unmarshal(l.raw, &req); err != nil || req.Method == "" {
		s.answer(nil, nil, Errorf(CodeBadRequest, "That line was not a request the helper understands."))
		return
	}
	if len(req.ID) == 0 {
		return
	}
	handler, ok := handlers[req.Method]
	if !ok {
		s.answer(req.ID, nil, Errorf(CodeUnknownMethod, "The helper does not know the request %q.", req.Method))
		return
	}
	if handler.serial {
		s.run(ctx, handler, req)
		return
	}
	messenger := s.messenger
	s.inFlight.Add(1)
	go func() {
		defer s.inFlight.Done()
		s.runWith(ctx, handler, req, messenger)
	}()
}

func (s *server) run(ctx context.Context, h handler, req request) {
	s.runWith(ctx, h, req, s.messenger)
}

func (s *server) runWith(ctx context.Context, h handler, req request, messenger Messenger) {
	if !s.greeted && req.Method != "hello" {
		s.answer(req.ID, nil, Errorf(CodeHelloFirst, "Say hello before anything else."))
		return
	}
	if h.needsLogin && messenger == nil {
		s.answer(req.ID, nil, Errorf(CodeNotLoggedIn, "Nobody is logged in yet. Log in first."))
		return
	}
	result, err := h.fn(ctx, &call{server: s, messenger: messenger, folder: s.folder, params: req.Params})
	s.answer(req.ID, result, err)
}

func (s *server) answer(id json.RawMessage, result any, err error) {
	if id == nil {
		id = json.RawMessage("null")
	}
	resp := response{JSONRPC: "2.0", ID: id, Result: result}
	if err != nil {
		resp.Result = nil
		resp.Error = toWireError(s.plain(err))
	} else if result == nil {
		resp.Result = struct{}{}
	}
	writeErr := s.out.write(resp)
	if errors.Is(writeErr, errLineTooLong) {
		resp.Result = nil
		resp.Error = toWireError(Errorf(CodeTooLarge, "The answer was too big to send. Ask for less at a time."))
		writeErr = s.out.write(resp)
	}
	if writeErr != nil {
		s.log.Error().Err(writeErr).Msg("could not write an answer")
	}
}

func (s *server) plain(err error) *Error {
	var known *Error
	if errors.As(err, &known) {
		return known
	}
	s.log.Error().Err(err).Msg("request failed")
	return errInternal
}

func (s *server) emit(evt Event) {
	if evt.Method == "session" {
		s.saveState(evt)
	}
	err := s.out.write(notification{JSONRPC: "2.0", Method: evt.Method, Params: evt.Params})
	if errors.Is(err, errLineTooLong) && evt.Method == "session" {
		err = s.out.write(notification{JSONRPC: "2.0", Method: "state", Params: State{
			State:   StateConnected,
			Code:    CodeTooLarge,
			Message: "The login details have grown too big to hand over. Log out and in again to start fresh.",
		}})
	}
	if err != nil {
		s.log.Error().Err(err).Str("event", evt.Method).Msg("could not write an event")
	}
}

func (s *server) statePath() string {
	return filepath.Join(s.opts.StateDir, "session.json")
}

func (s *server) saveState(evt Event) {
	if s.opts.StateDir == "" {
		return
	}
	raw, err := json.Marshal(evt.Params)
	if err == nil {
		err = os.WriteFile(s.statePath(), raw, 0o600)
	}
	if err != nil {
		s.log.Error().Err(err).Msg("could not save the session to the state folder")
	}
}

func (s *server) loadState() (Session, bool) {
	if s.opts.StateDir == "" {
		return Session{}, false
	}
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		return Session{}, false
	}
	var saved struct {
		Session Session `json:"session"`
	}
	if json.Unmarshal(raw, &saved) != nil {
		return Session{}, false
	}
	return saved.Session, true
}
