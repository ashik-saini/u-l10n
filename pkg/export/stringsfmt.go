package export

import (
	"bytes"
	"fmt"
)

// StringsOptions configures the iOS serializer.
type StringsOptions struct {
	// LineEnding defaults to LF, which matches seven of the eight committed
	// files. See the LineEnding docs for why this is configurable at all.
	LineEnding LineEnding
}

// Strings writes an iOS Localizable.strings file.
//
//	"key" = "value";\n
//
// Keys are NOT transformed — hyphens survive, unlike Android.
//
// Placeholder conversion (printf %n$s to %n$@) is deliberately NOT done here.
// The canonical stored form is whatever the corpus holds, and converting in
// the writer would mean the round-trip no longer proves what it claims. When
// the importer establishes a single canonical placeholder form, conversion
// belongs there or in an explicit transform step, not hidden in a serializer.
func Strings(entries []Entry, opts StringsOptions) []byte {
	le := opts.LineEnding.or(LF)

	var b bytes.Buffer
	b.Grow(len(entries) * 96)

	for _, e := range entries {
		b.WriteByte('"')
		writeStringsEscaped(&b, e.Key)
		b.WriteString(`" = "`)
		writeStringsEscaped(&b, e.Value)
		b.WriteString(`";`)
		b.WriteString(string(le))
	}

	return b.Bytes()
}

// writeStringsEscaped applies the .strings escaping rules.
//
// Only backslash, double quote and the control characters need escaping; a
// literal newline inside a value becomes \n, which is what keeps one statement
// on one line. Everything else, including all non-ASCII, is raw UTF-8.
func writeStringsEscaped(b *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c < 0x20 {
				fmt.Fprintf(b, `\U%04X`, c)
				continue
			}
			b.WriteByte(c)
		}
	}
}
