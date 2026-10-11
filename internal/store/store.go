// Package store is the only package that talks to SQLite. Secret values are
// sealed before they reach a row and opened only on the way out.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/aiulian25/composewright/internal/secrets"
)

//go:embed migrations/*.sql
var migrations embed.FS

const (
	driverName  = "sqlite"
	idBytes     = 16
	fileMode    = 0o600
	dirMode     = 0o700
	busyTimeout = 5000
)

var ErrNotFound = errors.New("not found")

type Store struct {
	database *sql.DB
	sealer   *secrets.Sealer
}

// Open creates the database file with mode 0600 if needed, applies pending
// migrations (backing the database up first) and returns a ready store.
func Open(ctx context.Context, path, backupsDir string, sealer *secrets.Sealer) (*Store, error) {
	if err := createPrivate(path); err != nil {
		return nil, err
	}
	database, err := sql.Open(driverName, dataSourceName(path))
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	store := &Store{database: database, sealer: sealer}
	if err := store.migrate(ctx, backupsDir); err != nil {
		return nil, errors.Join(err, database.Close())
	}
	return store, nil
}

func (store *Store) Close() error {
	return store.database.Close()
}

func dataSourceName(path string) string {
	query := url.Values{}
	query.Add("_pragma", "foreign_keys(1)")
	query.Add("_pragma", "journal_mode(WAL)")
	query.Add("_pragma", "busy_timeout("+strconv.Itoa(busyTimeout)+")")
	return (&url.URL{Scheme: "file", Path: path, RawQuery: query.Encode()}).String()
}

// createPrivate makes sure SQLite never creates the file itself, because it
// would use the process umask; the -wal and -shm files copy this mode.
func createPrivate(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), dirMode); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		return err
	}
	return file.Close()
}

func (store *Store) migrate(ctx context.Context, backupsDir string) error {
	if _, err := store.database.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_version (version INTEGER NOT NULL)`); err != nil {
		return err
	}
	current, err := store.schemaVersion(ctx)
	if err != nil {
		return err
	}
	pending, err := pendingMigrations(current)
	if err != nil || len(pending) == 0 {
		return err
	}
	if current > 0 {
		if err := store.backup(ctx, backupsDir, current); err != nil {
			return fmt.Errorf("back up the database before migrating: %w", err)
		}
	}
	for _, migration := range pending {
		if err := store.apply(ctx, migration); err != nil {
			return fmt.Errorf("migration %s: %w", migration.name, err)
		}
	}
	return nil
}

func (store *Store) schemaVersion(ctx context.Context) (int, error) {
	var version sql.NullInt64
	err := store.database.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_version`).Scan(&version)
	return int(version.Int64), err
}

type migration struct {
	version int
	name    string
	script  string
}

func pendingMigrations(current int) ([]migration, error) {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return nil, err
	}
	var pending []migration
	for _, entry := range entries {
		prefix, _, _ := strings.Cut(entry.Name(), "_")
		version, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %s has no numeric prefix", entry.Name())
		}
		if version <= current {
			continue
		}
		script, err := fs.ReadFile(migrations, "migrations/"+entry.Name())
		if err != nil {
			return nil, err
		}
		pending = append(pending, migration{version: version, name: entry.Name(), script: string(script)})
	}
	sort.Slice(pending, func(left, right int) bool { return pending[left].version < pending[right].version })
	return pending, nil
}

func (store *Store) apply(ctx context.Context, migration migration) error {
	return store.transaction(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, migration.script); err != nil {
			return err
		}
		_, err := transaction.ExecContext(ctx, `INSERT INTO schema_version (version) VALUES (?)`, migration.version)
		return err
	})
}

func (store *Store) backup(ctx context.Context, backupsDir string, version int) error {
	if err := os.MkdirAll(backupsDir, dirMode); err != nil {
		return err
	}
	target := filepath.Join(backupsDir, fmt.Sprintf("composewright-v%d-%s.db", version, time.Now().UTC().Format("20060102T150405Z")))
	if _, err := store.database.ExecContext(ctx, `VACUUM INTO ?`, target); err != nil {
		return err
	}
	return os.Chmod(target, fileMode)
}

func (store *Store) transaction(ctx context.Context, work func(*sql.Tx) error) error {
	transaction, err := store.database.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := work(transaction); err != nil {
		return errors.Join(err, transaction.Rollback())
	}
	return transaction.Commit()
}

func newID() string {
	buffer := make([]byte, idBytes)
	if _, err := rand.Read(buffer); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(buffer)
}

func unix(moment time.Time) int64 {
	return moment.UTC().Unix()
}

func fromUnix(seconds int64) time.Time {
	if seconds == 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}
