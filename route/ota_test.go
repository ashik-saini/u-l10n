package route

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestMatchesETag covers the If-None-Match forms a real client or CDN sends.
//
// Comparing the raw header against our tag would fail every case below except
// the first, and the 304 path — the entire point of the OTA design — would
// silently never fire. The failure mode is invisible: everything still works,
// it just ships 60-80KB on every app launch instead of a few hundred bytes.
func TestMatchesETag(t *testing.T) {
	const etag = `"abc123"`

	cases := []struct {
		name, header string
		want         bool
	}{
		{"exact match", `"abc123"`, true},
		{"no header", "", false},
		{"different tag", `"def456"`, false},
		{"wildcard", "*", true},
		{"weak validator", `W/"abc123"`, true},
		{"multiple, ours last", `"x", "y", "abc123"`, true},
		{"multiple, ours absent", `"x", "y"`, false},
		{"multiple with spacing and weak", `W/"x" ,  W/"abc123"`, true},
		{"unquoted junk", `abc123`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, matchesETag(tc.header, etag))
		})
	}
}

func TestSplitAndTrim(t *testing.T) {
	assert.Equal(t, []string{`"a"`, `"b"`}, splitAndTrim(`"a", "b"`, ','))
	assert.Equal(t, []string{`"a"`}, splitAndTrim(`  "a"  `, ','))
	assert.Empty(t, splitAndTrim(`  ,  `, ','))
}

func TestTrimWeakPrefix(t *testing.T) {
	assert.Equal(t, `"abc"`, trimWeakPrefix(`W/"abc"`))
	assert.Equal(t, `"abc"`, trimWeakPrefix(`"abc"`))
	// A tag that merely starts with W must not be mangled.
	assert.Equal(t, `"W123"`, trimWeakPrefix(`"W123"`))
}
