package device

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"sync"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// uploadTimeout bounds one upload: the plugin sends the file to the
// device over its exec leg, in two commands of up to 35 and 305 seconds
// (plugin/device/core.py UPLOAD_TIMEOUT_S), so the hub waits longer than
// the plugin can take.
const uploadTimeout = 7 * time.Minute

// codeBusy is the plugin's refusal of an upload while others run.
const codeBusy = "busy"

// codeOffline is the plugin's answer for a device that is not connected.
const codeOffline = "device_offline"

// Plugin error codes of file routes.
const (
	codeRunsOnPod = "runs_on_pod"
	codeTooLarge  = "too_large"
	codeNotFound  = "not_found"
)

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
	if errors.As(err, &pe) && pe.Code == codeRunsOnPod {
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
	case pe.Code == codeOffline:
		return fmt.Errorf("%w: The computer this chat runs on is offline, so %s cannot reach it", hub.ErrAttachment, name)
	case pe.Code == codeBusy:
		return fmt.Errorf("%w: Hermes is busy with other uploads. Send %s again in a moment", hub.ErrAttachment, name)
	case pe.Code == codeTooLarge:
		return fmt.Errorf("%w: The file %s is too large for the computer: %s", hub.ErrAttachment, name, pe.Message)
	case pe.Code == StateFailClosed:
		return fmt.Errorf("%w: This chat runs on no computer now, so %s has nowhere to go (%s)", hub.ErrAttachment, name, pe.Message)
	case pe.Code == "write_refused":
		return fmt.Errorf("%w: The computer refused to save %s: %s", hub.ErrAttachment, name, pe.Message)
	case pe.Status == http.StatusNotFound || pe.Status == http.StatusMethodNotAllowed:
		return fmt.Errorf("%w: The Talaria device plugin is too old to take files. Update talaria on the Hermes server", hub.ErrAttachment)
	}
	return fmt.Errorf("%w: Could not save %s on the computer: %w", hub.ErrAttachment, name, err)
}

const (
	// readTimeout bounds one file read from a device and its transfer to
	// the hub: the plugin gives the device 125 seconds (plugin/device/core.py
	// FILE_READ_TIMEOUT_S) and then sends at most 32 MiB.
	readTimeout = 4 * time.Minute
	// maxReads is how many reads of one backend wait for the plugin at
	// once, the plugin's own limit (MAX_FILE_READS). The rest wait here
	// instead of getting the plugin's busy answer.
	maxReads = 2
	// busyRetries and busyWait bound the retries of a read that the plugin
	// refused as busy, for reads that another hub replica holds.
	busyRetries = 8
	busyWait    = 250 * time.Millisecond
)

var _ hub.DeviceMedia = (*Server)(nil)

// DeviceMedia opens a file of the device that runs a session. See
// hub.DeviceMedia.
func (s *Server) DeviceMedia(ctx context.Context, user, backendID, sessionID, p string) (*hub.MediaFile, bool, error) {
	if s.backends[backendID] == nil {
		return nil, false, s.opts.Access.Allowed(ctx, user, backendID)
	}
	b, err := s.deviceBackend(ctx, user, backendID)
	if err != nil {
		return nil, false, err
	}
	cctx, cancel := context.WithTimeout(ctx, readTimeout)
	resp, release, err := b.readFile(cctx, sessionID, p)
	if err != nil {
		cancel()
		var pe *PluginError
		if !errors.As(err, &pe) {
			return nil, false, err
		}
		switch {
		case pe.Code == codeRunsOnPod || pe.Code == StateFailClosed:
			return nil, false, nil
		case pe.Code == codeOffline:
			return nil, true, hub.ErrMediaOffline
		case pe.Code == codeTooLarge:
			return nil, true, hub.ErrMediaTooLarge
		case pe.Status == http.StatusNotFound || pe.Status == http.StatusMethodNotAllowed ||
			pe.Code == codeNotFound || pe.Code == "bad_path" || pe.Code == "read_refused" || pe.Code == "read_failed":
			if pe.Code != codeNotFound {
				s.log.Info("the device plugin did not read a file", "session", sessionID, "error", pe.Error())
			}
			return nil, true, hub.ErrNoMedia
		}
		return nil, false, err
	}
	done := func() {
		cancel()
		release()
	}
	return &hub.MediaFile{Name: hub.CleanName(path.Base(p)), Size: resp.ContentLength, NoStore: true,
		Body: &doneBody{ReadCloser: resp.Body, done: done}}, true, nil
}

// readFile reads a file through the plugin, at most maxReads at once, and
// retries a busy answer. release frees the slot; on success the caller
// calls it once the body is read, since the plugin holds the file until
// then.
func (b *backend) readFile(ctx context.Context, sessionID, p string) (*http.Response, func(), error) {
	select {
	case b.reads <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, ctx.Err()
	}
	release := func() { <-b.reads }
	for try := 1; ; try++ {
		resp, err := b.plugin.ReadFile(ctx, sessionID, p)
		var pe *PluginError
		if err == nil {
			return resp, release, nil
		}
		if try > busyRetries || !errors.As(err, &pe) || pe.Code != codeBusy {
			release()
			return nil, nil, err
		}
		t := time.NewTimer(time.Duration(try) * busyWait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			release()
			return nil, nil, ctx.Err()
		}
	}
}

// doneBody calls done once when the body is closed.
type doneBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (d *doneBody) Close() error {
	err := d.ReadCloser.Close()
	d.once.Do(d.done)
	return err
}
