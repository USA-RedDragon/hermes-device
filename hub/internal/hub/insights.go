package hub

import (
	"context"
	"errors"
	"time"
)

// SearchHit is one message that matches a search. RowID is the
// message's row_id in the chat's transcript. Snippet marks each match
// with ">>>" and "<<<".
type SearchHit struct {
	RowID     int64     `json:"row_id"`
	SessionID string    `json:"session_id"`
	Role      string    `json:"role"`
	Snippet   string    `json:"snippet"`
	At        time.Time `json:"at,omitzero"`
}

// SearchResult is one chat with messages that match a search. SessionID
// is the chat to open.
type SearchResult struct {
	BackendID  string      `json:"backend_id"`
	SessionID  string      `json:"session_id"`
	Title      string      `json:"title"`
	LastActive time.Time   `json:"last_active,omitzero"`
	Archived   bool        `json:"archived,omitempty"`
	Hits       []SearchHit `json:"hits"`
}

// SearchReport is the answer to a search: the chats that match, best
// first, and the backends that could not search, by name, with why.
type SearchReport struct {
	Results []SearchResult    `json:"results"`
	Errors  map[string]string `json:"errors,omitempty"`
}

// Tokens are token counters.
type Tokens struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Reasoning  int64   `json:"reasoning"`
	Calls      int64   `json:"calls"`
	CostUSD    float64 `json:"cost_usd"`
}

// UsageSession is the token use of one session row. ChatID is the chat
// it belongs to (the tip of its compression chain).
type UsageSession struct {
	SessionID  string    `json:"session_id"`
	ChatID     string    `json:"chat_id"`
	Title      string    `json:"title"`
	Source     string    `json:"source"`
	Model      string    `json:"model"`
	Provider   string    `json:"provider"`
	LastActive time.Time `json:"last_active"`
	Tokens
}

// UsageModel is the token use of one model and provider.
type UsageModel struct {
	Model    string `json:"model"`
	Provider string `json:"provider"`
	Tokens
}

// RateBucket is one rate-limit window a provider reported.
type RateBucket struct {
	Limit        int64   `json:"limit"`
	Remaining    int64   `json:"remaining"`
	ResetSeconds float64 `json:"reset_seconds"`
}

// ProviderLimits are the rate limits a provider last reported in its
// response headers, as Hermes parsed them, and when.
type ProviderLimits struct {
	Provider     string     `json:"provider"`
	At           time.Time  `json:"at"`
	RequestsMin  RateBucket `json:"requests_min"`
	RequestsHour RateBucket `json:"requests_hour"`
	TokensMin    RateBucket `json:"tokens_min"`
	TokensHour   RateBucket `json:"tokens_hour"`
}

// LiveUsage is the latest usage the hub saw for one chat while it ran.
type LiveUsage struct {
	SessionID      string    `json:"session_id"`
	Model          string    `json:"model,omitempty"`
	Provider       string    `json:"provider,omitempty"`
	ContextUsed    int64     `json:"context_used,omitempty"`
	ContextMax     int64     `json:"context_max,omitempty"`
	TokensPerSec   float64   `json:"tokens_per_s,omitempty"`
	AvgLatencySecs float64   `json:"avg_latency_s,omitempty"`
	At             time.Time `json:"at"`
}

// Observed is what the hub saw in the usage events of a backend's chats
// since it started: the latest rate limits of each provider and the
// latest usage of each chat. Each hub replica sees only the chats it ran.
type Observed struct {
	Limits []ProviderLimits `json:"limits"`
	Chats  []LiveUsage      `json:"chats"`
}

// Backlog is Honcho's queue of work for a workspace: messages it has not
// yet turned into memory.
type Backlog struct {
	Total      int64 `json:"total"`
	Completed  int64 `json:"completed"`
	InProgress int64 `json:"in_progress"`
	Pending    int64 `json:"pending"`
}

// GPUWindow is the local GPU's time over one window, from the Ollama
// proxy's metrics: Busy is the time any request was running, People and
// Background the summed time of the requests of each class, which can
// overlap.
type GPUWindow struct {
	Label      string  `json:"label"`
	Busy       float64 `json:"busy_s"`
	People     float64 `json:"people_s"`
	Background float64 `json:"background_s"`
	Requests   float64 `json:"requests"`
}

// UsageReport is the Usage page of one backend. Each part that could not
// be read has its error instead.
type UsageReport struct {
	BackendID     string         `json:"backend_id"`
	Days          int            `json:"days"`
	Since         time.Time      `json:"since,omitzero"`
	Sessions      []UsageSession `json:"sessions"`
	Models        []UsageModel   `json:"models"`
	Truncated     bool           `json:"truncated,omitempty"`
	TokensError   string         `json:"tokens_error,omitempty"`
	Observed      Observed       `json:"observed"`
	Backlog       *Backlog       `json:"backlog,omitempty"`
	BacklogError  string         `json:"backlog_error,omitempty"`
	GPU           []GPUWindow    `json:"gpu,omitempty"`
	GPUError      string         `json:"gpu_error,omitempty"`
	ObservedSince time.Time      `json:"observed_since,omitzero"`
}

// Insights serves chat search and the Usage page. Every method fails
// with ErrForbidden when user may not use the backend.
type Insights interface {
	// Search searches the messages of user's chats on every backend
	// they may use.
	Search(ctx context.Context, user, query string) (SearchReport, error)
	// Usage reports one backend's token use over the last days days.
	Usage(ctx context.Context, user, backendID string, days int) (UsageReport, error)
}

// ErrBadQuery is a search the backend refused, wrapping why.
var ErrBadQuery = errors.New("bad search")

// ErrSearchBusy is a search while the user's earlier searches still run.
var ErrSearchBusy = errors.New("earlier searches are still running; try again in a moment")

// FolderEntry is one entry of a chat's folder.
type FolderEntry struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	Link bool   `json:"link,omitempty"`
	Size int64  `json:"size"`
}

// ChatFolder is one folder on the computer of a chat. Dir is the chat's
// own folder.
type ChatFolder struct {
	Path      string        `json:"path"`
	Dir       string        `json:"dir"`
	Roots     []string      `json:"roots,omitempty"`
	Entries   []FolderEntry `json:"entries"`
	Truncated bool          `json:"truncated,omitempty"`
}

// DiffFile is one changed file of a diff.
type DiffFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// ChatDiff is what git shows as changed in a chat's folder. Git is false
// when the folder is not in a git repository. Hidden counts changed
// files left out because they are protected.
type ChatDiff struct {
	Dir string `json:"dir"`
	Git bool   `json:"git"`
	// Refused is a folder in a repository whose git files are outside
	// the computer's roots or protected, so git does not run.
	Refused   bool       `json:"refused,omitempty"`
	Files     []DiffFile `json:"files"`
	Untracked []string   `json:"untracked"`
	Diff      string     `json:"diff"`
	Truncated bool       `json:"truncated,omitempty"`
	Hidden    int        `json:"hidden,omitempty"`
}

// WrittenFile is a file the agent wrote in a chat: Path as it named it,
// Abs that path on the computer, or "" when the hub cannot tell.
type WrittenFile struct {
	Path  string    `json:"path"`
	Abs   string    `json:"abs,omitempty"`
	Tools []string  `json:"tools"`
	Count int       `json:"count"`
	At    time.Time `json:"at,omitzero"`
	RowID int64     `json:"row_id,omitempty"`
}

// ChatChanges are the files the agent wrote in a chat.
type ChatChanges struct {
	Files     []WrittenFile `json:"files"`
	Truncated bool          `json:"truncated,omitempty"`
}

// ChatFiles lets a user look at the folder of a chat that runs on one of
// their own computers or in the sandbox. It never changes a file. Every
// method fails with ErrForbidden when user may not use the backend and
// with ErrNoFolder when the chat has no folder the user may see.
type ChatFiles interface {
	ChatFolder(ctx context.Context, user, backendID, sessionID, path string) (ChatFolder, error)
	// ChatFile opens a file; ErrMediaTooLarge for one over the plugin's
	// limit, ErrMediaOffline when the computer is offline.
	ChatFile(ctx context.Context, user, backendID, sessionID, path string) (*MediaFile, error)
	ChatDiff(ctx context.Context, user, backendID, sessionID string) (ChatDiff, error)
	ChatChanges(ctx context.Context, user, backendID, sessionID string) (ChatChanges, error)
}

// Chat file errors. ErrFileRefused wraps the reason for the user.
var (
	ErrNoFolder     = errors.New("this chat does not run on one of your computers or in the sandbox")
	ErrFileRefused  = errors.New("refused")
	ErrFileNotFound = errors.New("not found")
	ErrFilesBusy    = errors.New("the computer is busy with other file reads; try again in a moment")
	ErrFilesTooOld  = errors.New("the Talaria device plugin is too old for this; update talaria on the Hermes server")
)
