package backup

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aiulian25/composewright/internal/config"
)

// TestFolderKeepsBackupsByKindAndTime: backups are listed newest first
// whatever their kind, only automatic ones are pruned, and a name that is
// not a stored backup's reaches no file.
func TestFolderKeepsBackupsByKindAndTime(t *testing.T) {
	folder := Folder(filepath.Join(t.TempDir(), "backups"))
	start := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	for day, kind := range []string{KindAutomatic, KindManual, KindAutomatic, KindAutomatic} {
		if _, err := folder.Write(kind, start.AddDate(0, 0, day), []byte("sealed")); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(string(folder), "composewright-v2-20261001T030000Z.db"), []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	stored, err := folder.List()
	if err != nil || len(stored) != 4 || stored[0].Created.Day() != 4 || stored[2].Kind != KindManual {
		t.Fatalf("listed %+v, %v", stored, err)
	}
	if last := LastAutomatic(stored); last.Day() != 4 {
		t.Fatalf("last automatic %v", last)
	}
	info, err := os.Stat(filepath.Join(string(folder), stored[0].Name))
	if err != nil || info.Mode().Perm() != fileMode {
		t.Fatalf("a backup file is not private: %v %v", info.Mode(), err)
	}
	pruned, err := folder.Prune(2)
	if err != nil || len(pruned) != 1 || pruned[0] != "composewright-auto-20261001-030000.cwbackup" {
		t.Fatalf("pruned %v, %v", pruned, err)
	}
	if left, _ := folder.List(); len(left) != 3 || !slices.ContainsFunc(left, func(item Stored) bool { return item.Kind == KindManual }) {
		t.Fatalf("left %+v", left)
	}
	for _, name := range []string{"../config.yaml", "composewright-v2-20261001T030000Z.db", "composewright-auto-20261001-030000.cwbackup"} {
		if _, err := folder.Read(name); !errors.Is(err, ErrNoSuchBackup) {
			t.Errorf("read %q: %v", name, err)
		}
	}
}

// TestSchedule: off never runs, the first backup is due at once, and the
// next one week, one month or the custom number of days after the last.
func TestSchedule(t *testing.T) {
	last := time.Date(2026, 1, 31, 3, 0, 0, 0, time.UTC)
	cases := []struct {
		schedule config.Backup
		last     time.Time
		next     time.Time
	}{
		{config.Backup{Schedule: config.BackupOff}, last, time.Time{}},
		{config.Backup{Schedule: config.BackupWeekly}, time.Time{}, last},
		{config.Backup{Schedule: config.BackupWeekly}, last, last.AddDate(0, 0, 7)},
		{config.Backup{Schedule: config.BackupMonthly}, last, time.Date(2026, 3, 3, 3, 0, 0, 0, time.UTC)},
		{config.Backup{Schedule: config.BackupCustom, EveryDays: 3}, last, last.AddDate(0, 0, 3)},
	}
	for _, testCase := range cases {
		if next := Next(testCase.schedule, testCase.last, last); !next.Equal(testCase.next) {
			t.Errorf("%+v after %v: next %v, want %v", testCase.schedule, testCase.last, next, testCase.next)
		}
	}
	weekly := config.Backup{Schedule: config.BackupWeekly}
	if Due(weekly, last, last.AddDate(0, 0, 6)) || !Due(weekly, last, last.AddDate(0, 0, 7)) || Due(config.Backup{Schedule: config.BackupOff}, time.Time{}, last) {
		t.Fatal("a weekly backup is due at the wrong time, or an off schedule runs")
	}
}

// TestResealMovesABackupToANewPassphrase: after a passphrase change a stored
// backup opens with the new passphrase and no longer with the old one.
func TestResealMovesABackupToANewPassphrase(t *testing.T) {
	sealed, err := Seal(sampleArchive(), testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	const newPassphrase = "another long passphrase"
	resealed, err := Reseal(sealed, testPassphrase, newPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(resealed, newPassphrase); err != nil {
		t.Fatalf("new passphrase: %v", err)
	}
	if _, err := Open(resealed, testPassphrase); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("old passphrase: %v", err)
	}
}
