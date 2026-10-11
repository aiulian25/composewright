package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// CachedImage returns cached registry metadata for a reference.
func (store *Store) CachedImage(ctx context.Context, reference string) (string, time.Time, bool, error) {
	var metadata string
	var fetched int64
	err := store.database.QueryRowContext(ctx, `SELECT metadata_json, fetched_at FROM image_cache WHERE reference=?`, reference).Scan(&metadata, &fetched)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	return metadata, fromUnix(fetched), err == nil, err
}

// CacheImage stores registry metadata so repeated runs do not spend registry quota.
func (store *Store) CacheImage(ctx context.Context, reference, metadata string) error {
	_, err := store.database.ExecContext(ctx, `INSERT INTO image_cache (reference, metadata_json, fetched_at) VALUES (?, ?, ?)
		ON CONFLICT(reference) DO UPDATE SET metadata_json=excluded.metadata_json, fetched_at=excluded.fetched_at`,
		reference, metadata, unix(time.Now()))
	return err
}
