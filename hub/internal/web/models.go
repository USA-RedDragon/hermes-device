package web

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// modelRoutes serve Settings > Models and the Connect a model dialog. Any
// user of a backend may read its status; every other route is for the
// backend's model admins (models.admins, or the hub's admins when that is
// empty), checked here on the server. Request bodies are never logged:
// they may hold an API key.
func (s *Server) modelRoutes(mux *http.ServeMux) {
	const base = "/api/models/{backend}"
	mux.Handle("GET "+base+"/status", s.api(s.handleModelStatus))
	admin := func(op hub.ModelOp, body bool) http.Handler { return s.modelAdmin(s.modelCall(op, body)) }
	mux.Handle("GET "+base+"/catalog", admin(hub.ModelCatalog, false))
	mux.Handle("GET "+base+"/connected", admin(hub.ModelConnected, false))
	mux.Handle("GET "+base+"/options", admin(hub.ModelOptions, false))
	mux.Handle("GET "+base+"/info", admin(hub.ModelInfo, false))
	mux.Handle("GET "+base+"/recommended", admin(hub.ModelRecommended, false))
	mux.Handle("POST "+base+"/default", admin(hub.ModelSetDefault, true))
	mux.Handle("POST "+base+"/endpoints/test", admin(hub.ModelTestEndpoint, true))
	mux.Handle("POST "+base+"/endpoints", admin(hub.ModelSaveEndpoint, true))
	mux.Handle("POST "+base+"/endpoints/{id}/use", admin(hub.ModelUseEndpoint, false))
	mux.Handle("DELETE "+base+"/endpoints/{id}", admin(hub.ModelRemoveEndpoint, false))
	mux.Handle("POST "+base+"/keys/test", admin(hub.ModelTestKey, true))
	mux.Handle("PUT "+base+"/keys", admin(hub.ModelSaveKey, true))
	mux.Handle("DELETE "+base+"/keys/{id}", admin(hub.ModelRemoveKey, false))
	mux.Handle("POST "+base+"/signin/{id}", admin(hub.ModelSignInStart, false))
	mux.Handle("GET "+base+"/signin/{id}/{session}", admin(hub.ModelSignInPoll, false))
	mux.Handle("DELETE "+base+"/signin/{id}", admin(hub.ModelSignOut, false))
	mux.Handle("DELETE "+base+"/signin/{id}/{session}", admin(hub.ModelSignInCancel, false))
}

// isModelAdmin says whether the user may connect models to the request's
// backend.
func (s *Server) isModelAdmin(r *http.Request) bool {
	return s.modelAdminOf(r.Context(), userFrom(r), r.PathValue("backend"))
}

// modelAdminOf says whether user may connect models to backendID:
// models.admins names the pair, or, with models.admins empty, the user is
// a hub admin. Access to the backend itself is checked on every call.
func (s *Server) modelAdminOf(ctx context.Context, user, backendID string) bool {
	if s.models == nil {
		return false
	}
	if len(s.modelAdmins) == 0 {
		return s.isAdmin(ctx, user)
	}
	return slices.Contains(s.modelAdmins[backendID], user)
}

// modelAdminBackends are the backends of the user's that they may connect
// models to, for /api/me.
func (s *Server) modelAdminBackends(ctx context.Context, user string) []string {
	out := []string{}
	if s.models == nil {
		return out
	}
	bs, err := s.svc.Backends(ctx, user)
	if err != nil {
		return out
	}
	for _, b := range bs {
		if s.modelAdminOf(ctx, user, b.ID) {
			out = append(out, b.ID)
		}
	}
	return out
}

func (s *Server) modelAdmin(h http.HandlerFunc) http.Handler {
	return s.api(func(w http.ResponseWriter, r *http.Request) {
		if !s.isModelAdmin(r) {
			errorJSON(w, http.StatusForbidden, "Only an admin may connect models")
			return
		}
		h(w, r)
	})
}

func (s *Server) modelFail(w http.ResponseWriter, err error) {
	var me *hub.ModelSetupError
	switch {
	case errors.As(err, &me):
		errorJSON(w, me.Status, me.Message)
	default:
		s.fail(w, err)
	}
}

func (s *Server) handleModelStatus(w http.ResponseWriter, r *http.Request) {
	if s.models == nil {
		errorJSON(w, http.StatusNotFound, "This hub cannot check models")
		return
	}
	st, err := s.models.ModelStatus(r.Context(), userFrom(r), r.PathValue("backend"))
	if err != nil {
		s.modelFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		hub.ModelStatus
		Admin bool `json:"admin"`
	}{st, s.isModelAdmin(r)})
}

// modelBody is what a setup call's request body may hold.
type modelBody struct {
	hub.EndpointSetup
	Key      string `json:"key"`
	Value    string `json:"value"`
	Provider string `json:"provider"`
	Confirm  bool   `json:"confirm"`
}

func (s *Server) modelCall(op hub.ModelOp, hasBody bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := hub.ModelCall{Op: op, ID: r.PathValue("id"), Session: r.PathValue("session"),
			Refresh: r.URL.Query().Get("refresh") == "1", Provider: r.URL.Query().Get("provider")}
		if hasBody {
			var b modelBody
			if !decode(w, r, &b) {
				return
			}
			c.Default = &hub.DefaultSetup{Provider: b.Provider, Model: b.Model, Confirm: b.Confirm}
			c.Key = &hub.KeySetup{Key: b.Key, Value: b.Value}
			c.Endpoint = &b.EndpointSetup
		}
		out, err := s.models.ModelCall(r.Context(), userFrom(r), r.PathValue("backend"), c)
		if err != nil {
			s.modelFail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
	}
}
