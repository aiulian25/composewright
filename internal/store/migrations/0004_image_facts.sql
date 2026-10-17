-- What the registry said about each image when its version was made: the
-- OCI labels as JSON and when the image was built.
ALTER TABLE images ADD COLUMN labels_json TEXT NOT NULL DEFAULT '';
ALTER TABLE images ADD COLUMN created INTEGER NOT NULL DEFAULT 0;
