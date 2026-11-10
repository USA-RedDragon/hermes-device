package telegram

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Config is the Telegram bot configuration. Nest it in the app config.
type Config struct {
	Enabled         bool          `name:"enabled" default:"false" description:"Run the Telegram bot"`
	Token           string        `name:"token" secret:"true" description:"Telegram bot token from BotFather"`
	APIURL          string        `name:"api_url" default:"https://api.telegram.org" description:"Telegram Bot API base URL"`
	DefaultBackends []string      `name:"default_backends" description:"Default backend per hub user, as user=backend pairs. A user without one gets their first backend"`
	AllowedGroups   []int64       `name:"allowed_groups" description:"Group chat IDs the bot answers in. The bot ignores every other group"`
	PollTimeout     time.Duration `name:"poll_timeout" default:"50s" description:"Long poll timeout for getUpdates"`
	EditInterval    time.Duration `name:"edit_interval" default:"1500ms" description:"Minimum time between edits of one live message"`
	MaxRetries      int           `name:"max_retries" default:"5" description:"Retries of a Bot API call that Telegram answers with 429"`
	LeaderRetry     time.Duration `name:"leader_retry" default:"2s" description:"With store.driver postgres, only one hub replica runs the bot. This is how often each other replica tries to take it over"`
}

// Errors returned by Config.Validate.
var (
	ErrMissingToken      = errors.New("telegram.token is required when telegram.enabled is true")
	ErrBadDefaultBackend = errors.New("telegram.default_backends entries must be user=backend")
	ErrBadInterval       = errors.New("telegram.poll_timeout, telegram.edit_interval and telegram.leader_retry must be positive")
)

// Validate checks the configuration. It accepts anything when the bot is
// disabled.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.Token == "" {
		return ErrMissingToken
	}
	if c.PollTimeout <= 0 || c.EditInterval <= 0 || c.LeaderRetry <= 0 {
		return ErrBadInterval
	}
	_, err := c.defaultBackends()
	return err
}

func (c Config) defaultBackends() (map[string]string, error) {
	out := make(map[string]string, len(c.DefaultBackends))
	for _, pair := range c.DefaultBackends {
		user, backend, ok := strings.Cut(pair, "=")
		user, backend = strings.TrimSpace(user), strings.TrimSpace(backend)
		if !ok || user == "" || backend == "" {
			return nil, fmt.Errorf("%w: %q", ErrBadDefaultBackend, pair)
		}
		out[user] = backend
	}
	return out, nil
}
