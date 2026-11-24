-- Which English each language becomes (internal/plugins/dragoman/dialects.go).
-- guide and previous are written by a model and never hold what a member
-- wrote; version guards every write of one.
CREATE TABLE dragoman_dialects (
    guild_id    TEXT    NOT NULL,
    language    TEXT    NOT NULL,
    name        TEXT    NOT NULL,
    guide       JSONB,
    previous    JSONB,
    version     INTEGER NOT NULL DEFAULT 0,
    uses        INTEGER NOT NULL DEFAULT 0,
    missed      INTEGER NOT NULL DEFAULT 0,
    losses      INTEGER NOT NULL DEFAULT 0,
    next_review INTEGER NOT NULL DEFAULT 0,
    notes       TEXT[]  NOT NULL DEFAULT '{}',
    problem     TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (guild_id, language)
);

-- The dialects script this replaces was Dutch into AAVE and nothing else. A
-- guild that had it on keeps it; its guide comes written (guide.go).
INSERT INTO dragoman_dialects (guild_id, language, name)
    SELECT guild_id, 'nl', 'African American Vernacular English' FROM scripts_enabled WHERE script = 'dialects';
DELETE FROM scripts_enabled WHERE script = 'dialects';
UPDATE settings_permission_overrides SET action = 'dragoman.dialects' WHERE action = 'dragoman.scripts';
