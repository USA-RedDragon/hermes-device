package hub

import (
	"context"
	"time"
)

// TelegramStatus is Settings, Telegram for an admin.
type TelegramStatus struct {
	// Install is true when the install sets the token: read-only here.
	Install bool `json:"install"`
	// Saved is true when an admin saved a token here.
	Saved   bool   `json:"saved"`
	Running bool   `json:"running"`
	Bot     string `json:"bot,omitempty"`
	Steps   string `json:"steps,omitempty"`
	Error   string `json:"error,omitempty"`
}

// TelegramError is a refusal with its HTTP status and plain words.
type TelegramError struct {
	Status  int
	Message string
}

func (e *TelegramError) Error() string { return e.Message }

// TelegramAdmin sets the hub's Telegram bot token from Settings.
type TelegramAdmin interface {
	TelegramStatus(ctx context.Context) TelegramStatus
	// TestTelegram asks Telegram who the token belongs to: "@name".
	TestTelegram(ctx context.Context, token string) (string, error)
	// SaveTelegram tests, keeps and starts the bot with the token.
	SaveTelegram(ctx context.Context, token string) (string, error)
	// ClearTelegram forgets the token and stops the bot.
	ClearTelegram(ctx context.Context) error
	// Steps is the bot's steps setting while a bot runs, else "".
	Steps() string
}

// Release is a version of Talaria the update check found.
type Release struct {
	Version string    `json:"version"`
	URL     string    `json:"url"`
	At      time.Time `json:"at"`
}

// UpdateStatus is what the admin-only update banner shows.
type UpdateStatus struct {
	// Current is the hub's own version.
	Current string `json:"current"`
	// Latest is the newest hub release, when newer than Current.
	Latest *Release `json:"latest,omitempty"`
	// Agent is the agent release each backend runs and the newest one.
	Agent []AgentVersion `json:"agent,omitempty"`
	// Computers run an older talaria than the newest release.
	Computers []ComputerVersion `json:"computers,omitempty"`
	// Install is how Talaria was installed (ui.install).
	Install   string    `json:"install,omitempty"`
	CheckedAt time.Time `json:"checked_at"`
}

// AgentVersion is the agent release of one backend.
type AgentVersion struct {
	Backend string `json:"backend"`
	Running string `json:"running"`
	Latest  string `json:"latest,omitempty"`
}

// ComputerVersion is a paired computer with an older talaria.
type ComputerVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Latest  string `json:"latest"`
}
