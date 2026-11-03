-- Standard IDs detected from the current decoded contract interface.
ALTER TABLE contracts ADD COLUMN implements TEXT NOT NULL DEFAULT '[]';
