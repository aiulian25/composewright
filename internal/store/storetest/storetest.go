// Package storetest edits a store's database behind its back, for tests in
// other packages that check tampering is noticed. Only tests import it.
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
