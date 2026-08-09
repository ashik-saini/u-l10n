# u-l10n — how it fits together

The machinery view. For the persona journeys — translator, reviewer, mobile app, operator — see [USER_FLOWS.md](USER_FLOWS.md).

## 1. Architecture & request flow

```mermaid
flowchart TB
    subgraph clients["Clients"]
        portal["Translator portal SPA"]
        apps["Mobile apps (Flutter / Android / iOS)"]
        ci["CI / u-mobile build"]
        operator["Operator CLI"]
    end

    subgraph edge["route/  — chi router :8080"]
        mw["captureSocketAddr → RealIP → per-IP rate limit<br/>(rightmost public XFF hop, /api/v1 + /ota/v1)"]
        api["/api/v1 portal handlers<br/>keys · translations · branches · MRs · tags · assets · releases · users"]
        ota["/ota/v1/bundles/:locale<br/>ETag / 304 · kill switch · min_app_version floor"]
        exp["/export  — zip of all 3 formats"]
        health["healthz / readyz (rate-limit exempt)"]
    end

    subgraph auth["Identity"]
        google["Google tokeninfo verifier<br/>(pkg/googleauth + users table roles)"]
        token["API tokens<br/>crypto/rand · SHA-256 at rest"]
    end

    subgraph svc["pkg/service/  — business rules, OWNS transactions"]
        keysvc["keysvc<br/>three-state values · OCC via version predicates"]
        branchsvc["branchsvc / mrsvc<br/>copy-on-write deltas · review state machine"]
        mergesvc["mergesvc<br/>advisory lock · 7-step merge tx"]
        releasesvc["releasesvc<br/>publish · materialise bundle"]
        assetsvc["assetsvc<br/>presigned POST · content-addressed verify"]
        importsvc["importsvc / seed"]
        exportsvc["exportsvc"]
    end

    subgraph data["Data layer"]
        repo["pkg/repository/ — ALL SQL<br/>raw ON CONFLICT · FOR UPDATE · db(ctx, tx)"]
        pg[("PostgreSQL<br/>Flyway .db/V1.00–V1.08")]
    end

    subgraph formats["Format engines (independent — round-trip gates R1/R2)"]
        parse["pkg/parse<br/>Flutter JSON · Android XML · iOS .strings"]
        export2["pkg/export<br/>byte-faithful serializers"]
    end

    subgraph ext["External"]
        lokalise["Lokalise API<br/>(rate-limited client)"]
        s3[("S3 via storage/v4")]
        gapi["Google tokeninfo"]
    end

    portal --> mw
    apps -->|"app launch"| mw
    ci -->|"Bearer token"| mw
    operator -->|"serve · seed · import · token · user"| svc

    mw --> api & ota & exp & health
    api --> google --> gapi
    exp --> token
    api --> keysvc & branchsvc & mergesvc & releasesvc & assetsvc
    ota --> releasesvc
    exp --> exportsvc

    keysvc & branchsvc & mergesvc & releasesvc & assetsvc & importsvc --> repo --> pg
    importsvc --> lokalise
    importsvc & importsvc --> parse
    exportsvc --> export2
    assetsvc --> s3
```

## 2. The life of a copy change — branch → merge → OTA

```mermaid
flowchart TB
    edit["Translator edits on a branch<br/>branch_translations delta · base_master_version captured on FIRST touch only"]
    mr["Merge request opened"]
    review["Review state machine<br/>open ⇄ changes_requested → approved → merged (terminal)<br/>SetStatus is a compare-and-swap"]
    resolve["Conflicts computed: master version ≠ base_master_version<br/>Resolve accepts only pairs actually in the conflict set<br/>choice = mine | master"]

    subgraph mergetx["mergesvc.Merge — one transaction, seven steps"]
        lock["1–2 · pg_advisory_xact_lock(8675309)<br/>serialises merges (mutation-checked test)"]
        rowlocks["3 · FOR UPDATE on affected translations AND keys rows<br/>deterministic order — no deadlocks"]
        conflicts["4 · conflicts still unresolved? → refuse (409)"]
        apply["5 · version-predicated CTE apply<br/>delta lands only if master still at base version or resolved 'mine'<br/>blocked rows → ErrConcurrentMasterWrite (409, retry)"]
        history["6 · translation_history + key_history rows (source 'merge')"]
        release["7 · release version allocated · bundle materialised<br/>sha256 + byte_size over the jsonb::text the wire serves"]
    end

    served["GET /ota/v1/bundles/:locale<br/>newest eligible release · X-App-Version floor (validated N.N.N)<br/>ETag/304 · 410 after kill switch"]
    phones["Strings reach users — no app release"]

    edit --> mr --> review -->|approved| resolve --> mergetx
    lock --> rowlocks --> conflicts --> apply --> history --> release
    mergetx --> served --> phones

    style mergetx fill:#f6f8fa,stroke:#57606a
```

### Reading guide

- **Three states everywhere:** no row = untranslated (omitted from export), `''` = deliberately blank (exported as `""`), else translated. Repositories return `(value, found, err)` — never a bare string.
- **`pkg/parse` and `pkg/export` never import each other** — that independence is what makes gate R1 (98,320 values round-tripped, zero alterations) a real check instead of a tautology.
- **Services own transactions; repositories take `tx` and speak SQL.** `WithTransaction` does not nest.
- **The advisory lock serialises merge-vs-merge; the version-predicated applies (step 5) close portal-vs-merge races** — two different mechanisms, both mutation-verified.
