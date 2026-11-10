// Package storetest gives tests a PostgreSQL database of their own.
package storetest

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
)

// DSNEnv names the environment variable with a PostgreSQL connection URL
// whose user may create databases. Tests that need PostgreSQL skip without
// it.
const DSNEnv = "HUB_TEST_POSTGRES_DSN"

// Postgres creates an empty database for the test, drops it when the test
// ends and returns its connection URL. It skips the test when DSNEnv is
// not set.
func Postgres(t testing.TB) string {
	t.Helper()
	base := os.Getenv(DSNEnv)
	if base == "" {
		t.Skipf("PostgreSQL tests skipped: set %s to a postgres:// URL of a user that may create databases", DSNEnv)
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatalf("%s must be a postgres:// URL", DSNEnv)
	}
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	name := "hubtest_" + hex.EncodeToString(b)
	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE "+name); err != nil {
		_ = admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := admin.ExecContext(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("drop test database: %v", err)
		}
		_ = admin.Close()
	})
	u.Path = "/" + name
	return u.String()
}
