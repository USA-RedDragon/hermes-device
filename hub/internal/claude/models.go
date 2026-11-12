package claude

import (
	"context"
	"slices"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// NewChatModels lists what a chat that does not exist yet can use: the
// models of the providers Hermes is signed in to and its default model
// with that model's context window. It reads them from the dashboard, as
// a chat's model list reads them from the gateway.
func (s *Service) NewChatModels(ctx context.Context, user, backendID string) (hub.NewChatModels, error) {
	c, err := s.client(ctx, user, backendID)
	if err != nil {
		return hub.NewChatModels{}, err
	}
	cctx, cancel := context.WithTimeout(ctx, s.o.Timeout)
	defer cancel()
	o, err := c.ModelOptions(cctx)
	if err = mapError(err); err != nil {
		return hub.NewChatModels{}, err
	}
	out := hub.NewChatModels{Models: hub.Models{Provider: o.Provider, Model: o.Model, Providers: []hub.ModelProvider{}}}
	for _, p := range o.Providers {
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
			ch := hub.ModelChoice{ID: m, Reasoning: true, CanDisableReasoning: true}
			if caps, ok := p.Capabilities[m]; ok {
				ch.Reasoning = caps.Reasoning
				ch.CanDisableReasoning = caps.CanDisableReasoning == nil || *caps.CanDisableReasoning
			}
			hp.Models = append(hp.Models, ch)
		}
		if len(hp.Models) > 0 {
			out.Providers = append(out.Providers, hp)
		}
	}
	hub.ArrangeModels(out.Providers)
	if info, err := c.ModelInfo(cctx); err == nil && info.Model == o.Model {
		out.ContextLength = info.EffectiveContextLength
	}
	return out, nil
}
