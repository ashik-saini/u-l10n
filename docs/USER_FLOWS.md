# u-l10n — user flows

One sequence per persona. Companion to [ARCHITECTURE.md](ARCHITECTURE.md), which shows the machinery underneath each arrow.

## 1. Translator — editing copy on a branch

```mermaid
sequenceDiagram
    autonumber
    actor T as Translator
    participant P as Portal
    participant S as u-l10n

    T->>P: sign in with Google Workspace
    P->>S: token → verified → role from users table
    T->>P: browse keys (tag · status · text search)
    Note over S: search resolves through the branch,<br/>not master
    T->>P: pick/create a branch
    T->>P: edit a value
    P->>S: save (base_version)
    Note over S: delta lands in branch_translations —<br/>base_master_version captured on FIRST touch
    alt nobody else touched the cell
        S-->>T: saved — new version
    else concurrent edit
        S-->>T: 409 with BOTH values — mine and theirs
        T->>P: reconcile, save with fresh base
        S-->>T: saved
    end
    opt context screenshot
        T->>S: presign → upload to S3 → confirm (bytes verified)
    end
    T->>S: open merge request
```

Three states while editing: **no value** (untranslated — omitted from exports), **empty** (deliberately blank — exported as `""`), **translated**. Clearing and deleting are different actions.

## 2. Reviewer & approver — the merge request

```mermaid
sequenceDiagram
    autonumber
    actor T as Translator
    actor RV as Reviewer
    actor AP as Approver
    participant S as u-l10n

    T->>S: open MR
    loop until satisfied
        RV->>S: review diff (branch deltas vs master)
        RV->>S: request changes
        T->>S: revise on the branch
    end
    opt conflicts exist (master moved past base)
        AP->>S: resolve each pair — mine / master
        Note over S: only pairs actually in conflict<br/>can be resolved
    end
    AP->>S: approve
    AP->>S: merge
    alt master untouched during the window
        S-->>AP: merged — release cut, history recorded
    else concurrent master write
        S-->>AP: 409 concurrent_master_write
        AP->>S: re-check conflicts, retry merge
    end
    Note over S: merged is terminal — every status change is a<br/>compare-and-swap, so a close racing a merge loses cleanly
```

## 3. Mobile app user — strings over the air

```mermaid
sequenceDiagram
    autonumber
    participant App as Mobile app
    participant OTA as /ota/v1/bundles/:locale
    participant PG as PostgreSQL

    App->>OTA: X-App-Version · If-None-Match "etag"
    Note over OTA: rate-limited per client IP ·<br/>version validated N.N.N, else reads as 0.0.0
    OTA->>PG: newest eligible release (not rolled back, floor satisfied)
    alt nothing changed
        OTA-->>App: 304 — zero bytes
    else new strings
        OTA-->>App: 200 — bundle + ETag (sha256 of served bytes)
    else unknown locale / no release
        OTA-->>App: 404 (Cache-Control max-age=60)
    else release killed
        OTA-->>App: 410 — fall back to shipped strings
    end
    Note over App: copy changes reach users without an app release
```

## 4. Operator — CLI administration

```mermaid
sequenceDiagram
    autonumber
    actor OP as Operator
    participant CLI as u-l10n CLI
    participant PG as PostgreSQL

    OP->>CLI: project create --code --name --actor
    CLI->>PG: projects row + actor's admin grant, ONE transaction
    Note over PG: platform admin only; the one privilege<br/>with no project to scope it to
    OP->>CLI: user grant --email --role [--platform-admin]
    CLI->>PG: users row (viewer < editor < approver < admin)
    Note over PG: a Google login means nothing<br/>until a role exists here
    OP->>CLI: token create --name --scope
    CLI->>PG: SHA-256 at rest
    CLI-->>OP: plaintext shown ONCE on stdout
    OP->>CLI: seed-from-files (--dry-run to rehearse)
    CLI->>PG: one-time load of the committed u-mobile tree
    OP->>CLI: import (cutover reconciliation from Lokalise)
```

## 5. CI — pulling the release formats

```mermaid
sequenceDiagram
    autonumber
    participant CI as CI pipeline
    participant EX as /api/v1/export
    participant S as u-l10n

    CI->>EX: GET (X-Api-Token, read_export scope)
    EX->>S: hash lookup — live tokens only
    S-->>CI: zip — Flutter JSON · Android XML · iOS .strings
    Note over S: an Android name collision fails the<br/>WHOLE export before a single file ships
```

### Where the flows meet

Translator edits land on a **branch**; the approver's **merge** is the only door to master; a merge **cuts a release**; the app's next launch **picks it up**. Every arrow that crosses a trust boundary — login, token, version header, resolution choice — is validated server-side, never assumed from the client.
