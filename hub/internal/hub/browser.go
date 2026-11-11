package hub

import (
	"context"
	"time"
)

// Browser sign-in lifetimes (README, Devices).
const (
	// BrowserCodeTTL is how long an authorization code from the consent
	// page stays valid.
	BrowserCodeTTL = 2 * time.Minute
	// BrowserIdleTTL ends a browser sign-in that was not used for this
	// long.
	BrowserIdleTTL = 90 * 24 * time.Hour
)

// BrowserSession is a browser signed in to the hub for one device of its
// user: the Hermes Firefox extension in one browser profile (InstanceID).
type BrowserSession struct {
	ID         string
	User       string
	DeviceID   string
	InstanceID string
	Browser    string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// BrowserGrant is what the user approved on the consent page, kept under a
// single-use authorization code until the extension redeems it.
type BrowserGrant struct {
	User        string
	DeviceID    string
	InstanceID  string
	Browser     string
	Challenge   string
	RedirectURI string
}

// BrowserStore keeps browser sign-ins and their authorization codes. Only
// hashes of codes and tokens are stored.
type BrowserStore interface {
	// CreateBrowserCode stores g under a new code valid for BrowserCodeTTL.
	CreateBrowserCode(ctx context.Context, g BrowserGrant) (string, error)
	// RedeemBrowserCode deletes code and returns its grant. ok is false for
	// an unknown, used or expired code.
	RedeemBrowserCode(ctx context.Context, code string) (BrowserGrant, bool, error)
	// CreateBrowserSession signs a browser in and returns its token. An
	// active sign-in of the same device and instance is revoked.
	CreateBrowserSession(ctx context.Context, user, deviceID, instanceID, browser string) (string, BrowserSession, error)
	// BrowserSessionByToken returns the active sign-in of a token.
	BrowserSessionByToken(ctx context.Context, token string) (BrowserSession, bool, error)
	// BrowserSession returns an active sign-in by id. Active means not
	// revoked, its device not revoked, and used within BrowserIdleTTL.
	BrowserSession(ctx context.Context, id string) (BrowserSession, bool, error)
	// TouchBrowserSession records a use of a sign-in.
	TouchBrowserSession(ctx context.Context, id string) error
	// BrowserSessions lists the active sign-ins of user's devices, oldest
	// first.
	BrowserSessions(ctx context.Context, user string) ([]BrowserSession, error)
	// RevokeBrowserSession revokes one of user's sign-ins and returns its
	// device id.
	RevokeBrowserSession(ctx context.Context, user, id string) (string, bool, error)
	// RevokeBrowserToken revokes the sign-in of a token.
	RevokeBrowserToken(ctx context.Context, token string) (BrowserSession, bool, error)
}
