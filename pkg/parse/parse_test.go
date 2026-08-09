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

// TestStringsSurrogatePairEscapes covers the \uXXXX form the corpus actually
// uses for emoji: UTF-16 surrogate PAIRS. Four committed files (Base.lproj,
// en-MY.lproj, en-AU.lproj, ms.lproj) carry the MY flag emoji as the escape
// text backslash-uD83C-uDDF2-uD83C-uDDFE. Decoding each half with WriteRune
// yields U+FFFD runs — surrogate code points are not valid runes — which
// silently corrupts the copy.
func TestStringsSurrogatePairEscapes(t *testing.T) {
	t.Run("corpus pair decodes to the emoji", func(t *testing.T) {
		// The exact sequence committed in the corpus statements.
		file, err := Strings([]byte(`"k" = "Refer your \uD83C\uDDF2\uD83C\uDDFE kakis to get";`))
		require.NoError(t, err)

		got, ok := file.Lookup("k")
		require.True(t, ok)
		assert.Equal(t, "Refer your \U0001F1F2\U0001F1FE kakis to get", got)
		assert.NotContains(t, got, "�", "surrogate halves must never decode to U+FFFD")
	})

	t.Run("uppercase U pairs too", func(t *testing.T) {
		file, err := Strings([]byte(`"k" = "\UD83C\UDDF2";`))
		require.NoError(t, err)
		got, _ := file.Lookup("k")
		assert.Equal(t, "\U0001F1F2", got)
	})

	t.Run("unpaired high surrogate stays verbatim", func(t *testing.T) {
		// An unpaired surrogate cannot be a rune. Per the file's
		// unknown-escapes-verbatim doctrine it is preserved as text, never
		// turned into U+FFFD.
		file, err := Strings([]byte(`"k" = "a\uD83Cb";`))
		require.NoError(t, err)
		got, _ := file.Lookup("k")
		assert.Equal(t, `a\uD83Cb`, got)
		assert.NotContains(t, got, "�")
	})

	t.Run("high surrogate followed by a non-low escape stays verbatim", func(t *testing.T) {
		file, err := Strings([]byte(`"k" = "\uD83CA";`))
		require.NoError(t, err)
		got, _ := file.Lookup("k")
		assert.Equal(t, `\uD83C`+"A", got)
	})

	t.Run("lone low surrogate stays verbatim", func(t *testing.T) {
		file, err := Strings([]byte(`"k" = "\uDDF2";`))
		require.NoError(t, err)
		got, _ := file.Lookup("k")
		assert.Equal(t, `\uDDF2`, got)
	})
}

// TestStringsUnicodeEscapeValidation pins the escape scanner to EXACTLY four
// hex digits. The old fmt.Sscanf("%04x") accepted "\U12G4" by parsing 0x12 and
// silently swallowing "G4", and accepted short input.
func TestStringsUnicodeEscapeValidation(t *testing.T) {
	t.Run("valid escape still decodes", func(t *testing.T) {
		file, err := Strings([]byte(`"k" = "\U0041";`))
		require.NoError(t, err)
		got, _ := file.Lookup("k")
		assert.Equal(t, "A", got)
	})

	t.Run("non-hex digit makes it an unrecognised escape", func(t *testing.T) {
		file, err := Strings([]byte(`"k" = "\U12G4";`))
		require.NoError(t, err)
		got, _ := file.Lookup("k")
		assert.Equal(t, `\U12G4`, got,
			"must not parse 0x12 and swallow G4; the whole thing is verbatim text")
	})

	t.Run("short input makes it an unrecognised escape", func(t *testing.T) {
		file, err := Strings([]byte(`"k" = "\U1";`))
		require.NoError(t, err)
		got, _ := file.Lookup("k")
		assert.Equal(t, `\U1`, got)
	})
}

// TestXMLNumericCharacterReferences covers &#dd; and &#xhh; forms, which the
// doc comment always claimed and the code only honoured for &#39; and &#34;.
func TestXMLNumericCharacterReferences(t *testing.T) {
	file, err := XMLBytes([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<resources>
  <string name="dec">a&#8230;b</string>
  <string name="hex">a&#x1F600;b</string>
  <string name="apos_dec">Don&#39;t</string>
  <string name="quot_dec">say &#34;hi&#34;</string>
  <string name="amp_last">a&amp;#8230;b</string>
  <string name="surrogate">a&#xD800;b</string>
</resources>`))
	require.NoError(t, err)

	for _, tc := range []struct{ key, want string }{
		{"dec", "a…b"},
		{"hex", "a\U0001F600b"},
		{"apos_dec", "Don't"},
		{"quot_dec", `say "hi"`},
		// &amp; resolves LAST, so &amp;#8230; is a literal "&#8230;" in the
		// copy, not an ellipsis.
		{"amp_last", "a&#8230;b"},
		// Surrogate code points are not characters; leave the reference
		// literal rather than emitting U+FFFD.
		{"surrogate", "a&#xD800;b"},
	} {
		got, ok := file.Lookup(tc.key)
		require.True(t, ok, tc.key)
		assert.Equal(t, tc.want, got, tc.key)
	}

	// Structurally malformed or out-of-range references never reach this
	// package's decoder: encoding/xml's tokeniser rejects them while scanning
	// the document, which is a loud failure and therefore fine. Pinned here
	// so a stdlib behaviour change would surface. (The checks in
	// resolveNumericCharRefs still guard what the tokeniser lets through,
	// e.g. the surrogate above.)
	for name, ref := range map[string]string{
		"too_big":      "&#1114112;",
		"no_digits":    "&#;",
		"unterminated": "&#8230",
	} {
		_, err := XMLBytes([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<resources>
  <string name="x">a` + ref + `b</string>
</resources>`))
		require.Error(t, err, name)
	}
}

// TestJSONRejectsTrailingContent: {"a":"1"}{"b":"2"} used to parse as the
// first object, silently dropping the second. Failing loudly beats that.
func TestJSONRejectsTrailingContent(t *testing.T) {
	_, err := JSONBytes([]byte(`{"a":"1"}{"b":"2"}`))
	require.Error(t, err, "a second object after the close must not be silently ignored")

	_, err = JSONBytes([]byte(`{"a":"1"} x`))
	require.Error(t, err)

	_, err = JSONBytes([]byte("{\"a\":\"1\"}\n\t "))
	require.NoError(t, err, "trailing whitespace is fine")
}

// TestXMLRejectsPartialCDATA: the single-wrapper check used to be fooled by
// values where CDATA markers appear beyond a full wrap, producing garbled
// output ("a]]>b<![CDATA[c") or keeping "<![CDATA[" as literal copy.
func TestXMLRejectsPartialCDATA(t *testing.T) {
	t.Run("two CDATA sections", func(t *testing.T) {
		_, err := XMLBytes([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<resources>
  <string name="a"><![CDATA[a]]>b<![CDATA[c]]></string>
</resources>`))
		require.Error(t, err, "multiple CDATA sections must fail loudly, not garble")
		assert.Contains(t, err.Error(), `"a"`)
	})

	t.Run("CDATA after leading text", func(t *testing.T) {
		_, err := XMLBytes([]byte(`<?xml version="1.0" encoding="UTF-8"?>
<resources>
  <string name="b">text<![CDATA[a]]></string>
</resources>`))
		require.Error(t, err, "a CDATA section not spanning the whole value must fail loudly")
		assert.Contains(t, err.Error(), `"b"`)
	})
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
