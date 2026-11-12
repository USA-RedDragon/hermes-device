package hub

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Limits of a user's prompt library.
const (
	MaxPromptName  = 64
	MaxPromptText  = 64 << 10
	MaxUserPrompts = 200
)

var (
	// ErrPromptExists is a prompt name that the user already has.
	ErrPromptExists = errors.New("prompt name in use")
	// ErrNoPrompt is a prompt that does not exist.
	ErrNoPrompt = errors.New("no such prompt")
	// ErrTooManyPrompts is a library that is full.
	ErrTooManyPrompts = errors.New("too many prompts")
	// ErrBadPrompt is a prompt that fails ValidatePrompt.
	ErrBadPrompt = errors.New("bad prompt")
)

// SavedPrompt is a prompt that a user keeps in the hub, to put into a
// message with /prompt:<name>.
type SavedPrompt struct {
	Name      string    `json:"name"`
	Text      string    `json:"text"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// PromptLibrary keeps each user's saved prompts. Names are unique for a
// user regardless of case.
type PromptLibrary interface {
	// Prompts lists user's prompts by name.
	Prompts(ctx context.Context, user string) ([]SavedPrompt, error)
	// Prompt returns one prompt; false when there is none.
	Prompt(ctx context.Context, user, name string) (SavedPrompt, bool, error)
	// AddPrompt adds a prompt: ErrPromptExists when the name is taken,
	// ErrTooManyPrompts when the library is full.
	AddPrompt(ctx context.Context, user string, p SavedPrompt) error
	// UpdatePrompt replaces the prompt named name with p, which may have a
	// new name: ErrNoPrompt when there is none, ErrPromptExists when the
	// new name is taken.
	UpdatePrompt(ctx context.Context, user, name string, p SavedPrompt) error
	// DeletePrompt deletes a prompt: ErrNoPrompt when there is none.
	DeletePrompt(ctx context.Context, user, name string) error
}

// PromptKey is the form of a prompt name that is unique for a user.
func PromptKey(name string) string { return strings.ToLower(name) }

// ValidatePrompt checks a prompt's name and text. The error wraps
// ErrBadPrompt and says what is wrong in words for the user.
func ValidatePrompt(p SavedPrompt) error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrBadPrompt, msg) }
	switch {
	case p.Name == "":
		return bad("Give the prompt a name")
	case len(p.Name) > MaxPromptName:
		return bad(fmt.Sprintf("A name can have at most %d characters", MaxPromptName))
	case !validPromptName(p.Name):
		return bad("A name can use only the letters A to Z, digits and dashes, with no spaces")
	case strings.TrimSpace(p.Text) == "":
		return bad("Give the prompt some text")
	case len(p.Text) > MaxPromptText:
		return bad("The text is longer than 64 KB")
	}
	return nil
}

func validPromptName(s string) bool {
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

// PromptReason is the words for the user in an ErrBadPrompt error.
func PromptReason(err error) string {
	return strings.TrimPrefix(err.Error(), ErrBadPrompt.Error()+": ")
}
