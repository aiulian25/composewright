-- Each generation keeps the number it was shown with (v1, v2...), so
-- pruning old ones never renumbers the rest.
ALTER TABLE generations ADD COLUMN number INTEGER NOT NULL DEFAULT 0;
UPDATE generations SET number = (
  SELECT COUNT(*) FROM generations AS earlier
  WHERE earlier.project_id = generations.project_id
    AND (earlier.created_at < generations.created_at
      OR (earlier.created_at = generations.created_at AND earlier.rowid <= generations.rowid))
);
