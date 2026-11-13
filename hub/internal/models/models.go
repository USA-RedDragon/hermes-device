// Package models connects model providers to a backend's Hermes from the
// hub, through Hermes's own setup routes on the dashboard listener: own
// servers (custom endpoints), API keys and device-code sign-ins. Hermes
// keeps the providers, keys and default model; the hub keeps nothing. A key
// travels in one request and is never logged, kept or sent back: Hermes
// answers with masked previews only. Only the routes in this file are
// reached, never a generic proxy, and never Hermes's key reveal.
package models

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

	"github.com/USA-RedDragon/talaria/hub/internal/config"
	"github.com/USA-RedDragon/talaria/hub/internal/device"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// Access says which backends a user may use. *service.Service is one.
type Access interface {
	Allowed(ctx context.Context, user, backendID string) error
}

// Options are the dependencies of a Service.
type Options struct {
	Access   Access
	Backends []config.Backend
	// Requesters reach each backend's dashboard with the hub's session.
	Requesters map[string]device.Requester
	// SlowTimeout bounds calls that probe other hosts (an address test, a
	// sign-in poll, a model list refresh). Default 60s.
	SlowTimeout time.Duration
	// Claude says whether a backend's Hermes runs Claude subscription
	// accounts (the Claude accounts plugin). Such a backend offers no
	// Anthropic API key, which would compete with the accounts. Nil: none
	// does, unless its model options list a subscription provider.
	Claude func(ctx context.Context, user, backendID string) bool
}

// Service implements hub.ModelSetup.
type Service struct {
	access Access
	rs     map[string]device.Requester
	slow   time.Duration
	claude func(ctx context.Context, user, backendID string) bool
}

var _ hub.ModelSetup = (*Service)(nil)

// New returns a Service.
func New(opts Options) *Service {
	if opts.SlowTimeout == 0 {
		opts.SlowTimeout = 60 * time.Second
	}
	s := &Service{access: opts.Access, rs: map[string]device.Requester{}, slow: opts.SlowTimeout, claude: opts.Claude}
	for _, b := range opts.Backends {
		if r := opts.Requesters[b.ID]; r != nil {
			s.rs[b.ID] = r
		}
	}
	return s
}

// unreachable is the hub's own words for a Hermes it could not ask.
const unreachable = "Could not reach Hermes. It may be restarting. Try again in a moment."

const maxAnswer = 4 << 20

// pollWait is how long a sign-in poll waits for Hermes to see a change,
// under Hermes's own cap of 25 seconds.
const pollWait = 20

var (
	endpointIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:~-]{1,128}$`)
	providerPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	sessionPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	keyPattern        = regexp.MustCompile(`^[A-Z][A-Z0-9_]{1,80}$`)
)

const (
	geminiKey     = "GEMINI_API_KEY"
	notUnderstood = "Hermes's answer was not understood."
	fieldProvider = "provider"
	fieldModel    = "model"
	fieldKey      = "key"
	flowDevice    = "device_code"
)

// probeKey is the variable Hermes checks a key of name with before a save
// (_CREDENTIAL_PROBES), or "" when Hermes has no check: then it answers ok
// with reachable false.
func probeKey(name string) string {
	switch name {
	case "OPENROUTER_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", geminiKey:
		return name
	case "GOOGLE_API_KEY":
		// Google AI Studio's main variable has no probe; its alias does.
		return geminiKey
	}
	return ""
}

// hiddenKeyProvider reports API-key providers the dialog leaves to the
// server: LM Studio is a server of one's own, Azure Foundry needs an
// endpoint address as well, custom is the own-server path.
func hiddenKeyProvider(p string) bool {
	return p == "lmstudio" || p == "azure-foundry" || p == "custom"
}

// commonProviders lead the API key list, in this order.
func commonProviders() []string {
	return []string{"openrouter", "anthropic", "openai-api", "gemini", "deepseek", "xai"}
}

func (s *Service) requester(ctx context.Context, user, backendID string) (device.Requester, error) {
	if err := s.access.Allowed(ctx, user, backendID); err != nil {
		return nil, err
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

// call sends one request to Hermes and returns its answer. A refusal comes
// back as a *hub.ModelSetupError with Hermes's own detail.
func (s *Service) call(ctx context.Context, r device.Requester, slow bool, method, path string, body any) (json.RawMessage, error) {
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
		cctx, cancel := context.WithTimeout(ctx, s.slow)
		defer cancel()
		resp, err = sr.RequestSlow(cctx, method, path, b)
	} else {
		resp, err = r.Request(ctx, method, path, b)
	}
	if err != nil {
		return nil, &hub.ModelSetupError{Status: http.StatusBadGateway, Message: unreachable}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswer))
	if err != nil {
		return nil, &hub.ModelSetupError{Status: http.StatusBadGateway, Message: unreachable}
	}
	if resp.StatusCode/100 != 2 {
		return nil, refusal(resp.StatusCode, data)
	}
	if !json.Valid(data) {
		return nil, &hub.ModelSetupError{Status: http.StatusBadGateway, Message: notUnderstood}
	}
	return data, nil
}

// refusal turns a Hermes error answer into its own words. FastAPI puts
// them in detail, as a string or a list of field errors.
func refusal(status int, data []byte) error {
	var e struct {
		Detail  json.RawMessage `json:"detail"`
		Message string          `json:"message"`
		Error   string          `json:"error"`
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
		case e.Message != "":
			msg = e.Message
		case e.Error != "":
			msg = e.Error
		}
	}
	if msg == "" {
		msg = fmt.Sprintf("Hermes answered HTTP %d.", status)
	}
	code := status
	if status >= 500 || status == http.StatusUnauthorized || status == http.StatusForbidden {
		code = http.StatusBadGateway
	}
	return &hub.ModelSetupError{Status: code, Message: msg}
}

func bad(msg string) error {
	return &hub.ModelSetupError{Status: http.StatusBadRequest, Message: msg}
}

// optionsRow is one provider row of Hermes's model options.
type optionsRow struct {
	Slug          string   `json:"slug"`
	Name          string   `json:"name"`
	Models        []string `json:"models"`
	TotalModels   int      `json:"total_models"`
	Authenticated *bool    `json:"authenticated"`
	AuthType      string   `json:"auth_type"`
	KeyEnv        string   `json:"key_env"`
	IsCurrent     bool     `json:"is_current"`
	IsUserDefined bool     `json:"is_user_defined"`
}

type options struct {
	Providers []optionsRow `json:"providers"`
	Model     string       `json:"model"`
	Provider  string       `json:"provider"`
}

func (s *Service) options(ctx context.Context, r device.Requester, refresh, all bool) (options, error) {
	q := url.Values{}
	if refresh {
		q.Set("refresh", "true")
	}
	if all {
		q.Set("include_unconfigured", "true")
	}
	path := "/api/model/options"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out options
	data, err := s.call(ctx, r, refresh, http.MethodGet, path, nil)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return out, &hub.ModelSetupError{Status: http.StatusBadGateway, Message: notUnderstood}
	}
	return out, nil
}

// readDefault reads the default model back after a change; false when
// Hermes did not answer.
func (s *Service) readDefault(ctx context.Context, r device.Requester) (options, bool) {
	o, err := s.options(ctx, r, false, false)
	return o, err == nil
}

// ModelStatus implements hub.ModelSetup.
func (s *Service) ModelStatus(ctx context.Context, user, backendID string) (hub.ModelStatus, error) {
	r, err := s.requester(ctx, user, backendID)
	if err != nil {
		return hub.ModelStatus{}, err
	}
	o, err := s.options(ctx, r, false, false)
	if err != nil {
		return hub.ModelStatus{}, err
	}
	return status(o), nil
}

// status reads readiness from the model options: a default model whose
// provider Hermes can sign in to. Hermes lists a configured provider that
// lost its credentials as not authenticated.
func status(o options) hub.ModelStatus {
	st := hub.ModelStatus{Model: o.Model, Provider: o.Provider}
	if o.Model == "" {
		return st
	}
	authed := func(p optionsRow) bool { return p.Authenticated == nil || *p.Authenticated }
	auto := o.Provider == "" || strings.EqualFold(o.Provider, "auto")
	for _, p := range o.Providers {
		if p.Slug == o.Provider || (auto && p.IsCurrent) {
			st.Ready = authed(p)
			return st
		}
	}
	// A provider the list does not name: an authenticated one exists and
	// Hermes resolves the default at chat start. With provider auto (the
	// agent's stock config: a model and no key) Hermes picks a provider
	// from keys and sign-ins only, never a server added by name (such as
	// the demo model), so only those count.
	for _, p := range o.Providers {
		if authed(p) && (!auto || !p.IsUserDefined) {
			st.Ready = true
		}
	}
	return st
}

// ModelCall implements hub.ModelSetup.
func (s *Service) ModelCall(ctx context.Context, user, backendID string, c hub.ModelCall) (json.RawMessage, error) {
	r, err := s.requester(ctx, user, backendID)
	if err != nil {
		return nil, err
	}
	switch c.Op {
	case hub.ModelCatalog:
		return s.catalog(ctx, r, user, backendID)
	case hub.ModelConnected:
		return s.connected(ctx, r)
	case hub.ModelOptions:
		o, err := s.options(ctx, r, c.Refresh, false)
		if err != nil {
			return nil, err
		}
		return json.Marshal(authenticated(o))
	case hub.ModelInfo:
		return s.call(ctx, r, true, http.MethodGet, "/api/model/info", nil)
	case hub.ModelRecommended:
		if !providerPattern.MatchString(c.Provider) {
			return nil, bad("Unknown provider.")
		}
		return s.call(ctx, r, true, http.MethodGet, "/api/model/recommended-default?provider="+url.QueryEscape(c.Provider), nil)
	case hub.ModelSetDefault:
		return s.setDefault(ctx, r, c.Default)
	case hub.ModelTestEndpoint, hub.ModelSaveEndpoint:
		return s.endpoint(ctx, r, c.Op, c.Endpoint)
	case hub.ModelUseEndpoint, hub.ModelRemoveEndpoint:
		if !endpointIDPattern.MatchString(c.ID) {
			return nil, bad("Unknown server.")
		}
		path := "/api/providers/custom-endpoints/" + url.PathEscape(c.ID)
		if c.Op == hub.ModelUseEndpoint {
			return s.checkedDefault(ctx, r, c.ID, "", func() (json.RawMessage, error) {
				return s.call(ctx, r, false, http.MethodPost, path+"/activate", nil)
			})
		}
		return s.call(ctx, r, false, http.MethodDelete, path, nil)
	case hub.ModelTestKey, hub.ModelSaveKey, hub.ModelRemoveKey:
		return s.key(ctx, r, c)
	case hub.ModelSignInStart, hub.ModelSignInPoll, hub.ModelSignInCancel, hub.ModelSignOut:
		return s.signIn(ctx, r, c)
	}
	return nil, bad("Unknown call.")
}

func (s *Service) setDefault(ctx context.Context, r device.Requester, d *hub.DefaultSetup) (json.RawMessage, error) {
	if d == nil || strings.TrimSpace(d.Model) == "" || !providerPattern.MatchString(strings.ToLower(d.Provider)) {
		return nil, bad("Pick a model first.")
	}
	// No base_url: Hermes resolves a provider's address itself, and one
	// sent here would stay in model.base_url for the next default too.
	body := map[string]any{"scope": "main", fieldProvider: d.Provider, fieldModel: strings.TrimSpace(d.Model),
		"confirm_expensive_model": d.Confirm}
	return s.checkedDefault(ctx, r, d.Provider, strings.TrimSpace(d.Model), func() (json.RawMessage, error) {
		return s.call(ctx, r, true, http.MethodPost, "/api/model/set", body)
	})
}

// checkedDefault runs set, a call that makes provider's model (any model
// of it when model is "") the default, then reads the default back. A
// Hermes whose default is set elsewhere (a deployment's managed config)
// can answer ok and keep its own; that comes back as unchanged, never as
// a success.
func (s *Service) checkedDefault(ctx context.Context, r device.Requester, provider, model string, set func() (json.RawMessage, error)) (json.RawMessage, error) {
	before, err := s.options(ctx, r, false, false)
	if err != nil {
		return nil, err
	}
	data, err := set()
	if err != nil {
		return nil, err
	}
	ans := map[string]any{}
	_ = json.Unmarshal(data, &ans)
	if v, _ := ans["confirm_required"].(bool); v {
		return data, nil
	}
	after, read := s.readDefault(ctx, r)
	if !read {
		// The call went through; only the check could not be made.
		return data, nil
	}
	changed := after.Model != before.Model || after.Provider != before.Provider
	already := strings.EqualFold(before.Provider, provider) && (model == "" || before.Model == model)
	if !changed && !already {
		return json.Marshal(map[string]any{"ok": false, "unchanged": true, fieldModel: after.Model, fieldProvider: after.Provider,
			"message": "Hermes kept " + after.Model + " as the model new chats start with. Its default may be set in the deployment config."})
	}
	// Hermes may store another name for the model (an alias it resolved).
	ans["ok"], ans[fieldModel], ans[fieldProvider] = true, after.Model, after.Provider
	return json.Marshal(ans)
}

func (s *Service) endpoint(ctx context.Context, r device.Requester, op hub.ModelOp, e *hub.EndpointSetup) (json.RawMessage, error) {
	if e == nil || strings.TrimSpace(e.BaseURL) == "" {
		return nil, bad("Enter the server's address first.")
	}
	if e.ID != "" && !endpointIDPattern.MatchString(e.ID) {
		return nil, bad("Unknown server.")
	}
	if e.APIMode != nil && !slices.Contains([]string{"", "chat_completions", "codex_responses", "anthropic_messages"}, *e.APIMode) {
		return nil, bad("Unknown API mode.")
	}
	body := *e
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		body.Name = "Local server"
	}
	body.BaseURL = strings.TrimSpace(body.BaseURL)
	// Hermes requires a model name; its probe uses the first listed model
	// when this one is blank.
	body.Model = strings.TrimSpace(body.Model)
	if op == hub.ModelTestEndpoint {
		body.MakeDefault = false
		return s.call(ctx, r, true, http.MethodPost, "/api/providers/custom-endpoints/validate", body)
	}
	if body.Model == "" {
		return nil, bad("Pick a model first.")
	}
	return s.call(ctx, r, true, http.MethodPost, "/api/providers/custom-endpoints", body)
}

// envRow is one row of Hermes's GET /api/env.
type envRow struct {
	IsSet           bool   `json:"is_set"`
	RedactedValue   string `json:"redacted_value"`
	Description     string `json:"description"`
	URL             string `json:"url"`
	Category        string `json:"category"`
	IsPassword      bool   `json:"is_password"`
	Provider        string `json:"provider"`
	ProviderLabel   string `json:"provider_label"`
	ProviderPrimary bool   `json:"provider_primary"`
	// Managed and InEnvironment say the deployment sets the key: in
	// Hermes's managed .env, or in the environment Hermes started with
	// (Hermes's dashboard.managed_read_only; never with the value).
	Managed       bool `json:"managed"`
	InEnvironment bool `json:"in_environment"`
}

// deployment reports a key the deployment sets, which the hub shows but
// never writes: one saved to Hermes's .env would hide it for good.
func (row envRow) deployment() bool {
	return row.Managed || row.InEnvironment
}

func (s *Service) env(ctx context.Context, r device.Requester) (map[string]envRow, error) {
	data, err := s.call(ctx, r, false, http.MethodGet, "/api/env", nil)
	if err != nil {
		return nil, err
	}
	var rows map[string]envRow
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, &hub.ModelSetupError{Status: http.StatusBadGateway, Message: notUnderstood}
	}
	return rows, nil
}

// keyVar reports whether name is a provider's API key variable: a
// provider row that is no address override. Only these may be saved or
// removed here, so an admin cannot write any other variable of Hermes.
func keyVar(rows map[string]envRow, name string) bool {
	row, ok := rows[name]
	return ok && row.Provider != "" && row.Category == fieldProvider && !strings.HasSuffix(name, "_BASE_URL") &&
		(row.ProviderPrimary || row.IsPassword) && !hiddenKeyProvider(row.Provider)
}

func (s *Service) key(ctx context.Context, r device.Requester, c hub.ModelCall) (json.RawMessage, error) {
	name := c.ID
	if c.Key != nil && c.Key.Key != "" {
		name = c.Key.Key
	}
	if !keyPattern.MatchString(name) {
		return nil, bad("Unknown provider.")
	}
	rows, err := s.env(ctx, r)
	if err != nil {
		return nil, err
	}
	if !keyVar(rows, name) {
		return nil, bad("Unknown provider.")
	}
	if row := rows[name]; row.deployment() && c.Op != hub.ModelTestKey {
		label := row.ProviderLabel
		if label == "" {
			label = name
		}
		return nil, &hub.ModelSetupError{Status: http.StatusConflict,
			Message: "The " + label + " key is set in the deployment, so it cannot be changed or removed here. Change it where the deployment sets it."}
	}
	if c.Op == hub.ModelRemoveKey {
		return s.call(ctx, r, false, http.MethodDelete, "/api/env", map[string]string{fieldKey: name})
	}
	if c.Key == nil || strings.TrimSpace(c.Key.Value) == "" {
		return nil, bad("Enter the key first.")
	}
	if c.Op == hub.ModelTestKey {
		probe := probeKey(name)
		if probe == "" {
			// Hermes has no check for this provider: say so as it does.
			return json.RawMessage(`{"ok":true,"reachable":false,"message":""}`), nil
		}
		return s.call(ctx, r, true, http.MethodPost, "/api/providers/validate", map[string]string{fieldKey: probe, "value": strings.TrimSpace(c.Key.Value)})
	}
	return s.call(ctx, r, false, http.MethodPut, "/api/env", map[string]any{fieldKey: name, "value": strings.TrimSpace(c.Key.Value), "provider_setup": true})
}

// oauthProvider is one row of GET /api/providers/oauth.
type oauthProvider struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Flow           string `json:"flow"`
	CLICommand     string `json:"cli_command"`
	Disconnectable bool   `json:"disconnectable"`
	Status         struct {
		LoggedIn bool `json:"logged_in"`
	} `json:"status"`
}

func (s *Service) oauth(ctx context.Context, r device.Requester) ([]oauthProvider, error) {
	data, err := s.call(ctx, r, false, http.MethodGet, "/api/providers/oauth", nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		Providers []oauthProvider `json:"providers"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, &hub.ModelSetupError{Status: http.StatusBadGateway, Message: notUnderstood}
	}
	return out.Providers, nil
}

func (s *Service) signIn(ctx context.Context, r device.Requester, c hub.ModelCall) (json.RawMessage, error) {
	if c.Op == hub.ModelSignInCancel {
		if !sessionPattern.MatchString(c.Session) {
			return nil, bad("Unknown sign-in.")
		}
		return s.call(ctx, r, false, http.MethodDelete, "/api/providers/oauth/sessions/"+c.Session, nil)
	}
	if !providerPattern.MatchString(c.ID) {
		return nil, bad("Unknown account.")
	}
	ps, err := s.oauth(ctx, r)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(ps, func(p oauthProvider) bool { return p.ID == c.ID })
	if i < 0 {
		return nil, bad("Unknown account.")
	}
	base := "/api/providers/oauth/" + c.ID
	if c.Op == hub.ModelSignOut {
		return s.call(ctx, r, false, http.MethodDelete, base, nil)
	}
	if c.Op == hub.ModelSignInPoll {
		if !sessionPattern.MatchString(c.Session) {
			return nil, bad("Unknown sign-in.")
		}
		data, err := s.call(ctx, r, true, http.MethodGet, fmt.Sprintf("%s/poll/%s?wait=%d", base, c.Session, pollWait), nil)
		var me *hub.ModelSetupError
		if errors.As(err, &me) && me.Status == http.StatusNotFound {
			// Hermes forgot the session: it ran out, or Hermes restarted.
			return json.RawMessage(`{"status":"expired"}`), nil
		}
		return data, err
	}
	if ps[i].Flow != flowDevice {
		return nil, bad("Sign in to " + ps[i].Name + " on the server: " + ps[i].CLICommand)
	}
	return s.call(ctx, r, true, http.MethodPost, base+"/start", nil)
}

// keyProvider is an API-key provider the dialog offers.
type keyProvider struct {
	Provider    string `json:"provider"`
	Name        string `json:"name"`
	KeyEnv      string `json:"key_env"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
	Set         bool   `json:"set"`
	Preview     string `json:"preview,omitempty"`
	// Deployment is true for a key the deployment sets: shown, never
	// edited or removed here.
	Deployment bool `json:"deployment,omitempty"`
	// Checked is true when Hermes checks the key with the provider
	// before a save.
	Checked bool `json:"checked"`
	Common  bool `json:"common"`
}

// signInAccount is an account the dialog can sign in to.
type signInAccount struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	SignedIn bool   `json:"signed_in"`
	// Here is false for accounts only the server can sign in to.
	Here       bool   `json:"here"`
	CLICommand string `json:"cli_command,omitempty"`
	Removable  bool   `json:"removable"`
}

// keyProviders are the API-key providers of rows, common ones first, then
// in order. hide names providers left out.
func keyProviders(rows map[string]envRow, order []string, hide ...string) []keyProvider {
	common := commonProviders()
	byProvider := map[string]keyProvider{}
	for name, row := range rows {
		if !keyVar(rows, name) || !row.ProviderPrimary || slices.Contains(hide, row.Provider) {
			continue
		}
		label := row.ProviderLabel
		if label == "" {
			label = row.Provider
		}
		checked := probeKey(name) != ""
		byProvider[row.Provider] = keyProvider{Provider: row.Provider, Name: label, KeyEnv: name, URL: row.URL, Description: row.Description,
			Set: row.IsSet || row.deployment(), Preview: preview(row), Deployment: row.deployment(), Checked: checked,
			Common: slices.Contains(common, row.Provider)}
	}
	rank := func(p string) int {
		if i := slices.Index(common, p); i >= 0 {
			return i
		}
		if i := slices.Index(order, p); i >= 0 {
			return len(common) + i
		}
		return len(common) + len(order)
	}
	out := make([]keyProvider, 0, len(byProvider))
	for _, p := range byProvider {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b keyProvider) int {
		if d := rank(a.Provider) - rank(b.Provider); d != 0 {
			return d
		}
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	return out
}

// preview is the masked key Hermes shows, without its marker.
func preview(row envRow) string {
	if !row.IsSet {
		return ""
	}
	v := strings.TrimSuffix(strings.TrimPrefix(row.RedactedValue, "«redacted:"), "»")
	return v
}

// endpoints is Hermes's list of custom endpoints, with the default and
// whether the deployment pins it.
type endpoints struct {
	Endpoints []json.RawMessage `json:"endpoints"`
	Current   struct {
		Provider      string `json:"provider"`
		Model         string `json:"model"`
		Managed       bool   `json:"managed"`
		ManagedSource string `json:"managed_source"`
	} `json:"current"`
}

func (s *Service) endpoints(ctx context.Context, r device.Requester) (endpoints, error) {
	var ep endpoints
	data, err := s.call(ctx, r, false, http.MethodGet, "/api/providers/custom-endpoints", nil)
	if err != nil {
		return ep, err
	}
	if err := json.Unmarshal(data, &ep); err != nil {
		return ep, &hub.ModelSetupError{Status: http.StatusBadGateway, Message: notUnderstood}
	}
	if ep.Endpoints == nil {
		ep.Endpoints = []json.RawMessage{}
	}
	return ep, nil
}

// pinned is the default model a deployment sets, for the answers of
// catalog and connected; nil when the default is Hermes's own to change.
func (ep endpoints) pinned() map[string]string {
	if !ep.Current.Managed {
		return nil
	}
	return map[string]string{fieldModel: ep.Current.Model, fieldProvider: ep.Current.Provider, "source": ep.Current.ManagedSource}
}

// subscriptionProvider reports a provider slug of a Claude subscription
// plugin: the Claude accounts pool or Nous's Claude subscription plugin.
func subscriptionProvider(slug string) bool {
	return slug == "claude-pool" || strings.HasPrefix(slug, "claude-subscription")
}

func (s *Service) catalog(ctx context.Context, r device.Requester, user, backendID string) (json.RawMessage, error) {
	rows, err := s.env(ctx, r)
	if err != nil {
		return nil, err
	}
	ps, err := s.oauth(ctx, r)
	if err != nil {
		return nil, err
	}
	ep, err := s.endpoints(ctx, r)
	if err != nil {
		return nil, err
	}
	var order []string
	claude := s.claude != nil && s.claude(ctx, user, backendID)
	all, err := s.options(ctx, r, false, true)
	if err == nil {
		for _, p := range all.Providers {
			order = append(order, p.Slug)
			claude = claude || subscriptionProvider(p.Slug)
		}
	}
	var hide []string
	if claude {
		// Claude runs on the backend's subscription accounts; an API key
		// would compete with them.
		hide = append(hide, "anthropic")
	}
	accounts := make([]signInAccount, 0, len(ps))
	for _, p := range ps {
		accounts = append(accounts, signInAccount{ID: p.ID, Name: p.Name, SignedIn: p.Status.LoggedIn, Here: p.Flow == flowDevice,
			CLICommand: p.CLICommand, Removable: p.Disconnectable && p.Flow == flowDevice})
	}
	return json.Marshal(map[string]any{"keys": keyProviders(rows, order, hide...), "accounts": accounts, "pinned": ep.pinned()})
}

// modelsOf is the authenticated providers of the options with their
// models.
type providerModels struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Models  []string `json:"models"`
	Current bool     `json:"current"`
}

func authenticated(o options) map[string]any {
	ps := []providerModels{}
	for _, p := range o.Providers {
		if p.Authenticated != nil && !*p.Authenticated {
			continue
		}
		name := p.Name
		if name == "" {
			name = p.Slug
		}
		ps = append(ps, providerModels{ID: p.Slug, Name: name, Models: nonNil(p.Models), Current: p.IsCurrent})
	}
	return map[string]any{fieldModel: o.Model, fieldProvider: o.Provider, "providers": ps}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (s *Service) connected(ctx context.Context, r device.Requester) (json.RawMessage, error) {
	o, err := s.options(ctx, r, false, false)
	if err != nil {
		return nil, err
	}
	ep, err := s.endpoints(ctx, r)
	if err != nil {
		return nil, err
	}
	rows, err := s.env(ctx, r)
	if err != nil {
		return nil, err
	}
	ps, err := s.oauth(ctx, r)
	if err != nil {
		return nil, err
	}
	keys := []keyProvider{}
	for _, k := range keyProviders(rows, nil) {
		if k.Set {
			keys = append(keys, k)
		}
	}
	accounts := []signInAccount{}
	for _, p := range ps {
		if p.Status.LoggedIn {
			accounts = append(accounts, signInAccount{ID: p.ID, Name: p.Name, SignedIn: true, Here: p.Flow == flowDevice,
				CLICommand: p.CLICommand, Removable: p.Disconnectable && p.Flow == flowDevice})
		}
	}
	out := authenticated(o)
	out["endpoints"], out["keys"], out["accounts"], out["status"], out["pinned"] = ep.Endpoints, keys, accounts, status(o), ep.pinned()
	return json.Marshal(out)
}
