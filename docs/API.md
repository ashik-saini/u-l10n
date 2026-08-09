# u-l10n — HTTP API reference

Derived from `route/`. For how the pieces fit together see
[ARCHITECTURE.md](ARCHITECTURE.md); for the persona journeys see
[USER_FLOWS.md](USER_FLOWS.md).

## Overview

HTTP only, on `:8080`. Every response is JSON except the export zip and the
OTA 304. Timestamps leave the service as UTC `YYYY-MM-DDTHH:MM:SSZ`, always.

Three surfaces, one router:

| Prefix | Who calls it | Auth | Rate limited |
| --- | --- | --- | --- |
| `/api/v1` (portal routes) | Humans via the portal SPA (the gateway exposes it as `/api/l10n/*`) | Google access token in `Authorization: Bearer`, plus a row in the `users` table | Yes |
| `/api/v1/export`, `/api/v1/assets/*`, `/api/v1/keys/{id}/assets*` | Scripts and CI | `X-Api-Token` header | Yes |
| `/ota/v1` | The mobile app, at launch, before login | None, deliberately | Yes |
| `/healthz`, `/readyz` | kubelet | None | **No** — a 429 on a liveness probe restarts a healthy pod |

There are no HTTP routes for managing API tokens or creating users — both are
CLI commands (`token`, `user grant` in `main.go`), on purpose.

### Human auth: `RequireIdentity`

Two facts are established in order:

1. **Who** — the bearer token is verified against Google. Missing token → 401
   `missing_token`; invalid/expired/revoked → 401 `invalid_token`. Both carry
   `WWW-Authenticate: Bearer realm="u-l10n"`. Google unreachable → **503**
   `identity_provider_unavailable` with `Retry-After: 5` — not 401 (the token
   may be fine) and not 500 (the fault is a dependency).
2. **What** — the email is looked up in this service's own `users` table.
   No row → 403 `not_provisioned`; disabled account → 403 `account_disabled`;
   role too low → 403 `insufficient_role`. 403s never carry
   `WWW-Authenticate`: re-authenticating cannot help.

Roles are ordered: `viewer < editor < approver < admin`. Each route states its
minimum. An unknown role in the database satisfies nothing, not even viewer —
it fails closed. The `x-yp-role` header the portal sends is **ignored
entirely**; the portal must gate its UI on `GET /api/v1/me`.

### Script auth: `RequireAPIToken`

`X-Api-Token: <token>`. Missing → 401 `missing_token` with
`WWW-Authenticate: X-Api-Token realm="u-l10n"`. Wrong, revoked and expired all
answer the same 401 `invalid_token` — a distinguishable error tells an attacker
which tokens exist. Scopes are ordered: `read_export < read_write`.
Insufficient scope → 403 `insufficient_scope`. Export needs `read_export`;
every asset route needs `read_write` (the images carry customer PII).

### Error envelope

```json
{"error": "<machine_code>", "details": "<human message>"}
```

`details` is omitted when empty; a 500 body is always just
`{"error":"internal_error"}` — the reason goes to the log, not the wire. Three
409s carry extra fields beyond the envelope (documented at their endpoints):
the translation `version_conflict` (mine/theirs), `unresolved_conflicts` (the
conflicting rows) and `name_collision` (the colliding keys).

The status semantics are uniform, mapped by one shared function
(`route/portal.go`):

- **400** — the caller sent something wrong (validation, unknown parameter,
  malformed body).
- **404** — the thing addressed does not exist.
- **409** — the caller's view of the world is out of date. Every 409 is an
  expected workflow outcome a human must act on, never a fault.
- **500** — only a genuinely unrecognised error, and only that case is logged
  as an error.

### Strict inputs

- **Unknown query parameters are refused** with 400, naming the parameter and
  the allowed list. A portal that misspells `untranslated_in` must be told,
  not handed an unfiltered page.
- **Unknown JSON body fields are refused** (`DisallowUnknownFields`), the body
  must be exactly one JSON object, and it is size-capped: 64 KiB on portal
  routes, 8 KiB on asset routes (those bodies carry declarations, never image
  data).
- Boolean parameters accept exactly `true` or `false`. Integer parameters must
  be non-negative integers; a present-but-unparseable value is a 400, never a
  silent default.
- Comma-separated list parameters (`locales`) drop empty elements, so a
  trailing comma is not an error.

### Rate limiting

One in-process token bucket per client IP, shared across `/api/v1` and
`/ota/v1` so a client cannot multiply its budget across prefixes. It runs
**before** authentication, because the expensive paths are pre-auth (an unseen
Google token costs an outbound call with a 10-second timeout). Over budget:

```
429  Retry-After: <whole seconds until a token accrues>
{"error":"rate_limited","details":"too many requests from this client; retry after the indicated delay"}
```

Defaults: 300 requests/minute sustained, burst 60
(`SERVICECONFIG_RATE_LIMIT_PER_MINUTE`, `_BURST`, `_ENABLE`). The client key
walks `X-Forwarded-For` right-to-left past private/loopback hops and falls
back to the socket address captured before `RealIP` runs — client-supplied
entries are never trusted.

---

## Health

| Method + path | Auth | Success |
| --- | --- | --- |
| `GET /healthz` | none | 200 `{"status":"ok","service":"u-l10n"}` |
| `GET /readyz` | none | 200 `{"status":"ready","checks":{"database":"ok"}}` |

`/healthz` touches no dependency — it answers "is this process wedged?" and
nothing else. `/readyz` pings the database; unreachable → 503
`{"status":"unavailable","checks":{"database":"unreachable"}}`.

## Identity

### `GET /api/v1/me` — viewer

Returns the caller's identity and role: `{"email":"a@you.co","role":"editor"}`.
No query parameters. This is what the portal gates its UI on. Nothing is
queried — the middleware already read the row.

## Keys

The three-state rule is visible in every value cell. A `(key, locale)` pair is
one of:

```json
{"translated": false}                      // no row: untranslated
{"translated": true,  "value": ""}         // deliberately blank
{"translated": true,  "value": "Top up"}   // translated
```

Cells also carry `version` (master's version — the value to send back as
`base_version`; 0 means no master row, which is a legitimate base),
`from_branch` (the branch overrides this cell), `render_hint`, `updated_by`,
`updated_at`.

### `GET /api/v1/keys` — viewer

The key browser's bulk fetch. All parameters optional:

| Parameter | Type | Rules |
| --- | --- | --- |
| `branch` | string | View the corpus as this branch sees it; absent = master |
| `locales` | csv | Locale codes to include, e.g. `en-SG,ms-MY` |
| `platform` | string | `flutter`, `android` or `ios` |
| `tag` | string | Filter to keys carrying this tag |
| `search` | string | Text search |
| `untranslated_in` | string | Keys with no row for this locale |
| `include_deleted` | bool | Strictly `true`/`false` |
| `limit` | int | ≥ 0, **max 20000** (400 above that) |
| `offset` | int | ≥ 0 |

200 → `{"keys":[…],"total":N,"limit":N,"offset":N,"locales":[…],"branch":null|"name"}`.
`total` is the whole matching set, not the page. `branch` is null on master.
Each key carries `id`, `name`, `description`, `platforms`, `android_name` /
`ios_name` (null = derived from the key name), `status`, `version`,
`sort_index`, `tags` (always an array, never null), `values` keyed by locale
code with an entry per requested locale, `branch_modified`, timestamps.

### `GET /api/v1/keys/{id}` — viewer

Params: `branch`, `locales`. 200 → one key object. 404 `not_found` for an
unknown id; 400 for a non-positive or non-integer id.

### `GET /api/v1/keys/{id}/history` — viewer

| Parameter | Type | Rules |
| --- | --- | --- |
| `locale` | string | Restrict the value timeline to one locale |
| `limit` | int | default 200, **max 1000** |

200 → `{"key":[…],"translations":[…]}` — the metadata and value timelines,
insert-only (a rollback appears as a new forward entry). Translation entries
carry the same three-state `translated`/`value` pair; a null value in history
means the pair *became* untranslated.

### `POST /api/v1/keys` — editor

Query: `branch` (optional). Body:

| Field | Type | Rules |
| --- | --- | --- |
| `name` | string | required, ≤ 255 chars, no leading/trailing whitespace, no `\n` `\r` `\t` |
| `description` | string | ≤ 2000 chars |
| `platforms` | []string | required, non-empty, each of `flutter` `android` `ios` |
| `android_name` | string\|null | optional export-name override |
| `ios_name` | string\|null | optional export-name override |

201 → the created key. 409 `name_taken` when an active key already claims the
name. With `?branch=` the key is a draft on master plus a branch delta —
invisible to exports and OTA until merged.

### `PATCH /api/v1/keys/{id}` — editor

Query: `branch`. Partial update: an absent field is left alone.

| Field | Type | Rules |
| --- | --- | --- |
| `name` | string | same rules as create |
| `description` | string | ≤ 2000 chars |
| `platforms` | []string | replaces the set |
| `android_name` | three-state | absent = leave alone; `null` = clear the override (derive from name); string = set it |
| `ios_name` | three-state | same |
| `base_version` | int | optimistic-lock precondition |

200 → the updated key. 409 `version_conflict` on a stale `base_version`;
409 `name_taken`; 409 `branch_not_open` when the branch is closed or merged.

### `DELETE /api/v1/keys/{id}` — editor

Query: `branch`. Optional body `{"base_version": N}` — an empty body is the
common case; a malformed one is a 400, not silently dropped. 204 on success.
Soft delete: the row survives as `deleted` and the name becomes reusable.

### `PUT /api/v1/keys/{id}/translations/{locale}` — editor

The inline cell edit. Query: `branch`. Body:

| Field | Type | Rules |
| --- | --- | --- |
| `value` | string | **required** (pointer semantics: an omitted field is 400). `""` is a deliberate blank; removal is DELETE, not null. ≤ 20000 chars |
| `render_hint` | string | `plain` or `cdata`; empty defaults to `plain` |
| `base_version` | int | **required on master**; **must be absent on a branch** (branch edits reconcile at merge) |

200 → the new cell. Losing the optimistic-concurrency race answers **409 with
both sides in the body** so the portal needs no second round trip:

```json
{
  "error": "version_conflict", "details": "…",
  "key_id": 12, "locale": "en-SG",
  "base_version": 4,
  "mine": "what I tried to write",
  "theirs": {"translated": true, "value": "what won", "version": 5, …}
}
```

`theirs` is a full cell, so "somebody deleted it" and "somebody blanked it"
stay different answers.

### `DELETE /api/v1/keys/{id}/translations/{locale}` — editor

Makes the pair **untranslated** (removes the row; does not write `""`).
Query: `branch`. Optional body `{"base_version": N}`. 204 on success. A lost
race is the same 409 conflict body, minus `mine` — a delete carries no value
to echo back.

## Tags

Tags are global per key, not branch-scoped: no tag route takes `?branch=` and
tag changes never enter a merge.

### `GET /api/v1/tags` — viewer

No parameters. 200 → `{"tags":[{"id","name","colour","key_count","created_at"}]}`.

### `POST /api/v1/tags` — editor

Body `{"name","colour"}`. Name: required, ≤ 64 chars, no surrounding
whitespace, no control characters. Colour: empty or `#rrggbb`. 201 → the tag.
409 `name_taken`.

### `PUT /api/v1/tags/{id}` — editor

Full replace of both fields (a tag has only two). Same validation. 200 → the
tag. Tag ids are `int16`: any id outside 1–32767 is a 400.

### `DELETE /api/v1/tags/{id}` — editor

Destructive beyond the row: `key_tags` cascades. 200 →
`{"detached_keys": N}` — not a bare 204, because "deleted the tag" and
"deleted the tag and detached it from 812 keys" are different outcomes.

### `PUT /api/v1/keys/{id}/tags` — editor

Body `{"tag_ids":[1,4]}` — the **complete set** the key ends up with, not a
diff. `[]` clears every tag; an omitted `tag_ids` is a 400. 200 →
`{"key_id":N,"tags":[…]}`.

### `POST /api/v1/tags/{id}/keys` — editor

Bulk assign. Body `{"key_ids":[12,13,14]}`. Idempotent (`ON CONFLICT DO
NOTHING`). 200 → `{"tag":{…},"requested":N}` — no "added" count, because a
retried request would report 0 and read as a failure.

### `DELETE /api/v1/tags/{id}/keys` — editor

Bulk unassign, same body. 200 → `{"tag":{…},"requested":N,"removed":M}`.
`removed` is routinely below `requested` — not every selected key carried the
tag.

## Branches

Branch names are validated at creation to survive a URL path segment:
≤ 100 chars, only letters, digits, `-`, `_`, `.` — a branch called
`release/4.12` would be creatable and then unreachable.

### `GET /api/v1/branches` — viewer

| Parameter | Rules |
| --- | --- |
| `status` | `open`, `merged` or `closed`; anything else is 400 |

200 → `{"branches":[…]}`. Each branch: `id`, `name`, `description`, `status`,
`created_by`, `created_at`, `last_edited_at` (null until first write — this is
what invalidates an approval), `merged_at`, `value_changes` / `meta_changes`
(delta counts), `merge_request_id` (null when no **live** request) and
`merge_request_status`.

### `GET /api/v1/branches/{name}` — viewer

200 → one branch. 404 `not_found`.

### `GET /api/v1/branches/{name}/changes` — viewer

The branch's diff against master. 200 →
`{"branch","status","values":[…],"meta":[…],"conflicts":N}`. Value rows carry
both sides in three-state form (`removed` = the branch tombstones the pair;
`master_translated: false` = master has no row), `base_master_version`,
`master_version` and a `conflict` flag computed with the merge's own rule —
master moved since the branch first touched the pair. `conflicts` counts the
flagged rows across both lists.

### `POST /api/v1/branches` — editor

Body `{"name","description"}` (description ≤ 2000 chars). 201 → the branch
**without** delta counts — the write path has not counted, and omitting the
fields is different from asserting zero. 409 `name_taken`.

### `POST /api/v1/branches/{name}/close` — editor

Abandons without merging; the deltas survive. 200 → the branch.

### `POST /api/v1/branches/{name}/reopen` — editor

Returns a closed branch to editable. A **merged** branch is refused with 409
`branch_not_open` — its deltas are the permanent record of what the merge
applied. 200 → the branch.

## Merge requests

Statuses: `open`, `approved`, `changes_requested`, `rejected`, `merged`,
`closed`. One live request per branch, enforced.

### `GET /api/v1/merge-requests` — viewer

Optional `status` filter (invalid values → 400). 200 →
`{"merge_requests":[…]}`, newest first. Each: `id`, `title`, `status`,
`branch_id`, `branch`, `created_by`, `created_at`, `approved_by` /
`approved_at` (null until approved, **reset to null** when a later branch edit
invalidates the approval), `merged_at`.

### `GET /api/v1/merge-requests/{id}` — viewer

200 → `{"merge_request":{…},"branch":{…},"events":[…],"conflicts":{…}}`. The
conflicts travel with it so the review screen never renders "ready to merge"
from a stale second request. Event actors include `"system"` for automatic
transitions (e.g. an approval invalidated by a branch edit).

### `GET /api/v1/merge-requests/{id}/conflicts` — viewer

200 → the conflicts object:

```json
{
  "values":         [{"key_id","key_name","locale","mine_removed","mine","theirs_translated","theirs","base_master_version","master_version","resolution"}],
  "metadata":       [{"key_id","mine_name","theirs_name","mine_status","theirs_status", …, "resolution"}],
  "name_collisions":[{"name","branch_key_id","master_key_id"}],
  "unresolved": N,
  "mergeable": true
}
```

Value and metadata conflicts are resolved by choosing a side (`resolution` is
`"mine"`, `"master"`, or empty while undecided). Name collisions carry **no
resolution field**: both sides independently claimed a name and one key must be
renamed — choosing a side cannot fix it. `mergeable` is **advisory**: the merge
recomputes everything inside its own transaction under an advisory lock, and
that answer is the one that counts.

### `POST /api/v1/merge-requests` — editor

Body `{"branch","title"}`. `branch` required; `title` required, ≤ 200 chars.
201 → the request. 409 `live_merge_request_exists` when the branch already has
one; 409 `branch_not_open` for a closed/merged branch.

### Review transitions

All take an optional body `{"comment":"…"}` (≤ 4000 chars) and return 200 with
the moved request. A transition from a state the table does not allow answers
409 `merge_request_not_live`, naming the allowed source states.

| Method + path | Role | From → to | Notes |
| --- | --- | --- | --- |
| `POST /api/v1/merge-requests/{id}/approve` | approver | open, changes_requested → approved | Records who and when; any later branch edit re-opens the request automatically |
| `POST /api/v1/merge-requests/{id}/request-changes` | approver | open, approved → changes_requested | **Comment required** (400 without one) |
| `POST /api/v1/merge-requests/{id}/reject` | approver | open, approved, changes_requested → rejected | Terminal; the branch survives |
| `POST /api/v1/merge-requests/{id}/close` | editor | open, approved, changes_requested → closed | Withdrawing your own proposal |
| `POST /api/v1/merge-requests/{id}/reopen` | editor | rejected, closed → open | 409 `live_merge_request_exists` if another live request appeared |

### `PUT /api/v1/merge-requests/{id}/resolutions` — editor

Body:

```json
{"resolutions":[{"key_id":12,"locale":"en-SG","resolution":"mine"},
                {"key_id":13,"resolution":"master"}]}
```

A resolution with no `locale` decides a key-**metadata** conflict. `resolution`
must be `mine` or `master`. The whole set lands in one transaction; 200 → the
recomputed conflicts object, so the reviewer sees what is left without a second
request.

### `POST /api/v1/merge-requests/{id}/merge` — approver

Folds the branch into master and cuts a release. 200 →

```json
{"release_id":41,"release_version":41,"values_applied":12,"keys_applied":2,"bundles_written":6}
```

Every refusal is a 409 carrying enough to act on:

| `error` | Meaning | Body extras |
| --- | --- | --- |
| `not_approved` | The request is not in `approved` | — |
| `stale_approval` | The branch was edited after approval; re-review | — |
| `unresolved_conflicts` | Decisions outstanding | `values` and `metadata` — the conflicting **rows**, not a count |
| `name_collision` | Both sides claim a key name; rename one first | `name_collisions` |
| `concurrent_master_write` | A master write raced the merge between conflict computation and apply. The transaction rolled back whole. **Retry the merge**; it will surface the new conflict for a human | — |
| `merge_request_not_live` | The final approved → merged move lost a race | — |

## Releases

Releases are addressed by their human-facing **version**, not row id — that is
the number in the incident channel. Versions are positive integers in the path.

### `GET /api/v1/releases` — viewer

| Parameter | Rules |
| --- | --- |
| `limit` | int ≥ 0, default 50 |
| `offset` | int ≥ 0 |

200 → `{"releases":[…],"limit","offset"}`, newest first. Each release: `id`,
`version`, `source`, `notes`, `merge_request_id` (null for a manual publish or
import — the difference between "somebody approved this" and "somebody pushed
it"), `min_app_version` (null = every client eligible), `created_by`,
`created_at`, `rolled_back` / `rolled_back_at` / `rolled_back_by`,
`locale_count`, `key_count`.

### `GET /api/v1/releases/{version}` — viewer

200 → one release. 404 `not_found`.

### `GET /api/v1/releases/{version}/bundles/{locale}` — viewer

Exactly what the release shipped for one locale, read from the materialised
bundle — the answer cannot drift. 200 →
`{"version","locale","sha256","key_count","byte_size","strings":{…}}`.
`sha256` doubles as the OTA ETag. Response carries
`Cache-Control: private, max-age=300` — private because it sits behind an
identity.

### `POST /api/v1/releases` — approver

Manual publish: cuts a release from master's current state, no diff reviewed.
Optional body:

| Field | Rules |
| --- | --- |
| `notes` | free text |
| `min_app_version` | empty, or **exactly three numeric components** (`4.12.0`); `4.12` is refused with 400 |

201 → the release. The release row and all bundles land in one transaction.
Two simultaneous publishes → the loser gets 409 `release_version_race`, which
is retryable by design.

### `POST /api/v1/releases/{version}/rollback` — approver

The OTA kill switch. Withholds the release from serving; clients fall back to
the newest earlier eligible release or, when there is none, receive 410 from
OTA and use the strings compiled into the binary. It does **not** undo the
values — master keeps what the merge applied. 200 → the release. Rolling back
an already-rolled-back release → 409 `already_rolled_back` (a second write
would overwrite `rolled_back_by`, the answer to the only question anybody asks
afterwards).

## Assets — `X-Api-Token`, scope `read_write`

Context screenshots. Three-step flow: presign → browser uploads to S3 →
confirm. Bodies capped at 8 KiB; the image bytes never pass through this
service.

### `POST /api/v1/assets/presign`

| Field | Rules |
| --- | --- |
| `filename` | cosmetic; truncated to 200 chars |
| `content_type` | **`image/png` or `image/jpeg` only** |
| `bytes` | 1 … 10485760 (10 MiB) |
| `sha256` | hex digest of the bytes |

200 → either `{"deduplicated":true,"asset":{…}}` (these exact bytes are
already stored; skip the upload) or
`{"deduplicated":false,"upload":{"url","fields":{…},"s3_key"}}` — a signed
multipart/form-data POST target. Every field must be sent verbatim; S3 rejects
the upload otherwise. The URL and fields are credentials and are never logged.

### `POST /api/v1/assets/confirm`

Body `{"sha256":"<hex>"}` — only the hash, on purpose: everything else is read
back from the object's own metadata, which S3 enforced against the signed
policy. 201 → the asset
(`{"id","sha256","filename","content_type","bytes","uploaded_by","created_at"}`).

409s: `upload_not_found` — no uploaded object for that hash (409, not 404: the
URL is fine, the state the client believes in is not); `upload_mismatch` — the
object does not match, or does not carry, its declaration.

### `GET /api/v1/assets/{id}/url`

200 → `{"url":"<short-lived presigned GET>"}` with
`Cache-Control: no-store` — the URL is a bearer credential for customer PII.
404 `not_found` for an unknown asset.

### `PUT /api/v1/keys/{id}/assets`

Body `{"asset_id":88,"note":"truncates past 18 characters"}` (`asset_id`
required and positive). Idempotent: repeating with a different note amends the
note. 204 on success.

### `DELETE /api/v1/keys/{id}/assets/{assetId}`

Removes the link; the asset survives (content-addressed, possibly shared).
204 on success.

## Export — `X-Api-Token`, scope `read_export`

### `GET /api/v1/export`

Synchronous: the response body **is** the zip. No job to poll.

| Parameter | Rules |
| --- | --- |
| `format` | **required**: `json` (Flutter), `xml` (Android) or `strings` (iOS) |
| `locales` | csv of locale codes; absent = all |
| `empty_mode` | `include` (default) or `skip_empty` |
| `line_ending` | `lf` (default) or `crlf` |

200 → `Content-Type: application/zip`,
`Content-Disposition: attachment; filename="u-l10n-<format>-<timestamp>.zip"`,
`Cache-Control: no-store` (the archive reflects mutable state). Caller
mistakes — unknown parameter, unknown locale, export-name collision — are 400;
anything else is 500 `export_failed`.

## OTA — public

### `GET /ota/v1/bundles/{locale}`

Unauthenticated, deliberately: the app calls it at launch before login, and
the payload is strings that already ship inside the binary. The controls are
rate limiting, CDN caching, and the by-construction rule that no PII enters a
bundle.

Request headers:

| Header | Rules |
| --- | --- |
| `X-App-Version` | strict `N.N.N`; anything else is treated as absent, which reads as `0.0.0` — the conservative floor, so an odd build string costs that client only the releases with a version floor, never a 500 |
| `If-None-Match` | standard ETag semantics: multiple values, `*` and `W/` weak validators all handled |

Responses:

| Status | Meaning | Body / headers |
| --- | --- | --- |
| 200 | Fresh bundle | `{"version","locale","sha256","strings":{…}}` with `ETag: "<sha256>"`, `Cache-Control: public, max-age=300`, `Vary: X-App-Version` |
| 304 | ETag matched | No body; **same cache headers as the 200** so the CDN knows how long the revalidated entry stays fresh. This is the steady state on every app launch |
| 404 `unknown_locale` | No such locale | `Cache-Control: public, max-age=60` |
| 404 `no_release` | No servable release yet — not an error; the app uses its bundled assets | `Cache-Control: public, max-age=60` |
| 410 `release_rolled_back` | The kill switch. The client must **delete its cached bundle** and fall back to the strings shipped in the app — which a 404 would not signal | `Cache-Control: public, max-age=60` |

The negative answers carry an explicit short TTL so CDN negative caching is a
decision rather than a default; a 404 flips to a 200 on the very next publish.

## Users / admin

### `PATCH /api/v1/admin/users/{email}/role` — admin

Body `{"role":"approver"}` — one of `viewer`, `editor`, `approver`, `admin`.
The email path segment may be percent-encoded; a segment that fails decoding
is a 400. 200 → `{"email","role"}`. It changes an existing user and **never
creates one** — a typo'd address is a 404, not a new account. Creating the
first user is the `user grant` CLI command's job. Every change is audited.

---

## Error codes

| `error` | Status | Meaning |
| --- | --- | --- |
| `bad_request` | 400 | Validation failure, unknown query parameter, malformed or oversized body |
| `missing_token` | 401 | No credential presented (`WWW-Authenticate` names which kind) |
| `invalid_token` | 401 | Credential presented but not valid — expired, revoked, fabricated, all indistinguishable |
| `not_provisioned` | 403 | Google authenticated the caller, but no `users` row exists |
| `account_disabled` | 403 | The account exists and is disabled |
| `insufficient_role` | 403 | Provisioned, but below the route's minimum role |
| `insufficient_scope` | 403 | API token authenticated, but below the route's minimum scope |
| `not_found` | 404 | The addressed entity does not exist |
| `version_conflict` | 409 | Optimistic lock lost; on translation writes the body carries `mine` and `theirs` |
| `name_taken` | 409 | An active key, tag or branch already claims the name |
| `branch_not_open` | 409 | The branch is closed or merged and cannot be edited/reopened |
| `merge_request_not_live` | 409 | The request is not in a state that allows this transition |
| `live_merge_request_exists` | 409 | The branch already has a live merge request |
| `not_approved` | 409 | Merge attempted on an unapproved request |
| `stale_approval` | 409 | The branch was edited after approval; re-review required |
| `unresolved_conflicts` | 409 | Conflicts block the merge; body carries the rows |
| `name_collision` | 409 | Branch and master both claim a key name; rename one |
| `concurrent_master_write` | 409 | A master write raced the merge; the transaction rolled back whole — retry the merge |
| `release_version_race` | 409 | Two simultaneous publishes; retry |
| `already_rolled_back` | 409 | The release is already rolled back |
| `upload_not_found` | 409 | Confirm called but no object with that hash was uploaded |
| `upload_mismatch` | 409 | The uploaded object disagrees with, or lacks, its declaration |
| `rate_limited` | 429 | Over the per-client budget; honour `Retry-After` |
| `internal_error` | 500 | Genuinely unrecognised failure; reason in the log, not the body |
| `export_failed` | 500 | Export-specific unrecognised failure |
| `unknown_locale` | 404 | OTA: no such locale |
| `no_release` | 404 | OTA: nothing servable yet |
| `release_rolled_back` | 410 | OTA: kill switch — clear the cache, use bundled strings |
| `identity_provider_unavailable` | 503 | Google unreachable; retry after the indicated delay |
