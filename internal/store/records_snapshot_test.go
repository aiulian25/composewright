package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/aiulian25/composewright/internal/secrets"
)

func TestBackupSnapshotSurvivesConcurrentCommit(t *testing.T) {
	database, path, key := openTestStore(t)
	ctx := context.Background()
	project := saveProject(t, database, "snapshot")
	first, _, err := database.RecordGeneration(ctx, project, Generation{IRJSON: "{}", ComposeYAML: "old"}, nil, []EnvVar{{Key: "VALUE", Value: "old"}}, "test", func(Project, Generation) []Event { return nil })
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, path, filepath.Join(t.TempDir(), "backups"), secrets.NewSealer(key))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	err = database.transaction(ctx, func(tx *sql.Tx) error {
		view := database.view(tx)
		if _, err := view.ListProjects(ctx); err != nil {
			return err
		} // establish the snapshot
		if _, _, err := other.RecordGeneration(ctx, first, Generation{IRJSON: "{}", ComposeYAML: "new"}, nil, []EnvVar{{Key: "VALUE", Value: "new"}}, "test", func(Project, Generation) []Event { return nil }); err != nil {
			return err
		}
		records, err := view.projectRecords(ctx)
		if err != nil {
			return err
		}
		if records[0].Generations[0].Generation.ComposeYAML != "old" || records[0].Variables[0].Value != "old" {
			t.Fatal("snapshot mixed generations and values")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	records, err := database.ProjectRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if records[0].Generations[0].Generation.ComposeYAML != "new" || records[0].Variables[0].Value != "new" {
		t.Fatal("later snapshot did not see commit")
	}
}

func TestRestoreBatchRollsBackAndCheckpoints(t *testing.T) {
	source, _, _ := openTestStore(t)
	target, _, _ := openTestStore(t)
	ctx := context.Background()
	for _, name := range []string{"first", "second"} {
		p := saveProject(t, source, name)
		if _, _, err := source.RecordGeneration(ctx, p, Generation{IRJSON: "{}"}, nil, []EnvVar{{Key: "VALUE", Value: name}}, "test", func(Project, Generation) []Event { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	records, err := source.ProjectRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bad := append([]ProjectRecord(nil), records...)
	bad[1].Variables = append(append([]EnvVar(nil), bad[1].Variables...), bad[1].Variables[0])
	if _, err := target.RestoreRecords(ctx, bad, nil, map[string]string{"provider": "new"}, false, "checkpoint"); err == nil {
		t.Fatal("bad batch committed")
	}
	all, err := target.ProjectRecords(ctx)
	if err != nil || len(all) != 0 {
		t.Fatal("partial project batch")
	}
	if _, found, _ := target.Secret(ctx, "provider"); found {
		t.Fatal("partial credential batch")
	}
	if _, found, _ := target.Secret(ctx, "checkpoint"); found {
		t.Fatal("checkpoint for failed batch")
	}
	progress, err := target.RestoreRecords(ctx, records, nil, map[string]string{"provider": "new"}, false, "checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	text, found, err := target.Secret(ctx, "checkpoint")
	if err != nil || !found {
		t.Fatal("missing committed checkpoint")
	}
	var checkpoint RestoreProgress
	if err = json.Unmarshal([]byte(text), &checkpoint); err != nil || len(checkpoint.Restored) != len(progress.Restored) {
		t.Fatal("checkpoint differs")
	}
	// A later mutation must not be overwritten using the old recovery snapshot.
	expected, err := target.ProjectRecords(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = target.ReplaceEnvVars(ctx, expected[0].Project.ID, []EnvVar{{Key: "VALUE", Value: "later"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = target.RestoreRecords(ctx, records, expected, nil, true, "checkpoint"); err == nil {
		t.Fatal("concurrent edit overwritten")
	}
}

func TestBackupRecordsStayCoherentWhileWriting(t *testing.T) {
	database, path, key := openTestStore(t)
	ctx := context.Background()
	project := saveProject(t, database, "concurrent")
	events := func(Project, Generation) []Event { return nil }
	if _, _, err := database.RecordGeneration(ctx, project, Generation{IRJSON: "{}", ComposeYAML: "old"}, nil, []EnvVar{{Key: "VALUE", Value: "old"}}, "test", events); err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, path, filepath.Join(t.TempDir(), "backups"), secrets.NewSealer(key))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 80; i++ {
			value := "a"
			if i%2 != 0 {
				value = "b"
			}
			if _, _, err := other.RecordGeneration(ctx, project, Generation{IRJSON: "{}", ComposeYAML: value}, nil, []EnvVar{{Key: "VALUE", Value: value}}, "test", events); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	var mixed bool
	for i := 0; i < 80; i++ {
		records, err := database.ProjectRecords(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if records[0].Generations[0].Generation.ComposeYAML != records[0].Variables[0].Value {
			mixed = true
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if mixed {
		t.Fatal("backup mixed a generation with another revision's values")
	}
}
