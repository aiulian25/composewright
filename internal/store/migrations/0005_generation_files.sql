-- The files a version bundles beside compose.yaml, such as a config file a
-- bind mount needs, each sealed with the master key.
CREATE TABLE generation_files (
  id            TEXT PRIMARY KEY,
  generation_id TEXT NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
  path          TEXT NOT NULL,
  content_ct    BLOB NOT NULL,
  nonce         BLOB NOT NULL,
  key_id        TEXT NOT NULL,
  UNIQUE(generation_id, path)
);
