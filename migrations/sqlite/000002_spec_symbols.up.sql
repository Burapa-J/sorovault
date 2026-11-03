-- Searchable symbol names for each stored interface: every function,
-- user-defined type and event name, newline-separated and sorted.
ALTER TABLE specs ADD COLUMN symbols TEXT NOT NULL DEFAULT '';
