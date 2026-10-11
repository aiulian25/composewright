CREATE TABLE projects (
  id            TEXT PRIMARY KEY,
  name          TEXT UNIQUE NOT NULL,
  slug          TEXT UNIQUE NOT NULL,
  source_kind   TEXT NOT NULL,
  source_url    TEXT NOT NULL DEFAULT '',
  source_ref    TEXT NOT NULL DEFAULT '',
  source_commit TEXT NOT NULL DEFAULT '',
  source_owner  TEXT NOT NULL DEFAULT '',
  status        TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE generations (
  id                      TEXT PRIMARY KEY,
  project_id              TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  iteration               INTEGER NOT NULL,
  engine                  TEXT NOT NULL,
  provider                TEXT NOT NULL DEFAULT '',
  model                   TEXT NOT NULL DEFAULT '',
  prompt_hash             TEXT NOT NULL DEFAULT '',
  ir_json                 TEXT NOT NULL,
  compose_yaml            TEXT NOT NULL DEFAULT '',
  deploy_md               TEXT NOT NULL DEFAULT '',
  options_json            TEXT NOT NULL DEFAULT '{}',
  policy_verdict_json     TEXT NOT NULL DEFAULT '',
  validation_verdict_json TEXT NOT NULL DEFAULT '',
  logs_path               TEXT NOT NULL DEFAULT '',
  created_at              INTEGER NOT NULL
);
CREATE INDEX generations_by_project ON generations(project_id, created_at);

CREATE TABLE env_vars (
  id          TEXT PRIMARY KEY,
  project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  key         TEXT NOT NULL,
  is_secret   INTEGER NOT NULL,
  value_plain TEXT,
  value_ct    BLOB,
  nonce       BLOB,
  key_id      TEXT,
  source      TEXT NOT NULL DEFAULT '',
  source_ref  TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  required    INTEGER NOT NULL DEFAULT 0,
  UNIQUE(project_id, key),
  CHECK ((is_secret = 1 AND value_plain IS NULL AND value_ct IS NOT NULL) OR (is_secret = 0 AND value_ct IS NULL))
);

CREATE TABLE images (
  id            TEXT PRIMARY KEY,
  generation_id TEXT NOT NULL REFERENCES generations(id) ON DELETE CASCADE,
  service       TEXT NOT NULL,
  reference     TEXT NOT NULL,
  digest        TEXT NOT NULL DEFAULT '',
  registry      TEXT NOT NULL DEFAULT '',
  oci_source    TEXT NOT NULL DEFAULT '',
  verified_at   INTEGER NOT NULL DEFAULT 0,
  scan_json     TEXT NOT NULL DEFAULT ''
);

CREATE TABLE image_cache (
  reference     TEXT PRIMARY KEY,
  metadata_json TEXT NOT NULL,
  fetched_at    INTEGER NOT NULL
);

CREATE TABLE settings (
  key         TEXT PRIMARY KEY,
  value_plain TEXT,
  value_ct    BLOB,
  nonce       BLOB,
  key_id      TEXT
);

CREATE TABLE keys (
  key_id     TEXT PRIMARY KEY,
  created_at INTEGER NOT NULL,
  retired_at INTEGER
);

CREATE TABLE audit_log (
  seq          INTEGER PRIMARY KEY,
  ts           INTEGER NOT NULL,
  actor        TEXT NOT NULL,
  action       TEXT NOT NULL,
  project_id   TEXT NOT NULL DEFAULT '',
  details_json TEXT NOT NULL,
  prev_hash    TEXT NOT NULL,
  hash         TEXT NOT NULL
);
