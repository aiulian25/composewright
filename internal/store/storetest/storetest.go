// Package storetest edits a store's database behind its back, for tests in
// other packages that check tampering is noticed or a failed write handled.
// Only tests import it.
package storetest

import (
	"database/sql"
	"errors"
	"testing"

	_ "modernc.org/sqlite" // the driver internal/store uses
)

const driverName = "sqlite"

// EditAuditActor rewrites the actor of one audit entry in the database at
// path, as someone with access to the file could.
func EditAuditActor(t testing.TB, path string, sequence int) {
	t.Helper()
	database, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.ExecContext(t.Context(), `UPDATE audit_log SET actor='cli:mallory' WHERE seq=?`, sequence)
	if err := errors.Join(err, database.Close()); err != nil {
		t.Fatal(err)
	}
}

// RefuseSecrets makes every later write of a sealed setting to the database
// at path fail, as a full disk would.
func RefuseSecrets(t testing.TB, path string) {
	t.Helper()
	database, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.ExecContext(t.Context(), `CREATE TRIGGER refuse_secrets BEFORE INSERT ON settings BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
	if err := errors.Join(err, database.Close()); err != nil {
		t.Fatal(err)
	}
}
