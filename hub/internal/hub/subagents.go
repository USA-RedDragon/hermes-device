package hub

import (
	"context"
	"errors"
)

// Subagents choices that every backend with the subagent-routes plugin
// offers. The others are route names and "help-" plus a route name.
const (
	SubagentsAuto = "auto"
	SubagentsNone = "none"
)

// SubagentChoice is one choice of a chat's Subagents picker. Line says
// what it means, and Em is the part of Line to show in bold.
type SubagentChoice struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Group string `json:"group,omitempty"`
	Line  string `json:"line"`
	Em    string `json:"em,omitempty"`
}

// Subagents is a chat's Subagents setting: which model routes its
// subagents may use, and the choices it can take.
type Subagents struct {
	Choice  string           `json:"choice"`
	Choices []SubagentChoice `json:"choices"`
}

// Current is the choice the chat has, or false when it is not listed.
func (s Subagents) Current() (SubagentChoice, bool) {
	for _, c := range s.Choices {
		if c.ID == s.Choice {
			return c, true
		}
	}
	return SubagentChoice{}, false
}

// SubagentRoutes reads and sets the Subagents setting of chats on
// backends that run the subagent-routes plugin.
type SubagentRoutes interface {
	ChatSubagents(ctx context.Context, user, backendID, sessionID string) (Subagents, error)
	SetChatSubagents(ctx context.Context, user, backendID, sessionID, choice string) (Subagents, error)
}

// ErrNoSubagentRoutes is a backend whose Hermes has no subagent-routes
// plugin.
var ErrNoSubagentRoutes = errors.New("this Hermes has no subagent-routes plugin")
