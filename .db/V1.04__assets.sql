-- Context screenshots for translators.
--
-- Portal-only: these never reach an export and never enter an OTA bundle.
--
-- SECURITY: these are screenshots of a fintech app and routinely contain
-- customer names, balances, card numbers and transaction history. The backing
-- bucket is private, with no public CDN and no public-read ACL; reads are
-- short-lived presigned URLs issued only to an authenticated portal identity.

CREATE TABLE IF NOT EXISTS assets (
    id           BIGSERIAL PRIMARY KEY,
    -- Content-addressed: screenshots/<aa>/<bb>/<sha256>.<ext>
    s3_key       TEXT   NOT NULL,
    -- Deduplication key. Re-uploading identical bytes reuses this row and
    -- transfers nothing, and it makes an asset immutable by construction: the
    -- name IS the content.
    sha256       TEXT   NOT NULL,
    filename     TEXT   NOT NULL,
    content_type TEXT   NOT NULL,
    bytes        INT    NOT NULL,
    width        INT,
    height       INT,
    uploaded_by  CITEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT assets_s3_key_unique UNIQUE (s3_key),
    CONSTRAINT assets_sha256_unique UNIQUE (sha256),
    CONSTRAINT assets_sha256_format_check CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    -- Last line of defence. The browser checks these for fast feedback and the
    -- handler checks them against untrusted input, but only the database still
    -- holds when someone runs a script or curls the API directly.
    CONSTRAINT assets_content_type_check
        CHECK (content_type IN ('image/png', 'image/jpeg', 'image/webp')),
    CONSTRAINT assets_size_check CHECK (bytes > 0 AND bytes <= 10485760),
    CONSTRAINT assets_dimensions_check CHECK (
        (width IS NULL OR width > 0) AND (height IS NULL OR height > 0)
    )
);

-- ---------------------------------------------------------------------------
-- key_assets
--
-- Many-to-many by design: one screenshot of a screen provides context for
-- every string on that screen.
--
-- GLOBAL per key, not branch-scoped — the same call as tags. A screenshot is
-- authoring context, not translatable content, so it never enters conflict
-- computation or the merge transaction.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS key_assets (
    key_id     BIGINT   NOT NULL REFERENCES keys (id) ON DELETE CASCADE,
    asset_id   BIGINT   NOT NULL REFERENCES assets (id) ON DELETE CASCADE,
    -- Free-text guidance, e.g. "truncates past 18 characters".
    note       TEXT     NOT NULL DEFAULT '',
    sort_order SMALLINT NOT NULL DEFAULT 0,
    created_by CITEXT   NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT key_assets_pkey PRIMARY KEY (key_id, asset_id)
);

CREATE INDEX IF NOT EXISTS idx_key_assets_asset_id ON key_assets (asset_id);
