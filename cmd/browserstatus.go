package cmd

import (
	"github.com/USA-RedDragon/talaria/internal/browser/broker"
	"github.com/USA-RedDragon/talaria/internal/splice"
)

// browserControl gives the splice's control endpoints the broker's state.
type browserControl struct{ b *broker.Broker }

func (c browserControl) Status() splice.BrowserStatus {
	var st splice.BrowserStatus
	if h, ok := c.b.Connected(); ok {
		st.Connected, st.Version, st.Browser = true, h.Version, h.Browser
	}
	return st
}

// spliceBrowser is nil when browser control is off.
func spliceBrowser(br *broker.Broker) splice.Browser {
	if br == nil {
		return nil
	}
	return browserControl{br}
}
