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
// user, or the user's own words. ViewBoth marks a fact stored once in each
// of the two views.
const (
	ViewAll    = "all"
	ViewHermes = "hermes"
	ViewSelf   = "self"
	ViewBoth   = "both"
)

// Levels of a conclusion, as Honcho marks them. Honcho's deriver writes
// LevelExplicit facts from single messages; its dreams write the others
// from facts it already has.
const (
	LevelExplicit      = "explicit"
	LevelDeductive     = "deductive"
	LevelInductive     = "inductive"
	LevelContradiction = "contradiction"
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

// Project kinds: a git repository with a remote, one without a remote
// (keyed by its root commit), or a folder outside git.
const (
	ProjectRemote = "remote"
	ProjectRoot   = "root"
	ProjectFolder = "folder"
)

// Project is one project of the project-memory plugin: a git repository,
// keyed by its normalized remote URL, or a folder. Aliases are the other
// identities attached to it, such as a mirror on another host.
type Project struct {
	ID         string       `json:"id"`
	Identity   string       `json:"identity"`
	Kind       string       `json:"kind"`
	Name       string       `json:"name"`
	Aliases    []string     `json:"aliases"`
	RootCommit string       `json:"root_commit"`
	Folders    []SeenFolder `json:"folders"`
	Count      int          `json:"count"`
	Used       int          `json:"used"`
	Limit      int          `json:"limit"`
}

// SeenFolder is a folder on a device where Hermes saw a project. Device is
// the device's display name, or its id.
type SeenFolder struct {
	DeviceID string  `json:"device_id"`
	Device   string  `json:"device"`
	Dir      string  `json:"dir"`
	SeenAt   float64 `json:"seen_at"`
}

// ProjectNotes is a project's MEMORY.md, as TargetMemory of Notes, with
// the project. It has no USER.md, approval or pending writes.
type ProjectNotes struct {
	Notes
	Project Project `json:"project"`
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

// Conclusion is one fact Honcho drew from the user's chats. On the
// Learned tab it is one distinct fact: IDs holds every stored copy of it,
// ID the oldest, and View is ViewBoth when both views hold a copy. Level
// is the most condensed level of its copies. An exported conclusion is one
// stored copy and has no IDs.
type Conclusion struct {
	ID        string    `json:"id"`
	IDs       []string  `json:"ids,omitempty"`
	Content   string    `json:"content"`
	View      string    `json:"view"`
	Level     string    `json:"level"`
	SessionID string    `json:"session_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// LearnedQuery selects conclusions. Query non-empty searches by meaning;
// otherwise Page (from 1) pages through them newest first, or oldest first
// with Oldest. Without All, only facts Honcho condensed in its dreams show.
type LearnedQuery struct {
	View   string
	Query  string
	Page   int
	Oldest bool
	All    bool
}

// LearnedPage is one page of distinct facts. Pages is 0 for a search.
// Total counts the distinct facts that match, Copies their stored copies,
// and Hidden the distinct facts the query hid because they come from
// single messages. Partial is true when the user has more stored copies
// than the hub reads, so the oldest are left out.
type LearnedPage struct {
	Items   []Conclusion `json:"items"`
	Total   int          `json:"total"`
	Copies  int          `json:"copies"`
	Hidden  int          `json:"hidden"`
	Page    int          `json:"page"`
	Pages   int          `json:"pages"`
	Partial bool         `json:"partial,omitempty"`
}

// LearnedInfo is the Advanced part of the Learned tab. Total, Hermes, Self,
// Both, Condensed and Raw count distinct facts; Copies counts stored
// copies.
type LearnedInfo struct {
	Workspace string   `json:"workspace"`
	Total     int      `json:"total"`
	Copies    int      `json:"copies"`
	Hermes    int      `json:"hermes"`
	Self      int      `json:"self"`
	Both      int      `json:"both"`
	Condensed int      `json:"condensed"`
	Raw       int      `json:"raw"`
	Partial   bool     `json:"partial,omitempty"`
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
	// Projects lists the backend's projects by name. It fails with
	// ErrNoMemory when the backend's Hermes has no project-memory plugin.
	Projects(ctx context.Context, user, backendID string) ([]Project, error)
	ProjectNotes(ctx context.Context, user, backendID, projectID string) (ProjectNotes, error)
	// ChangeProjectNote applies a change, as ChangeNote does, to a
	// project's MEMORY.md.
	ChangeProjectNote(ctx context.Context, user, backendID, projectID, op, entry, content string, base []string) (ProjectNotes, error)
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
