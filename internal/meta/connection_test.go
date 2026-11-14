package meta

import (
	"testing"

	"github.com/marcus13371337/messenger-helper/internal/helper"
)

func TestPublicStateFollowsBothConnections(t *testing.T) {
	cases := []struct {
		name string
		conn connection
		want string
		code string
	}{
		{"nothing up yet", connection{}, helper.StateConnecting, ""},
		{"messenger up, encryption still coming", connection{messenger: linkUp}, helper.StateConnecting, ""},
		{"both up", connection{messenger: linkUp, encryption: linkUp}, helper.StateConnected, ""},
		{"encryption broken", connection{messenger: linkUp, encryption: linkBroken, wasConnected: true}, helper.StateConnected, helper.CodeNotAvailable},
		{"messenger dropped later", connection{encryption: linkUp, wasConnected: true}, helper.StateReconnecting, ""},
		{"encryption dropped later", connection{messenger: linkUp, wasConnected: true}, helper.StateReconnecting, ""},
	}
	for _, c := range cases {
		if got := c.conn.public(); got.State != c.want || got.Code != c.code {
			t.Fatalf("%s: got %+v", c.name, got)
		}
	}
}

func TestLoggedOutStaysLoggedOutWhateverTheConnectionsDo(t *testing.T) {
	conn := connection{wasConnected: true, ended: endedWith(helper.StateLoggedOut, errBadSession)}

	for _, messenger := range []link{linkDown, linkUp, linkBroken} {
		conn.messenger, conn.encryption = messenger, linkUp
		if got := conn.public(); got.State != helper.StateLoggedOut || got.Code != helper.CodeBadSession {
			t.Fatalf("with messenger link %d: got %+v", messenger, got)
		}
	}
}
