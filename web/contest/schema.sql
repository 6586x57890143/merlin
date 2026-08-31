-- D1 schema for the contest Worker.
--
-- Two tables, because merlin owns the schema and this side is storage. The
-- snapshot column holds the contest exactly as merlin pushed it, serialized
-- once and served back verbatim, so adding a field to a contest never means
-- a migration here.
--
-- There is no user id anywhere in this file. voter_hash is an HMAC merlin
-- computed under a key that never leaves the VPS, so this database cannot be
-- turned back into a list of who voted for what even by whoever holds it.

CREATE TABLE IF NOT EXISTS contests (
  slug       TEXT PRIMARY KEY,
  snapshot   TEXT NOT NULL,
  phase      TEXT NOT NULL,
  closes_at  INTEGER NOT NULL DEFAULT 0,
  frozen     INTEGER NOT NULL DEFAULT 0,
  updated_at INTEGER NOT NULL DEFAULT 0
);

-- One row per pick. The composite primary key is what makes a replayed
-- token harmless: re-sending the same ballot writes the same rows.
CREATE TABLE IF NOT EXISTS votes (
  slug       TEXT NOT NULL,
  voter_hash TEXT NOT NULL,
  entry_id   TEXT NOT NULL,
  PRIMARY KEY (slug, voter_hash, entry_id)
);

CREATE INDEX IF NOT EXISTS votes_slug_entry_idx ON votes (slug, entry_id);
