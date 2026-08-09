-- Immutable release snapshots and their per-locale materialised bundles.
--
-- Every merge creates a release and materialises all six bundles inside the
-- same transaction. That is what makes the read side trivial: the export
-- endpoint (engineers) and the OTA endpoint (end users) are both read-only
-- handlers over these rows, so they cannot disagree with each other.

CREATE TABLE IF NOT EXISTS releases (
    id               BIGSERIAL PRIMARY KEY,
    -- Monotonic, human-facing release number.
    version          BIGINT NOT NULL,
    source           TEXT   NOT NULL,
    -- NULL when the release came from a manual publish or an import.
    merge_request_id BIGINT REFERENCES merge_requests (id),
    notes            TEXT   NOT NULL DEFAULT '',
    created_by       CITEXT NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- OTA safety valve: a semver floor. Copy that references a feature shipped
    -- in 4.12 can be withheld from a 4.09 client rather than shipping nonsense.
    -- NULL means every client is eligible.
    min_app_version  TEXT,

    -- OTA kill switch. Setting this removes the release from serving; clients
    -- receive 410 and fall back to the strings bundled in the app binary.
    rolled_back_at   TIMESTAMPTZ,
    rolled_back_by   CITEXT,

    CONSTRAINT releases_version_unique UNIQUE (version),
    CONSTRAINT releases_source_check CHECK (source IN ('merge', 'publish', 'import')),
    CONSTRAINT releases_version_positive_check CHECK (version > 0),
    -- Either both rollback columns are set or neither is.
    CONSTRAINT releases_rollback_consistency_check CHECK (
        (rolled_back_at IS NULL AND rolled_back_by IS NULL)
        OR (rolled_back_at IS NOT NULL AND rolled_back_by IS NOT NULL)
    )
);

-- Serving path: newest eligible, not-rolled-back release.
CREATE INDEX IF NOT EXISTS idx_releases_servable
    ON releases (version DESC) WHERE rolled_back_at IS NULL;

-- ---------------------------------------------------------------------------
-- release_bundles
--
-- One row per (release, locale). `strings` is the flat key -> value map for the
-- flutter platform with empty values included — structurally identical to
-- assets/langs/<locale>.json in the mobile repo, so the OTA payload and the
-- bundled asset cannot drift apart.
--
-- sha256 is the content fingerprint. It doubles as the HTTP ETag: clients send
-- it back in If-None-Match and receive 304 with no body when it still matches.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS release_bundles (
    release_id BIGINT   NOT NULL REFERENCES releases (id) ON DELETE CASCADE,
    locale_id  SMALLINT NOT NULL REFERENCES locales (id),
    strings    JSONB    NOT NULL,
    sha256     TEXT     NOT NULL,
    key_count  INT      NOT NULL,
    byte_size  INT      NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT release_bundles_pkey PRIMARY KEY (release_id, locale_id),
    CONSTRAINT release_bundles_sha256_check CHECK (sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT release_bundles_counts_check CHECK (key_count >= 0 AND byte_size >= 0)
);
