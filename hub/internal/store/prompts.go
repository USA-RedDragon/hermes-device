package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/USA-RedDragon/talaria/hub/internal/hub"
)

var _ hub.PromptLibrary = (*Store)(nil)

// promptLockKey is the first key of the PostgreSQL advisory lock that
// keeps two adds of one user from passing the limit together.
const promptLockKey = 0x70726f6d // "prom"

// Prompts lists user's saved prompts by name. Times are stored in Unix
// milliseconds.
func (s *Store) Prompts(ctx context.Context, user string) ([]hub.SavedPrompt, error) {
	rows, err := s.db.QueryContext(ctx, s.q(`SELECT name, text, updated_at FROM saved_prompts WHERE "user" = ? ORDER BY name_key`), user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []hub.SavedPrompt{}
	for rows.Next() {
		var (
			p  hub.SavedPrompt
			at int64
		)
		if err := rows.Scan(&p.Name, &p.Text, &at); err != nil {
			return nil, err
		}
		p.UpdatedAt = time.UnixMilli(at)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Prompt returns user's prompt named name, in any case.
func (s *Store) Prompt(ctx context.Context, user, name string) (hub.SavedPrompt, bool, error) {
	var (
		p  hub.SavedPrompt
		at int64
	)
	err := s.row(ctx, `SELECT name, text, updated_at FROM saved_prompts WHERE "user" = ? AND name_key = ?`, user, hub.PromptKey(name)).
		Scan(&p.Name, &p.Text, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return hub.SavedPrompt{}, false, nil
	}
	if err != nil {
		return hub.SavedPrompt{}, false, err
	}
	p.UpdatedAt = time.UnixMilli(at)
	return p, true, nil
}

// AddPrompt adds a prompt to user's library.
func (s *Store) AddPrompt(ctx context.Context, user string, p hub.SavedPrompt) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		if s.dialect == postgresDialect {
			if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1, hashtext($2))`, promptLockKey, user); err != nil {
				return err
			}
		}
		var n int
		if err := tx.QueryRowContext(ctx, s.q(`SELECT COUNT(*) FROM saved_prompts WHERE "user" = ?`), user).Scan(&n); err != nil {
			return err
		}
		if n >= hub.MaxUserPrompts {
			return hub.ErrTooManyPrompts
		}
		return s.insertPrompt(ctx, tx, user, p)
	})
}

func (s *Store) insertPrompt(ctx context.Context, tx *sql.Tx, user string, p hub.SavedPrompt) error {
	res, err := tx.ExecContext(ctx, s.q(`INSERT INTO saved_prompts ("user", name_key, name, text, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT ("user", name_key) DO NOTHING`), user, hub.PromptKey(p.Name), p.Name, p.Text, s.now().UnixMilli())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return hub.ErrPromptExists
	}
	return nil
}

// UpdatePrompt replaces user's prompt named name with p.
func (s *Store) UpdatePrompt(ctx context.Context, user, name string, p hub.SavedPrompt) error {
	return s.tx(ctx, func(tx *sql.Tx) error {
		old, key := hub.PromptKey(name), hub.PromptKey(p.Name)
		if old == key {
			res, err := tx.ExecContext(ctx, s.q(`UPDATE saved_prompts SET name = ?, text = ?, updated_at = ? WHERE "user" = ? AND name_key = ?`),
				p.Name, p.Text, s.now().UnixMilli(), user, old)
			return gone(res, err)
		}
		if err := s.insertPrompt(ctx, tx, user, p); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, s.q(`DELETE FROM saved_prompts WHERE "user" = ? AND name_key = ?`), user, old)
		return gone(res, err)
	})
}

// DeletePrompt deletes user's prompt named name.
func (s *Store) DeletePrompt(ctx context.Context, user, name string) error {
	res, err := s.db.ExecContext(ctx, s.q(`DELETE FROM saved_prompts WHERE "user" = ? AND name_key = ?`), user, hub.PromptKey(name))
	return gone(res, err)
}

// gone turns a change that touched no row into hub.ErrNoPrompt.
func gone(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return hub.ErrNoPrompt
	}
	return nil
}
