package helper

import "context"

type loginParams struct {
	Session *Session `json:"session"`
}

type loginResult struct {
	User User `json:"user"`
}

func login(ctx context.Context, c *call) (any, error) {
	var p loginParams
	if err := c.decode(&p); err != nil {
		return nil, err
	}
	s := c.server
	session, err := s.sessionFor(p)
	if err != nil {
		return nil, err
	}
	if err := s.beginLogin(); err != nil {
		return nil, err
	}
	run := &loginRun{}
	messenger, err := s.opts.Connect(ctx, session, s.emitterFor(run))
	if err != nil {
		s.endLogin(nil, nil)
		return nil, err
	}
	s.endLogin(messenger, run)
	return loginResult{User: messenger.Self()}, nil
}

func (s *server) sessionFor(p loginParams) (Session, error) {
	if p.Session != nil && len(p.Session.Cookies) > 0 {
		return *p.Session, nil
	}
	if saved, ok := s.loadState(); ok {
		return saved, nil
	}
	return Session{}, Errorf(CodeBadSession, "The login needs the cookies from a signed-in Messenger browser session.")
}

func (s *server) beginLogin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messenger != nil || s.loggingIn {
		return Errorf(CodeAlreadyLoggedIn, "Someone is already logged in. Log out first.")
	}
	s.loggingIn = true
	return nil
}

func (s *server) endLogin(messenger Messenger, run *loginRun) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loggingIn = false
	s.messenger, s.run = messenger, run
}

func logout(ctx context.Context, c *call) (any, error) {
	s := c.server
	s.mu.Lock()
	current := s.messenger == c.messenger
	if current {
		s.messenger, s.run = nil, nil
	}
	s.mu.Unlock()
	if !current {
		return nil, Errorf(CodeNotLoggedIn, "Nobody is logged in yet. Log in first.")
	}
	if err := c.messenger.Logout(ctx); err != nil {
		return nil, err
	}
	if s.opts.StateDir != "" {
		s.forgetState()
	}
	return nil, nil
}
