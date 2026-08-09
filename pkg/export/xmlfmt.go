package export

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// XML writes an Android strings.xml file.
//
//	<?xml version="1.0" encoding="UTF-8"?>\n
//	<resources>\n
//	  <string name="key">value</string>\n      TWO-space indent
//	</resources>\n
//
// encoding/xml is not used. It would escape the inline <b> and <u> markup that
// is part of the copy, reorder nothing usefully, and give no control over
// CDATA — all three of which matter for byte-exactness.
func XML(entries []Entry) []byte {
	var b bytes.Buffer
	b.Grow(len(entries) * 96)

	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString("<resources>\n")

	for _, e := range entries {
		b.WriteString(`  <string name="`)
		// The key sits inside a double-quoted attribute, so '&', '"' and '<'
		// must be entity-escaped or the document is not well-formed. (Corpus
		// keys are all [A-Za-z0-9_], so this is a guard, not a behaviour
		// change; CheckAndroidNames is the gate that keeps it that way.)
		b.WriteString(escapeXMLAttr(e.Key))
		b.WriteString(`">`)
		if e.CDATA {
			b.WriteString("<![CDATA[")
			// CDATA content is literal by definition: no XML escaping. Only
			// Android's own backslash escapes apply.
			b.WriteString(escapeAndroidBackslashes(e.Value))
			b.WriteString("]]>")
		} else {
			writeAndroidValue(&b, e.Value)
		}
		b.WriteString("</string>\n")
	}

	b.WriteString("</resources>\n")
	return b.Bytes()
}

// inlineMarkup matches the formatting tags that are content rather than
// document structure. They must survive as raw markup; everything else that
// looks like a tag is escaped.
var inlineMarkup = regexp.MustCompile(`</?[bu]>`)

// writeAndroidValue applies both escaping layers in the order the parser
// reverses them: Android backslash escapes first, then XML entities, with the
// inline markup carved out.
func writeAndroidValue(b *bytes.Buffer, s string) {
	escaped := escapeAndroidBackslashes(s)

	// Split around inline markup so <b> stays raw while a literal '<'
	// elsewhere becomes &lt;.
	last := 0
	for _, loc := range inlineMarkup.FindAllStringIndex(escaped, -1) {
		b.WriteString(escapeXMLEntities(escaped[last:loc[0]]))
		b.WriteString(escaped[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(escapeXMLEntities(escaped[last:]))
}

// escapeAndroidBackslashes re-applies Android's backslash escapes.
//
// Order is critical and mirrors unescapeAndroid in pkg/parse: a literal
// backslash must become a double backslash BEFORE apostrophes and newlines are
// escaped, or the backslash introduced by those escapes would itself be
// doubled on a later pass. The scan is single-pass with lookahead for the same
// reason the parser's is.
//
// Two aapt behaviours shape the edges:
//
//   - \u followed by EXACTLY four hex digits is an escape aapt itself
//     resolves. The parser's default branch keeps it verbatim in memory
//     (eight corpus statements carry emoji this way), so it is copied through
//     verbatim here; doubling the backslash would make aapt render the escape
//     as literal text.
//   - a string whose FIRST character is '@' or '?' is a resource reference to
//     aapt, so that one position is escaped. This applies to CDATA values
//     too: CDATA is XML-level quoting only, and aapt inspects the string
//     value after the XML parse, where the wrapper is transparent. The parser
//     unescapes \@ and \? in both paths, so both round-trip.
func escapeAndroidBackslashes(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	if len(s) > 0 && (s[0] == '@' || s[0] == '?') {
		b.WriteByte('\\')
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\\':
			if i+5 < len(s) && s[i+1] == 'u' && isHex4(s[i+2:i+6]) {
				b.WriteString(s[i : i+6])
				i += 5
				continue
			}
			b.WriteString(`\\`)
		case '\'':
			b.WriteString(`\'`)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// isHex4 reports whether s is exactly four hex digit bytes.
func isHex4(s string) bool {
	if len(s) != 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// escapeXMLEntities escapes the characters that would otherwise be markup.
//
// '&' is escaped FIRST — the mirror image of the parser resolving it last —
// so that a literal "&lt;" in the copy round-trips as "&amp;lt;" rather than
// collapsing to "<".
func escapeXMLEntities(s string) string {
	if !strings.ContainsAny(s, `&<>`) {
		return s
	}
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// escapeXMLAttr escapes a value for a double-quoted XML attribute.
//
// '&' is escaped FIRST for the same reason as in escapeXMLEntities: escaping
// it after the others would double-escape the entities they introduce.
func escapeXMLAttr(s string) string {
	if !strings.ContainsAny(s, `&"<`) {
		return s
	}
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return s
}

// androidNamePattern matches characters that are illegal in an Android
// resource name.
var androidNamePattern = regexp.MustCompile(`[^A-Za-z0-9_]`)
var leadingDigits = regexp.MustCompile(`^[0-9]+`)

// AndroidName derives the Android resource name for a canonical key.
//
// The transform, verified against the committed corpus on two independent
// cases, is: strip leading digits, then replace every character outside
// [A-Za-z0-9_] with an underscore.
//
//	01-menu-help__category_title__17    -> _menu_help__category_title__17
//	ATMDecline_LimitExceedPNMsg_Non-MYR -> ATMDecline_LimitExceedPNMsg_Non_MYR
func AndroidName(key string) string {
	return androidNamePattern.ReplaceAllString(leadingDigits.ReplaceAllString(key, ""), "_")
}

// CheckAndroidNames reports keys that collide once transformed.
//
// Two distinct canonical keys can map to the same Android name — 'foo-bar' and
// 'foo_bar' both become 'foo_bar' — and the second silently overwrites the
// first in the generated file.
//
// This returns an error rather than a warning by design, and callers must fail
// the whole export on it. A warning in CI output is a warning nobody reads,
// and the failure mode is one string quietly replacing another in a shipped
// app. The corpus has zero collisions today; the check exists to keep it that
// way.
func CheckAndroidNames(keys []string) error {
	byName := make(map[string]string, len(keys))
	for _, key := range keys {
		name := AndroidName(key)
		// AndroidName strips leading digits, so an all-digit key produces the
		// empty string — and XML() would emit name="", a file the parser
		// itself refuses to read back. Refuse it here, at the gate.
		if name == "" {
			return fmt.Errorf("key %q produces an empty android name", key)
		}
		// A repeated canonical key is a DUPLICATE, not a collision — the
		// committed corpus has 23 of them in en-SG alone. Only two DIFFERENT
		// canonical keys mapping to one Android name can silently overwrite
		// each other, which is the failure this guards.
		if first, seen := byName[name]; seen && first != key {
			return fmt.Errorf(
				"android name collision: %q and %q both become %q", first, key, name)
		}
		byName[name] = key
	}
	return nil
}
