package parse

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// androidResources mirrors the Android strings.xml document.
//
// InnerXML is captured raw rather than as chardata because two things inside a
// <string> must survive verbatim: CDATA wrappers, and inline <b>/<u> markup
// that is part of the copy rather than document structure.
type androidResources struct {
	XMLName xml.Name        `xml:"resources"`
	Strings []androidString `xml:"string"`
}

type androidString struct {
	Name     string `xml:"name,attr"`
	InnerXML string `xml:",innerxml"`
}

// XML parses an Android strings.xml file.
//
// The real corpus contains only <string> elements — no <string-array>, no
// <plurals> — which is asserted by the corpus test rather than assumed here:
// encoding/xml simply ignores element types absent from the struct, so an
// unexpected <plurals> would be dropped silently. The count assertion is what
// catches that.
func XML(r io.Reader) (*File, error) {
	var doc androidResources
	if err := xml.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode resources: %w", err)
	}

	file := &File{Entries: make([]Entry, 0, len(doc.Strings))}
	for _, s := range doc.Strings {
		if s.Name == "" {
			return nil, fmt.Errorf("<string> element without a name attribute")
		}

		value, hint, err := decodeAndroidValue(s.InnerXML)
		if err != nil {
			return nil, fmt.Errorf("decode value for %q: %w", s.Name, err)
		}

		file.Entries = append(file.Entries, Entry{
			Key:        s.Name,
			Value:      value,
			RenderHint: hint,
		})
	}
	return file, nil
}

// XMLBytes is a convenience wrapper around XML.
func XMLBytes(data []byte) (*File, error) {
	return XML(bytes.NewReader(data))
}

const (
	cdataOpen  = "<![CDATA["
	cdataClose = "]]>"
)

// decodeAndroidValue turns the raw inner XML of a <string> into the in-memory
// value, and reports whether it was CDATA-wrapped.
//
// Two escaping layers stack here and must be peeled in the right order:
//
//	XML     &amp; -> &          (not applied inside CDATA)
//	Android \'    -> '          (applied in both cases)
//
// Inline <b> and <u> markup is left as literal text: it is content the app
// renders, not document structure.
func decodeAndroidValue(inner string) (string, RenderHint, error) {
	trimmed := strings.TrimSpace(inner)

	if strings.HasPrefix(trimmed, cdataOpen) {
		if !strings.HasSuffix(trimmed, cdataClose) {
			return "", "", fmt.Errorf("unterminated CDATA section")
		}
		raw := trimmed[len(cdataOpen) : len(trimmed)-len(cdataClose)]
		// A prefix+suffix check alone is fooled by <![CDATA[a]]>b<![CDATA[c]]>,
		// which would garble into "a]]>b<![CDATA[c". Only a SINGLE wrapper
		// spanning the whole value is representable; refuse anything else.
		if strings.Contains(raw, cdataOpen) || strings.Contains(raw, cdataClose) {
			return "", "", fmt.Errorf("multiple CDATA sections in one value are not supported")
		}
		// CDATA content is literal by definition — no XML unescaping — but
		// Android's own backslash escapes still apply.
		return unescapeAndroid(raw), RenderHintCDATA, nil
	}

	// A CDATA section that does not span the whole value — text<![CDATA[a]]>
	// — reaches here and would keep the markers as literal copy. Fail loudly
	// instead.
	if strings.Contains(inner, cdataOpen) || strings.Contains(inner, cdataClose) {
		return "", "", fmt.Errorf("CDATA section does not span the whole value")
	}

	return unescapeAndroid(unescapeXMLEntities(inner)), RenderHintPlain, nil
}

// unescapeXMLEntities resolves the five predefined XML entities plus decimal
// (&#39;) and hexadecimal (&#x1F600;) numeric character references. A
// reference that is malformed, names a surrogate code point, or exceeds
// U+10FFFF is not a character and is left literal.
//
// encoding/xml would do this for chardata, but InnerXML is deliberately raw so
// that CDATA and inline markup survive; the cost is doing this by hand.
func unescapeXMLEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	// Order matters: &amp; must be resolved LAST, or "&amp;lt;" — a literal
	// "&lt;" in the copy — would decode to "<" instead of "&lt;". The same
	// ordering keeps "&amp;#8230;" a literal "&#8230;" rather than an
	// ellipsis, which is why the numeric pass runs before it.
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&apos;", "'")
	s = resolveNumericCharRefs(s)
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
}

// resolveNumericCharRefs decodes &#dd; and &#xhh; references in a single
// left-to-right pass. Anything that is not a well-formed reference to a real
// character — missing ';', no digits, a surrogate code point, a value past
// U+10FFFF — is copied through literally rather than half-decoded.
func resolveNumericCharRefs(s string) string {
	if !strings.Contains(s, "&#") {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); {
		if s[i] != '&' || i+1 >= len(s) || s[i+1] != '#' {
			b.WriteByte(s[i])
			i++
			continue
		}

		j := i + 2
		base := 10
		if j < len(s) && s[j] == 'x' {
			base = 16
			j++
		}
		start := j
		for j < len(s) && isCharRefDigit(s[j], base) {
			j++
		}
		if j == start || j >= len(s) || s[j] != ';' {
			b.WriteByte(s[i]) // not a reference; the '&' is literal
			i++
			continue
		}

		v, err := strconv.ParseUint(s[start:j], base, 32)
		if err != nil || v > 0x10FFFF || (0xD800 <= v && v <= 0xDFFF) {
			b.WriteByte(s[i]) // not a character; leave the reference literal
			i++
			continue
		}

		b.WriteRune(rune(v))
		i = j + 1
	}
	return b.String()
}

func isCharRefDigit(c byte, base int) bool {
	if '0' <= c && c <= '9' {
		return true
	}
	return base == 16 && ('a' <= c && c <= 'f' || 'A' <= c && c <= 'F')
}

// unescapeAndroid resolves Android's backslash escapes.
//
// The scan is single-pass and left-to-right, consuming both characters of an
// escape at once. A naive chain of ReplaceAll would corrupt the corpus: a
// literal backslash-n is written "\\n" in the file, and replacing "\\" -> "\"
// first would turn it into "\n", which a later pass would then read as a real
// newline. Roughly 900 values in the Flutter corpus alone depend on that
// distinction holding.
func unescapeAndroid(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}

	var b strings.Builder
	b.Grow(len(s))

	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}

		i++ // consume the escape character as well
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case '\'':
			b.WriteByte('\'')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		case '@', '?':
			// Android escapes a leading @ or ? so the value is not mistaken
			// for a resource reference.
			b.WriteByte(s[i])
		default:
			// Not a recognised escape: emit both characters unchanged rather
			// than silently dropping the backslash.
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
