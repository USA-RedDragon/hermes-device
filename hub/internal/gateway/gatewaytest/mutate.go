package gatewaytest

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/USA-RedDragon/hermes-hub/internal/gateway"
)

// maxTitle is SessionDB.MAX_TITLE_LENGTH in hermes_state.py.
const maxTitle = 100

func validEffort(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "false" || v == "disabled" ||
		slices.Contains([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}, v)
}

// RequireConfirm makes session.mutate ask for a confirmation token
// before it commits these models.
func (g *Gateway) RequireConfirm(models ...string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ConfirmModels = models
}

// BumpRevision moves a session's revision, as another client's change does.
func (g *Gateway) BumpRevision(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sessions[id].Revision++
}

// hMutate follows gateway/session_mutations.py and hermes_state_mutations.py
// for the operations the hub sends.
func (g *Gateway) hMutate(_ *conn, p map[string]any) (any, string, func()) {
	// The keys besides expected_generation (gateway/session_mutations.py
	// _FIELDS), and the operations that need it (hermes_state_runtime.py).
	fields := []string{"expected_revision", kOperation, kPayload, "request_id", kSessionID}
	fenced := []string{gateway.OpDelete, gateway.OpModel, kReasoning}
	got := slices.DeleteFunc(keys(p), func(k string) bool { return k == "expected_generation" })
	s := g.sessions[str(p, kSessionID)]
	if !slices.Equal(got, fields) {
		return nil, errInvalidParams, nil
	}
	if s == nil {
		return nil, gateway.ReasonNotFound, nil
	}
	op := str(p, kOperation)
	payload, ok := p[kPayload].(map[string]any)
	rev, revOK := num(p, "expected_revision")
	gen, genOK := num(p, "expected_generation")
	if !ok || !revOK || rev < 0 || (slices.Contains(fenced, op) && !genOK) {
		return nil, errInvalidParams, nil
	}
	if rev != s.Revision {
		return nil, gateway.ReasonRevisionConflict, nil
	}
	if genOK && gen != s.Generation {
		return nil, gateway.ReasonStaleGeneration, nil
	}
	result := map[string]any{kSessionID: s.ID, kOperation: op}
	switch op {
	case gateway.OpRename:
		title, ok := payload[kTitle].(string)
		if len(payload) != 1 || !ok {
			return nil, errInvalidParams, nil
		}
		title = strings.TrimSpace(title)
		if utf8.RuneCountInString(title) > maxTitle {
			return nil, errInternal, nil
		}
		for _, o := range g.sessions {
			if o != s && title != "" && o.Title == title {
				return nil, errInternal, nil
			}
		}
		s.Title = title
		result[kTitle] = title
	case gateway.OpArchive:
		archived, ok := payload["archived"].(bool)
		if len(payload) != 1 || !ok {
			return nil, errInvalidParams, nil
		}
		s.Archived = archived
		result["archived"] = archived
	case gateway.OpDelete:
		if len(payload) != 0 {
			return nil, errInvalidParams, nil
		}
		for _, a := range s.Pending {
			if a.Status == gateway.AdmissionUnknown {
				return nil, gateway.ReasonUnknownExecution, nil
			}
		}
		if s.Running || len(s.Pending) > 0 {
			return nil, gateway.ReasonSessionBusy, nil
		}
		result["deleted_ids"] = []string{s.ID}
	case gateway.OpModel:
		return g.pickModel(s, payload)
	default:
		return nil, errInvalidParams, nil
	}
	s.Revision++
	result[kRevision], result[kExecGen] = s.Revision, s.Generation
	g.publishLocked(s, gateway.UpdatedEvent, result)
	if op == gateway.OpDelete {
		delete(g.sessions, s.ID)
	}
	return result, "", nil
}

func (g *Gateway) pickModel(s *Session, payload map[string]any) (any, string, func()) {
	for k, v := range payload {
		if sv, ok := v.(string); !ok || strings.TrimSpace(sv) == "" ||
			!slices.Contains([]string{kModel, kProvider, "confirm", kReasoning}, k) {
			return nil, errInvalidParams, nil
		}
	}
	model, provider := str(payload, kModel), str(payload, kProvider)
	found := false
	for _, pr := range g.Providers {
		if (provider == "" || pr.Slug == provider) && slices.Contains(pr.Models, model) {
			found, provider = true, pr.Slug
		}
	}
	if !found {
		return nil, gateway.ReasonModelResolution, nil
	}
	if slices.Contains(g.ConfirmModels, model) && str(payload, "confirm") != "tok-"+model {
		return map[string]any{"status": "confirmation_required", "confirm_required": true, "confirm": "tok-" + model,
			"target_model": model, "target_provider": provider, "confirm_message": model + " costs more per message.",
			"warnings": []any{}}, "", nil
	}
	if s.Running {
		return map[string]any{kSessionID: s.ID, kOperation: gateway.OpModel, kStatus: "deferred", kModel: model,
			kProvider: provider}, "", nil
	}
	s.Model, s.Provider = model, provider
	s.Revision++
	result := map[string]any{kSessionID: s.ID, kOperation: gateway.OpModel, kRevision: s.Revision, kExecGen: s.Generation,
		kModel: model, kProvider: provider}
	g.publishLocked(s, gateway.UpdatedEvent, result)
	g.publishLocked(s, gateway.InfoEvent, g.infoLocked(s, false))
	return result, "", nil
}

// setReasoning is config.set key=reasoning
// (gateway/session_tui_config.py _set_command_setting), which commits the
// fenced reasoning mutation itself.
func (g *Gateway) setReasoning(s *Session, p map[string]any) (any, string, func()) {
	if !onlyKeys(p, kSessionID, "profile", "key", "value", kScope) {
		return nil, errInvalidParams, nil
	}
	v := str(p, "value")
	switch {
	case v == gateway.ReasoningReset:
		s.Effort = ""
	case validEffort(v):
		s.Effort = strings.ToLower(v)
	default:
		return nil, errInvalidParams, nil
	}
	s.Revision++
	g.publishLocked(s, gateway.InfoEvent, g.infoLocked(s, false))
	g.publishLocked(s, gateway.UpdatedEvent, map[string]any{kSessionID: s.ID, kRevision: s.Revision, kExecGen: s.Generation,
		kOperation: kReasoning, "effort": v})
	return map[string]any{"key": gateway.ReasoningKey, "value": v, kScope: kSession}, "", nil
}

// hModelOptions follows gateway/session_config.py model_options.
func (g *Gateway) hModelOptions(_ *conn, p map[string]any) (any, string, func()) {
	if !onlyKeys(p, kSessionID, "profile", "explicit_only", "include_unconfigured", "refresh") {
		return nil, errInvalidParams, nil
	}
	model, provider := "", ""
	if s := g.sessions[str(p, kSessionID)]; s != nil {
		model, provider = s.Model, s.Provider
	}
	rows := make([]map[string]any, 0, len(g.Providers))
	for _, pr := range g.Providers {
		caps := map[string]any{}
		for m, c := range pr.Capabilities {
			caps[m] = map[string]any{"fast": false, kReasoning: c.Reasoning}
		}
		rows = append(rows, map[string]any{"slug": pr.Slug, "name": pr.Name, "models": pr.Models,
			"total_models": len(pr.Models), "is_current": pr.Slug == provider, "authenticated": pr.Authenticated,
			"unavailable_models": pr.UnavailableModels, "capabilities": caps})
	}
	return map[string]any{"providers": rows, kModel: model, kProvider: provider}, "", nil
}
