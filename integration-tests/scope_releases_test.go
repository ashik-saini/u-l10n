package integrationtests

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReleaseVersionsRestartPerProject: YouBiz's first release is 1, not
// YouTrip's next number. releases_project_version_unique is the constraint
// that makes this possible; a plain releases_version_unique would refuse the
// second project's version 9001 outright.
func TestReleaseVersionsRestartPerProject(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('relscope', 'Release Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, cleanupErr)
	})

	// Cleanup for the rows below is registered after the project's, so it
	// runs FIRST (t.Cleanup is LIFO): releases_project_fkey carries no
	// ON DELETE CASCADE, and leaving a release behind would make the project
	// delete above fail silently in Cleanup.
	_, err := testDB.Exec(
		`INSERT INTO releases (project_id, version, source, created_by)
		 VALUES (1, 9001, 'publish', 'test@you.co')`)
	require.NoError(t, err)
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM releases WHERE project_id = 1 AND version = 9001`)
		require.NoError(t, cleanupErr)
	})

	_, err = testDB.Exec(
		`INSERT INTO releases (project_id, version, source, created_by)
		 VALUES ($1, 9001, 'publish', 'test@you.co')`, otherProject)
	require.NoError(t, err, "the same version number in another project must be allowed")
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM releases WHERE project_id = $1 AND version = 9001`, otherProject)
		require.NoError(t, cleanupErr)
	})

	_, err = testDB.Exec(
		`INSERT INTO releases (project_id, version, source, created_by)
		 VALUES (1, 9001, 'publish', 'test@you.co')`)
	requireRejected(t, err, "duplicate version within one project")
}

// TestCrossProjectReleaseBundleIsRefused proves release_bundles_release_fkey
// and release_bundles_locale_fkey individually. A release_bundle is the join
// between a release and a locale; each side must independently name a row in
// the SAME project, or the OTA endpoint could serve one project's strings
// under another project's release.
func TestCrossProjectReleaseBundleIsRefused(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('bundlescope', 'Bundle Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, cleanupErr)
	})

	// A locale belonging to the other project. A code distinct from any real
	// YouTrip locale avoids making localeID's unscoped `WHERE code = $1`
	// lookup ambiguous for the rest of this test (and any test sharing the
	// package) while this fixture is alive.
	var otherLocale int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO locales (project_id, code, flutter_dir, android_values_dir, ios_lproj, sort_order)
		 VALUES ($1, 'zz-BUNDLESCOPE', 'zz_scope', 'values-zz-scope', 'zz-scope.lproj', 1)
		 RETURNING id`, otherProject).Scan(&otherLocale))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM locales WHERE id = $1`, otherLocale)
		require.NoError(t, cleanupErr)
	})

	// A release belonging to YouTrip. 9200 rather than 9001/9002/9100: those
	// literals are already claimed, uncleaned, by schema_test.go.
	var youtripRelease int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO releases (project_id, version, source, created_by)
		 VALUES (1, 9200, 'publish', 'test@you.co') RETURNING id`).Scan(&youtripRelease))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM releases WHERE id = $1`, youtripRelease)
		require.NoError(t, cleanupErr)
	})

	t.Run("YouTrip release bundled for another project's locale", func(t *testing.T) {
		// (project_id, release_id) = (1, youtripRelease) is a real row in
		// releases, so release_bundles_release_fkey is satisfied.
		// (project_id, locale_id) = (1, otherLocale) is not — otherLocale
		// belongs to otherProject — so only release_bundles_locale_fkey can be
		// the one refusing this row.
		_, err := testDB.Exec(
			`INSERT INTO release_bundles (project_id, release_id, locale_id, strings, sha256, key_count, byte_size)
			 VALUES (1, $1, $2, '{}'::jsonb, $3, 0, 2)`,
			youtripRelease, otherLocale, assetSHA("bundle-cross-locale"))
		requireRejected(t, err, "YouTrip release bundled for another project's locale")
	})

	t.Run("another project claiming a YouTrip release", func(t *testing.T) {
		// (project_id, locale_id) = (otherProject, otherLocale) is a real row
		// in locales, so release_bundles_locale_fkey is satisfied.
		// (project_id, release_id) = (otherProject, youtripRelease) is not —
		// youtripRelease belongs to project 1 — so only
		// release_bundles_release_fkey can be the one refusing this row.
		_, err := testDB.Exec(
			`INSERT INTO release_bundles (project_id, release_id, locale_id, strings, sha256, key_count, byte_size)
			 VALUES ($1, $2, $3, '{}'::jsonb, $4, 0, 2)`,
			otherProject, youtripRelease, otherLocale, assetSHA("bundle-cross-release"))
		requireRejected(t, err, "another project claiming a YouTrip release via release_bundles")
	})
}

// TestCrossProjectKeyAssetIsRefused proves key_assets_key_fkey and
// key_assets_asset_fkey individually — the same shape as
// TestCrossProjectReleaseBundleIsRefused, for the other pair of tables V1.12
// scopes.
func TestCrossProjectKeyAssetIsRefused(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('assetscope', 'Asset Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, cleanupErr)
	})

	// An asset belonging to the other project.
	var otherAsset int64
	sha := assetSHA("key-asset-cross-project")
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO assets (project_id, s3_key, sha256, filename, content_type, bytes, uploaded_by)
		 VALUES ($1, $2, $3, 'scope.png', 'image/png', 100, 'test@you.co')
		 RETURNING id`,
		otherProject, "screenshots/"+sha[0:2]+"/"+sha[2:4]+"/"+sha, sha).Scan(&otherAsset))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM assets WHERE id = $1`, otherAsset)
		require.NoError(t, cleanupErr)
	})

	// A key belonging to YouTrip. sort_index has no default (see V1.00), so it
	// must be supplied even though this test has nothing to do with ordering.
	var youtripKey int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO keys (project_id, name, platforms, status, sort_index)
		 VALUES (1, 'scope.test.assetkey', '{"flutter"}', 'active', $1)
		 RETURNING id`, nextSortIndex()).Scan(&youtripKey))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM keys WHERE id = $1`, youtripKey)
		require.NoError(t, cleanupErr)
	})

	t.Run("YouTrip key linked to another project's asset", func(t *testing.T) {
		// (project_id, key_id) = (1, youtripKey) is real, so key_assets_key_fkey
		// is satisfied. (project_id, asset_id) = (1, otherAsset) is not —
		// otherAsset belongs to otherProject — so only key_assets_asset_fkey can
		// be the one refusing this row.
		_, err := testDB.Exec(
			`INSERT INTO key_assets (project_id, key_id, asset_id, created_by)
			 VALUES (1, $1, $2, 'test@you.co')`,
			youtripKey, otherAsset)
		requireRejected(t, err, "YouTrip key linked to another project's asset")
	})

	t.Run("another project claiming a YouTrip key via key_assets", func(t *testing.T) {
		// (project_id, asset_id) = (otherProject, otherAsset) is real, so
		// key_assets_asset_fkey is satisfied. (project_id, key_id) =
		// (otherProject, youtripKey) is not — youtripKey belongs to project 1 —
		// so only key_assets_key_fkey can be the one refusing this row.
		_, err := testDB.Exec(
			`INSERT INTO key_assets (project_id, key_id, asset_id, created_by)
			 VALUES ($1, $2, $3, 'test@you.co')`,
			otherProject, youtripKey, otherAsset)
		requireRejected(t, err, "another project claiming a YouTrip key via key_assets")
	})
}

// TestAssetSha256IsUniquePerProjectNotGlobally: two products may legitimately
// upload the identical bytes — a shared icon, a shared onboarding screenshot —
// and that must succeed, while a project may still never store its own bytes
// twice. This is exactly the case U1.12 warns about: restoring the global
// assets_sha256_unique would make the second INSERT below impossible.
func TestAssetSha256IsUniquePerProjectNotGlobally(t *testing.T) {
	var otherProject int16
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO projects (code, name) VALUES ('shascope', 'Sha Scope')
		 RETURNING id`).Scan(&otherProject))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM projects WHERE id = $1`, otherProject)
		require.NoError(t, cleanupErr)
	})

	sha := assetSHA("shared-across-projects")

	var firstAsset int64
	require.NoError(t, testDB.QueryRow(
		`INSERT INTO assets (project_id, s3_key, sha256, filename, content_type, bytes, uploaded_by)
		 VALUES (1, 'screenshots/aa/bb/shared-1.png', $1, 'shared.png', 'image/png', 100, 'test@you.co')
		 RETURNING id`, sha).Scan(&firstAsset))
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM assets WHERE id = $1`, firstAsset)
		require.NoError(t, cleanupErr)
	})

	var secondAsset int64
	err := testDB.QueryRow(
		`INSERT INTO assets (project_id, s3_key, sha256, filename, content_type, bytes, uploaded_by)
		 VALUES ($1, 'screenshots/aa/bb/shared-2.png', $2, 'shared.png', 'image/png', 100, 'test@you.co')
		 RETURNING id`, otherProject, sha).Scan(&secondAsset)
	require.NoError(t, err, "the same sha256 in another project must be allowed")
	t.Cleanup(func() {
		_, cleanupErr := testDB.Exec(`DELETE FROM assets WHERE id = $1`, secondAsset)
		require.NoError(t, cleanupErr)
	})

	_, err = testDB.Exec(
		`INSERT INTO assets (project_id, s3_key, sha256, filename, content_type, bytes, uploaded_by)
		 VALUES (1, 'screenshots/aa/bb/shared-3.png', $1, 'shared.png', 'image/png', 100, 'test@you.co')`,
		sha)
	requireRejected(t, err, "duplicate sha256 within one project")
}
