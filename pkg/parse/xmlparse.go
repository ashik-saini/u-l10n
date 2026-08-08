package parse

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
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
		// CDATA content is literal by definition — no XML unescaping — but
		// Android's own backslash escapes still apply.
		return unescapeAndroid(raw), RenderHintCDATA, nil
	}

	return unescapeAndroid(unescapeXMLEntities(inner)), RenderHintPlain, nil
}

// unescapeXMLEntities resolves the five predefined XML entities plus numeric
// character references.
//
// encoding/xml would do this for chardata, but InnerXML is deliberately raw so
// that CDATA and inline markup survive; the cost is doing this by hand.
func unescapeXMLEntities(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	// Order matters: &amp; must be resolved LAST, or "&amp;lt;" — a literal
	// "&lt;" in the copy — would decode to "<" instead of "&lt;".
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	s = strings.ReplaceAll(s, "&quot;", `"`)
	s = strings.ReplaceAll(s, "&apos;", "'")
	s = strings.ReplaceAll(s, "&#39;", "'")
	s = strings.ReplaceAll(s, "&#34;", `"`)
	s = strings.ReplaceAll(s, "&amp;", "&")
	return s
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
