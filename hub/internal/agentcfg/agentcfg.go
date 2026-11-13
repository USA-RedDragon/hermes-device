// Package agentcfg drives the agent's own settings from the hub: its
// personality, how it asks before risky commands, long-chat behavior, time
// zone, toolsets, skills, connections (MCP servers), diagnostics and its
// share of a backup. It calls a fixed list of the agent's dashboard routes,
// never a generic proxy, and each call checks what it sends. Settings the
// install owns (agent.locked_keys) are read-only here.
package agentcfg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// Access says which backends a user may use. *service.Service is one.
type Access interface {
	Allowed(ctx context.Context, user, backendID string) error
}

// Options are the dependencies of a Service.
type Options struct {
	Access Access
	// Requesters reach each backend's agent with the hub's own login.
	Requesters map[string]device.Requester
	// Locked are the setting keys the install owns (agent.locked_keys):
	// shown read-only, never written.
	Locked []string
	// Profile is the agent profile whose personality the hub edits.
	// Default "default".
	Profile string
	// ActionWait bounds how long a background action (health check,
	// skill install, backup) may run before the hub gives up. Default 10m.
	ActionWait time.Duration
	// Poll is how often a running action is checked. Default 1s.
	Poll time.Duration
}

// Service is the agent settings core.
type Service struct {
	access  Access
	rs      map[string]device.Requester
	locked  []string
	profile string
	wait    time.Duration
	poll    time.Duration
}

// New returns a Service.
func New(o Options) *Service {
	if o.Profile == "" {
		o.Profile = "default"
	}
	if o.ActionWait == 0 {
		o.ActionWait = 10 * time.Minute
	}
	if o.Poll == 0 {
		o.Poll = time.Second
	}
	rs := map[string]device.Requester{}
	for id, r := range o.Requesters {
		if r != nil {
			rs[id] = r
		}
	}
	return &Service{access: o.Access, rs: rs, locked: slices.Clone(o.Locked), profile: o.Profile, wait: o.ActionWait, poll: o.Poll}
}

// Error is an answer the hub passes on with its status.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

const (
	unreachable   = "Could not reach the agent. It may be restarting. Try again in a moment."
	notUnderstood = "The agent's answer was not understood."
	fieldEnabled  = "enabled"
	fieldName     = "name"
)

const maxAnswer = 8 << 20

func bad(msg string) error { return &Error{Status: http.StatusBadRequest, Message: msg} }

// Requester returns the backend's requester after the access check.
func (s *Service) Requester(ctx context.Context, user, backendID string) (device.Requester, error) {
	if s.access != nil {
		if err := s.access.Allowed(ctx, user, backendID); err != nil {
			return nil, err
		}
	}
	r, ok := s.rs[backendID]
	if !ok {
		return nil, hub.ErrUnknownBackend
	}
	return r, nil
}

type slowRequester interface {
	RequestSlow(ctx context.Context, method, path string, body []byte) (*http.Response, error)
}

// Call sends one request to the agent and returns its JSON answer. A
// refusal comes back as an *Error with the agent's own words.
func Call(ctx context.Context, r device.Requester, slow bool, method, path string, body any) (json.RawMessage, error) {
	var b []byte
	if body != nil {
		var err error
		if b, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	var resp *http.Response
	var err error
	if sr, ok := r.(slowRequester); ok && slow {
		resp, err = sr.RequestSlow(ctx, method, path, b)
	} else {
		resp, err = r.Request(ctx, method, path, b)
	}
	if err != nil {
		return nil, &Error{Status: http.StatusBadGateway, Message: unreachable}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, &Error{Status: http.StatusBadGateway, Message: unreachable}
	}
	if resp.StatusCode/100 != 2 {
		return nil, refusal(resp.StatusCode, data)
	}
	if !json.Valid(data) {
		return nil, &Error{Status: http.StatusBadGateway, Message: notUnderstood}
	}
	return data, nil
}

func refusal(status int, data []byte) error {
	var e struct {
		Detail json.RawMessage `json:"detail"`
		Error  string          `json:"error"`
	}
	msg := ""
	if json.Unmarshal(data, &e) == nil {
		var text string
		var list []struct {
			Msg string `json:"msg"`
		}
		switch {
		case json.Unmarshal(e.Detail, &text) == nil && text != "":
			msg = text
		case json.Unmarshal(e.Detail, &list) == nil && len(list) > 0:
			parts := make([]string, 0, len(list))
			for _, l := range list {
				parts = append(parts, l.Msg)
			}
			msg = strings.Join(parts, "; ")
		case e.Error != "":
			msg = e.Error
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("The agent answered HTTP %d.", status)
	}
	code := status
	if status >= 500 || status == http.StatusUnauthorized || status == http.StatusForbidden {
		code = http.StatusBadGateway
	}
	return &Error{Status: code, Message: msg}
}

func decode[T any](data json.RawMessage, err error) (T, error) {
	var out T
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, &Error{Status: http.StatusBadGateway, Message: notUnderstood}
	}
	return out, nil
}

// ---- Settings, Agent ----

// Keys the hub edits in Settings, Agent.
const (
	KeyApprovals   = "approvals.mode"
	KeyCompression = "compression.enabled"
	KeyThreshold   = "compression.threshold"
	KeyTimeZone    = "timezone"
	KeySoul        = "soul"
)

// ApprovalModes are the agent's approvals.mode values.
func ApprovalModes() []string { return []string{"manual", "smart", "off"} }

// Thresholds are the long-chat summary points the hub offers.
func Thresholds() []float64 { return []float64{0.4, 0.5, 0.7, 0.85} }

// Settings is Settings, Agent.
type Settings struct {
	ApprovalsMode        string   `json:"approvals_mode"`
	CompressionEnabled   bool     `json:"compression_enabled"`
	CompressionThreshold float64  `json:"compression_threshold"`
	TimeZone             string   `json:"timezone"`
	Soul                 string   `json:"soul"`
	Locked               []string `json:"locked"`
	// Live is true when the agent applies changes to open chats.
	Live bool `json:"live"`
}

// SettingsUpdate holds the settings to change; nil ones stay.
type SettingsUpdate struct {
	ApprovalsMode        *string  `json:"approvals_mode"`
	CompressionEnabled   *bool    `json:"compression_enabled"`
	CompressionThreshold *float64 `json:"compression_threshold"`
	TimeZone             *string  `json:"timezone"`
}

// Locked reports whether the install owns key (or a section above it).
func (s *Service) Locked(key string) bool {
	for _, k := range s.locked {
		if k == key || strings.HasPrefix(key, k+".") {
			return true
		}
	}
	return false
}

func lockedErr() error {
	return &Error{Status: http.StatusConflict, Message: "Set in the deployment config, so it cannot be changed here."}
}

// AgentSettings reads Settings, Agent.
func (s *Service) AgentSettings(ctx context.Context, user, backendID string) (Settings, error) {
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return Settings{}, err
	}
	cfg, err := decode[map[string]any](Call(ctx, r, false, http.MethodGet, "/api/config", nil))
	if err != nil {
		return Settings{}, err
	}
	st := Settings{ApprovalsMode: "manual", CompressionEnabled: true, CompressionThreshold: 0.5, Locked: []string{}}
	if a, ok := cfg["approvals"].(map[string]any); ok {
		switch v := a["mode"].(type) {
		case string:
			st.ApprovalsMode = strings.ToLower(v)
		case bool:
			if !v {
				st.ApprovalsMode = "off"
			}
		}
	}
	if c, ok := cfg["compression"].(map[string]any); ok {
		if v, ok := c["enabled"].(bool); ok {
			st.CompressionEnabled = v
		}
		if v, ok := c["threshold"].(float64); ok {
			st.CompressionThreshold = v
		}
	}
	if v, ok := cfg["timezone"].(string); ok {
		st.TimeZone = v
	}
	if d, ok := cfg["dashboard"].(map[string]any); ok {
		st.Live, _ = d["apply_to_live_chats"].(bool)
	}
	soul, err := decode[struct {
		Content string `json:"content"`
	}](Call(ctx, r, false, http.MethodGet, "/api/profiles/"+url.PathEscape(s.profile)+"/soul", nil))
	if err != nil {
		return Settings{}, err
	}
	st.Soul = soul.Content
	for _, k := range []string{KeySoul, KeyApprovals, KeyCompression, KeyThreshold, KeyTimeZone} {
		if s.Locked(k) {
			st.Locked = append(st.Locked, k)
		}
	}
	return st, nil
}

// SaveSettings writes the settings that u holds, only those.
func (s *Service) SaveSettings(ctx context.Context, user, backendID string, u SettingsUpdate) error {
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return err
	}
	cfg := map[string]any{}
	set := func(key string, section, name string, v any) error {
		if s.Locked(key) {
			return lockedErr()
		}
		if section == "" {
			cfg[name] = v
			return nil
		}
		m, _ := cfg[section].(map[string]any)
		if m == nil {
			m = map[string]any{}
			cfg[section] = m
		}
		m[name] = v
		return nil
	}
	if u.ApprovalsMode != nil {
		if !slices.Contains(ApprovalModes(), *u.ApprovalsMode) {
			return bad("Unknown choice for risky commands.")
		}
		if err := set(KeyApprovals, "approvals", "mode", *u.ApprovalsMode); err != nil {
			return err
		}
	}
	if u.CompressionEnabled != nil {
		if err := set(KeyCompression, "compression", "enabled", *u.CompressionEnabled); err != nil {
			return err
		}
	}
	if u.CompressionThreshold != nil {
		if !slices.Contains(Thresholds(), *u.CompressionThreshold) {
			return bad("Unknown point for long chats.")
		}
		if err := set(KeyThreshold, "compression", "threshold", *u.CompressionThreshold); err != nil {
			return err
		}
	}
	if u.TimeZone != nil {
		tz := strings.TrimSpace(*u.TimeZone)
		if _, err := time.LoadLocation(tz); err != nil || tz == "" || strings.EqualFold(tz, "local") {
			return bad("Unknown time zone.")
		}
		if err := set(KeyTimeZone, "", "timezone", tz); err != nil {
			return err
		}
	}
	if len(cfg) == 0 {
		return nil
	}
	_, err = Call(ctx, r, false, http.MethodPut, "/api/config", map[string]any{"config": cfg})
	return err
}

const maxSoul = 64 << 10

// SaveSoul writes the agent's personality. It answers whether open chats
// get it on their next message.
func (s *Service) SaveSoul(ctx context.Context, user, backendID, content string) (bool, error) {
	if s.Locked(KeySoul) {
		return false, lockedErr()
	}
	if len(content) > maxSoul {
		return false, bad("The personality is too long. Keep it under 64 KB.")
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return false, err
	}
	ans, err := decode[liveAnswer](Call(ctx, r, false, http.MethodPut, "/api/profiles/"+url.PathEscape(s.profile)+"/soul",
		map[string]string{"content": content}))
	return ans.AppliedLive, err
}

type liveAnswer struct {
	AppliedLive bool `json:"applied_live"`
}

// ---- Skills and tools ----

// Toolset is one group of tools the agent offers.
type Toolset struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	Configured  bool   `json:"configured"`
	// Always is true for a toolset Talaria needs: shown on, never off.
	Always bool `json:"always,omitempty"`
}

// alwaysOn are toolsets Talaria's own parts need: files and commands run
// on the user's computers or the sandbox through them.
func alwaysOn(name string) bool {
	return name == "terminal" || name == "file"
}

// talariaOwned are the toolsets of Talaria's own plugin (its computers,
// their browsers and their tools): Talaria turns them on, never a switch.
func talariaOwned(name string) bool {
	return strings.HasPrefix(name, "device-")
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Toolsets lists the agent's toolsets.
func (s *Service) Toolsets(ctx context.Context, user, backendID string) ([]Toolset, error) {
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	rows, err := decode[[]Toolset](Call(ctx, r, false, http.MethodGet, "/api/tools/toolsets", nil))
	if err != nil {
		return nil, err
	}
	out := make([]Toolset, 0, len(rows))
	for _, t := range rows {
		if talariaOwned(t.Name) {
			continue
		}
		t.Always = alwaysOn(t.Name)
		if t.Always {
			t.Enabled = true
		}
		out = append(out, t)
	}
	return out, nil
}

// SetToolset turns a toolset on or off; it answers whether open chats get
// the change on their next message.
func (s *Service) SetToolset(ctx context.Context, user, backendID, name string, on bool) (bool, error) {
	if !namePattern.MatchString(name) {
		return false, bad("Unknown tool.")
	}
	if alwaysOn(name) || talariaOwned(name) {
		return false, &Error{Status: http.StatusConflict, Message: "Talaria needs this one: it runs commands and files on your computers and the sandbox."}
	}
	if s.Locked("tools." + name) {
		return false, lockedErr()
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return false, err
	}
	ans, err := decode[liveAnswer](Call(ctx, r, true, http.MethodPut, "/api/tools/toolsets/"+url.PathEscape(name), map[string]bool{fieldEnabled: on}))
	return ans.AppliedLive, err
}

var envPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,80}$`)

// SetToolsetKeys saves the keys a toolset needs (its provider's API key).
func (s *Service) SetToolsetKeys(ctx context.Context, user, backendID, name string, env map[string]string) (bool, error) {
	if !namePattern.MatchString(name) || alwaysOn(name) {
		return false, bad("Unknown tool.")
	}
	if len(env) == 0 {
		return false, bad("Enter the key first.")
	}
	for k := range env {
		if !envPattern.MatchString(k) {
			return false, bad("Unknown key.")
		}
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return false, err
	}
	ans, err := decode[liveAnswer](Call(ctx, r, true, http.MethodPut, "/api/tools/toolsets/"+url.PathEscape(name)+"/env", map[string]any{"env": env}))
	return ans.AppliedLive, err
}

// ToolsetKeys lists the keys a toolset's providers take, never their values.
func (s *Service) ToolsetKeys(ctx context.Context, user, backendID, name string) (json.RawMessage, error) {
	if !namePattern.MatchString(name) || alwaysOn(name) {
		return nil, bad("Unknown tool.")
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	type envVar struct {
		Key    string `json:"key"`
		Prompt string `json:"prompt"`
		URL    string `json:"url,omitempty"`
		IsSet  bool   `json:"is_set"`
	}
	type provider struct {
		Name    string   `json:"name"`
		Tag     string   `json:"tag"`
		EnvVars []envVar `json:"env_vars"`
		Active  bool     `json:"active"`
	}
	cfg, err := decode[struct {
		Providers []provider `json:"providers"`
	}](Call(ctx, r, false, http.MethodGet, "/api/tools/toolsets/"+url.PathEscape(name)+"/config", nil))
	if err != nil {
		return nil, err
	}
	if cfg.Providers == nil {
		cfg.Providers = []provider{}
	}
	return json.Marshal(cfg)
}

// Skill is one skill of the agent.
type Skill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Category    string `json:"category,omitempty"`
	Enabled     bool   `json:"enabled"`
	Provenance  string `json:"provenance"`
}

// Skills lists the agent's skills, on and off.
func (s *Service) Skills(ctx context.Context, user, backendID string) ([]Skill, error) {
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	rows, err := decode[[]Skill](Call(ctx, r, false, http.MethodGet, "/api/skills", nil))
	if rows == nil && err == nil {
		rows = []Skill{}
	}
	return rows, err
}

// SetSkill turns a skill on or off.
func (s *Service) SetSkill(ctx context.Context, user, backendID, name string, on bool) (bool, error) {
	if !namePattern.MatchString(name) {
		return false, bad("Unknown skill.")
	}
	if s.Locked("skills." + name) {
		return false, lockedErr()
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return false, err
	}
	ans, err := decode[liveAnswer](Call(ctx, r, false, http.MethodPut, "/api/skills/toggle", map[string]any{fieldName: name, fieldEnabled: on}))
	return ans.AppliedLive, err
}

// SearchSkills searches the skill catalog.
func (s *Service) SearchSkills(ctx context.Context, user, backendID, q string) (json.RawMessage, error) {
	q = strings.TrimSpace(q)
	if q == "" || len(q) > 100 {
		return nil, bad("Enter what to look for.")
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	return Call(ctx, r, true, http.MethodGet, "/api/skills/hub/search?limit=20&q="+url.QueryEscape(q), nil)
}

var identPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@+-]{0,255}$`)

// CheckSkill previews a catalog skill and runs the install check on it,
// without installing.
func (s *Service) CheckSkill(ctx context.Context, user, backendID, identifier string) (json.RawMessage, error) {
	if !identPattern.MatchString(identifier) {
		return nil, bad("Unknown skill.")
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	q := "?identifier=" + url.QueryEscape(identifier)
	preview, err := Call(ctx, r, true, http.MethodGet, "/api/skills/hub/preview"+q, nil)
	if err != nil {
		return nil, err
	}
	scan, err := Call(ctx, r, true, http.MethodGet, "/api/skills/hub/scan"+q, nil)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]json.RawMessage{"preview": preview, "scan": scan})
}

// ActionResult is how a background action of the agent ended.
type ActionResult struct {
	OK       bool     `json:"ok"`
	ExitCode *int     `json:"exit_code"`
	Lines    []string `json:"lines"`
	// Live is true when open chats get the change on their next message.
	Live bool `json:"applied_live"`
}

// ErrActionTimeout is an action that did not end within the wait.
var ErrActionTimeout = &Error{Status: http.StatusGatewayTimeout, Message: "The agent is still working on it. Look again in a few minutes."}

// waitAction polls a background action until it ends.
func (s *Service) waitAction(ctx context.Context, r device.Requester, name string) (ActionResult, error) {
	ctx, cancel := context.WithTimeout(ctx, s.wait)
	defer cancel()
	t := time.NewTicker(s.poll)
	defer t.Stop()
	for {
		st, err := decode[struct {
			Running  bool     `json:"running"`
			ExitCode *int     `json:"exit_code"`
			Lines    []string `json:"lines"`
		}](Call(ctx, r, false, http.MethodGet, "/api/actions/"+url.PathEscape(name)+"/status?lines=400", nil))
		if err != nil && ctx.Err() == nil {
			return ActionResult{}, err
		}
		if err == nil && !st.Running {
			if st.Lines == nil {
				st.Lines = []string{}
			}
			return ActionResult{OK: st.ExitCode != nil && *st.ExitCode == 0, ExitCode: st.ExitCode, Lines: st.Lines}, nil
		}
		select {
		case <-ctx.Done():
			return ActionResult{}, ErrActionTimeout
		case <-t.C:
		}
	}
}

func (s *Service) runAction(ctx context.Context, r device.Requester, path string, body any) (ActionResult, error) {
	started, err := decode[struct {
		Name string `json:"name"`
	}](Call(ctx, r, false, http.MethodPost, path, body))
	if err != nil {
		return ActionResult{}, err
	}
	if !namePattern.MatchString(started.Name) {
		return ActionResult{}, &Error{Status: http.StatusBadGateway, Message: notUnderstood}
	}
	return s.waitAction(ctx, r, started.Name)
}

// applyPrompts makes open chats rebuild their instructions on their next
// message, after a skill was installed or removed.
func applyPrompts(ctx context.Context, r device.Requester) bool {
	ans, err := decode[liveAnswer](Call(ctx, r, false, http.MethodPost, "/api/dashboard/apply-live", map[string]any{"kinds": []string{"prompt"}}))
	return err == nil && ans.AppliedLive
}

// InstallSkill installs a catalog skill and waits until it is in.
func (s *Service) InstallSkill(ctx context.Context, user, backendID, identifier string) (ActionResult, error) {
	if !identPattern.MatchString(identifier) {
		return ActionResult{}, bad("Unknown skill.")
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return ActionResult{}, err
	}
	res, err := s.runAction(ctx, r, "/api/skills/hub/install", map[string]string{"identifier": identifier})
	if err == nil && res.OK {
		res.Live = applyPrompts(ctx, r)
	}
	return res, err
}

// RemoveSkill removes a skill added from the catalog.
func (s *Service) RemoveSkill(ctx context.Context, user, backendID, name string) (ActionResult, error) {
	if !namePattern.MatchString(name) {
		return ActionResult{}, bad("Unknown skill.")
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return ActionResult{}, err
	}
	res, err := s.runAction(ctx, r, "/api/skills/hub/uninstall", map[string]string{"name": name})
	if err == nil && res.OK {
		res.Live = applyPrompts(ctx, r)
	}
	return res, err
}

// Connection is one MCP server.
type Connection struct {
	Name      string `json:"name"`
	Transport string `json:"transport"`
	URL       string `json:"url,omitempty"`
	Command   string `json:"command,omitempty"`
	Auth      string `json:"auth,omitempty"`
	Enabled   bool   `json:"enabled"`
	// Plugin is true for a connection a plugin provides: it cannot be
	// changed here.
	Plugin bool `json:"plugin"`
}

// Connections lists the agent's MCP servers.
func (s *Service) Connections(ctx context.Context, user, backendID string) ([]Connection, error) {
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	type row struct {
		Connection
		Source  string  `json:"source"`
		URL     *string `json:"url"`
		Command *string `json:"command"`
		Auth    *string `json:"auth"`
	}
	got, err := decode[struct {
		Servers []row `json:"servers"`
	}](Call(ctx, r, false, http.MethodGet, "/api/mcp/servers", nil))
	if err != nil {
		return nil, err
	}
	out := make([]Connection, 0, len(got.Servers))
	for _, x := range got.Servers {
		c := x.Connection
		c.Plugin = x.Source == "plugin"
		if x.URL != nil {
			c.URL = *x.URL
		}
		if x.Command != nil {
			c.Command = *x.Command
		}
		if x.Auth != nil {
			c.Auth = *x.Auth
		}
		out = append(out, c)
	}
	return out, nil
}

var serverName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// AddConnection adds an MCP server by address, with an optional bearer
// token.
func (s *Service) AddConnection(ctx context.Context, user, backendID, name, address, token string) (bool, error) {
	if !serverName.MatchString(name) {
		return false, bad("Pick a name of letters, digits, dashes or underscores.")
	}
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false, bad("Enter an address that starts with https:// or http://.")
	}
	if s.Locked("mcp_servers." + name) {
		return false, lockedErr()
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return false, err
	}
	body := map[string]any{fieldName: name, "url": u.String()}
	if t := strings.TrimSpace(token); t != "" {
		body["auth"] = "header"
		body["bearer_token"] = t
	}
	ans, err := decode[liveAnswer](Call(ctx, r, true, http.MethodPost, "/api/mcp/servers", body))
	return ans.AppliedLive, err
}

// RemoveConnection removes an MCP server.
func (s *Service) RemoveConnection(ctx context.Context, user, backendID, name string) (bool, error) {
	if !serverName.MatchString(name) {
		return false, bad("Unknown connection.")
	}
	if s.Locked("mcp_servers." + name) {
		return false, lockedErr()
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return false, err
	}
	ans, err := decode[liveAnswer](Call(ctx, r, true, http.MethodDelete, "/api/mcp/servers/"+url.PathEscape(name), nil))
	return ans.AppliedLive, err
}

// SetConnection turns an MCP server on or off.
func (s *Service) SetConnection(ctx context.Context, user, backendID, name string, on bool) (bool, error) {
	if !serverName.MatchString(name) {
		return false, bad("Unknown connection.")
	}
	if s.Locked("mcp_servers." + name) {
		return false, lockedErr()
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return false, err
	}
	ans, err := decode[liveAnswer](Call(ctx, r, true, http.MethodPut, "/api/mcp/servers/"+url.PathEscape(name)+"/enabled", map[string]bool{fieldEnabled: on}))
	return ans.AppliedLive, err
}

// TestConnection connects to an MCP server once and lists its tools.
func (s *Service) TestConnection(ctx context.Context, user, backendID, name string) (json.RawMessage, error) {
	if !serverName.MatchString(name) {
		return nil, bad("Unknown connection.")
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	return Call(ctx, r, true, http.MethodPost, "/api/mcp/servers/"+url.PathEscape(name)+"/test", nil)
}

// ---- Diagnostics ----

// Status is the agent's part of Settings, Diagnostics.
type Status struct {
	Answering bool   `json:"answering"`
	Version   string `json:"version,omitempty"`
	Release   string `json:"release,omitempty"`
	MemoryMB  int64  `json:"memory_mb,omitempty"`
	Error     string `json:"error,omitempty"`
}

// AgentStatus reads whether the agent answers, its release and memory.
func (s *Service) AgentStatus(ctx context.Context, user, backendID string) (Status, error) {
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return Status{}, err
	}
	st, err := decode[map[string]any](Call(ctx, r, false, http.MethodGet, "/api/status", nil))
	if err != nil {
		var e *Error
		if errors.As(err, &e) {
			return Status{Error: e.Message}, nil
		}
		return Status{}, err
	}
	out := Status{Answering: true}
	out.Version, _ = st["version"].(string)
	if stats, err := decode[map[string]any](Call(ctx, r, false, http.MethodGet, "/api/system/stats", nil)); err == nil {
		out.MemoryMB = memoryMB(stats)
	}
	return out, nil
}

// memoryMB finds the agent process's resident memory in /api/system/stats.
func memoryMB(stats map[string]any) int64 {
	const proc = "process"
	for _, path := range [][]string{{proc, "rss_mb"}, {proc, "memory_mb"}, {"memory", "process_rss_mb"}, {"rss_mb"}} {
		var cur any = stats
		for _, k := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				cur = nil
				break
			}
			cur = m[k]
		}
		if v, ok := cur.(float64); ok && v > 0 {
			return int64(v)
		}
	}
	for _, path := range [][]string{{proc, "rss"}, {proc, "rss_bytes"}, {"memory", "process_rss"}} {
		var cur any = stats
		for _, k := range path {
			m, ok := cur.(map[string]any)
			if !ok {
				cur = nil
				break
			}
			cur = m[k]
		}
		if v, ok := cur.(float64); ok && v > 0 {
			return int64(v / (1 << 20))
		}
	}
	return 0
}

// LogLevels are the filters of the agent log.
func LogLevels() []string { return []string{"", "WARNING", "ERROR"} }

// Log reads the newest lines of the agent's log.
func (s *Service) Log(ctx context.Context, user, backendID, level, search string, lines int) ([]string, error) {
	if !slices.Contains(LogLevels(), level) {
		return nil, bad("Unknown log level.")
	}
	if lines <= 0 || lines > 500 {
		lines = 200
	}
	if len(search) > 200 {
		return nil, bad("Search for something shorter.")
	}
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	q := url.Values{"file": {"agent"}, "lines": {fmt.Sprint(lines)}}
	if level != "" {
		q.Set("level", level)
	}
	if search != "" {
		q.Set("search", search)
	}
	got, err := decode[struct {
		Lines []string `json:"lines"`
	}](Call(ctx, r, false, http.MethodGet, "/api/logs?"+q.Encode(), nil))
	if got.Lines == nil {
		got.Lines = []string{}
	}
	return got.Lines, err
}

// HealthCheck runs the agent's health check and waits for its result.
func (s *Service) HealthCheck(ctx context.Context, user, backendID string) (ActionResult, error) {
	r, err := s.Requester(ctx, user, backendID)
	if err != nil {
		return ActionResult{}, err
	}
	return s.runAction(ctx, r, "/api/ops/doctor", nil)
}

// ---- Backup ----

var archivePattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]+/backups/hermes-backup-[0-9-]+-[0-9a-f]{8}\.zip$`)

// Backup packs the agent's data and answers where the archive is.
func (s *Service) Backup(ctx context.Context, backendID string) (string, error) {
	r, ok := s.rs[backendID]
	if !ok {
		return "", hub.ErrUnknownBackend
	}
	started, err := decode[struct {
		Name    string `json:"name"`
		Archive string `json:"archive"`
	}](Call(ctx, r, false, http.MethodPost, "/api/ops/backup", map[string]string{}))
	if err != nil {
		return "", err
	}
	if !archivePattern.MatchString(started.Archive) || !namePattern.MatchString(started.Name) {
		return "", &Error{Status: http.StatusBadGateway, Message: notUnderstood}
	}
	res, err := s.waitAction(ctx, r, started.Name)
	if err != nil {
		return "", err
	}
	if !res.OK {
		return "", &Error{Status: http.StatusBadGateway, Message: "The agent could not pack its data."}
	}
	return started.Archive, nil
}

// OpenBackup streams an archive Backup made.
func (s *Service) OpenBackup(ctx context.Context, backendID, archive string) (io.ReadCloser, error) {
	if !archivePattern.MatchString(archive) {
		return nil, bad("Unknown backup.")
	}
	r, ok := s.rs[backendID]
	if !ok {
		return nil, hub.ErrUnknownBackend
	}
	var resp *http.Response
	var err error
	path := "/api/ops/backup/download?archive=" + url.QueryEscape(archive)
	if sr, ok := r.(slowRequester); ok {
		resp, err = sr.RequestSlow(ctx, http.MethodGet, path, nil)
	} else {
		resp, err = r.Request(ctx, http.MethodGet, path, nil)
	}
	if err != nil {
		return nil, &Error{Status: http.StatusBadGateway, Message: unreachable}
	}
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		return nil, refusal(resp.StatusCode, data)
	}
	return resp.Body, nil
}

// Backends are the backend ids the service reaches.
func (s *Service) Backends() []string {
	out := make([]string, 0, len(s.rs))
	for id := range s.rs {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}
