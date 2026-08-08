// Package export writes the three localization file formats.
//
// It is the counterpart to pkg/parse and MUST NOT import it. Their only shared
// vocabulary is the entry type each defines for itself. Two independent
// implementations that must agree is a real check; one implementation compared
// against itself is a tautology, and a shared escaping helper would let a
// symmetric bug cancel out and pass Gate R while corrupting production copy.
package export

import (
	"bytes"
	"fmt"
)

// Entry is one string to write.
type Entry struct {
	Key   string
	Value string
	// CDATA requests CDATA wrapping. Android only.
	CDATA bool
}

// JSON writes a Flutter localization file.
//
// The format is pinned by the committed corpus, not by taste:
//
//	{\n                          opening brace on its own line
//	    "key": "value",\n        FOUR-space indent, comma-separated
//	    "last": "value"\n        no trailing comma
//	}\n                          closing brace, and a trailing newline
//
// encoding/json cannot produce this. It will not escape '/' as '\/' — which
// Lokalise does, 369 times in en-SG alone — and it HTML-escapes '<', '>' and
// '&' by default, which Lokalise does not. Hence a hand-rolled writer.
func JSON(entries []Entry) []byte {
	var b bytes.Buffer
	b.Grow(len(entries) * 96)

	b.WriteString("{\n")
	for i, e := range entries {
		b.WriteString(`    "`)
		writeJSONString(&b, e.Key)
		b.WriteString(`": "`)
		writeJSONString(&b, e.Value)
		b.WriteByte('"')
		if i < len(entries)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("}\n")

	return b.Bytes()
}

// writeJSONString applies Lokalise's escaping rules.
//
// Escaped: backslash, double quote, forward slash, and the C0 control
// characters. Everything else — including all non-ASCII — is written as raw
// UTF-8, which is why Thai renders literally rather than as \uXXXX escapes.
func writeJSONString(b *bytes.Buffer, s string) {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '/':
			// Not required by JSON, but Lokalise emits it and byte-exactness
			// is the whole point.
			b.WriteString(`\/`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			if c < 0x20 {
				fmt.Fprintf(b, `\u%04x`, c)
				continue
			}
			// U+2028 LINE SEPARATOR and U+2029 PARAGRAPH SEPARATOR are valid
			// JSON but are statement terminators in JavaScript, so exporters
			// escape them. The corpus carries 9 in en-SG and 16 in en-MY;
			// emitting them raw produces a file that is byte-different and, in
			// a JS context, syntactically broken.
			if c == 0xE2 && i+2 < len(s) && s[i+1] == 0x80 {
				if s[i+2] == 0xA8 || s[i+2] == 0xA9 {
					fmt.Fprintf(b, `\u202%c`, '8'+(s[i+2]-0xA8))
					i += 2
					continue
				}
			}
			// Raw UTF-8 passthrough. Bytes >= 0x80 are continuation or lead
			// bytes of a multi-byte rune and are copied verbatim.
			b.WriteByte(c)
		}
	}
}

// LineEnding selects the line terminator a serializer emits.
//
// It is an OPTION rather than a constant because the committed iOS corpus is
// mixed: of the eight files, only ios/Runner/Localizable.strings uses CRLF
// (959 carriage returns) while the rest are pure LF. Since .gitattributes sets
// no eol rule and core.autocrlf is unset, that mix is committed content rather
// than a checkout artefact. Which one is correct for a fresh export can only
// be settled against Lokalise.
type LineEnding string

const (
	LF   LineEnding = "\n"
	CRLF LineEnding = "\r\n"
)

func (le LineEnding) or(def LineEnding) LineEnding {
	if le == "" {
		return def
	}
	return le
}
