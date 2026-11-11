package hub

import (
	"context"
	"errors"
)

// Claude Code usage source kinds.
const (
	ClaudeCodeDevice = "device"
	ClaudeCodeHermes = "hermes"
)

// ClaudeCodeSource is where Claude Code usage comes from: a paired
// computer, or a backend's Hermes pod.
type ClaudeCodeSource struct {
	// ID is "device:<device id>" or "hermes:<backend id>".
	ID        string
	Kind      string
	User      string
	BackendID string
}

// ClaudeCodeRow is the tokens of one source, UTC hour, account,
// organization, model, project and thread kind.
type ClaudeCodeRow struct {
	Source       string `json:"-"`
	Hour         int64  `json:"hour"`
	Account      string `json:"account_uuid"`
	Org          string `json:"org_uuid"`
	Model        string `json:"model"`
	Project      string `json:"project"`
	Sidechain    bool   `json:"sidechain"`
	Input        int64  `json:"input"`
	Output       int64  `json:"output"`
	CacheWrite   int64  `json:"cache_write"`
	CacheWrite1h int64  `json:"cache_write_1h"`
	CacheRead    int64  `json:"cache_read"`
	Requests     int64  `json:"requests"`
}

// ClaudeCodeAccount is an account a source is signed in as, or a
// claude-accounts slot that is not signed in (empty UUID).
type ClaudeCodeAccount struct {
	Source string `json:"-"`
	UUID   string `json:"account_uuid"`
	Org    string `json:"org_uuid"`
	Label  string `json:"label"`
	Slot   string `json:"slot,omitempty"`
}

// ClaudeCodeReport is what a computer or Hermes sends: rows that replace
// the source's rows of each hour in Hours and of every hour from Since on
// (talaria docs/protocol.md section 13).
type ClaudeCodeReport struct {
	Since    int64               `json:"since,omitempty"`
	Hours    []int64             `json:"hours,omitempty"`
	Rows     []ClaudeCodeRow     `json:"rows"`
	Accounts []ClaudeCodeAccount `json:"accounts"`
}

// ClaudeCodeStore keeps Claude Code usage.
type ClaudeCodeStore interface {
	// PutClaudeCodeReport stores a report of src in one transaction.
	PutClaudeCodeReport(ctx context.Context, src ClaudeCodeSource, r ClaudeCodeReport) error
	ClaudeCodeSources(ctx context.Context) ([]ClaudeCodeSource, error)
	// ClaudeCodeRows returns the rows of sources with from <= hour < to.
	ClaudeCodeRows(ctx context.Context, sources []string, from, to int64) ([]ClaudeCodeRow, error)
	ClaudeCodeAccounts(ctx context.Context, sources []string) ([]ClaudeCodeAccount, error)
	// ClaudeAccountNames maps the account uuids user named to the names.
	ClaudeAccountNames(ctx context.Context, user string) (map[string]string, error)
	// SetClaudeAccountName names an account for user; "" removes the name.
	SetClaudeAccountName(ctx context.Context, user, accountUUID, name string) error
}

// ClaudeCodeTotals are summed tokens and their price at API rates.
type ClaudeCodeTotals struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheWrite int64   `json:"cache_write"`
	CacheRead  int64   `json:"cache_read"`
	Total      int64   `json:"total"`
	Requests   int64   `json:"requests"`
	CostUSD    float64 `json:"cost_usd"`
}

// ClaudeCodeGroup is one account, computer, model or project.
type ClaudeCodeGroup struct {
	Key string `json:"key"`
	// Label is "" for an account nobody named and no source labels.
	Label       string `json:"label"`
	AccountUUID string `json:"account_uuid,omitempty"`
	ClaudeCodeTotals
	// Daily is the total tokens of each day of the range.
	Daily []int64 `json:"daily"`
}

// ClaudeCodeUsage is the Usage page's Claude Code panel.
type ClaudeCodeUsage struct {
	Range    string            `json:"range"`
	Group    string            `json:"group"`
	TimeZone string            `json:"time_zone"`
	Days     []string          `json:"days"`
	Groups   []ClaudeCodeGroup `json:"groups"`
	Total    ClaudeCodeTotals  `json:"total"`
	// Unpriced are the models without a price, whose tokens cost counts
	// as nothing.
	Unpriced []string `json:"unpriced_models"`
}

// ClaudeCode serves the Claude Code panel.
type ClaudeCode interface {
	ClaudeCodeUsage(ctx context.Context, user, rng, group string) (ClaudeCodeUsage, error)
	NameClaudeAccount(ctx context.Context, user, accountUUID, name string) error
}

// Claude Code errors.
var (
	ErrBadRange = errors.New("range must be today, 7d, 30d or mtd, and group account, computer, model or project")
	ErrBadName  = errors.New("an account uuid and a name of at most 64 characters are needed")
	// ErrBadClaudeCodeReport is a usage report the hub refuses.
	ErrBadClaudeCodeReport = errors.New("bad Claude Code usage report")
)
