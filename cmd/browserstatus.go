package cmd

import (
	"github.com/USA-RedDragon/hermes-device/internal/browser/broker"
	"github.com/USA-RedDragon/hermes-device/internal/splice"
)

// browserControl gives the splice's control endpoints the broker's state.
type browserControl struct{ b *broker.Broker }

func (c browserControl) Status() splice.BrowserStatus {
	a := c.b.Auth()
	st := splice.BrowserStatus{Auth: a.State, User: a.User, Reason: a.Reason}
	if h, ok := c.b.Connected(); ok {
		st.Connected, st.Version, st.Browser = true, h.Version, h.Browser
	}
	return st
}

func (c browserControl) RequestSignIn() bool { return c.b.RequestSignIn() }

// spliceBrowser is nil when browser control is off.
func spliceBrowser(br *broker.Broker) splice.Browser {
	if br == nil {
		return nil
	}
	return browserControl{br}
}
