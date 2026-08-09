package export_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/yougroupteam/u-l10n/pkg/export"
	"github.com/yougroupteam/u-l10n/pkg/parse"
)

// TestXMLPreservesAndroidUnicodeEscapes covers the \uXXXX escapes Android
// itself understands. The parser's default branch passes them through
// verbatim, so they sit in memory with a SINGLE backslash; doubling that
// backslash on export makes aapt render the escape as literal text instead of
// the emoji. Eight committed statements (values-am, values-au, values-en-rMY,
// values-ms) depend on this.
func TestXMLPreservesAndroidUnicodeEscapes(t *testing.T) {
	const flagEscapes = `\uD83C\uDDF2\uD83C\uDDFE` // the MY flag, as aapt wants it

	t.Run("corpus-shaped value", func(t *testing.T) {
		out := string(export.XML([]export.Entry{{
			Key:   "nudge_referralmy_desc",
			Value: "Refer your " + flagEscapes + " kakis to get",
		}}))
		assert.Contains(t, out, ">Refer your "+flagEscapes+" kakis to get<",
			"the byte sequence must survive export unchanged")
		assert.NotContains(t, out, `\\uD83C`, "the escape must not be doubled")

		back, err := parse.XMLBytes([]byte(out))
		require.NoError(t, err)
		got, ok := back.Lookup("nudge_referralmy_desc")
		require.True(t, ok)
		assert.Equal(t, "Refer your "+flagEscapes+" kakis to get", got,
			"parse -> export -> parse must be lossless")
	})

	t.Run("live corpus statement", func(t *testing.T) {
		root := uMobileRoot(t)
		raw, err := os.ReadFile(filepath.Join(
			root, "android", "app", "src", "main", "res", "values-en-rMY", "strings.xml"))
		require.NoError(t, err)

		f, err := parse.XMLBytes(raw)
		require.NoError(t, err)
		v, ok := f.Lookup("nudge_referralmy_desc")
		require.True(t, ok)
		require.Contains(t, v, flagEscapes,
			"the parser keeps the Android escape verbatim in memory")

		out := string(export.XML(toExport(f)))
		assert.Contains(t, out, flagEscapes,
			"aapt needs the single-backslash form to render the emoji")
		assert.NotContains(t, out, `\\uD83C`)
	})

	t.Run("lookalikes are still doubled", func(t *testing.T) {
		// Anything that is not backslash-u plus EXACTLY four hex digits is a
		// literal backslash and must be doubled — mirroring the parser, whose
		// default branch preserved it verbatim on the way in.
		out := string(export.XML([]export.Entry{
			{Key: "bad_hex", Value: `\u12G4`},
			{Key: "short", Value: `\u12`},
			{Key: "trailing", Value: `end\`},
			{Key: "upper_u", Value: `\UD83C`}, // Android only knows lowercase \u
		}))
		assert.Contains(t, out, `>\\u12G4<`)
		assert.Contains(t, out, `>\\u12<`)
		assert.Contains(t, out, `>end\\<`)
		assert.Contains(t, out, `>\\UD83C<`)
	})
}

// TestXMLEscapesLeadingResourceReferenceChars: aapt treats a string whose
// FIRST character is '@' or '?' as a resource reference. The parser unescapes
// \@ and \?, so an unprotected export would turn \@YouTripSG in the source
// file into a bare @YouTripSG — a dangling reference instead of copy.
func TestXMLEscapesLeadingResourceReferenceChars(t *testing.T) {
	t.Run("plain path", func(t *testing.T) {
		out := string(export.XML([]export.Entry{
			{Key: "handle", Value: "@YouTripSG"},
			{Key: "question", Value: "?really"},
			{Key: "mid", Value: "email @YouTripSG?"},
		}))
		assert.Contains(t, out, `>\@YouTripSG<`)
		assert.Contains(t, out, `>\?really<`)
		assert.Contains(t, out, `>email @YouTripSG?<`,
			"aapt only special-cases position 0; nothing else is touched")

		back, err := parse.XMLBytes([]byte(out))
		require.NoError(t, err)
		v, ok := back.Lookup("handle")
		require.True(t, ok)
		assert.Equal(t, "@YouTripSG", v, "the parser unescapes \\@, closing the round trip")
	})

	t.Run("cdata path", func(t *testing.T) {
		// CDATA values go through the same escaping deliberately. CDATA is
		// XML-level quoting only: aapt inspects the string value AFTER the
		// XML parse, where the CDATA wrapper is transparent, so a leading '@'
		// is just as much a resource reference there. It round-trips because
		// the parser applies unescapeAndroid to CDATA content too.
		out := string(export.XML([]export.Entry{{Key: "c", Value: "@ref", CDATA: true}}))
		assert.Contains(t, out, `<![CDATA[\@ref]]>`)

		back, err := parse.XMLBytes([]byte(out))
		require.NoError(t, err)
		v, ok := back.Lookup("c")
		require.True(t, ok)
		assert.Equal(t, "@ref", v)
	})
}

// TestCheckAndroidNamesRejectsEmptyName: AndroidName("123") is "" — every
// character stripped — and XML() would emit name="", a file the parser itself
// refuses to read back. CheckAndroidNames is the designated pre-export gate,
// so that is where the refusal lives.
func TestCheckAndroidNamesRejectsEmptyName(t *testing.T) {
	err := export.CheckAndroidNames([]string{"ok_key", "123"})
	require.Error(t, err, "an empty android name must fail the export, never warn")
	assert.Contains(t, err.Error(), `"123"`)
}

// TestXMLEscapesNameAttribute: the key is written into a double-quoted XML
// attribute, so '&' and '"' (and '<') in it must be entity-escaped or the
// emitted document is not well-formed.
func TestXMLEscapesNameAttribute(t *testing.T) {
	out := string(export.XML([]export.Entry{{Key: `a"b&c`, Value: "v"}}))
	assert.Contains(t, out, `name="a&quot;b&amp;c"`)

	back, err := parse.XMLBytes([]byte(out))
	require.NoError(t, err, "the emitted attribute must be well-formed XML")
	v, ok := back.Lookup(`a"b&c`)
	require.True(t, ok)
	assert.Equal(t, "v", v)
}
