package meta

import "github.com/marcus13371337/messenger-helper/internal/helper"

type link int

const (
	linkDown link = iota
	linkUp
	linkBroken
)

type connection struct {
	messenger    link
	encryption   link
	wasConnected bool
	ended        *helper.State
}

func (c connection) public() helper.State {
	waiting := helper.State{State: helper.StateConnecting}
	if c.wasConnected {
		waiting.State = helper.StateReconnecting
	}
	switch {
	case c.ended != nil:
		return *c.ended
	case c.messenger != linkUp:
		return waiting
	case c.encryption == linkBroken:
		return helper.State{State: helper.StateConnected, Code: helper.CodeNotAvailable, Message: "Connected, but encrypted chats are not working right now. The helper keeps trying."}
	case c.encryption != linkUp:
		return waiting
	default:
		return helper.State{State: helper.StateConnected}
	}
}

func (c connection) healthy() bool {
	return c.ended == nil && c.messenger == linkUp && c.encryption == linkUp
}

func endedWith(state string, reason *helper.Error) *helper.State {
	return &helper.State{State: state, Code: reason.Code, Message: reason.Message}
}
