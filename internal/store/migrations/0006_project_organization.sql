CREATE TABLE project_organization (
  project_id TEXT PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
  labels_json TEXT NOT NULL DEFAULT '[]',
  pin_order INTEGER NOT NULL DEFAULT 0 CHECK (pin_order >= 0),
  revision INTEGER NOT NULL DEFAULT 0 CHECK (revision >= 0)
);
CREATE INDEX project_pin_order ON project_organization(pin_order);
