package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jinzhu/gorm"

	"github.com/yougroupteam/u-common-components/database"
	"github.com/yougroupteam/u-l10n/pkg/model"
)

// Release is an immutable, numbered snapshot of master.
type Release struct {
	ID      int64
	Version int64
	Source  string
}

// ReleaseRepository creates releases and materialises their bundles.
type ReleaseRepository interface {
	// Create allocates the next version and inserts the release.
	Create(ctx context.Context, tx *gorm.DB, source string, mergeRequestID *int64, createdBy string) (Release, error)

	// MaterialiseBundle writes one locale's flat key/value map for a release,
	// with its sha256 — which doubles as the HTTP ETag on the OTA path.
	MaterialiseBundle(ctx context.Context, tx *gorm.DB, releaseID int64, locale model.Locale, rows []ExportRow) error

	BundleSHA(ctx context.Context, tx *gorm.DB, releaseID int64, localeID int16) (string, error)

	// ServableBundle returns what an OTA client should receive for a locale.
	ServableBundle(ctx context.Context, tx *gorm.DB, localeID int16, appVersion string) (ServableBundle, error)

	// CreatePublish allocates a release with no merge behind it.
	//
	// Separate from Create because a manual publish carries two things a merge's
	// release never does: release notes, and a semver floor. Widening Create's
	// signature would put two always-nil arguments on the merge path, which is
	// the one place in this service where an extra nil is most expensive to
	// misread.
	CreatePublish(ctx context.Context, tx *gorm.DB, notes string, minAppVersion *string, createdBy string) (Release, error)

	// List returns releases newest first, each with its bundle summary.
	List(ctx context.Context, tx *gorm.DB, limit, offset int) ([]ReleaseDetail, error)

	// ByVersion reads one release by its human-facing number.
	ByVersion(ctx context.Context, tx *gorm.DB, version int64) (ReleaseDetail, error)

	// Rollback is the OTA kill switch: it removes a release from serving.
	//
	// Returns ErrNotFound when there is no such release and ErrAlreadyRolledBack
	// when it is already withheld — the second is not a failure to fix, it is a
	// caller acting on a stale view.
	Rollback(ctx context.Context, tx *gorm.DB, version int64, actor string) (ReleaseDetail, error)

	// BundleFor reads one materialised bundle by release version and locale.
	BundleFor(ctx context.Context, tx *gorm.DB, version int64, localeID int16) (Bundle, error)
}

// ErrAlreadyRolledBack is returned when a rollback targets a release that is
// already withheld.
//
// An exported sentinel so the handler answers 409 rather than repeating the
// write: the kill switch has already been pulled, and rolled_back_by records who
// pulled it. Overwriting that with a second name would erase the answer to the
// only question anyone asks afterwards.
var ErrAlreadyRolledBack = errors.New("this release has already been rolled back")

// ReleaseDetail is a release with everything the portal's release list shows.
type ReleaseDetail struct {
	ID      int64
	Version int64
	Source  string
	Notes   string

	// MergeRequestID is nil for a manual publish or an import — which is the
	// distinction between "somebody approved this" and "somebody pushed it".
	MergeRequestID *int64

	// MinAppVersion is nil when every client is eligible. Compared as an integer
	// triple on the serving path, never lexically: '4.9.0' > '4.10.0' as text.
	MinAppVersion *string

	CreatedBy string
	CreatedAt time.Time

	// RolledBackAt and RolledBackBy are set together or not at all — the schema
	// enforces it — and a non-nil pair is the kill switch having been pulled.
	RolledBackAt *time.Time
	RolledBackBy *string

	// LocaleCount and KeyCount summarise the materialised bundles, so a list row
	// can show what shipped without loading six JSON documents.
	LocaleCount int
	KeyCount    int
}

// RolledBack reports whether the release is withheld from serving.
func (r ReleaseDetail) RolledBack() bool { return r.RolledBackAt != nil }

// Bundle is one locale's materialised strings for one release.
type Bundle struct {
	ReleaseVersion int64
	LocaleCode     string
	// Strings is the flat key -> value JSON document, returned raw so it is not
	// decoded and re-encoded on the way through.
	Strings  []byte
	SHA256   string
	KeyCount int
	ByteSize int
}

type releaseRepository struct{ base }

func ProvideReleaseRepository(connector database.GORMConnector) ReleaseRepository {
	return &releaseRepository{base{connector: connector}}
}

// Version is allocated as max+1 inside the merge transaction, under the same
// advisory lock that serialises merges — so two concurrent merges cannot pick
// the same number. A sequence would also work, but would leave gaps on
// rollback, and release numbers are read by humans.
//
// A globally monotonic counter is only safe because u-l10n runs as a single
// global instance; under per-market deployment this would need a composite
// (market, version) key.
const createReleaseSQL = `
INSERT INTO releases (version, source, merge_request_id, created_by)
VALUES (COALESCE((SELECT max(version) FROM releases), 0) + 1, $1, $2, $3)
RETURNING id, version, source`

func (r *releaseRepository) Create(
	ctx context.Context, tx *gorm.DB, source string, mergeRequestID *int64, createdBy string,
) (Release, error) {
	var rel Release
	row := r.db(ctx, tx).Raw(createReleaseSQL, source, mergeRequestID, createdBy).Row()
	if err := row.Scan(&rel.ID, &rel.Version, &rel.Source); err != nil {
		return rel, fmt.Errorf("create release: %w", err)
	}
	return rel, nil
}

// materialiseBundleSQL computes sha256 and byte_size from the SAME bytes the
// serving path returns.
//
// The strings column is JSONB, and the OTA path serves `strings::text` —
// Postgres's re-serialisation, whose key order and spacing differ from Go's
// json.Marshal output. A fingerprint computed over the Go bytes would describe
// bytes no client ever receives: the ETag/304 dance would still work (the sha
// only has to be stable), but a client checksumming its download against the
// advertised sha256 would never match. Both derived from the one $3 parameter,
// in the one statement, so they cannot skew. jsonb's text form is canonical —
// identical content always re-serialises identically — which is what keeps the
// sha stable across merges that change nothing.
const materialiseBundleSQL = `
INSERT INTO release_bundles (release_id, locale_id, strings, sha256, key_count, byte_size)
VALUES ($1, $2, $3::jsonb,
        encode(sha256(convert_to(($3::jsonb)::text, 'UTF8')), 'hex'),
        $4,
        octet_length(($3::jsonb)::text))`

func (r *releaseRepository) MaterialiseBundle(
	ctx context.Context, tx *gorm.DB, releaseID int64, locale model.Locale, rows []ExportRow,
) error {
	// The bundle is the flutter key set with empty values INCLUDED — exactly
	// what assets/langs/<locale>.json holds today, so the OTA payload and the
	// asset bundled in the app binary are structurally identical and cannot
	// drift apart.
	strings := make(map[string]string, len(rows))
	for _, row := range rows {
		strings[row.Key] = row.Value
	}

	payload, err := json.Marshal(strings)
	if err != nil {
		return fmt.Errorf("marshal bundle for %s: %w", locale.Code, err)
	}

	err = r.db(ctx, tx).Exec(materialiseBundleSQL,
		releaseID, locale.ID, string(payload), len(strings)).Error
	if err != nil {
		return fmt.Errorf("materialise bundle for %s: %w", locale.Code, err)
	}
	return nil
}

func (r *releaseRepository) BundleSHA(
	ctx context.Context, tx *gorm.DB, releaseID int64, localeID int16,
) (string, error) {
	var sha string
	row := r.db(ctx, tx).Raw(
		`SELECT sha256 FROM release_bundles WHERE release_id = $1 AND locale_id = $2`,
		releaseID, localeID).Row()
	if err := row.Scan(&sha); err != nil {
		return "", fmt.Errorf("read bundle sha: %w", err)
	}
	return sha, nil
}

// ServableBundle is one locale's bundle from the newest eligible release.
type ServableBundle struct {
	ReleaseVersion int64
	Strings        []byte
	SHA256         string
	// KillSwitched reports that the newest release for this locale has been
	// rolled back and no earlier one is eligible.
	KillSwitched bool
}

// servableBundleSQL returns the newest release that is eligible for a client.
//
// Eligibility has two parts, both expressed in SQL so no caller can forget one:
//
//	rolled_back_at IS NULL     the kill switch
//	min_app_version <= client  the semver floor
//
// The floor is compared as an INTEGER TRIPLE, not as text: '4.9.0' > '4.10.0'
// lexically, which would withhold a release from exactly the clients it was
// meant for. NULL means every client is eligible.
//
// The r.project_id predicate is load-bearing for the query PLAN, not for
// correctness: a locale_id already names exactly one project (locales carries
// a UNIQUE (project_id, id) since V1.10), so the join could never actually
// cross a project boundary even without it. But V1.12 replaced
// releases_version_unique with a project_id-LED index
// (releases_project_version_unique / idx_releases_servable), and a leading
// column an equality predicate never touches cannot be used to satisfy
// `ORDER BY version DESC` once more than one project_id value exists in the
// table — the planner falls back to scanning and sorting every eligible
// release across every project before applying LIMIT 1. Naming the project
// explicitly is what lets the planner use that index for the ordering again.
const servableBundleSQL = `
WITH client AS (
    SELECT COALESCE(NULLIF($2, ''), '0.0.0') AS v
)
SELECT r.version, rb.strings::text, rb.sha256
  FROM releases r
  JOIN release_bundles rb ON rb.release_id = r.id
 WHERE rb.locale_id = $1
   AND r.project_id = (SELECT project_id FROM locales WHERE id = $1)
   AND r.rolled_back_at IS NULL
   AND (
        r.min_app_version IS NULL
     OR string_to_array(r.min_app_version, '.')::int[]
        <= string_to_array((SELECT v FROM client), '.')::int[]
   )
 ORDER BY r.version DESC
 LIMIT 1`

// ServableBundle returns what an OTA client should receive.
//
// appVersion may be empty, which is treated as 0.0.0 — the most conservative
// reading, so a client that omits the header only ever receives releases with
// no floor at all.
func (r *releaseRepository) ServableBundle(
	ctx context.Context, tx *gorm.DB, localeID int16, appVersion string,
) (ServableBundle, error) {
	var b ServableBundle

	row := r.db(ctx, tx).Raw(servableBundleSQL, localeID, appVersion).Row()
	switch err := row.Scan(&b.ReleaseVersion, &b.Strings, &b.SHA256); {
	case err == nil:
		return b, nil
	case isNoRows(err):
		// Nothing eligible. Distinguish "rolled back" from "never released":
		// the first tells a client to clear its cache, the second is simply a
		// service with no releases yet.
		var anyRolledBack bool
		row = r.db(ctx, tx).Raw(`
			SELECT EXISTS (
			    SELECT 1 FROM releases r
			      JOIN release_bundles rb ON rb.release_id = r.id
			     WHERE rb.locale_id = $1 AND r.rolled_back_at IS NOT NULL)`, localeID).Row()
		if scanErr := row.Scan(&anyRolledBack); scanErr != nil {
			return b, fmt.Errorf("check rollback state: %w", scanErr)
		}
		b.KillSwitched = anyRolledBack
		return b, ErrNotFound
	default:
		return b, fmt.Errorf("read servable bundle: %w", err)
	}
}

// createPublishSQL allocates a manual release.
//
// The version comes from the same max+1 expression a merge uses, so both kinds
// share one monotonic sequence — a portal showing release 41 next to release 40
// must be able to say those are adjacent.
//
// Unlike a merge, this runs under no advisory lock, so two simultaneous
// publishes could pick the same number. releases_version_unique is what stops
// that, and the loser gets a 23505 the caller can retry. Serialising manual
// publishes globally would be borrowing a lock the merge holds for a reason that
// does not apply here.
const createPublishSQL = `
INSERT INTO releases (version, source, notes, min_app_version, created_by)
VALUES (COALESCE((SELECT max(version) FROM releases), 0) + 1, 'publish', $1, $2, $3)
RETURNING id, version, source`

// ErrReleaseVersionRace is returned when two simultaneous publishes allocate
// the same version number and releases_version_unique refuses the loser.
//
// An exported sentinel, following ErrLiveMergeRequestExists: this is the
// retry-shaped outcome the comment above promises, not a fault. The portal
// answers 409 and the caller simply publishes again — unclassified it would
// surface as a driver error and page an engineer for a race that resolves
// itself.
var ErrReleaseVersionRace = errors.New(
	"another release took this version number; retry the publish")

func (r *releaseRepository) CreatePublish(
	ctx context.Context, tx *gorm.DB, notes string, minAppVersion *string, createdBy string,
) (Release, error) {
	var rel Release
	row := r.db(ctx, tx).Raw(createPublishSQL, notes, minAppVersion, createdBy).Row()
	if err := row.Scan(&rel.ID, &rel.Version, &rel.Source); err != nil {
		if isUniqueViolation(err) {
			return rel, fmt.Errorf("create publish release: %w", ErrReleaseVersionRace)
		}
		return rel, fmt.Errorf("create publish release: %w", err)
	}
	return rel, nil
}

// releaseDetailColumns and the bundle summary in one statement.
//
// The counts come from correlated subqueries rather than a join with GROUP BY,
// because a release whose bundles failed to materialise must still appear — an
// inner join would hide exactly the release somebody is investigating.
const releaseDetailSQL = `
SELECT r.id, r.version, r.source, r.notes, r.merge_request_id, r.min_app_version,
       r.created_by, r.created_at, r.rolled_back_at, r.rolled_back_by,
       (SELECT count(*)             FROM release_bundles rb WHERE rb.release_id = r.id),
       (SELECT COALESCE(max(rb.key_count), 0) FROM release_bundles rb WHERE rb.release_id = r.id)
  FROM releases r`

func scanReleaseDetail(row interface{ Scan(...interface{}) error }) (ReleaseDetail, error) {
	var d ReleaseDetail
	err := row.Scan(&d.ID, &d.Version, &d.Source, &d.Notes, &d.MergeRequestID,
		&d.MinAppVersion, &d.CreatedBy, &d.CreatedAt, &d.RolledBackAt,
		&d.RolledBackBy, &d.LocaleCount, &d.KeyCount)
	return d, err
}

func (r *releaseRepository) List(
	ctx context.Context, tx *gorm.DB, limit, offset int,
) ([]ReleaseDetail, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := r.db(ctx, tx).Raw(
		releaseDetailSQL+` ORDER BY r.version DESC LIMIT ? OFFSET ?`, limit, offset).Rows()
	if err != nil {
		return nil, fmt.Errorf("list releases: %w", err)
	}
	defer rows.Close()

	var out []ReleaseDetail
	for rows.Next() {
		d, err := scanReleaseDetail(rows)
		if err != nil {
			return nil, fmt.Errorf("scan release: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TODO(plan-2): this WHERE has no project_id, and V1.12 replaced the global
// releases_version_unique with releases_project_version_unique, so a version
// number no longer names at most one release. `.Row()` takes whichever the
// planner reaches first — first-row-wins, no error — which means the portal
// could show one project's release detail under another project's version.
// Rollback below also calls it, both to decide between "no such release" and
// "already rolled back" and to build the response it returns on success, so a
// wrong row here misreports what was just rolled back as well.
// Not reachable until a second project publishes; it needs a
// project_id parameter and an AND project_id = $N before one does.
func (r *releaseRepository) ByVersion(
	ctx context.Context, tx *gorm.DB, version int64,
) (ReleaseDetail, error) {
	row := r.db(ctx, tx).Raw(releaseDetailSQL+` WHERE r.version = ?`, version).Row()

	d, err := scanReleaseDetail(row)
	switch {
	case err == nil:
		return d, nil
	case isNoRows(err):
		return d, fmt.Errorf("release %d: %w", version, ErrNotFound)
	default:
		return d, fmt.Errorf("read release %d: %w", version, err)
	}
}

// rollbackSQL pulls the kill switch, and only from the not-yet-pulled state.
//
// `rolled_back_at IS NULL` in the predicate rather than a check in a service:
// rolled_back_by answers the only question anyone asks after an incident, and a
// second rollback overwriting it with a different name would erase that answer.
// The schema's rollback_consistency_check requires both columns together, which
// is why they are set in one statement.
//
// TODO(plan-2): THIS IS THE MOST DESTRUCTIVE UNSCOPED STATEMENT IN THE
// CODEBASE. The WHERE names no project and the UPDATE carries no LIMIT.
// V1.12 dropped releases_version_unique for releases_project_version_unique,
// and V1.12's own header states that release versions restart per project —
// YouBiz's first release is 1, not YouTrip's next number — so version numbers
// are expected to COLLIDE across projects rather than merely be able to.
// The moment a second project publishes, `POST /releases/1/rollback` rolls back
// EVERY project's release 1 in one statement. Nothing downstream notices:
// RowsAffected is 2, the `== 0` branch below is the only check there is, and
// the handler answers 200. Rollback is the kill switch — it withdraws shipped
// copy from every client on that release and makes OTA answer 410 — so the
// blast radius of this is another product's live app, not a wrong row.
// Unreachable today only because one project exists. Scoping this (a
// project_id parameter and an AND project_id = $N) is the FIRST thing Plan 2
// must do, before anything creates a second project.
const rollbackSQL = `
UPDATE releases
   SET rolled_back_at = now(), rolled_back_by = $2
 WHERE version = $1 AND rolled_back_at IS NULL`

func (r *releaseRepository) Rollback(
	ctx context.Context, tx *gorm.DB, version int64, actor string,
) (ReleaseDetail, error) {
	db := r.db(ctx, tx)

	res := db.Exec(rollbackSQL, version, actor)
	if res.Error != nil {
		return ReleaseDetail{}, fmt.Errorf("roll back release %d: %w", version, res.Error)
	}

	if res.RowsAffected == 0 {
		// Two different facts behind one missing row, and a caller must be able
		// to tell them apart: there is no such release, or the switch was
		// already pulled.
		existing, err := r.ByVersion(ctx, tx, version)
		if err != nil {
			return ReleaseDetail{}, err
		}
		return existing, fmt.Errorf("release %d: %w", version, ErrAlreadyRolledBack)
	}

	return r.ByVersion(ctx, tx, version)
}

// bundleForSQL reads a materialised bundle.
//
// Joined on the release VERSION rather than its id, because that is the number
// a human types. The strings column is cast to text and handed back raw: the
// portal renders it, and decoding a 60KB JSON document only to re-encode it
// would be work nobody asked for.
const bundleForSQL = `
SELECT r.version, l.code, rb.strings::text, rb.sha256, rb.key_count, rb.byte_size
  FROM releases r
  JOIN release_bundles rb ON rb.release_id = r.id
  JOIN locales l          ON l.id = rb.locale_id
 WHERE r.version = $1 AND rb.locale_id = $2`

func (r *releaseRepository) BundleFor(
	ctx context.Context, tx *gorm.DB, version int64, localeID int16,
) (Bundle, error) {
	var b Bundle
	row := r.db(ctx, tx).Raw(bundleForSQL, version, localeID).Row()

	switch err := row.Scan(&b.ReleaseVersion, &b.LocaleCode, &b.Strings,
		&b.SHA256, &b.KeyCount, &b.ByteSize); {
	case err == nil:
		return b, nil
	case isNoRows(err):
		return b, fmt.Errorf("bundle for release %d locale %d: %w", version, localeID, ErrNotFound)
	default:
		return b, fmt.Errorf("read bundle for release %d: %w", version, err)
	}
}
