# u-l10n — user flows

Who touches the system, and what their path through it looks like. Companion to [ARCHITECTURE.md](ARCHITECTURE.md), which shows the machinery underneath.

## 1. Translator — editing copy

```mermaid
flowchart TB
    login["Sign in with Google Workspace<br/>tokeninfo verified → role from users table"]
    browse["Browse keys — /api/v1/keys<br/>filter by tag · status · text search<br/>(search resolves through the branch)"]
    pick["Pick or create a branch<br/>master stays untouched"]
    edit["Edit a translation<br/>delta stored in branch_translations<br/>base_master_version captured on first touch"]
    ctx["Attach a context screenshot<br/>presigned POST → S3 → Confirm verifies bytes hash to their name"]
    stale{"Someone else changed<br/>this cell meanwhile?"}
    conflict409["409 with BOTH values —<br/>mine and theirs, side by side"]
    reconcile["Reconcile and resave<br/>with the fresh base_version"]
    openmr["Open a merge request"]

    login --> browse --> pick --> edit --> stale
    edit -.optional.-> ctx
    stale -->|no| openmr
    stale -->|yes| conflict409 --> reconcile --> stale
```

Three states matter while editing: **no value** (untranslated — omitted from exports), **empty** (deliberately blank — exported as `""`), and **translated**. Clearing a value and deleting it are different actions with different outcomes.

## 2. Reviewer / approver — the merge request

```mermaid
stateDiagram-v2
    [*] --> open : translator opens MR
    open --> changes_requested : reviewer requests changes
    changes_requested --> open : translator revises
    open --> approved : approver approves
    changes_requested --> approved : approver approves
    approved --> merged : approver merges
    open --> closed : abandoned
    changes_requested --> closed : abandoned
    approved --> closed : abandoned
    merged --> [*]
    closed --> [*]

    note right of approved
        Conflicts must be resolved first:
        each conflicting pair gets an explicit
        choice — mine or master. Only pairs
        actually in conflict can be resolved.
    end note
    note right of merged
        Terminal. Every transition is a
        compare-and-swap in SQL — a close
        racing a merge loses cleanly.
    end note
```

```mermaid
flowchart LR
    review["Review diff<br/>branch deltas vs master"]
    conflicts{"Conflicts?<br/>master moved past<br/>base_master_version"}
    resolve["Resolve each pair:<br/>mine / master"]
    merge["Merge<br/>one serialized transaction"]
    outcome{"Master moved during<br/>the merge window?"}
    retry["409 concurrent_master_write —<br/>re-check and retry"]
    done["Release cut · bundle materialised ·<br/>history recorded · MR merged"]

    review --> conflicts
    conflicts -->|yes| resolve --> merge
    conflicts -->|no| merge
    merge --> outcome
    outcome -->|no| done
    outcome -->|yes| retry --> review
```

## 3. Mobile app user — strings over the air

```mermaid
sequenceDiagram
    participant App as Mobile app
    participant OTA as GET /ota/v1/bundles/:locale
    participant DB as PostgreSQL

    App->>OTA: X-App-Version 4.12.0 · If-None-Match "<etag>"
    Note over OTA: rate-limited per client IP<br/>version validated as N.N.N,<br/>anything else reads as 0.0.0
    OTA->>DB: newest release for locale<br/>not rolled back · version floor satisfied
    alt nothing changed
        OTA-->>App: 304 — zero bytes
    else new strings available
        OTA-->>App: 200 — bundle JSON + ETag (sha256 of the served bytes)
    else locale unknown / no release yet
        OTA-->>App: 404 (Cache-Control max-age=60)
    else release killed
        OTA-->>App: 410 — app falls back to shipped strings
    end
    Note over App: copy updates without an app release
```

## 4. Operator — CLI and administration

```mermaid
flowchart TB
    subgraph cli["u-l10n CLI"]
        serve["serve — run the service"]
        seedcmd["seed-from-files<br/>load the committed u-mobile tree (--dry-run to rehearse)"]
        importcmd["import<br/>reconcile from Lokalise ($LOKALISE_API_TOKEN)<br/>duplicate canonical names dedupe last-wins with warnings"]
        tokencmd["token create / revoke<br/>plaintext shown once on stdout · SHA-256 at rest"]
        usercmd["user grant / list<br/>roles: viewer < editor < approver < admin"]
    end

    subgraph consumers["Who uses what"]
        ci["CI pipeline<br/>Bearer token → GET /export<br/>zip: Flutter JSON · Android XML · iOS .strings"]
        newhire["New operator<br/>admin grants a role before<br/>their Google login means anything"]
    end

    tokencmd --> ci
    usercmd --> newhire
    seedcmd -->|"one-time, at setup"| serve
    importcmd -->|"cutover reconciliation"| serve
```

### Where the flows meet

Translator edits land on a **branch**; the approver's **merge** is the only door to master; a merge **cuts a release**; the app's next launch **picks it up**. Nothing reaches users' screens without passing every gate in between — and each arrow above that crosses a trust boundary (login, token, version header, resolution choice) is validated on the server side, never assumed from the client.
