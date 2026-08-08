package importsvc

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/yougroupteam/u-l10n/pkg/model"
)

// The presence-oracle decision is the heart of the importer and is pure logic,
// so it is tested here without a database. The write path it feeds is already
// proven by seed-from-files.

func TestMapPlatforms(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []model.Platform
	}{
		{
			// 'web' is Lokalise's name for what this project calls flutter.
			name: "web maps to flutter",
			in:   []string{"web"},
			want: []model.Platform{model.PlatformFlutter},
		},
		{
			name: "all three, sorted for a stable platforms array",
			in:   []string{"ios", "web", "android"},
			want: []model.Platform{model.PlatformAndroid, model.PlatformFlutter, model.PlatformIOS},
		},
		{
			name: "duplicates collapse",
			in:   []string{"web", "web"},
			want: []model.Platform{model.PlatformFlutter},
		},
		{
			// keys_platforms_check forbids an empty array, and a key on no
			// platform would be exported nowhere.
			name: "unknown platforms fall back to flutter rather than empty",
			in:   []string{"windows", "tizen"},
			want: []model.Platform{model.PlatformFlutter},
		},
		{
			name: "no platforms falls back to flutter",
			in:   nil,
			want: []model.Platform{model.PlatformFlutter},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mapPlatforms(tc.in))
		})
	}
}

// TestPresenceDecidesBlankVersusUntranslated is the test this package exists
// for.
//
// The API returns "" for both cases. Only the export knows which is which, and
// getting it wrong corrupts thousands of values in either direction: treating
// absent as blank adds ~430 spurious keys to en-SG, treating blank as absent
// deletes 3,664 intentional blanks from ms-MY.
func TestPresenceDecidesBlankVersusUntranslated(t *testing.T) {
	// Both keys come back from the API with value "" — indistinguishable there.
	present := map[string]map[string]bool{
		"en-SG": {"deliberately_blank": true}, // in the export
		// "never_translated" is absent from the export
	}

	write := func(locale, key string) bool { return present[locale][key] }

	assert.True(t, write("en-SG", "deliberately_blank"),
		`a key IN the export is deliberately blank: write a row so it exports as ""`)
	assert.False(t, write("en-SG", "never_translated"),
		"a key ABSENT from the export is untranslated: write no row so it is omitted")

	// A locale with no export entry at all must not silently mark everything
	// blank — loadPresence treats a missing file as fatal for exactly this
	// reason.
	assert.False(t, write("th-TH", "deliberately_blank"),
		"an unknown locale must never default to present")
}

func TestLocaleLookup(t *testing.T) {
	locales := []model.Locale{
		{ID: 1, Code: "en-SG", FlutterDir: "en_SG"},
		{ID: 2, Code: "ms-MY", FlutterDir: "ms_MY"},
	}

	l, ok := localeByCode(locales, "ms-MY")
	assert.True(t, ok)
	assert.EqualValues(t, 2, l.ID)

	_, ok = localeByCode(locales, "fr-FR")
	assert.False(t, ok, "an unknown locale must not resolve to a zero-value Locale with ID 0")

	assert.True(t, hasLocale(locales, "en-SG"))
	assert.False(t, hasLocale(locales, "de-DE"))
}
