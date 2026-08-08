package integrationtests

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-common-components/database"

	"github.com/yougroupteam/u-l10n/pkg/repository"
	"github.com/yougroupteam/u-l10n/pkg/service/releasesvc"
)

func newReleaseSvc(t *testing.T) *releasesvc.Service {
	t.Helper()
	conn := testGORM(t)
	return releasesvc.ProvideService(
		database.ProvideTransactional(conn),
		repository.ProvideReleaseRepository(conn),
		repository.ProvideLocaleRepository(conn),
		repository.ProvideExportRowReader(conn),
		repository.ProvideAuditRepository(conn),
	)
}

// publishAndQuarantine publishes and rolls the result back afterwards.
//
// Same reasoning as mergeAndQuarantine: a publish materialises a bundle for
// EVERY locale and becomes the newest servable release globally, and the OTA
// tests assert on exactly which release a client receives. The package never
// resets between cases, so without this the OTA suite would start failing for
// reasons unrelated to OTA.
func publishAndQuarantine(
	t *testing.T, svc *releasesvc.Service, notes, minAppVersion string,
) (repository.ReleaseDetail, error) {
	t.Helper()

	published, err := svc.Publish(context.Background(), notes, minAppVersion, testActor, "req-1")
	if published.Version > 0 {
		version := published.Version
		t.Cleanup(func() {
			_, cleanupErr := testDB.Exec(`
				UPDATE releases SET rolled_back_at = now(), rolled_back_by = 'test-cleanup'
				 WHERE version = $1 AND rolled_back_at IS NULL`, version)
			require.NoError(t, cleanupErr)
		})
	}
	return published, err
}

// TestPublishMaterialisesEveryLocaleInOneTransaction.
//
// A release that became servable a moment before its bundles existed would hand
// an OTA client an empty document behind an ETag — and the client would cache it
// and stop asking.
func TestPublishMaterialisesEveryLocaleInOneTransaction(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	releases := newReleaseSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "publish"))
	setPortalValue(t, keys, key.ID, "en-SG", "published copy", 0)

	published, err := publishAndQuarantine(t, releases, "manual fix", "")
	require.NoError(t, err)

	assert.Equal(t, "publish", published.Source)
	assert.Nil(t, published.MergeRequestID,
		"a manual publish has no merge behind it, and that null is the point")
	assert.Equal(t, 6, published.LocaleCount)
	assert.Nil(t, published.MinAppVersion)

	// The bundle really holds the value, read back from storage rather than
	// re-derived.
	bundle, err := releases.Bundle(ctx, published.Version, "en-SG")
	require.NoError(t, err)
	assert.Equal(t, "en-SG", bundle.LocaleCode)
	assert.Regexp(t, "^[0-9a-f]{64}$", bundle.SHA256)

	var strings map[string]string
	require.NoError(t, json.Unmarshal(bundle.Strings, &strings))
	assert.Equal(t, "published copy", strings[key.Name])
}

// TestMinAppVersionMustBeThreeNumericComponents.
//
// The serving query casts the floor to an int[]. A malformed value fails at READ
// time, on the unauthenticated OTA path, for every client asking for that locale
// — so a bad publish would take string delivery down rather than merely being
// rejected. Two components are refused for a subtler reason: Postgres compares
// arrays element-wise then by length, so ARRAY[4,12] < ARRAY[4,12,0] and a
// two-component floor silently admits clients the publisher meant to exclude.
func TestMinAppVersionMustBeThreeNumericComponents(t *testing.T) {
	releases := newReleaseSvc(t)

	for _, raw := range []string{"4.12", "4.12.0-beta", "v4.12.0", "4.12.0.1", "latest", "4..0"} {
		t.Run(raw, func(t *testing.T) {
			_, err := releases.Publish(context.Background(), "", raw, testActor, "req-1")
			require.Error(t, err)
			assert.ErrorIs(t, err, releasesvc.ErrBadRequest)
		})
	}

	t.Run("a well-formed floor is stored and the OTA query can read it", func(t *testing.T) {
		published, err := publishAndQuarantine(t, releases, "", "4.12.0")
		require.NoError(t, err)
		require.NotNil(t, published.MinAppVersion)
		assert.Equal(t, "4.12.0", *published.MinAppVersion)

		// The proof that the cast survives: this is the same comparison the
		// unauthenticated OTA endpoint performs.
		var eligible bool
		require.NoError(t, testDB.QueryRow(`
			SELECT string_to_array(min_app_version, '.')::int[]
			    <= string_to_array('4.12.0', '.')::int[]
			  FROM releases WHERE version = $1`, published.Version).Scan(&eligible))
		assert.True(t, eligible)
	})
}

// TestRollbackIsTheKillSwitchAndNotAnUndo.
//
// It withholds the release from serving. It does NOT revert the values: master
// keeps whatever the merge applied. A rollback happens in a hurry and must not
// also rewrite the corpus.
func TestRollbackIsTheKillSwitchAndNotAnUndo(t *testing.T) {
	ctx := context.Background()
	keys := newKeySvc(t)
	releases := newReleaseSvc(t)

	key := createTestKey(t, keys, uniqueName(t, "rollback"))
	setPortalValue(t, keys, key.ID, "en-SG", "shipped then withdrawn", 0)

	published, err := publishAndQuarantine(t, releases, "", "")
	require.NoError(t, err)

	rolled, err := releases.Rollback(ctx, published.Version, "oncall@you.co", "req-1")
	require.NoError(t, err)
	assert.True(t, rolled.RolledBack())
	require.NotNil(t, rolled.RolledBackBy)
	assert.Equal(t, "oncall@you.co", *rolled.RolledBackBy)

	// The values are untouched.
	current, err := keys.Get(ctx, key.ID, "", []string{"en-SG"})
	require.NoError(t, err)
	assert.Equal(t, "shipped then withdrawn",
		current.Keys[0].Values[current.Locales[0].ID].Value)

	t.Run("rolling back twice is refused, not repeated", func(t *testing.T) {
		// rolled_back_by answers the only question anybody asks afterwards. A
		// second write would replace that name with whoever pressed it last.
		_, err := releases.Rollback(ctx, published.Version, "someone-else@you.co", "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, releasesvc.ErrAlreadyRolledBack)

		still, err := releases.Get(ctx, published.Version)
		require.NoError(t, err)
		require.NotNil(t, still.RolledBackBy)
		assert.Equal(t, "oncall@you.co", *still.RolledBackBy,
			"the first responder's name must survive")
	})

	t.Run("an unknown release is a 404, not a conflict", func(t *testing.T) {
		_, err := releases.Rollback(ctx, 999999, testActor, "req-1")
		require.Error(t, err)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})
}

// TestReleaseHistoryIsNewestFirstAndCarriesItsProvenance.
func TestReleaseHistoryIsNewestFirstAndCarriesItsProvenance(t *testing.T) {
	ctx := context.Background()
	releases := newReleaseSvc(t)

	first, err := publishAndQuarantine(t, releases, "first", "")
	require.NoError(t, err)
	second, err := publishAndQuarantine(t, releases, "second", "")
	require.NoError(t, err)
	assert.Greater(t, second.Version, first.Version, "versions are monotonic across sources")

	list, err := releases.List(ctx, 10, 0)
	require.NoError(t, err)
	require.NotEmpty(t, list)
	assert.Equal(t, second.Version, list[0].Version, "newest first")

	for i := 1; i < len(list); i++ {
		assert.Less(t, list[i].Version, list[i-1].Version)
	}
}

// TestReleaseReadRefusals.
func TestReleaseReadRefusals(t *testing.T) {
	ctx := context.Background()
	releases := newReleaseSvc(t)

	published, err := publishAndQuarantine(t, releases, "", "")
	require.NoError(t, err)

	t.Run("unknown release", func(t *testing.T) {
		_, err := releases.Get(ctx, 999999)
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})

	t.Run("unknown locale is a bad request, not a missing bundle", func(t *testing.T) {
		// Locales are reference data seeded by migration, so an unrecognised
		// code is a typo rather than something that could exist later.
		_, err := releases.Bundle(ctx, published.Version, "fr-FR")
		require.Error(t, err)
		assert.ErrorIs(t, err, releasesvc.ErrBadRequest)
	})

	t.Run("a real locale on a release that has no bundle is a 404", func(t *testing.T) {
		var version int64
		require.NoError(t, testDB.QueryRow(`
			INSERT INTO releases (version, source, created_by)
			VALUES (COALESCE((SELECT max(version) FROM releases), 0) + 1, 'import', 'importer')
			RETURNING version`).Scan(&version))

		_, err := releases.Bundle(ctx, version, "en-SG")
		assert.ErrorIs(t, err, repository.ErrNotFound)
	})

	t.Run("non-positive versions and limits are caller errors", func(t *testing.T) {
		_, err := releases.Get(ctx, 0)
		assert.ErrorIs(t, err, releasesvc.ErrBadRequest)

		_, err = releases.List(ctx, 100000, 0)
		assert.ErrorIs(t, err, releasesvc.ErrBadRequest)

		_, err = releases.List(ctx, 10, -1)
		assert.ErrorIs(t, err, releasesvc.ErrBadRequest)
	})
}

// TestPublishAndRollbackAreAudited: both are exactly the actions an incident
// review has to be able to attribute.
func TestPublishAndRollbackAreAudited(t *testing.T) {
	ctx := context.Background()
	releases := newReleaseSvc(t)

	published, err := publishAndQuarantine(t, releases, "audited publish", "")
	require.NoError(t, err)
	_, err = releases.Rollback(ctx, published.Version, "oncall@you.co", "req-1")
	require.NoError(t, err)

	events := auditFor(t, "release:"+itoa(published.Version))
	var actions []string
	for _, e := range events {
		actions = append(actions, e.Action)
	}
	assert.Equal(t, []string{
		repository.ActionReleasePublish, repository.ActionReleaseRollback,
	}, actions)
	assert.Equal(t, "oncall@you.co", events[1].Actor)
}

// TestARolledBackReleaseStopsBeingServed.
//
// The end of the chain: the kill switch has to reach the unauthenticated OTA
// path, or it is a database column nobody consults.
func TestARolledBackReleaseStopsBeingServed(t *testing.T) {
	ctx := context.Background()
	releases := newReleaseSvc(t)
	repo := repository.ProvideReleaseRepository(testGORM(t))

	enTH := localeID(t, "en-TH") // a locale the OTA tests do not touch

	published, err := publishAndQuarantine(t, releases, "", "")
	require.NoError(t, err)

	served, err := repo.ServableBundle(ctx, nil, enTH, "4.12.0")
	require.NoError(t, err)
	assert.Equal(t, published.Version, served.ReleaseVersion)

	_, err = releases.Rollback(ctx, published.Version, "oncall@you.co", "req-1")
	require.NoError(t, err)

	_, err = repo.ServableBundle(ctx, nil, enTH, "4.12.0")
	require.ErrorIs(t, err, repository.ErrNotFound,
		"a rolled-back release must stop being served")
}
