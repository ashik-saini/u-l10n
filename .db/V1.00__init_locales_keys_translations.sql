-- Foundational schema for u-l10n: the locale dimension, the key entity, and
-- the key x locale translation matrix.
--
-- Status/enum-like columns are TEXT + CHECK rather than native PostgreSQL
-- ENUM types. Adding a value to a native enum requires ALTER TYPE, which
-- cannot run inside a transaction on older servers; a CHECK constraint is
-- dropped and recreated in an ordinary migration.

CREATE EXTENSION IF NOT EXISTS citext;

-- ---------------------------------------------------------------------------
-- locales
--
-- Six rows, seeded here as reference data. Each row owns its export naming, so
-- the serializers are table-driven and never hardcode a path. Adding a seventh
-- locale is an INSERT, not a code change.
--
-- The directory names are the names u-l10n must EXPORT under, taken from
-- u-mobile/scripts/l10n/run.sh. They are 1:1 with locales. The fan-out that
-- copies one exported directory into several repository directories (for
-- example values-en-rMY into both values-am/ and values-en-rMY/) is a property
-- of run.sh, not of this table.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS locales (
    id                 SMALLSERIAL PRIMARY KEY,
    code               TEXT     NOT NULL,
    flutter_dir        TEXT     NOT NULL,
    android_values_dir TEXT     NOT NULL,
    ios_lproj          TEXT     NOT NULL,
    sort_order         SMALLINT NOT NULL,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT locales_code_unique UNIQUE (code)
);

INSERT INTO locales (code, flutter_dir, android_values_dir, ios_lproj, sort_order) VALUES
    ('en-SG', 'en_SG', 'values',          'en-SG.lproj', 1),
    ('en-MY', 'en_MY', 'values-en-rMY',   'en-MY.lproj', 2),
    ('en-TH', 'en_TH', 'values-en-rTH',   'en-TH.lproj', 3),
    ('th-TH', 'th_TH', 'values-th-rTH',   'th-TH.lproj', 4),
    ('ms-MY', 'ms_MY', 'values-ms-rMY',   'ms-MY.lproj', 5),
    ('en-AU', 'en_AU', 'values-en-rAU',   'en-AU.lproj', 6)
ON CONFLICT (code) DO NOTHING;

-- ---------------------------------------------------------------------------
-- keys
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS keys (
    id              BIGSERIAL PRIMARY KEY,
    name            TEXT   NOT NULL,
    description     TEXT   NOT NULL DEFAULT '',
    platforms       TEXT[] NOT NULL,
    -- NULL means "derive from name". A non-NULL value means "we deliberately
    -- overrode the derivation" — a distinction that cannot be recovered later
    -- if the derived value is materialised instead.
    android_name    TEXT,
    ios_name        TEXT,
    status          TEXT   NOT NULL DEFAULT 'active',
    -- Optimistic concurrency anchor for key metadata. See translations.version.
    version         INT    NOT NULL DEFAULT 1,
    -- Export order is data, not alphabetical: it must reproduce Lokalise's
    -- ordering or every export is a 6,300-line meaningless diff. Seeded from
    -- the Lokalise key id at import, with gaps so inserting between two keys
    -- is one UPDATE rather than a renumber.
    sort_index      BIGINT NOT NULL,
    lokalise_key_id BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT keys_status_check   CHECK (status IN ('active', 'deleted', 'draft')),
    CONSTRAINT keys_platforms_check CHECK (
        platforms <@ ARRAY['flutter', 'android', 'ios']::TEXT[]
        AND cardinality(platforms) > 0
    ),
    CONSTRAINT keys_version_check  CHECK (version > 0),
    CONSTRAINT keys_lokalise_key_id_unique UNIQUE (lokalise_key_id)
);

-- Names are unique only among ACTIVE keys. Without the predicate, soft-deleting
-- 'login_button' would block anyone from ever creating that name again.
CREATE UNIQUE INDEX IF NOT EXISTS idx_keys_name_active
    ON keys (name) WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_keys_sort_index
    ON keys (sort_index) WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_keys_platforms
    ON keys USING GIN (platforms);

-- ---------------------------------------------------------------------------
-- translations
--
-- THE critical invariant of this schema: a (key, locale) pair has THREE
-- states, not two.
--
--   no row          -> untranslated; the key is OMITTED from the export
--   value = ''      -> explicitly empty; exported as "key": ""
--   value = 'Hello' -> translated
--
-- Collapsing absent into empty adds ~430 spurious keys to en-SG; collapsing
-- empty into absent deletes 3,664 intentional blanks from ms-MY. The
-- repository layer must therefore never return a bare string — presence and
-- content are separate facts.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS translations (
    key_id      BIGINT   NOT NULL REFERENCES keys (id) ON DELETE CASCADE,
    locale_id   SMALLINT NOT NULL REFERENCES locales (id),
    value       TEXT     NOT NULL,
    -- Per-value render hint. Exactly 5 values are CDATA-wrapped in the current
    -- Android export; that is a property of the value, not of the key.
    render_hint TEXT     NOT NULL DEFAULT 'plain',
    -- Optimistic concurrency anchor. Writers issue
    --   UPDATE ... SET version = version + 1 WHERE ... AND version = $expected
    -- and treat zero affected rows as the conflict signal (HTTP 409).
    version     INT      NOT NULL DEFAULT 1,
    updated_by  CITEXT   NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT translations_pkey PRIMARY KEY (key_id, locale_id),
    CONSTRAINT translations_render_hint_check CHECK (render_hint IN ('plain', 'cdata')),
    CONSTRAINT translations_version_check CHECK (version > 0)
);

CREATE INDEX IF NOT EXISTS idx_translations_locale_id ON translations (locale_id);
