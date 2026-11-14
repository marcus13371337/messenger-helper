package helper_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

func TestLoginHandsTheSessionToMetaAndAnswersWhoLoggedIn(t *testing.T) {
	a := startHelper(t)

	got := a.login()

	user := got["user"].(map[string]any)
	if user["id"] != fakeUserID || user["name"] != "Marcus" {
		t.Fatalf("login answered %v", got)
	}
	session := a.meta.sessions[0]
	if session.Cookies["xs"] != validCookieValue || session.Encryption != "ZW5jcnlwdGlvbi1zdGF0ZQ==" {
		t.Fatalf("Meta got session %+v", session)
	}
}

func TestLoginReportsEveryConnectionChangeAndTheRefreshedSession(t *testing.T) {
	a := startHelper(t)

	a.login()

	if got := a.event("state")["state"]; got != "connecting" {
		t.Fatalf("first state %v", got)
	}
	if got := a.event("state")["state"]; got != "connected" {
		t.Fatalf("second state %v", got)
	}
	session := a.event("session")["session"].(map[string]any)
	if session["encryption"] != "after-connect" || session["cookies"].(map[string]any)["xs"] != validCookieValue {
		t.Fatalf("session event %v", session)
	}
}

func TestRequestsBeforeLoginAreRefused(t *testing.T) {
	a := startHelper(t)
	a.hello()

	for _, method := range []string{"conversations", "messages", "send", "react", "mark_read", "attachment", "logout"} {
		a.fails(method, nil, "not_logged_in")
	}
}

func TestLoginWithoutCookiesIsRefused(t *testing.T) {
	a := startHelper(t)
	a.hello()

	a.fails("login", map[string]any{"session": map[string]any{"cookies": map[string]any{}}}, "bad_session")
}

func TestLoginWithASessionMetaRejectsIsRefusedAndCanBeRetried(t *testing.T) {
	a := startHelper(t)
	a.hello()

	a.fails("login", map[string]any{"session": map[string]any{"cookies": map[string]any{"xs": "expired"}}}, "bad_session")

	a.ok("login", map[string]any{"session": validSession()})
}

func TestLoginWhenMetaAsksForACheckSaysThePersonIsNeeded(t *testing.T) {
	a := startHelper(t)
	a.meta.checkpoint = true
	a.hello()

	a.fails("login", map[string]any{"session": validSession()}, "needs_person")

	if got := a.event("state")["state"]; got != "needs_person" {
		t.Fatalf("state %v", got)
	}
}

func TestSecondLoginIsRefused(t *testing.T) {
	a := startHelper(t)
	a.login()

	a.fails("login", map[string]any{"session": validSession()}, "already_logged_in")
}

func TestLogoutLogsOutOfMetaAndAllowsANewLogin(t *testing.T) {
	a := startHelper(t)
	a.login()

	a.ok("logout", nil)

	if !a.meta.loggedOut {
		t.Fatal("Meta was not told to log out")
	}
	for a.event("state")["state"] != "logged_out" {
	}
	a.fails("conversations", nil, "not_logged_in")
	a.ok("login", map[string]any{"session": validSession()})
}

func TestClosingStdinHandsBackAFinalSessionBeforeExit(t *testing.T) {
	a := startHelper(t)
	a.login()
	a.event("session")

	a.stdin.Close()

	session := a.event("session")["session"].(map[string]any)
	if session["encryption"] != "at-close" {
		t.Fatalf("final session %v", session)
	}
	if err := a.waitExit(); err != nil {
		t.Fatalf("exit error %v", err)
	}
}

func TestSessionTooBigForOneLineBecomesAReadableStateProblem(t *testing.T) {
	a := startHelper(t)
	a.meta.sessionBytes = helper.MaxLineBytes
	a.login()

	for {
		state := a.event("state")
		if state["code"] == "too_large" {
			if state["state"] != "connected" || state["message"] == "" {
				t.Fatalf("state %v", state)
			}
			break
		}
	}
}

func withStateDir(dir string) option {
	return func(o *helper.Options) { o.StateDir = dir }
}

func TestStateDirKeepsTheSessionSoALaterRunCanLogInWithoutOne(t *testing.T) {
	dir := t.TempDir()
	first := startHelper(t, withStateDir(dir))
	first.login()
	first.stdin.Close()
	first.waitExit()

	second := startHelper(t, withStateDir(dir))
	second.hello()
	second.ok("login", nil)

	if got := second.meta.sessions[0]; got.Cookies["xs"] != validCookieValue || got.Encryption != "at-close" {
		t.Fatalf("second run logged in with %+v", got)
	}
	info, err := os.Stat(filepath.Join(dir, "session.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("saved session file: %v %v", info, err)
	}
}

func TestWithoutStateDirNothingIsWrittenToDisk(t *testing.T) {
	a := startHelper(t)
	a.login()
	a.stdin.Close()
	a.waitExit()

	entries, err := os.ReadDir(a.folder)
	if err != nil || len(entries) != 0 {
		t.Fatalf("folder has %d entries (%v)", len(entries), err)
	}
}

func TestLogoutForgetsTheSessionInTheStateDir(t *testing.T) {
	dir := t.TempDir()
	a := startHelper(t, withStateDir(dir))
	a.login()

	a.ok("logout", nil)

	if _, err := os.Stat(filepath.Join(dir, "session.json")); !os.IsNotExist(err) {
		t.Fatalf("session file still there: %v", err)
	}
}

func TestLogsNeverCarryCookiesKeysOrMessageText(t *testing.T) {
	var log strings.Builder
	a := startHelper(t, func(o *helper.Options) { o.Log = &log })
	a.login()
	a.ok("send", map[string]any{"conversation_id": plainChatID, "text": "a very private sentence"})
	a.fails("send", map[string]any{"conversation_id": "nope", "text": "another private sentence"}, "not_found")
	a.fails("attachment", map[string]any{"ref": "secret-media-key"}, "not_found")
	a.writeLine(`{"jsonrpc":"2.0","id":99,"method":"login","params":"` + validCookieValue + `"}`)
	a.read()
	a.stdin.Close()
	a.waitExit()

	for _, secret := range []string{validCookieValue, "ZW5jcnlwdGlvbi1zdGF0ZQ==", "private sentence", "secret-media-key", "after-connect"} {
		if strings.Contains(log.String(), secret) {
			t.Fatalf("log contains %q:\n%s", secret, log.String())
		}
	}
}

func TestWhenMetaEndsTheLoginTheAppCanLogInAgain(t *testing.T) {
	a := startHelper(t)
	a.login()

	a.meta.push(helper.StateEvent(helper.State{State: helper.StateLoggedOut, Code: helper.CodeBadSession, Message: "Messenger no longer accepts this login."}))

	for a.event("state")["state"] != "logged_out" {
	}
	a.fails("conversations", nil, "not_logged_in")
	a.ok("login", map[string]any{"session": validSession()})
	a.ok("conversations", nil)
}
