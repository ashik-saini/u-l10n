-- Workflow tags and the append-only audit trail.

-- ---------------------------------------------------------------------------
-- tags
--
-- Deliberately GLOBAL per key, not branch-scoped. Tags are workflow metadata,
-- not translatable content, so keeping them out of branch scope keeps them out
-- of conflict computation and out of the merge transaction entirely. Every
-- dimension excluded from the merge is a class of conflict nobody has to
-- resolve.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tags (
    id         SMALLSERIAL PRIMARY KEY,
    name       TEXT NOT NULL,
    -- Lokalise does not expose tag colours through its API; these are captured
    -- manually from the UI at import.
    colour     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT tags_name_unique UNIQUE (name)
);

CREATE TABLE IF NOT EXISTS key_tags (
    key_id     BIGINT   NOT NULL REFERENCES keys (id) ON DELETE CASCADE,
    tag_id     SMALLINT NOT NULL REFERENCES tags (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT key_tags_pkey PRIMARY KEY (key_id, tag_id)
);

CREATE INDEX IF NOT EXISTS idx_key_tags_tag_id ON key_tags (tag_id);

-- ---------------------------------------------------------------------------
-- History
--
-- Insert-only. A rollback is a NEW FORWARD WRITE, not a delete or an update:
-- the timeline reads v1 -> v2 -> v3 -> v2', where v2' is a fresh row whose
-- value equals v2's.
--
-- These tables answer "who changed the customer-facing text that caused the
-- complaint". A log that can be edited answers nothing, and in a regulated
-- environment the immutability is a requirement rather than a preference.
--
-- branch_id is deliberately NOT a foreign key. An audit record must survive
-- whatever happens to the thing it describes; a constraint that could block a
-- write or null a column is the wrong tool on an append-only table.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS translation_history (
    id          BIGSERIAL PRIMARY KEY,
    key_id      BIGINT   NOT NULL REFERENCES keys (id) ON DELETE CASCADE,
    locale_id   SMALLINT NOT NULL REFERENCES locales (id),
    -- NULL distinguishes "became untranslated" from "became empty string".
    value       TEXT,
    render_hint TEXT     NOT NULL DEFAULT 'plain',
    version     INT      NOT NULL,
    source      TEXT     NOT NULL,
    -- NULL for master; set when the change was recorded against a branch.
    branch_id   BIGINT,
    changed_by  CITEXT   NOT NULL,
    changed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT translation_history_source_check
        CHECK (source IN ('ui', 'merge', 'import', 'rollback', 'api')),
    CONSTRAINT translation_history_render_hint_check
        CHECK (render_hint IN ('plain', 'cdata'))
);

CREATE INDEX IF NOT EXISTS idx_translation_history_key_locale
    ON translation_history (key_id, locale_id, changed_at DESC);

CREATE TABLE IF NOT EXISTS key_history (
    id           BIGSERIAL PRIMARY KEY,
    key_id       BIGINT NOT NULL REFERENCES keys (id) ON DELETE CASCADE,
    name         TEXT   NOT NULL,
    description  TEXT   NOT NULL DEFAULT '',
    platforms    TEXT[] NOT NULL,
    android_name TEXT,
    ios_name     TEXT,
    status       TEXT   NOT NULL,
    version      INT    NOT NULL,
    source       TEXT   NOT NULL,
    branch_id    BIGINT,
    changed_by   CITEXT NOT NULL,
    changed_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT key_history_source_check
        CHECK (source IN ('ui', 'merge', 'import', 'rollback', 'api')),
    CONSTRAINT key_history_status_check
        CHECK (status IN ('active', 'deleted', 'draft'))
);

CREATE INDEX IF NOT EXISTS idx_key_history_key_id
    ON key_history (key_id, changed_at DESC);
