package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/USA-RedDragon/talaria/hub/internal/gateway"
	"github.com/USA-RedDragon/talaria/hub/internal/hub"
	"github.com/google/uuid"
)

// maxTitle is SessionDB.MAX_TITLE_LENGTH in hermes_state.py.
const maxTitle = 100

// codeInternal is the JSON-RPC error the gateway sends when a handler
// raises something other than a refusal, as a title clash does.
const codeInternal = -32603

// refusal maps a gateway refusal to a hub error.
func refusal(err error) error {
	var r *gateway.RPCError
	if !errors.As(err, &r) {
		return err
	}
	switch r.Message {
	case gateway.ReasonSessionBusy, gateway.ReasonUnknownExecution:
		return fmt.Errorf("%w: %s", hub.ErrBusy, r.Message)
	case gateway.ReasonInvalidParams, gateway.ReasonModelResolution:
		return fmt.Errorf("%w: %s", hub.ErrBadValue, r.Message)
	case gateway.ReasonNotFound:
		return fmt.Errorf("%w: %s", hub.ErrUnknownSession, r.Message)
	}
	return err
}

// withSession runs fn on a subscribed, pinned session.
func (s *Service) withSession(ctx context.Context, user, backendID, sessionID string, fn func(*backend, *live) error) error {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return err
	}
	ls, err := b.session(ctx, sessionID)
	if err != nil {
		return refusal(err)
	}
	defer ls.unpin()
	return fn(b, ls)
}

// mutate sends one session.mutate fenced on the revision (and with fenced,
// the generation) the hub last saw. A refusal for stale fences carries the
// fresh ones, so it retries once with those.
func (b *backend) mutate(ctx context.Context, ls *live, op string, payload any, fenced bool) (gateway.MutateResult, error) {
	var res gateway.MutateResult
	raw, err := b.mutateRaw(ctx, ls, op, payload, fenced)
	if err == nil {
		err = json.Unmarshal(raw, &res)
	}
	return res, err
}

// mutateRaw is mutate that returns the whole result.
func (b *backend) mutateRaw(ctx context.Context, ls *live, op string, payload any, fenced bool) (json.RawMessage, error) {
	return b.mutateRawWithin(ctx, ls, 0, op, payload, fenced)
}

// mutateRawWithin is mutateRaw with its own bound for the call: d, or
// CallTimeout for 0.
func (b *backend) mutateRawWithin(ctx context.Context, ls *live, d time.Duration, op string, payload any, fenced bool) (json.RawMessage, error) {
	var raw json.RawMessage
	for attempt := 0; ; attempt++ {
		ls.mu.Lock()
		p := gateway.MutateParams{SessionID: ls.id, RequestID: uuid.NewString(), ExpectedRevision: ls.revision,
			Operation: op, Payload: payload}
		if fenced {
			gen := ls.generation
			p.ExpectedGeneration = &gen
		}
		ls.mu.Unlock()
		raw = nil
		err := b.callWithin(ctx, ls, d, gateway.MethodMutate, p, &raw)
		var r *gateway.RPCError
		if attempt == 0 && errors.As(err, &r) &&
			(r.Message == gateway.ReasonRevisionConflict || r.Message == gateway.ReasonStaleGeneration) {
			var f gateway.Fences
			if json.Unmarshal(r.Data, &f) == nil && f.Revision != nil {
				ls.mu.Lock()
				ls.setFences(f)
				ls.mu.Unlock()
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		var res gateway.MutateResult
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, err
		}
		ls.mu.Lock()
		ls.fences(res.Revision, res.ExecutionGeneration)
		ls.mu.Unlock()
		return raw, nil
	}
}

// RenameSession sets a session's title.
func (s *Service) RenameSession(ctx context.Context, user, backendID, sessionID, title string) error {
	title = strings.TrimSpace(title)
	if title == "" || utf8.RuneCountInString(title) > maxTitle || strings.ContainsAny(title, "\r\n") {
		return fmt.Errorf("%w: a title has 1 to %d characters on one line", hub.ErrBadValue, maxTitle)
	}
	return s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		_, err := b.mutate(ctx, ls, gateway.OpRename, map[string]string{"title": title}, false)
		var r *gateway.RPCError
		if errors.As(err, &r) && r.Code == codeInternal {
			return fmt.Errorf("%w: %s", hub.ErrTitleInUse, r.Message)
		}
		return refusal(err)
	})
}

// ArchiveSession hides a session from the list, or shows it again.
func (s *Service) ArchiveSession(ctx context.Context, user, backendID, sessionID string, archived bool) error {
	return s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		_, err := b.mutate(ctx, ls, gateway.OpArchive, map[string]bool{"archived": archived}, false)
		return refusal(err)
	})
}

// DeleteSession deletes a session and ends every stream of it.
func (s *Service) DeleteSession(ctx context.Context, user, backendID, sessionID string) error {
	return s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		if b.heldCount(sessionID) > 0 {
			return fmt.Errorf("%w: messages are waiting for the backend", hub.ErrBusy)
		}
		if _, err := b.mutate(ctx, ls, gateway.OpDelete, map[string]any{}, true); err != nil {
			return refusal(err)
		}
		b.gone(ls)
		return nil
	})
}

// Models lists the models Hermes offers the session: those of providers
// it can sign in to, without the ones it marks unavailable.
func (s *Service) Models(ctx context.Context, user, backendID, sessionID string) (hub.Models, error) {
	var out hub.Models
	err := s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		var res gateway.ModelOptionsResult
		if err := b.read(ctx, ls, gateway.MethodModelOptions, gateway.ModelOptionsParams{SessionID: ls.id}, &res); err != nil {
			return refusal(err)
		}
		out = hub.Models{Provider: res.Provider, Model: res.Model, Providers: []hub.ModelProvider{}}
		for _, p := range res.Providers {
			if p.Authenticated != nil && !*p.Authenticated {
				continue
			}
			hp := hub.ModelProvider{ID: p.Slug, Name: p.Name, Models: []hub.ModelChoice{}}
			if hp.Name == "" {
				hp.Name = p.Slug
			}
			if p.Slug == hub.ClaudeAPIProvider {
				hp.Name = hub.ClaudeAPIName
			}
			for _, m := range p.Models {
				if slices.Contains(p.UnavailableModels, m) {
					continue
				}
				c := hub.ModelChoice{ID: m, Reasoning: true, CanDisableReasoning: true}
				if caps, ok := p.Capabilities[m]; ok {
					c.Reasoning = caps.Reasoning
					c.CanDisableReasoning = caps.CanDisableReasoning == nil || *caps.CanDisableReasoning
				}
				hp.Models = append(hp.Models, c)
			}
			if len(hp.Models) > 0 {
				out.Providers = append(out.Providers, hp)
			}
		}
		hub.ArrangeModels(out.Providers)
		return nil
	})
	return out, err
}

// SetModel picks the session's model. A pick while a turn runs applies
// when it ends.
func (s *Service) SetModel(ctx context.Context, user, backendID, sessionID, provider, model, confirm string) (hub.ModelChange, error) {
	model, provider = strings.TrimSpace(model), strings.TrimSpace(provider)
	if model == "" {
		return hub.ModelChange{}, fmt.Errorf("%w: no model", hub.ErrBadValue)
	}
	var out hub.ModelChange
	err := s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		res, err := b.mutate(ctx, ls, gateway.OpModel, gateway.ModelPayload{Model: model, Provider: provider, Confirm: confirm}, true)
		if err != nil {
			return refusal(err)
		}
		switch res.Status {
		case "confirmation_required":
			out = hub.ModelChange{Status: hub.ModelConfirm, Confirm: res.Confirm, Message: res.ConfirmMessage}
		case "deferred":
			out = hub.ModelChange{Status: hub.ModelDeferred}
		case "noop":
			out = hub.ModelChange{Status: hub.ModelUnchanged}
		default:
			out = hub.ModelChange{Status: hub.ModelChanged}
		}
		return nil
	})
	return out, err
}

// SetReasoning sets the session's reasoning effort, or with "" returns it
// to the backend's default. A change while a turn runs applies when it ends.
func (s *Service) SetReasoning(ctx context.Context, user, backendID, sessionID, effort string) error {
	value := strings.ToLower(strings.TrimSpace(effort))
	switch {
	case value == "":
		value = gateway.ReasoningReset
	case !slices.Contains(hub.ReasoningEfforts(), value):
		return fmt.Errorf("%w: unknown reasoning effort %q", hub.ErrBadValue, effort)
	}
	return s.withSession(ctx, user, backendID, sessionID, func(b *backend, ls *live) error {
		return refusal(b.call(ctx, ls, gateway.MethodConfigSet,
			gateway.ConfigSetParams{SessionID: ls.id, Key: gateway.ReasoningKey, Value: value}, nil))
	})
}

// gone forgets a deleted session and ends its viewers' streams.
func (b *backend) gone(ls *live) {
	ls.opMu.Lock()
	defer ls.opMu.Unlock()
	b.mu.Lock()
	for id, other := range b.sessions {
		if other == ls {
			delete(b.sessions, id)
		}
	}
	b.mu.Unlock()
	ls.client = nil
	ls.mu.Lock()
	defer ls.mu.Unlock()
	ls.cur, ls.retired, ls.subID = nil, true, ""
	ls.seq++
	e := hub.Event{Seq: ls.seq, Kind: hub.KindSessionDeleted, Data: json.RawMessage(`{}`)}
	for v := range ls.viewers {
		select {
		case v.ch <- e:
		default:
		}
		v.closed = true
		close(v.ch)
		delete(ls.viewers, v)
	}
}

var _ hub.SessionChecker = (*Service)(nil)

// SessionExists asks the backend whether it still has a session, without
// subscribing to it. Hermes answers session.info with not_found for a
// deleted session.
func (s *Service) SessionExists(ctx context.Context, user, backendID, sessionID string) (bool, error) {
	b, err := s.backend(ctx, user, backendID)
	if err != nil {
		return false, err
	}
	c, err := b.conn(ctx)
	if err != nil {
		return false, err
	}
	cctx, cancel := b.timeout(ctx)
	defer cancel()
	err = c.Call(cctx, gateway.MethodSessionInfo, gateway.SessionParams{SessionID: sessionID}, nil)
	var rerr *gateway.RPCError
	switch {
	case errors.As(err, &rerr) && rerr.Message == gateway.ReasonNotFound:
		return false, nil
	case err != nil:
		return false, err
	}
	return true, nil
}
