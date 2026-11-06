package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/aiulian25/composewright/internal/secrets"
)

func organize(t *testing.T, db *Store, id string, labels []string, action string) Project {
	t.Helper()
	ctx := context.Background()
	current, err := db.FindProject(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.OrganizeProject(ctx, id, current.OrganizationRevision, labels, action); err != nil {
		t.Fatal(err)
	}
	updated, err := db.FindProject(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}
func orderedPins(t *testing.T, db *Store) []string {
	t.Helper()
	rows, err := db.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(rows, func(a, b ProjectSummary) int { return a.PinOrder - b.PinOrder })
	var names []string
	for _, row := range rows {
		if row.PinOrder > 0 {
			names = append(names, row.Name)
		}
	}
	return names
}

func TestOrganizationPersistsAndNeverChangesDeployment(t *testing.T) {
	ctx := context.Background()
	db, path, key := openTestStore(t)
	a, b, c := saveProject(t, db, "alpha"), saveProject(t, db, "beta"), saveProject(t, db, "gamma")
	before, _ := db.FindProject(ctx, a.ID)
	current := organize(t, db, a.ID, []string{"Media", " media ", "Home"}, "pin")
	if !reflect.DeepEqual(current.Labels, []string{"Home", "Media"}) {
		t.Fatalf("labels not normalized: %v", current.Labels)
	}
	stripped := current
	stripped.Labels = nil
	stripped.PinOrder = 0
	stripped.OrganizationRevision = 0
	if !reflect.DeepEqual(before, stripped) {
		t.Fatal("organization modified deployment identity/status/timestamps")
	}
	organize(t, db, b.ID, []string{"Media"}, "pin")
	organize(t, db, c.ID, nil, "pin")
	organize(t, db, c.ID, nil, "up")
	organize(t, db, c.ID, nil, "up")
	if got := orderedPins(t, db); !reflect.DeepEqual(got, []string{"gamma", "alpha", "beta"}) {
		t.Fatalf("wrong pin order: %v", got)
	}
	// Regeneration writes source/status through SaveProject, never organization.
	if _, err := db.SaveProject(ctx, Project{Name: "alpha", Slug: "alpha", SourceKind: "local", Status: StatusPassed}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path, filepath.Join(filepath.Dir(path), "backups"), secrets.NewSealer(key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	current, err = reopened.FindProject(ctx, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if current.PinOrder != 2 || !reflect.DeepEqual(current.Labels, []string{"Home", "Media"}) || current.Status != StatusPassed {
		t.Fatalf("restart/regeneration lost organization: %+v", current)
	}
	otherBefore, _ := reopened.FindProject(ctx, b.ID)
	if err := reopened.DeleteProject(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	otherAfter, _ := reopened.FindProject(ctx, b.ID)
	if !reflect.DeepEqual(otherBefore, otherAfter) {
		t.Fatal("delete changed another project's organization")
	}
	var count int
	if err := reopened.database.QueryRowContext(ctx, `SELECT count(*) FROM project_organization WHERE project_id=?`, a.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("deleted project's metadata survived", err)
	}
}

func TestOrganizationRejectsStaleAndInvalidChangesAtomically(t *testing.T) {
	ctx := context.Background()
	db, _, _ := openTestStore(t)
	a, b := saveProject(t, db, "alpha"), saveProject(t, db, "beta")
	a = organize(t, db, a.ID, []string{"Media"}, "pin")
	b = organize(t, db, b.ID, nil, "pin")
	stale := a.OrganizationRevision
	organize(t, db, b.ID, nil, "up")
	before, err := db.ProjectRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.OrganizeProject(ctx, a.ID, stale, []string{"Wrong"}, "save"); !errors.Is(err, ErrOrganizationStale) {
		t.Fatalf("stale form accepted: %v", err)
	}
	for _, labels := range [][]string{{"bad/label"}, {"bad\nlabel"}, {"123456789012345678901234567890123"}} {
		current, _ := db.FindProject(ctx, a.ID)
		if err := db.OrganizeProject(ctx, a.ID, current.OrganizationRevision, labels, "save"); !errors.Is(err, ErrOrganizationInvalid) {
			t.Fatalf("invalid labels accepted: %v", err)
		}
	}
	after, err := db.ProjectRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("failed organization write changed metadata or deployment")
	}
}

func TestOrganizationBackupRestoreAndSnapshotConflict(t *testing.T) {
	ctx := context.Background()
	db, _, _ := openTestStore(t)
	a, b := saveProject(t, db, "alpha"), saveProject(t, db, "beta")
	organize(t, db, a.ID, []string{"Media"}, "pin")
	organize(t, db, b.ID, []string{"Home"}, "pin")
	organize(t, db, b.ID, []string{"Home"}, "up")
	records, err := db.ProjectRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	target, _, _ := openTestStore(t)
	for _, record := range records {
		if _, err := target.RestoreProject(ctx, record, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := orderedPins(t, target); !reflect.DeepEqual(got, []string{"beta", "alpha"}) {
		t.Fatal("backup lost pin order", got)
	}
	copy, err := target.FindProject(ctx, "alpha")
	if err != nil || !reflect.DeepEqual(copy.Labels, []string{"Media"}) {
		t.Fatal("backup lost labels", err)
	}
	organize(t, db, a.ID, []string{"Changed"}, "save")
	if _, err := db.RestoreRecords(ctx, records, records, nil, true, ""); err == nil {
		t.Fatal("restore ignored concurrent organization change")
	}
	current, _ := db.FindProject(ctx, a.ID)
	if !reflect.DeepEqual(current.Labels, []string{"Changed"}) {
		t.Fatal("failed restore mutated labels")
	}
	// Legacy archives carry no metadata and remain restorable.
	legacy := ProjectRecord{Project: Project{Name: "legacy", Slug: "legacy", Status: StatusGenerated}}
	if _, err := target.RestoreProject(ctx, legacy, false); err != nil {
		t.Fatal(err)
	}
}

func TestOrganizationMigrationPreservesExistingProjects(t *testing.T) {
	ctx := context.Background()
	db, path, key := openTestStore(t)
	a := saveProject(t, db, "alpha")
	if _, err := db.database.ExecContext(ctx, `DROP TABLE project_organization`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.database.ExecContext(ctx, `DELETE FROM schema_version WHERE version=6`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, path, filepath.Join(filepath.Dir(path), "upgrade-backups"), secrets.NewSealer(key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	current, err := reopened.FindProject(ctx, a.ID)
	if err != nil || current.ID != a.ID || len(current.Labels) != 0 || current.PinOrder != 0 {
		t.Fatal("metadata migration changed existing project", err)
	}
	backups, err := filepath.Glob(filepath.Join(filepath.Dir(path), "upgrade-backups", "*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatal("upgrade did not retain its database backup", err)
	}
	organize(t, reopened, a.ID, []string{"Media"}, "pin")
}

func TestOrganizationTargetsAnExactProjectID(t *testing.T) {
	ctx := context.Background()
	db, _, _ := openTestStore(t)
	a := saveProject(t, db, "alpha")
	a = organize(t, db, a.ID, []string{"Media"}, "pin")
	// Name lookups can resolve another project, but this command takes a primary key.
	collision := saveProject(t, db, "beta")
	if _, err := db.database.ExecContext(ctx, `UPDATE projects SET name=?,slug=? WHERE id=?`, a.ID, a.ID, collision.ID); err != nil {
		t.Fatal(err)
	}
	collision = organize(t, db, collision.ID, []string{"Home"}, "save")
	if err := db.OrganizeProject(ctx, a.ID, a.OrganizationRevision, []string{"Changed"}, "save"); err != nil {
		t.Fatal("metadata command did not use the exact ID", err)
	}
	target, err := scanProject(db.database.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id=?`, collision.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := readOrganization(ctx, db.database, &target); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(target.Labels, collision.Labels) || target.OrganizationRevision != collision.OrganizationRevision {
		t.Fatal("name/id collision modified another project")
	}
}
