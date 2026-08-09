// Package parse reads the three localization file formats into a common
// in-memory form.
//
// It is one half of a deliberately duplicated pair: pkg/parse reads, pkg/export
// writes, and the two MUST NOT share code. Their only common vocabulary is the
// types in this file. That is what makes the Phase 3 round-trip gate honest —
// if both sides called one escaping helper and that helper were wrong, the bug
// would cancel itself out and the test would pass green while production copy
// was corrupted.
//
// Parsers unescape to the real in-memory value; serializers re-escape. The
// distinction that matters most is between a value containing a real newline
// (byte 0x0A) and one containing a literal backslash followed by 'n'. Both
// render identically in every editor, both occur in the real corpus in the
// hundreds, and conflating them corrupts roughly a thousand strings.
package parse

// Entry is one translated string as it appears in a file.
type Entry struct {
	// Key is the identifier as written in the file. Android files carry the
	// TRANSFORMED name (leading digits stripped, non-alphanumerics replaced);
	// reversing that transform is the importer's job, not the parser's.
	Key string

	// Value is the unescaped, in-memory string: escape sequences have been
	// resolved, so a real newline is byte 0x0A and a literal backslash-n is
	// two characters.
	Value string

	// RenderHint records a per-value presentation detail that must be
	// reproduced on export. Only Android CDATA wrapping uses it today.
	RenderHint RenderHint
}

// RenderHint mirrors the translations.render_hint column.
type RenderHint string

const (
	RenderHintPlain RenderHint = "plain"
	RenderHintCDATA RenderHint = "cdata"
)

// File is the ordered contents of one localization file.
//
// Order is preserved because export order is data, not alphabetical: it must
// reproduce Lokalise's ordering or every generated file is a multi-thousand
// line diff against the committed one.
type File struct {
	Entries []Entry
}

// Len reports the number of entries parsed.
func (f *File) Len() int { return len(f.Entries) }

// Keys returns the keys in file order.
func (f *File) Keys() []string {
	keys := make([]string, len(f.Entries))
	for i, e := range f.Entries {
		keys[i] = e.Key
	}
	return keys
}

// Duplicates reports keys that appear more than once, in first-seen order,
// together with every value observed for them.
//
// This is not hypothetical. The committed corpus contains duplicates —
// 23 keys in en-SG.json, 29 in ios/Runner/Localizable.strings — and a handful
// carry DIFFERENT values for the same key, so which one takes effect depends
// on the consumer's parser (both Flutter and iOS take the last).
//
// The parser deliberately preserves duplicates rather than collapsing them:
// its job is to report what the file says. Resolving them is a policy decision
// that belongs to the importer, which must also warn about it, because the
// schema's unique index on active key names cannot represent two.
func (f *File) Duplicates() map[string][]string {
	seen := make(map[string][]string, len(f.Entries))
	order := make([]string, 0)
	for _, e := range f.Entries {
		if _, ok := seen[e.Key]; !ok {
			order = append(order, e.Key)
		}
		seen[e.Key] = append(seen[e.Key], e.Value)
	}

	dups := make(map[string][]string)
	for _, key := range order {
		if values := seen[key]; len(values) > 1 {
			dups[key] = values
		}
	}
	return dups
}

// UniqueLen reports the number of distinct keys, which is what the database
// can hold. It differs from Len exactly when the file contains duplicates.
func (f *File) UniqueLen() int {
	seen := make(map[string]struct{}, len(f.Entries))
	for _, e := range f.Entries {
		seen[e.Key] = struct{}{}
	}
	return len(seen)
}

// Lookup returns the value for a key and whether it was present.
//
// It returns (value, found) rather than a bare string on purpose. A key that is
// absent and a key whose value is the empty string are DIFFERENT facts: absent
// means untranslated and is omitted from an export, empty means deliberately
// blank and is exported as "". Go's zero value for string is "", so a bare
// return would silently merge the two.
// When a key is duplicated, the LAST occurrence wins — matching what the app
// actually does, since both Dart's json.decode and iOS's .strings loader take
// the last. Returning the first would make this package disagree with the
// runtime about what a duplicated key means.
func (f *File) Lookup(key string) (string, bool) {
	value, found := "", false
	for _, e := range f.Entries {
		if e.Key == key {
			value, found = e.Value, true
		}
	}
	return value, found
}
