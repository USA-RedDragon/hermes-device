// Package claudetest has a fake of the Claude Hermes plugin for
// tests, mounted on a gatewaytest.Gateway as the dashboard mounts it.
package claudetest

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/claude"
	"github.com/USA-RedDragon/hermes-hub/internal/gateway/gatewaytest"
)

// GoodCode is the code the fake accepts.
const GoodCode = "good-code#state"

// Plugin is the fake plugin. Its fields may be changed under Lock.
type Plugin struct {
	sync.Mutex
	// Accounts are the account slots, in order.
	Accounts []claude.Account
	// Usages are the usage answers by account.
	Usages map[string]claude.Usage
	// Credits are the API credit estimates by account, sent with usage.
	Credits map[string]*claude.Credits
	// Pools are the answers by session; a session without one gets
	// Pool.
	Pools map[string]claude.Pool
	Pool  claude.Pool
	// SignIns are the sign-ins by account.
	SignIns map[string]*claude.SignIn
	// Calls are the requests served, as "METHOD path".
	Calls []string
	// Codes are the codes the plugin got, by sign-in ID.
	Codes map[string]string
	// SignInEmail is the email a sign-in with GoodCode signs in to.
	SignInEmail string
	n           int
}

// New mounts a fake plugin with two signed-out accounts, account1 and
// account2, on gw.
func New(gw *gatewaytest.Gateway) *Plugin {
	p := &Plugin{Usages: map[string]claude.Usage{}, SignIns: map[string]*claude.SignIn{}, Codes: map[string]string{},
		Credits: map[string]*claude.Credits{}, Pools: map[string]claude.Pool{}, SignInEmail: "alice@example.com"}
	for _, id := range []string{"account1", "account2"} {
		p.Accounts = append(p.Accounts, claude.Account{ID: id, Label: "Claude (" + id + ")", Provider: "claude-" + id,
			ConfigDir: "/opt/data/hermes/claude/" + id, Status: claude.Status{CheckedAt: time.Now().UTC()}})
	}
	b := claude.PluginBase
	gw.Handle("GET "+b+"/accounts", http.HandlerFunc(p.list), true)
	gw.Handle("GET "+b+"/accounts/{id}/usage", http.HandlerFunc(p.usage), true)
	gw.Handle("GET "+b+"/accounts/{id}/status", http.HandlerFunc(p.status), true)
	gw.Handle("POST "+b+"/accounts/{id}/signin", http.HandlerFunc(p.start), true)
	gw.Handle("GET "+b+"/accounts/{id}/signin", http.HandlerFunc(p.get), true)
	gw.Handle("DELETE "+b+"/accounts/{id}/signin", http.HandlerFunc(p.cancel), true)
	gw.Handle("POST "+b+"/accounts/{id}/signin/code", http.HandlerFunc(p.code), true)
	gw.Handle("GET "+b+"/pool", http.HandlerFunc(p.pool), true)
	return p
}

// SetLoggedIn sets an account's login.
func (p *Plugin) SetLoggedIn(id string, in bool, email string) {
	p.Lock()
	defer p.Unlock()
	for i := range p.Accounts {
		if p.Accounts[i].ID == id {
			p.Accounts[i].Status.LoggedIn, p.Accounts[i].Status.Email = in, email
			if email != "" {
				p.Accounts[i].LastEmail = email
			}
			if !in {
				p.Accounts[i].Status.Email = ""
			}
		}
	}
}

// CallCount counts the served requests that start with prefix.
func (p *Plugin) CallCount(prefix string) int {
	p.Lock()
	defer p.Unlock()
	n := 0
	for _, c := range p.Calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func answer(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func fail(w http.ResponseWriter, status int, code string) {
	answer(w, status, map[string]string{"error": code})
}

// account finds an account and records the call; p is locked.
func (p *Plugin) account(r *http.Request) *claude.Account {
	p.Calls = append(p.Calls, r.Method+" "+strings.TrimPrefix(r.URL.Path, claude.PluginBase))
	for i := range p.Accounts {
		if p.Accounts[i].ID == r.PathValue("id") {
			return &p.Accounts[i]
		}
	}
	return nil
}

func (p *Plugin) list(w http.ResponseWriter, r *http.Request) {
	p.Lock()
	defer p.Unlock()
	p.Calls = append(p.Calls, "GET /accounts?"+r.URL.RawQuery)
	out := claude.Accounts{CLIVersion: "2.1.322", Accounts: []claude.Account{}}
	for _, a := range p.Accounts {
		a.SignIn = p.SignIns[a.ID]
		if r.URL.Query().Get("usage") == "1" {
			if u, ok := p.Usages[a.ID]; ok {
				a.Usage = &u
			}
			a.Credits = p.Credits[a.ID]
		}
		out.Accounts = append(out.Accounts, a)
	}
	answer(w, http.StatusOK, out)
}

func (p *Plugin) pool(w http.ResponseWriter, r *http.Request) {
	p.Lock()
	defer p.Unlock()
	p.Calls = append(p.Calls, "GET /pool?"+r.URL.RawQuery)
	out, ok := p.Pools[r.URL.Query().Get("session")]
	if !ok {
		out = p.Pool
	}
	answer(w, http.StatusOK, out)
}

func (p *Plugin) usage(w http.ResponseWriter, r *http.Request) {
	p.Lock()
	defer p.Unlock()
	a := p.account(r)
	if a == nil {
		fail(w, http.StatusNotFound, claude.CodeUnknownAccount)
		return
	}
	answer(w, http.StatusOK, p.Usages[a.ID])
}

func (p *Plugin) status(w http.ResponseWriter, r *http.Request) {
	p.Lock()
	defer p.Unlock()
	a := p.account(r)
	if a == nil {
		fail(w, http.StatusNotFound, claude.CodeUnknownAccount)
		return
	}
	out := *a
	out.SignIn = p.SignIns[a.ID]
	answer(w, http.StatusOK, out)
}

func (p *Plugin) start(w http.ResponseWriter, r *http.Request) {
	p.Lock()
	defer p.Unlock()
	a := p.account(r)
	if a == nil {
		fail(w, http.StatusNotFound, claude.CodeUnknownAccount)
		return
	}
	if s := p.SignIns[a.ID]; s != nil && (s.State == "waiting_code" || s.State == "starting") {
		answer(w, http.StatusOK, s)
		return
	}
	p.n++
	reason := "first"
	if a.LastEmail != "" {
		reason = "expired"
	}
	now := time.Now().UTC()
	s := &claude.SignIn{ID: "si" + strconv.Itoa(p.n), Account: a.ID, Label: a.Label, EmailHint: a.LastEmail, Reason: reason,
		State: "waiting_code", URL: "https://claude.com/cai/oauth/authorize?state=" + a.ID, StartedAt: now, ExpiresAt: now.Add(10 * time.Minute)}
	p.SignIns[a.ID] = s
	answer(w, http.StatusOK, s)
}

func (p *Plugin) get(w http.ResponseWriter, r *http.Request) {
	p.Lock()
	defer p.Unlock()
	a := p.account(r)
	if a == nil {
		fail(w, http.StatusNotFound, claude.CodeUnknownAccount)
		return
	}
	s := p.SignIns[a.ID]
	if s == nil {
		fail(w, http.StatusNotFound, claude.CodeNotFound)
		return
	}
	answer(w, http.StatusOK, s)
}

func (p *Plugin) cancel(w http.ResponseWriter, r *http.Request) {
	p.Lock()
	defer p.Unlock()
	a := p.account(r)
	if a == nil {
		fail(w, http.StatusNotFound, claude.CodeUnknownAccount)
		return
	}
	s := p.SignIns[a.ID]
	if s == nil {
		fail(w, http.StatusNotFound, claude.CodeNotFound)
		return
	}
	s.State, s.URL = "cancelled", ""
	answer(w, http.StatusOK, s)
}

func (p *Plugin) code(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SignInID string `json:"signin_id"`
		Code     string `json:"code"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	p.Lock()
	defer p.Unlock()
	a := p.account(r)
	if a == nil {
		fail(w, http.StatusNotFound, claude.CodeUnknownAccount)
		return
	}
	s := p.SignIns[a.ID]
	if s == nil || s.ID != body.SignInID {
		fail(w, http.StatusConflict, claude.CodeWrongSignIn)
		return
	}
	if !strings.Contains(body.Code, "#") {
		fail(w, http.StatusBadRequest, claude.CodeBadCode)
		return
	}
	p.Codes[s.ID] = body.Code
	if body.Code == GoodCode {
		s.State, s.Email, s.URL = "succeeded", p.SignInEmail, ""
		a.Status.LoggedIn, a.Status.Email, a.LastEmail = true, p.SignInEmail, p.SignInEmail
		a.Status.SubscriptionType = "max"
	} else {
		s.State, s.Message = "failed", "The code was not accepted. Start the sign-in again."
	}
	answer(w, http.StatusOK, s)
}

// ChatGPT is a fake of the dashboard's model options with Hermes's
// openai-codex row. Its fields may be changed under Lock.
type ChatGPT struct {
	sync.Mutex
	// Row is the openai-codex row. Nil: Hermes is not signed in to ChatGPT.
	Row *claude.ProviderRow
	// Cold is how many more answers leave the row's usage out, as Hermes
	// does before it has read ChatGPT.
	Cold int
	// Calls counts the answers.
	Calls int
	// Rows are more provider rows, as a model picker lists them.
	Rows []claude.ProviderRow
	// Model and Provider are the default for a new chat, and ContextLength
	// is the default model's context window (0: unresolved).
	Model, Provider string
	ContextLength   int64
}

// NewChatGPT mounts the fake model options on gw.
func NewChatGPT(gw *gatewaytest.Gateway) *ChatGPT {
	c := &ChatGPT{}
	gw.Handle("GET "+claude.ModelOptionsPath, http.HandlerFunc(c.options), true)
	gw.Handle("GET "+claude.ModelInfoPath, http.HandlerFunc(c.info), true)
	return c
}

func (c *ChatGPT) options(w http.ResponseWriter, _ *http.Request) {
	c.Lock()
	defer c.Unlock()
	c.Calls++
	yes := true
	out := claude.ModelOptions{Providers: []claude.ProviderRow{{Slug: "deepinfra", Authenticated: &yes}}, Model: c.Model, Provider: c.Provider}
	out.Providers = append(out.Providers, c.Rows...)
	if c.Row != nil {
		row := *c.Row
		if c.Cold > 0 {
			c.Cold--
			row.Usage = nil
		}
		out.Providers = append(out.Providers, row)
	}
	answer(w, http.StatusOK, out)
}

func (c *ChatGPT) info(w http.ResponseWriter, _ *http.Request) {
	c.Lock()
	defer c.Unlock()
	answer(w, http.StatusOK, claude.ModelInfo{Model: c.Model, Provider: c.Provider, EffectiveContextLength: c.ContextLength})
}

// CallCount returns how many answers the fake gave.
func (c *ChatGPT) CallCount() int {
	c.Lock()
	defer c.Unlock()
	return c.Calls
}
