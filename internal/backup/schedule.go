package backup

import (
	"time"

	"github.com/aiulian25/composewright/internal/config"
)

const weekDays = 7

// Next is when the next automatic backup is due: at once when there is none
// yet, then one interval after the last; zero when the schedule is off.
func Next(schedule config.Backup, last, now time.Time) time.Time {
	var next time.Time
	switch schedule.Schedule {
	case config.BackupWeekly:
		next = last.AddDate(0, 0, weekDays)
	case config.BackupMonthly:
		next = last.AddDate(0, 1, 0)
	case config.BackupCustom:
		next = last.AddDate(0, 0, schedule.EveryDays)
	default:
		return time.Time{}
	}
	if last.IsZero() {
		return now
	}
	return next
}

// Due reports whether an automatic backup should be made now.
func Due(schedule config.Backup, last, now time.Time) bool {
	next := Next(schedule, last, now)
	return !next.IsZero() && !now.Before(next)
}
