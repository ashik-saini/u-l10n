# u-l10n — how it fits together

The machinery view, one sequence per flow. For the persona journeys — translator, reviewer, mobile app, operator — see [USER_FLOWS.md](USER_FLOWS.md).

Layering rule behind every diagram: `route/` decodes and maps errors to status codes → `pkg/service/` decides and owns the transaction → `pkg/repository/` speaks SQL → PostgreSQL. Repositories never open their own transaction; values always travel as `(value, found)` — absent and empty are different states.

## 1. Portal write — saving a translation

```mermaid
sequenceDiagram
    autonumber
    participant SPA as Portal SPA
    participant R as route/
    participant G as googleauth
    participant K as keysvc
    participant T as translation repo
    participant PG as PostgreSQL

    SPA->>R: PUT translation (value, base_version)
    R->>G: verify Google token
    G-->>R: email → role (users table)
    R->>K: SetTranslation
    K->>T: upsert with version predicate
    T->>PG: INSERT … ON CONFLICT DO UPDATE WHERE version = base
    alt version still matches
        PG-->>SPA: 200 — new version
    else someone saved first
        K->>T: re-read current cell
        T-->>K: theirs (value, found, version)
        K-->>SPA: 409 — mine AND theirs, side by side
    end
```

## 2. Authenticated export — CI pulls the three formats

```mermaid
sequenceDiagram
    autonumber
    participant CI as CI pipeline
    participant R as route/export
    participant A as apitoken repo
    participant E as exportsvc
    participant F as pkg/export
    participant PG as PostgreSQL

    CI->>R: GET /export (Bearer token)
    R->>A: SHA-256(token) lookup
    A->>PG: match live token · throttled last_used_at
    A-->>R: scope ok
    R->>E: Zip(locales)
    E->>PG: keys + translations (master)
    E->>F: serialize — Flutter JSON · Android XML · iOS .strings
    Note over F: independent from pkg/parse —<br/>that independence makes gate R1 real
    F-->>E: byte-faithful files
    E-->>CI: zip (collision check failed the WHOLE export first, if any)
```

## 3. Asset upload — a context screenshot reaches S3

```mermaid
sequenceDiagram
    autonumber
    participant SPA as Portal SPA
    participant AS as assetsvc
    participant S3 as S3 (storage/v4)
    participant PG as PostgreSQL

    SPA->>AS: Presign (filename, type, size)
    AS-->>SPA: POST policy (metadata pinned)
    SPA->>S3: upload bytes directly
    SPA->>AS: Confirm
    AS->>S3: read object back
    AS->>AS: sha256(bytes) must equal its content-address
    alt bytes match their name
        AS->>PG: asset row + audit
        AS-->>SPA: attached
    else mismatch / absent / wrong size
        AS-->>SPA: refused — object never becomes an asset
    end
```

## 4. The merge transaction — seven steps, one lock

```mermaid
sequenceDiagram
    autonumber
    participant MR as mrsvc
    participant M as mergesvc
    participant PG as PostgreSQL

    MR->>M: Merge(branch)
    M->>PG: BEGIN
    M->>PG: pg_advisory_xact_lock — merges serialize globally
    M->>PG: FOR UPDATE on affected translations AND keys rows (ordered)
    M->>PG: compute conflicts (master version ≠ base_master_version)
    alt unresolved conflicts
        M-->>MR: refuse — 409, nothing applied
    else clean or resolved
        M->>PG: apply deltas via version-predicated CTEs
        alt master moved inside the window
            M-->>MR: ErrConcurrentMasterWrite — rollback, 409 retry
        else all deltas land
            M->>PG: translation_history + key_history (source 'merge')
            M->>PG: cut release · materialise bundle (sha over served bytes)
            M->>PG: COMMIT — lock releases
            M-->>MR: merged · release version
        end
    end
```

## 5. Import — reconciling from Lokalise

```mermaid
sequenceDiagram
    autonumber
    participant OP as Operator CLI
    participant I as importsvc
    participant L as Lokalise API
    participant P as pkg/parse
    participant PG as PostgreSQL

    OP->>I: u-l10n import (--dry-run first)
    loop paged fetch, rate-limited
        I->>L: keys + translations
        L-->>I: page (429 → honor Retry-After)
    end
    I->>P: file exports as presence oracle<br/>(API returns "" for blank AND untranslated)
    I->>I: canonical names — colliding keys dedupe last-wins, warned
    I->>PG: one transaction — batch upserts
    I-->>OP: report: changed · warnings · (or dry-run diff)
```
