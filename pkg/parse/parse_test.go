package parse

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestNewlineDuality is the most important test in this package.
//
// Two things exist in the corpus and look identical in every editor:
//
//	"a\nb"  in the file -> a REAL newline (byte 0x0A) in the value
//	"a\\nb" in the file -> a literal backslash followed by 'n'
//
// In the committed en-SG.json there are 153 of the first and 897 of the
// second. Conflating them corrupts roughly a thousand strings, and the change
// survives review because both forms look the same on screen.
func TestNewlineDuality(t *testing.T) {
	t.Run("json", func(t *testing.T) {
		// Go source: `\\n` is the two characters the FILE contains for a real
		// newline; `\\\\n` is the three characters it contains for a literal.
		file, err := JSONBytes([]byte(`{"real":"a\nb","literal":"a\\nb"}`))
		require.NoError(t, err)

		real, ok := file.Lookup("real")
		require.True(t, ok)
		assert.Equal(t, "a\nb", real)
		assert.Len(t, real, 3, "a real newline is ONE byte")

		literal, ok := file.Lookup("literal")
		require.True(t, ok)
		assert.Equal(t, `a\nb`, literal)
		assert.Len(t, literal, 4, "a literal backslash-n is TWO bytes")

		assert.NotEqual(t, real, literal, "these must never collapse into each other")
	})

	t.Run("strings", func(t *testing.T) {
		file, err := Strings([]byte(`"real" = "a\nb";` + "\n" + `"literal" = "a\\nb";`))
		require.NoError(t, err)

		real, _ := file.Lookup("real")
		literal, _ := file.Lookup("literal")
		assert.Equal(t, "a\nb", real)
		assert.Equal(t, `a\nb`, literal)
		assert.NotEqual(t, real, literal)
	})

	t.Run("xml", func(t *testing.T) {
		file, err := XMLBytes([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<resources>
  <string name="real">a\nb</string>
  <string name="literal">a\\nb</string>
</resources>`))
		require.NoError(t, err)

		real, _ := file.Lookup("real")
		literal, _ := file.Lookup("literal")
		assert.Equal(t, "a\nb", real)
		assert.Equal(t, `a\nb`, literal)
		assert.NotEqual(t, real, literal)
	})
}

func TestJSONPreservesOrderAndEmptyValues(t *testing.T) {
	// Deliberately NOT alphabetical: export order is data.
	file, err := JSONBytes([]byte(`{"zebra":"Z","alpha":"A","empty":"","middle":"M"}`))
	require.NoError(t, err)

	assert.Equal(t, []string{"zebra", "alpha", "empty", "middle"}, file.Keys(),
		"file order must survive; sorting would produce a 6,300-line diff on every export")

	value, found := file.Lookup("empty")
	assert.True(t, found, "an explicitly empty value is PRESENT")
	assert.Equal(t, "", value)

	_, found = file.Lookup("absent")
	assert.False(t, found, "an untranslated key is ABSENT, which is a different fact")
}

func TestJSONUnescaping(t *testing.T) {
	file, err := JSONBytes([]byte(`{
		"slash":   "https:\/\/you.co\/help",
		"quote":   "say \"hi\"",
		"unicode": "é’",
		"thai":    "เข้าสู่ระบบ",
		"tab":     "a\tb"
	}`))
	require.NoError(t, err)

	for _, tc := range []struct{ key, want string }{
		{"slash", "https://you.co/help"},
		{"quote", `say "hi"`},
		{"unicode", "é’"},
		{"thai", "เข้าสู่ระบบ"},
		{"tab", "a\tb"},
	} {
		got, ok := file.Lookup(tc.key)
		require.True(t, ok, tc.key)
		assert.Equal(t, tc.want, got, tc.key)
	}
}

func TestJSONRejectsNonStringValues(t *testing.T) {
	// Failing loudly beats silently dropping a key.
	_, err := JSONBytes([]byte(`{"a":"ok","b":{"nested":"x"}}`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"b"`)
}

func TestXMLEscapingAndInlineMarkup(t *testing.T) {
	file, err := XMLBytes([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<resources>
  <string name="amp">Sign Up &amp; Activation</string>
  <string name="apos">Don\'t worry</string>
  <string name="bold">Tap <b>here</b> to continue</string>
  <string name="underline">Read the <u>terms</u></string>
  <string name="entity_literal">Write &amp;lt; for less-than</string>
  <string name="empty"></string>
</resources>`))
	require.NoError(t, err)

	for _, tc := range []struct{ key, want string }{
		{"amp", "Sign Up & Activation"},
		{"apos", "Don't worry"},
		{"bold", "Tap <b>here</b> to continue"},
		{"underline", "Read the <u>terms</u>"},
		// &amp;lt; is a literal "&lt;" in the copy, not a less-than sign.
		// Resolving &amp; last is what keeps this correct.
		{"entity_literal", "Write &lt; for less-than"},
		{"empty", ""},
	} {
		got, ok := file.Lookup(tc.key)
		require.True(t, ok, tc.key)
		assert.Equal(t, tc.want, got, tc.key)
	}
}

func TestXMLCDATAIsRecordedAsARenderHint(t *testing.T) {
	file, err := XMLBytes([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<resources>
  <string name="plain">ordinary</string>
  <string name="wrapped"><![CDATA[Maintenance from %1$@ to %2$@. \nWe apologise.]]></string>
</resources>`))
	require.NoError(t, err)
	require.Len(t, file.Entries, 2)

	assert.Equal(t, RenderHintPlain, file.Entries[0].RenderHint)

	wrapped := file.Entries[1]
	assert.Equal(t, RenderHintCDATA, wrapped.RenderHint,
		"CDATA is a per-value presentation fact that must be reproduced on export")
	assert.Equal(t, "Maintenance from %1$@ to %2$@. \nWe apologise.", wrapped.Value)
}

func TestStringsFormat(t *testing.T) {
	// Keys keep hyphens on iOS — no transform, unlike Android.
	input := `/* a block comment */
"ATMDecline_LimitExceedPNMsg_Non-MYR" = "Your transaction of %1$@ %2$@ (%3$@ MYR) exceeded your limit.";
// a line comment
"with_quote" = "say \"hi\"";
"empty" = "";
"semicolon_in_value" = "a; b";
"equals_in_value" = "a = b";
`
	file, err := Strings([]byte(input))
	require.NoError(t, err)
	require.Len(t, file.Entries, 5, "comments must not be counted as entries")

	assert.Equal(t, "ATMDecline_LimitExceedPNMsg_Non-MYR", file.Entries[0].Key,
		"iOS keys are not transformed; hyphens survive")
	assert.Equal(t, "Your transaction of %1$@ %2$@ (%3$@ MYR) exceeded your limit.",
		file.Entries[0].Value)

	for _, tc := range []struct{ key, want string }{
		{"with_quote", `say "hi"`},
		{"empty", ""},
		{"semicolon_in_value", "a; b"},
		{"equals_in_value", "a = b"},
	} {
		got, ok := file.Lookup(tc.key)
		require.True(t, ok, tc.key)
		assert.Equal(t, tc.want, got, tc.key)
	}
}

func TestStringsToleratesCRLF(t *testing.T) {
	// Of the eight committed files, only ios/Runner/Localizable.strings has
	// carriage returns (959). The mix is committed content, so the parser
	// tolerates both and the line ending becomes a serializer option.
	crlf := "\"a\" = \"one\";\r\n\"b\" = \"two\";\r\n"
	lf := "\"a\" = \"one\";\n\"b\" = \"two\";\n"

	fromCRLF, err := Strings([]byte(crlf))
	require.NoError(t, err)
	fromLF, err := Strings([]byte(lf))
	require.NoError(t, err)

	assert.Equal(t, fromLF.Entries, fromCRLF.Entries,
		"line endings are a transport detail, not part of the value")
}

func TestStringsRejectsMalformedInput(t *testing.T) {
	cases := map[string]string{
		"missing semicolon":  `"a" = "b"`,
		"missing equals":     `"a" "b";`,
		"unterminated value": `"a" = "b;`,
		"unterminated key":   `"a = "b";`,
		"stray token":        `oops "a" = "b";`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Strings([]byte(input))
			assert.Error(t, err, "malformed input must fail loudly, not parse partially")
		})
	}
}

// TestUnescapeIsSinglePass guards the specific bug a chained-ReplaceAll
// implementation would introduce: resolving backslash-backslash before
// backslash-n turns a literal backslash-n into a real newline.
func TestUnescapeIsSinglePass(t *testing.T) {
	cases := []struct{ in, want string }{
		{`a\\nb`, `a\nb`},    // literal: backslash then 'n'
		{`a\nb`, "a\nb"},     // real newline
		{`a\\\nb`, "a\\\nb"}, // literal backslash, then a real newline
		{`a\\\\nb`, `a\\nb`}, // two literal backslashes, then 'n'
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, unescapeAndroid(tc.in), "input %q", tc.in)
	}
}
