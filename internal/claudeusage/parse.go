// Package claudeusage counts the tokens Claude Code used on this computer,
// from the transcripts it writes under <config dir>/projects. It reads only
// token counts, model names, ids, timestamps, the account of each session
// and the base name of each session's working directory, never message text.
// docs/protocol.md section 7 says exactly what leaves the computer.
package claudeusage

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//nolint:gochecknoglobals // fixed markers of the lines worth decoding
var (
	usageMarker = []byte(`"usage":{`)
	ownerMarker = []byte(`"session-owner"`)
	advisorMark = []byte(`"advisor_message"`)
)

// rawUsage is message.usage. Only these fields are decoded; encoding/json
// skips every other field of the line without keeping it.
type rawUsage struct {
	Input         *uint64 `json:"input_tokens"`
	Output        *uint64 `json:"output_tokens"`
	CacheCreation uint64  `json:"cache_creation_input_tokens"`
	CacheRead     uint64  `json:"cache_read_input_tokens"`
	Speed         string  `json:"speed"`
	Split         *struct {
		M5 uint64 `json:"ephemeral_5m_input_tokens"`
		H1 uint64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// rawIteration is an item of message.usage.iterations. Its token counts
// sit on the item itself, as ccusage reads them (serde flatten).
type rawIteration struct {
	Type  string  `json:"type"`
	Model *string `json:"model"`
	rawUsage
}

type rawLine struct {
	Type           string  `json:"type"`
	SessionID      *string `json:"sessionId"`
	Timestamp      string  `json:"timestamp"`
	Version        *string `json:"version"`
	RequestID      *string `json:"requestId"`
	IsSidechain    *bool   `json:"isSidechain"`
	CWD            string  `json:"cwd"`
	RequestedModel string  `json:"requestedModel"`
	AccountUUID    string  `json:"accountUuid"`
	OrgUUID        string  `json:"organizationUuid"`
	Message        *struct {
		ID    *string  `json:"id"`
		Model *string  `json:"model"`
		Usage rawUsage `json:"usage"`
	} `json:"message"`
}

type rawIterations struct {
	Message struct {
		Usage struct {
			Iterations []rawIteration `json:"iterations"`
		} `json:"usage"`
	} `json:"message"`
}

// tokens are the counts of one response. CacheWrite includes CacheWrite1h.
type tokens struct {
	Input, Output, CacheWrite, CacheWrite1h, CacheRead uint64
}

func (t tokens) total() uint64 { return t.Input + t.Output + t.CacheWrite + t.CacheRead }

// record is one usage line, reduced to what the collector keeps.
type record struct {
	At        time.Time
	Session   string
	MessageID string
	HasID     bool
	RequestID string
	HasReq    bool
	Sidechain bool
	Model     string
	Project   string
	Tokens    tokens
	// detail breaks ties between duplicates with equal totals, as ccusage
	// does: the speed tier, then whether the cache-write split is there.
	detail uint8
}

// owner is a session-owner line: the account a session runs as.
type owner struct {
	Session, Account, Org string
}

// parseLine reads one transcript line. It returns the usage records of a
// usage line (more than one when the line has advisor iterations), or the
// owner of a session-owner line. fileSession stands in for a missing
// sessionId.
func parseLine(line []byte, fileSession string) ([]record, *owner) {
	if bytes.Contains(line, ownerMarker) && !bytes.Contains(line, usageMarker) {
		var l rawLine
		if json.Unmarshal(line, &l) != nil || l.Type != "session-owner" || l.SessionID == nil || *l.SessionID == "" {
			return nil, nil
		}
		return nil, &owner{Session: *l.SessionID, Account: l.AccountUUID, Org: l.OrgUUID}
	}
	if !bytes.Contains(line, usageMarker) {
		return nil, nil
	}
	var l rawLine
	if json.Unmarshal(line, &l) != nil || l.Message == nil || l.Message.Usage.Input == nil || l.Message.Usage.Output == nil {
		return nil, nil
	}
	at, err := time.Parse(time.RFC3339Nano, l.Timestamp)
	if err != nil || !valid(&l) {
		return nil, nil
	}
	session := fileSession
	if l.SessionID != nil {
		session = *l.SessionID
	}
	model := l.RequestedModel
	if l.Message.Model != nil {
		model = *l.Message.Model
	}
	if model == "<synthetic>" {
		model = ""
	}
	u := l.Message.Usage
	if u.Speed == "fast" && model != "" {
		model += "-fast"
	}
	r := record{
		At: at.UTC(), Session: session, Sidechain: l.IsSidechain != nil && *l.IsSidechain,
		Model: model, Project: project(l.CWD), Tokens: count(u), detail: detail(u),
	}
	if l.Message.ID != nil {
		r.MessageID, r.HasID = *l.Message.ID, true
	}
	if l.RequestID != nil {
		r.RequestID, r.HasReq = *l.RequestID, true
	}
	out := []record{r}
	if bytes.Contains(line, advisorMark) {
		out = append(out, advisors(line, r)...)
	}
	return out, nil
}

// advisors are the advisor_message iterations of a line, each counted
// under its own model and a message id of its own, as ccusage does.
func advisors(line []byte, r record) []record {
	var it rawIterations
	if json.Unmarshal(line, &it) != nil {
		return nil
	}
	var out []record
	for _, x := range it.Message.Usage.Iterations {
		if x.Type != "advisor_message" || x.Model == nil || *x.Model == "" || x.Input == nil || x.Output == nil {
			continue
		}
		a := r
		a.Model, a.Tokens, a.detail = *x.Model, count(x.rawUsage), detail(x.rawUsage)
		if a.HasID {
			a.MessageID = r.MessageID + ":advisor:" + strconv.Itoa(len(out))
		}
		out = append(out, a)
	}
	return out
}

func count(u rawUsage) tokens {
	t := tokens{Input: *u.Input, Output: *u.Output, CacheWrite: u.CacheCreation, CacheRead: u.CacheRead}
	if u.Split != nil {
		t.CacheWrite = u.Split.M5 + u.Split.H1
		t.CacheWrite1h = u.Split.H1
	}
	return t
}

func detail(u rawUsage) uint8 {
	var d uint8
	switch u.Speed {
	case "standard":
		d = 2
	case "fast":
		d = 4
	}
	if u.Split != nil {
		d |= 1
	}
	return d
}

// valid drops the lines ccusage drops: a version that is not semver, and
// an empty session id, request id, message id or model.
func valid(l *rawLine) bool {
	switch {
	case l.Version != nil && !semverPrefix(*l.Version),
		l.SessionID != nil && *l.SessionID == "",
		l.RequestID != nil && *l.RequestID == "",
		l.Message.ID != nil && *l.Message.ID == "",
		l.Message.Model != nil && *l.Message.Model == "":
		return false
	}
	return true
}

func semverPrefix(v string) bool {
	i := 0
	digits := func() bool {
		s := i
		for i < len(v) && v[i] >= '0' && v[i] <= '9' {
			i++
		}
		return i > s
	}
	for range 2 {
		if !digits() || i >= len(v) || v[i] != '.' {
			return false
		}
		i++
	}
	return i < len(v) && v[i] >= '0' && v[i] <= '9'
}

// project is the base name of a working directory, the only part of a
// path that leaves the computer.
func project(cwd string) string {
	cwd = strings.TrimRight(cwd, "/\\")
	if cwd == "" {
		return "unknown"
	}
	b := filepath.Base(cwd)
	if i := strings.LastIndexAny(b, "\\"); i >= 0 {
		b = b[i+1:]
	}
	if b == "" || b == "." || b == "/" {
		return "unknown"
	}
	return b
}

// fileSession is the session a transcript file belongs to, for lines
// without a sessionId: the parent session of a subagent file, else the
// file name.
func fileSession(path string) string {
	dir := filepath.Dir(path)
	if filepath.Base(dir) == "subagents" {
		return filepath.Base(filepath.Dir(dir))
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}
