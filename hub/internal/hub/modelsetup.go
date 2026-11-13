package hub

import (
	"context"
	"encoding/json"
)

// ModelSetup connects model providers to a backend's Hermes through
// Hermes's own setup routes. The hub keeps none of it: keys pass through in
// one request, and every answer comes from Hermes.
type ModelSetup interface {
	// ModelStatus says whether the backend's Hermes has a model a chat can
	// use. Any user of the backend may ask.
	ModelStatus(ctx context.Context, user, backendID string) (ModelStatus, error)
	// ModelCall runs one setup call. The web layer lets only admins call
	// it.
	ModelCall(ctx context.Context, user, backendID string, call ModelCall) (json.RawMessage, error)
}

// ModelStatus is a backend's model state.
type ModelStatus struct {
	// Ready is true when Hermes names a default model and its provider
	// can sign in.
	Ready    bool   `json:"ready"`
	Model    string `json:"model"`
	Provider string `json:"provider"`
}

// ModelOp names one setup call.
type ModelOp string

// The setup calls. Each maps to one Hermes route; nothing else is reached.
const (
	// ModelCatalog lists what can be connected: API-key providers, sign-in
	// accounts and the kinds only the server can set up.
	ModelCatalog ModelOp = "catalog"
	// ModelConnected lists what is connected, with the default.
	ModelConnected ModelOp = "connected"
	// ModelOptions lists the models of the connected providers.
	ModelOptions ModelOp = "options"
	// ModelInfo is the default model with its context window.
	ModelInfo ModelOp = "info"
	// ModelRecommended is Hermes's suggested model of a provider.
	ModelRecommended ModelOp = "recommended"
	// ModelSetDefault makes a model the default of new chats.
	ModelSetDefault ModelOp = "set-default"
	// ModelTestEndpoint probes a server's address.
	ModelTestEndpoint ModelOp = "test-endpoint"
	// ModelSaveEndpoint adds or changes a server.
	ModelSaveEndpoint ModelOp = "save-endpoint"
	// ModelUseEndpoint makes a server's model the default.
	ModelUseEndpoint ModelOp = "use-endpoint"
	// ModelRemoveEndpoint removes a server.
	ModelRemoveEndpoint ModelOp = "remove-endpoint"
	// ModelTestKey checks an API key with the provider, where Hermes can.
	ModelTestKey ModelOp = "test-key"
	// ModelSaveKey saves an API key.
	ModelSaveKey ModelOp = "save-key"
	// ModelRemoveKey removes an API key.
	ModelRemoveKey ModelOp = "remove-key"
	// ModelSignInStart starts a device-code sign-in.
	ModelSignInStart ModelOp = "signin-start"
	// ModelSignInPoll waits for a sign-in to end.
	ModelSignInPoll ModelOp = "signin-poll"
	// ModelSignInCancel cancels a sign-in.
	ModelSignInCancel ModelOp = "signin-cancel"
	// ModelSignOut removes a signed-in account.
	ModelSignOut ModelOp = "signout"
)

// ModelCall is one setup call: the operation and its inputs. Fields an
// operation does not use are ignored.
type ModelCall struct {
	Op ModelOp
	// ID is an endpoint id, an API key's variable or a sign-in provider.
	ID string
	// Session is a sign-in session.
	Session string
	// Refresh asks Hermes for fresh model lists.
	Refresh bool
	// Provider names the provider of ModelRecommended.
	Provider string
	Endpoint *EndpointSetup
	Key      *KeySetup
	Default  *DefaultSetup
}

// EndpointSetup is a server that speaks an OpenAI-style API. APIKey nil
// keeps a saved key; "" removes it.
type EndpointSetup struct {
	ID             string   `json:"id,omitempty"`
	Name           string   `json:"name"`
	BaseURL        string   `json:"base_url"`
	Model          string   `json:"model"`
	APIKey         *string  `json:"api_key,omitempty"`
	APIMode        *string  `json:"api_mode,omitempty"`
	ContextLength  *int     `json:"context_length,omitempty"`
	DiscoverModels *bool    `json:"discover_models,omitempty"`
	MakeDefault    bool     `json:"make_default"`
	Models         []string `json:"models,omitempty"`
}

// KeySetup is an API key for the provider whose variable is Key.
type KeySetup struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// DefaultSetup is the default model of new chats.
type DefaultSetup struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Confirm  bool   `json:"confirm"`
}

// ModelSetupError is a setup call that failed, with the words to show:
// Hermes's own message, or the hub's when Hermes could not be asked.
type ModelSetupError struct {
	Status  int
	Message string
}

func (e *ModelSetupError) Error() string { return e.Message }
