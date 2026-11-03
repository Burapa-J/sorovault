ALTER TABLE contracts ADD COLUMN deprecated_since TEXT;
ALTER TABLE contracts ADD COLUMN successor TEXT NOT NULL DEFAULT '';
ALTER TABLE contracts ADD COLUMN deprecation_note TEXT NOT NULL DEFAULT '';
