package gateway

import (
	"context"
	"time"
)

// NewDialerWithAttempt is newDialer for tests: a Dialer whose calls time out after attempt.
func NewDialerWithAttempt(creds Credentials, attempt time.Duration) *Dialer {
	return newDialer(creds, attempt)
}

// LoginForTest signs in as Dial does.
func (d *Dialer) LoginForTest(ctx context.Context) error { return d.login(ctx) }

// TicketForTest mints a socket ticket as Dial does.
func (d *Dialer) TicketForTest(ctx context.Context) (string, error) { return d.ticket(ctx) }
