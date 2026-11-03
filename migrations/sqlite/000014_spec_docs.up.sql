-- Doc comments of every function and user-defined type joined with newlines.
ALTER TABLE specs ADD COLUMN docs_tsv TEXT NOT NULL DEFAULT '';
