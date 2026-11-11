package device

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"time"

	"github.com/USA-RedDragon/hermes-hub/internal/hub"
)

// uploadTimeout bounds one upload: the plugin sends the file to the
// device over its exec leg.
const uploadTimeout = 5 * time.Minute

var _ hub.SessionFiles = (*Server)(nil)

// PutFile writes a file a user attached into the device folder that runs a
// session. See hub.SessionFiles.
func (s *Server) PutFile(ctx context.Context, user, backendID, sessionID, name string, data []byte) (string, bool, error) {
	if s.backends[backendID] == nil {
		return "", false, s.opts.Access.Allowed(ctx, user, backendID)
	}
	b, err := s.deviceBackend(ctx, user, backendID)
	if err != nil {
		return "", false, err
	}
	cctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()
	f, err := b.plugin.PutFile(cctx, sessionID, name, data)
	var pe *PluginError
	if errors.As(err, &pe) && pe.Code == "runs_on_pod" {
		return "", false, nil
	}
	if err != nil {
		return "", false, uploadError(name, err)
	}
	if !path.IsAbs(f.Path) {
		return "", false, fmt.Errorf("%w: The computer did not say where it saved %s", hub.ErrAttachment, name)
	}
	return path.Clean(f.Path), true, nil
}

// uploadError turns a plugin refusal of an upload into a message for the
// user.
func uploadError(name string, err error) error {
	var pe *PluginError
	if !errors.As(err, &pe) {
		return fmt.Errorf("%w: Could not reach Hermes to save %s. Try again in a moment", hub.ErrAttachment, name)
	}
	switch {
	case pe.Code == "device_offline":
		return fmt.Errorf("%w: The computer this chat runs on is offline, so %s cannot reach it", hub.ErrAttachment, name)
	case pe.Code == "too_large":
		return fmt.Errorf("%w: The file %s is too large for the computer: %s", hub.ErrAttachment, name, pe.Message)
	case pe.Code == "fail_closed":
		return fmt.Errorf("%w: This chat runs on no computer now, so %s has nowhere to go (%s)", hub.ErrAttachment, name, pe.Message)
	case pe.Code == "write_refused":
		return fmt.Errorf("%w: The computer refused to save %s: %s", hub.ErrAttachment, name, pe.Message)
	case pe.Status == http.StatusNotFound || pe.Status == http.StatusMethodNotAllowed:
		return fmt.Errorf("%w: The Hermes device plugin is too old to take files. Update hermes-device on the Hermes server", hub.ErrAttachment)
	}
	return fmt.Errorf("%w: Could not save %s on the computer: %w", hub.ErrAttachment, name, err)
}
