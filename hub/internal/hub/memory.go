package hub

import (
	"context"
	"errors"
	"io"
	"time"
)

// Memory targets: Hermes's own notes (MEMORY.md) and its profile of the
// user (USER.md).
const (
	TargetMemory = "memory"
	TargetUser   = "user"
)

// Note changes.
const (
	NoteAdd     = "add"
	NoteEdit    = "edit"
	NoteRemove  = "remove"
	NoteReplace = "replace"
)

// Learned views: every conclusion about the user, Hermes's view of the
// user, or the user's own words.
const (
	ViewAll    = "all"
	ViewHermes = "hermes"
	ViewSelf   = "self"
)

// MaxLearnedDelete bounds the conclusions one delete may remove.
const MaxLearnedDelete = 100

// MemoryBackend is a backend on the Memory page. Notes is false when its
// Hermes has no memory routes; Learned is false when it has no Honcho.
type MemoryBackend struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Notes   bool   `json:"notes"`
	Learned bool   `json:"learned"`
	Pending int    `json:"pending"`
	Error   string `json:"error,omitempty"`
}

// Notes is a backend's built-in memory: both stores, whether Hermes asks
// before it saves, and the writes waiting for approval.
type Notes struct {
	Approval bool                 `json:"approval"`
	Targets  map[string]NoteStore `json:"targets"`
	Pending  []PendingNote        `json:"pending"`
}

// NoteStore is MEMORY.md or USER.md.
type NoteStore struct {
	File    string   `json:"file"`
	Limit   int      `json:"limit"`
	Used    int      `json:"used"`
	Enabled bool     `json:"enabled"`
	Entries []string `json:"entries"`
}

// PendingNote is a memory write Hermes staged for approval.
type PendingNote struct {
	ID        string       `json:"id"`
	Target    string       `json:"target"`
	Summary   string       `json:"summary"`
	Origin    string       `json:"origin"`
	CreatedAt float64      `json:"created_at"`
	Changes   []NoteChange `json:"changes"`
}

// NoteChange is one operation of a staged write. Entry is the whole entry
// a replace, patch or remove acts on.
type NoteChange struct {
	Action  string `json:"action"`
	Content string `json:"content"`
	OldText string `json:"old_text"`
	Entry   string `json:"entry"`
}

// Conclusion is one fact Honcho drew from the user's chats.
type Conclusion struct {
	ID        string    `json:"id"`
	Content   string    `json:"content"`
	View      string    `json:"view"`
	SessionID string    `json:"session_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// LearnedQuery selects conclusions. Query non-empty searches by meaning;
// otherwise Page (from 1) pages through them newest first, or oldest first
// with Oldest.
type LearnedQuery struct {
	View   string
	Query  string
	Page   int
	Oldest bool
}

// LearnedPage is one page of conclusions. Pages is 0 for a search.
type LearnedPage struct {
	Items []Conclusion `json:"items"`
	Total int          `json:"total"`
	Page  int          `json:"page"`
	Pages int          `json:"pages"`
}

// LearnedInfo is the Advanced part of the Learned tab.
type LearnedInfo struct {
	Workspace string   `json:"workspace"`
	Total     int      `json:"total"`
	Hermes    int      `json:"hermes"`
	Self      int      `json:"self"`
	Card      []string `json:"card"`
}

// Memory is the Memory page's core. Every method fails with ErrForbidden
// when user may not use the backend.
type Memory interface {
	MemoryBackends(ctx context.Context, user string) ([]MemoryBackend, error)
	Notes(ctx context.Context, user, backendID string) (Notes, error)
	// ChangeNote applies a NoteAdd (content), NoteEdit (entry, content),
	// NoteRemove (entry) or NoteReplace (content is the whole file, base
	// the entries the editor showed).
	ChangeNote(ctx context.Context, user, backendID, target, op, entry, content string, base []string) (Notes, error)
	ResolvePending(ctx context.Context, user, backendID, pendingID string, approve bool) (Notes, error)
	SetApproval(ctx context.Context, user, backendID string, on bool) (Notes, error)
	Learned(ctx context.Context, user, backendID string, q LearnedQuery) (LearnedPage, error)
	LearnedInfo(ctx context.Context, user, backendID string) (LearnedInfo, error)
	AddLearned(ctx context.Context, user, backendID, content string) (Conclusion, error)
	// DeleteLearned deletes conclusions about the user, at most
	// MaxLearnedDelete. It returns how many it deleted.
	DeleteLearned(ctx context.Context, user, backendID string, ids []string) (int, error)
	// ExportLearned writes every conclusion about the user as a JSON array.
	ExportLearned(ctx context.Context, user, backendID string, w io.Writer) error
}

// ErrNoMemory is a backend without the memory part asked for.
var ErrNoMemory = errors.New("this backend has no such memory")

// MemoryError is a change Hermes or Honcho refused, with the HTTP status
// and the message for the user.
type MemoryError struct {
	Status  int
	Message string
}

func (e *MemoryError) Error() string { return e.Message }
