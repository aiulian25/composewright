package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/secrets"
)

func TestUsersAndSessions(t *testing.T) {
	store, _, _ := openTestStore(t)
	ctx := context.Background()
	if has, err := store.HasUsers(ctx); err != nil || has {
		t.Fatalf("fresh store has users: %v %v", has, err)
	}
	user, err := store.CreateUser(ctx, "admin", "$argon2id$hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateUser(ctx, "admin", "$argon2id$other"); err == nil {
		t.Fatal("a second user with the same name was created")
	}
	found, err := store.UserByName(ctx, "admin")
	if err != nil || found.ID != user.ID || found.PasswordHash != "$argon2id$hash" {
		t.Fatalf("user by name: %+v %v", found, err)
	}
	if _, err := store.UserByName(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user: %v", err)
	}
	now := time.Now()
	for _, hash := range []string{"current", "other", "stale"} {
		session := Session{IDHash: hash, UserID: user.ID, CSRF: "csrf-" + hash, CreatedAt: now, LastSeen: now, ExpiresAt: now.Add(time.Hour)}
		if hash == "stale" {
			session.LastSeen = now.Add(-3 * time.Hour)
		}
		if err := store.CreateSession(ctx, session); err != nil {
			t.Fatal(err)
		}
	}
	if session, err := store.SessionByHash(ctx, "current"); err != nil || session.CSRF != "csrf-current" || session.UserID != user.ID {
		t.Fatalf("session: %+v %v", session, err)
	}
	if err := store.DeleteStaleSessions(ctx, now, 2*time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionByHash(ctx, "stale"); !errors.Is(err, ErrNotFound) {
		t.Fatal("an idle session survived")
	}
	if err := store.SetPassword(ctx, user.ID, "$argon2id$new", "current"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionByHash(ctx, "other"); !errors.Is(err, ErrNotFound) {
		t.Fatal("changing the password kept another session")
	}
	if _, err := store.SessionByHash(ctx, "current"); err != nil {
		t.Fatal("changing the password ended the session that changed it")
	}
	if err := store.DeleteSession(ctx, "current"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionByHash(ctx, "current"); !errors.Is(err, ErrNotFound) {
		t.Fatal("logout kept the session")
	}
}

// TestUpgradeFromVersionOne opens a database created by migration 0001 and
// checks that it is backed up, migrated, and keeps its rows.
func TestUpgradeFromVersionOne(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "composewright.db")
	if err := createPrivate(path); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open(driverName, dataSourceName(path))
	if err != nil {
		t.Fatal(err)
	}
	first, err := migrations.ReadFile("migrations/0001_initial.sql")
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{`CREATE TABLE schema_version (version INTEGER NOT NULL)`, string(first), `INSERT INTO schema_version (version) VALUES (1)`,
		`INSERT INTO projects (id, name, slug, source_kind, status, created_at, updated_at) VALUES ('p1', 'old', 'old', 'local', 'passed', 1, 1)`,
		`INSERT INTO generations (id, project_id, iteration, engine, ir_json, created_at) VALUES ('g1', 'p1', 1, 'rules', '{}', 1)`}
	for _, statement := range statements {
		if _, err := database.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	key, _ := secrets.NewKey()
	backups := filepath.Join(directory, "backups")
	store, err := Open(context.Background(), path, backups, secrets.NewSealer(key))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	generation, err := store.LatestGeneration(context.Background(), "p1")
	if err != nil || generation.ID != "g1" || generation.FactsJSON != "" {
		t.Fatalf("generation after upgrade: %+v %v", generation, err)
	}
	copies, _ := os.ReadDir(backups)
	if len(copies) != 1 {
		t.Fatalf("%d backups before the migration, want 1", len(copies))
	}
}
