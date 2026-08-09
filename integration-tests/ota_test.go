package integrationtests

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/repository"
)

// servable asks the REAL repository method — the one the OTA handler serves
// from — which release a client would receive. An earlier version of this
// helper duplicated servableBundleSQL inline and returned (0, false) on ANY
// error, so an input-dependent SQL failure was indistinguishable from "nothing
// to serve". Only repository.ErrNotFound means not-servable; anything else is
// a broken query and fails the test loudly.
func servable(t *testing.T, localeID int16, appVersion string) (version int64, found bool) {
	t.Helper()

	repo := repository.ProvideReleaseRepository(testGORM(t))
	bundle, err := repo.ServableBundle(context.Background(), nil, localeID, appVersion)
	switch {
	case err == nil:
		return bundle.ReleaseVersion, true
	case errors.Is(err, repository.ErrNotFound):
		return 0, false
	default:
		t.Fatalf("ServableBundle(locale %d, app %q): %v", localeID, appVersion, err)
		return 0, false // unreachable
	}
}

func newRelease(t *testing.T, localeID int16, minAppVersion interface{}, rolledBack bool) int64 {
	t.Helper()
	var id, version int64
	require.NoError(t, testDB.QueryRow(`
		INSERT INTO releases (version, source, created_by, min_app_version, rolled_back_at, rolled_back_by)
		VALUES (COALESCE((SELECT max(version) FROM releases), 0) + 1, 'merge', 'a@you.co', $1,
		        CASE WHEN $2 THEN now() ELSE NULL END,
		        CASE WHEN $2 THEN 'admin@you.co' ELSE NULL END)
		RETURNING id, version`, minAppVersion, rolledBack).Scan(&id, &version))

	_, err := testDB.Exec(`
		INSERT INTO release_bundles (release_id, locale_id, strings, sha256, key_count, byte_size)
		VALUES ($1, $2, '{"k":"v"}'::jsonb, md5(random()::text) || md5(random()::text), 1, 9)`,
		id, localeID)
	require.NoError(t, err)
	return version
}

// TestOTAServesNewestEligibleRelease covers the kill switch and the version
// floor together, since both are eligibility rules on the same query.
func TestOTAServesNewestEligibleRelease(t *testing.T) {
	enAU := localeID(t, "en-AU") // a locale the other tests do not touch

	v1 := newRelease(t, enAU, nil, false)
	got, found := servable(t, enAU, "4.12.0")
	require.True(t, found)
	assert.Equal(t, v1, got, "the only release is served")

	v2 := newRelease(t, enAU, nil, false)
	got, _ = servable(t, enAU, "4.12.0")
	assert.Equal(t, v2, got, "the NEWEST release wins")

	// Roll v2 back: the kill switch must fall back to v1, not serve nothing.
	_, err := testDB.Exec(`
		UPDATE releases SET rolled_back_at = now(), rolled_back_by = 'admin@you.co'
		 WHERE version = $1`, v2)
	require.NoError(t, err)

	got, found = servable(t, enAU, "4.12.0")
	require.True(t, found, "a rolled-back release must not blind clients to earlier ones")
	assert.Equal(t, v1, got)
}

// TestOTAVersionFloorComparesNumerically is the one that would pass a casual
// review and be wrong in production.
//
// '4.9.0' > '4.10.0' as TEXT. Comparing lexically would withhold a release from
// exactly the clients it was published for — and only from users on 4.10+, so it
// would look fine in testing and fail after the next app release.
func TestOTAVersionFloorComparesNumerically(t *testing.T) {
	msMY := localeID(t, "ms-MY")

	floor := newRelease(t, msMY, "4.10.0", false)

	got, found := servable(t, msMY, "4.10.0")
	require.True(t, found, "a client exactly at the floor is eligible")
	assert.Equal(t, floor, got)

	got, found = servable(t, msMY, "4.12.0")
	require.True(t, found, "4.12.0 is above 4.10.0 numerically")
	assert.Equal(t, floor, got)

	_, found = servable(t, msMY, "4.9.0")
	assert.False(t, found,
		"4.9.0 is BELOW 4.10.0 numerically, even though it sorts above it as text")

	_, found = servable(t, msMY, "")
	assert.False(t, found,
		"a client that omits its version is treated as 0.0.0 — the conservative reading")
}
