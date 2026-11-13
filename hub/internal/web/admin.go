package web

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

// UpdateChecker says which parts of the install are behind.
type UpdateChecker interface {
	Status(ctx context.Context) hub.UpdateStatus
}

// adminRoutes serve the hub-wide admin parts: the update banner, Settings >
// Telegram's token and Settings > Backup. Only hub admins reach them;
// members get 403 and never see the banner.
func (s *Server) adminRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/admin/updates", s.hubAdmin(s.handleUpdates))
	mux.Handle("GET /api/admin/telegram", s.hubAdmin(s.handleTelegramStatus))
	mux.Handle("POST /api/admin/telegram/test", s.hubAdmin(s.handleTelegramTest))
	mux.Handle("PUT /api/admin/telegram", s.hubAdmin(s.handleTelegramSave))
	mux.Handle("DELETE /api/admin/telegram", s.hubAdmin(s.handleTelegramClear))
	mux.Handle("POST /api/admin/backup", s.hubAdmin(s.handleBackupStart))
	mux.Handle("GET /api/admin/backup/{token}", s.hubAdmin(s.handleBackupDownload))
}

func (s *Server) hubAdmin(h http.HandlerFunc) http.Handler {
	return s.api(func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r.Context(), userFrom(r)) {
			errorJSON(w, http.StatusForbidden, "Only an admin may do this")
			return
		}
		h(w, r)
	})
}

func (s *Server) handleUpdates(w http.ResponseWriter, r *http.Request) {
	if s.updates == nil {
		writeJSON(w, http.StatusOK, hub.UpdateStatus{Current: s.version, Install: s.install})
		return
	}
	writeJSON(w, http.StatusOK, s.updates.Status(r.Context()))
}

func (s *Server) telegramFail(w http.ResponseWriter, err error) {
	var te *hub.TelegramError
	if errors.As(err, &te) {
		errorJSON(w, te.Status, te.Message)
		return
	}
	s.fail(w, err)
}

func (s *Server) handleTelegramStatus(w http.ResponseWriter, r *http.Request) {
	if s.telegram == nil {
		writeJSON(w, http.StatusOK, hub.TelegramStatus{})
		return
	}
	writeJSON(w, http.StatusOK, s.telegram.TelegramStatus(r.Context()))
}

type tokenBody struct {
	Token string `json:"token"`
}

// handleTelegramTest checks a token with Telegram. The body is never logged.
func (s *Server) handleTelegramTest(w http.ResponseWriter, r *http.Request) {
	var b tokenBody
	if s.telegram == nil || !decode(w, r, &b) {
		if s.telegram == nil {
			errorJSON(w, http.StatusNotFound, "This hub has no Telegram bot")
		}
		return
	}
	name, err := s.telegram.TestTelegram(r.Context(), b.Token)
	if err != nil {
		s.telegramFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"bot": name})
}

func (s *Server) handleTelegramSave(w http.ResponseWriter, r *http.Request) {
	var b tokenBody
	if s.telegram == nil || !decode(w, r, &b) {
		if s.telegram == nil {
			errorJSON(w, http.StatusNotFound, "This hub has no Telegram bot")
		}
		return
	}
	name, err := s.telegram.SaveTelegram(r.Context(), b.Token)
	if err != nil {
		s.telegramFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"bot": name})
}

func (s *Server) handleTelegramClear(w http.ResponseWriter, r *http.Request) {
	if s.telegram == nil {
		errorJSON(w, http.StatusNotFound, "This hub has no Telegram bot")
		return
	}
	if err := s.telegram.ClearTelegram(r.Context()); err != nil {
		s.telegramFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// backupTTL is how long a packed backup can be downloaded.
const backupTTL = 30 * time.Minute

type backupToken struct {
	Backend string    `json:"b"`
	Archive string    `json:"a"`
	Until   time.Time `json:"u"`
}

// handleBackupStart packs the agent's data and answers a token to download
// the backup with. It waits until the agent is done.
func (s *Server) handleBackupStart(w http.ResponseWriter, r *http.Request) {
	if s.agent == nil {
		errorJSON(w, http.StatusNotFound, "This hub cannot make backups")
		return
	}
	var b struct {
		Backend string `json:"backend"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.Backend == "" {
		if ids := s.agent.Backends(); len(ids) > 0 {
			b.Backend = ids[0]
		}
	}
	archive, err := s.agent.Backup(r.Context(), b.Backend)
	if err != nil {
		s.agentFail(w, err)
		return
	}
	tok, err := s.sessions.seal(backupToken{Backend: b.Backend, Archive: archive, Until: time.Now().Add(backupTTL)})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"token": tok})
}

// BackupReadme is the README of a backup file: what it holds and how to put
// it back.
const BackupReadme = `Talaria backup

This file holds what you need to set this Talaria server up again.

hub/hub.db       The hub's data: accounts, paired computers, Telegram links, saved prompts and settings
                 (with the Telegram token set in Settings). Missing when the hub keeps its data in
                 PostgreSQL: back that up with pg_dump.
hub/keys.yaml    The hub's keys: the session secret (it opens the Telegram token above), the key that
                 signs the hub's messages to computers and the Web Push key. Keep this file private.
agent/agent-backup.zip
                 The agent's data: chats, memory, skills, agent settings and model keys.

Not in it: files on your computers and the sandbox's work folder.

To restore on a new install (Docker Compose shown; the other install types are alike):

1. Start the new install once so it makes its volumes, then stop it: docker compose down
2. Put the keys back: write the values of hub/keys.yaml, without their quotes or a line break, into the
   secrets volume's generated/ files (session_secret into session-secret, device_signing_key into
   hub-signing-seed, vapid_private_key into vapid-key).
3. Put the hub's data back: copy hub/hub.db over talaria-hub.db in the hub's data volume, and delete
   talaria-hub.db-wal and talaria-hub.db-shm there if they exist.
4. Start it again: docker compose up -d --wait
5. Put the agent's data back. The import refuses to run while the agent is up, so in the server's folder:
   docker compose stop hermes
   docker compose cp agent/agent-backup.zip hermes:/opt/data/restore.zip
   docker compose run --rm --no-deps -u 10000 --entrypoint /opt/hermes/.venv/bin/hermes hermes import /opt/data/restore.zip --force
   docker compose up -d --wait

The full steps for each install type are in the Talaria docs, under Backups.
`

// handleBackupDownload streams one zip: the hub's data, its keys and the
// agent's archive.
func (s *Server) handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	var t backupToken
	if s.agent == nil || s.sessions.open(r.PathValue("token"), &t) != nil || time.Now().After(t.Until) {
		errorJSON(w, http.StatusNotFound, "This backup is gone. Make a new one.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()
	agent, err := s.agent.OpenBackup(ctx, t.Backend, t.Archive)
	if err != nil {
		s.agentFail(w, err)
		return
	}
	defer agent.Close()
	var db string
	if s.snapshot != nil {
		dir, err := os.MkdirTemp(s.scratch, "backup-")
		if err != nil {
			s.fail(w, err)
			return
		}
		defer os.RemoveAll(dir)
		db = filepath.Join(dir, "hub.db")
		if err := s.snapshot(ctx, db); err != nil {
			db = ""
		}
	}
	name := "talaria-backup-" + time.Now().UTC().Format("2006-01-02-1504") + ".zip"
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.WriteHeader(http.StatusOK)
	z := zip.NewWriter(w)
	add := func(path string, src io.Reader) error {
		f, err := z.CreateHeader(&zip.FileHeader{Name: path, Method: zip.Deflate, Modified: time.Now()})
		if err != nil {
			return err
		}
		_, err = io.Copy(f, src)
		return err
	}
	steps := []func() error{
		func() error { return add("README.txt", strings.NewReader(BackupReadme)) },
		func() error { return add("hub/keys.yaml", strings.NewReader(s.hubKeys)) },
		func() error {
			if db == "" {
				return nil
			}
			f, err := os.Open(db)
			if err != nil {
				return err
			}
			defer f.Close()
			return add("hub/hub.db", f)
		},
		func() error { return add("agent/agent-backup.zip", agent) },
		z.Close,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			s.log.Warn("backup download ended early", "error", err.Error())
			return
		}
	}
}
