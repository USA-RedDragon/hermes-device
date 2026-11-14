package hub

import (
	"context"
	"errors"
	"regexp"
	"time"
)

// AgentHeaderPrefix starts every agent message the hub delivers. The page
// and the Telegram bot know an agent message by its first line:
// "[Agent message am_<hex> from <sender>]".
const AgentHeaderPrefix = "[Agent message "

// AgentHeaderRE matches an agent message's first line; its groups are the
// message id and the sender.
var AgentHeaderRE = regexp.MustCompile(`^\[Agent message (am_[0-9a-f]{16}) from ([^\]\n]{1,400})\]\n`)

// AgentMessage is one message an agent sent another agent of the same
// backend through the hub.
type AgentMessage struct {
	ID        string
	BackendID string
	// FromSession is the sending agent's session: a chat, or a subagent's
	// own session. FromRoot is the chat at the root of its delegation
	// tree (FromSession for a chat). FromSubagent is the subagent id, or
	// "" for a chat.
	FromSession, FromRoot, FromSubagent, FromName string
	// ToSession is the target's session; ToSubagent its subagent id, or ""
	// for a chat.
	ToSession, ToSubagent, ToName string
	ReplyTo                       string
	// Hop counts the messages of one back-and-forth chain: 1 for a message
	// that answers none. Only an answer continues a chain: one that names
	// ReplyTo, or the first message a turn sends back to the agent whose
	// message the sender still has not answered.
	Hop  int
	Text string
	// Mode is how the hub delivered it: AgentModeInput, AgentModeInbox or
	// AgentModeSteer.
	Mode   string
	Status string
	// InTree is true when sender and target were in one delegation tree.
	// Such a message does not count toward the rate limits.
	InTree    bool
	CreatedAt time.Time
}

// Agent message delivery modes.
const (
	// AgentModeInput is a new input of the target chat (prompt.submit): it
	// starts a turn, or waits behind the running one.
	AgentModeInput = "input"
	// AgentModeInbox rides on the target agent's next tool result.
	AgentModeInbox = "inbox"
	// AgentModeSteer is Hermes's steer of a running turn: of a subagent,
	// from its parent, or of a chat, from a peer (the input that the
	// running turn took, prompt.steer_queued).
	AgentModeSteer = "steer"
)

// What SubmitAgent did with an agent message for a chat.
const (
	// AgentSubmitStarted: the chat was idle and started a turn with it.
	AgentSubmitStarted = "started"
	// AgentSubmitSteered: the chat's running turn took it as a steer, to
	// read it with its next tool result.
	AgentSubmitSteered = "steered"
	// AgentSubmitQueued: it waits as the chat's next input, behind the
	// running turn.
	AgentSubmitQueued = "queued"
)

// Agent message statuses.
const (
	AgentStatusSending      = "sending"
	AgentStatusDelivered    = "delivered"
	AgentStatusNotDelivered = "not_delivered"
)

// AgentCounts are the recent messages that limit an agent's sends.
type AgentCounts struct {
	// FromRoot is the messages of the sender's whole tree since the
	// short window began, Pair those from the sender's tree to the target,
	// Backend all the backend's messages since the long window began.
	// Messages inside one delegation tree (InTree) are not counted.
	FromRoot, Pair, Backend int
}

// ErrAgentNameTaken is a short name another chat of the backend has.
var ErrAgentNameTaken = errors.New("another chat has this name")

// AgentStore keeps agent messages and the short names of chats.
type AgentStore interface {
	AddAgentMessage(ctx context.Context, m AgentMessage) error
	SetAgentMessageStatus(ctx context.Context, id, mode, status string) error
	AgentMessage(ctx context.Context, backendID, id string) (AgentMessage, bool, error)
	// LastAgentIn is the newest message at or after since that any of
	// toSessions got from the session or delegation tree from. On equal
	// times the one with the higher hop.
	LastAgentIn(ctx context.Context, backendID string, toSessions []string, from string, since time.Time) (AgentMessage, bool, error)
	// LastAgentOut is the newest message at or after since that the tree
	// of fromRoot sent to toSession.
	LastAgentOut(ctx context.Context, backendID, fromRoot, toSession string, since time.Time) (AgentMessage, bool, error)
	AgentCounts(ctx context.Context, backendID, fromRoot, toSession string, short, long time.Time) (AgentCounts, error)
	// AgentNames maps each named session of the backend to its name.
	AgentNames(ctx context.Context, backendID string) (map[string]string, error)
	// SetAgentName names a session; "" removes the name.
	SetAgentName(ctx context.Context, backendID, session, name string) error
	AgentPartners
}

// AgentMessageReader looks up one agent message of a backend.
type AgentMessageReader interface {
	AgentMessage(ctx context.Context, backendID, id string) (AgentMessage, bool, error)
}

// MaxAgentPartners is the most partner pairs a user keeps per backend.
const MaxAgentPartners = 200

// ErrTooManyAgentPartners is a user who has MaxAgentPartners pairs.
var ErrTooManyAgentPartners = errors.New("too many partner pairs")

// AgentPartner is an unordered pair of chats the user marked as working
// together: A sorts before B.
type AgentPartner struct {
	A string `json:"a"`
	B string `json:"b"`
}

// NewAgentPartner orders a pair of sessions.
func NewAgentPartner(a, b string) AgentPartner {
	if b < a {
		a, b = b, a
	}
	return AgentPartner{A: a, B: b}
}

// Has reports whether s is one of the pair.
func (p AgentPartner) Has(s string) bool { return p.A == s || p.B == s }

// AgentPartners keeps each user's pairs of chats that work together. The
// agents of a pair, and of their subagents, may exchange longer chains.
type AgentPartners interface {
	// AgentPartners lists the user's pairs on a backend.
	AgentPartners(ctx context.Context, user, backendID string) ([]AgentPartner, error)
	// AddAgentPartner adds a pair; a pair that exists stays. It fails
	// with ErrTooManyAgentPartners past MaxAgentPartners.
	AddAgentPartner(ctx context.Context, user, backendID string, p AgentPartner) error
	// RemoveAgentPartner removes a pair, and reports whether it was there.
	RemoveAgentPartner(ctx context.Context, user, backendID string, p AgentPartner) (bool, error)
}
