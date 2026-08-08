package parse

import (
	"fmt"
	"strings"
)

// Strings parses an iOS Localizable.strings file.
//
// The format is a sequence of `"key" = "value";` statements, optionally
// separated by comments and whitespace. It is hand-scanned rather than
// regex-matched: values legitimately contain escaped quotes, and a regex that
// handles those correctly is harder to read than the scanner and no faster.
//
// iOS keys are NOT transformed the way Android keys are — hyphens survive — so
// what is read here is the canonical key.
//
// Line endings are accepted in any form. The corpus is mixed: of the eight
// committed files, only ios/Runner/Localizable.strings contains carriage
// returns (959 of them); the rest are pure LF. Since .gitattributes sets no eol
// rule and core.autocrlf is unset, that mix is committed content rather than a
// checkout artefact, which makes the line ending a serializer OPTION on the
// way out and something to simply tolerate on the way in.
func Strings(data []byte) (*File, error) {
	s := string(data)
	file := &File{Entries: make([]Entry, 0, 5200)}

	pos := 0
	for {
		var err error
		if pos, err = skipInsignificant(s, pos); err != nil {
			return nil, err
		}
		if pos >= len(s) {
			break
		}

		if s[pos] != '"' {
			return nil, fmt.Errorf("at byte %d: expected '\"' to open a key, found %q",
				pos, s[pos])
		}

		key, next, err := scanQuoted(s, pos)
		if err != nil {
			return nil, fmt.Errorf("scan key at byte %d: %w", pos, err)
		}
		pos = next

		if pos, err = skipInsignificant(s, pos); err != nil {
			return nil, err
		}
		if pos >= len(s) || s[pos] != '=' {
			return nil, fmt.Errorf("key %q: expected '=' after the key", key)
		}
		pos++ // consume '='

		if pos, err = skipInsignificant(s, pos); err != nil {
			return nil, err
		}
		if pos >= len(s) || s[pos] != '"' {
			return nil, fmt.Errorf("key %q: expected '\"' to open the value", key)
		}

		value, next, err := scanQuoted(s, pos)
		if err != nil {
			return nil, fmt.Errorf("scan value for %q: %w", key, err)
		}
		pos = next

		if pos, err = skipInsignificant(s, pos); err != nil {
			return nil, err
		}
		if pos >= len(s) || s[pos] != ';' {
			return nil, fmt.Errorf("key %q: expected ';' to terminate the statement", key)
		}
		pos++ // consume ';'

		file.Entries = append(file.Entries, Entry{
			Key:        key,
			Value:      value,
			RenderHint: RenderHintPlain,
		})
	}

	return file, nil
}

// skipInsignificant advances past whitespace and comments.
func skipInsignificant(s string, pos int) (int, error) {
	for pos < len(s) {
		switch {
		case s[pos] == ' ', s[pos] == '\t', s[pos] == '\n', s[pos] == '\r':
			pos++
		case strings.HasPrefix(s[pos:], "//"):
			end := strings.IndexByte(s[pos:], '\n')
			if end < 0 {
				return len(s), nil
			}
			pos += end + 1
		case strings.HasPrefix(s[pos:], "/*"):
			end := strings.Index(s[pos+2:], "*/")
			if end < 0 {
				return 0, fmt.Errorf("at byte %d: unterminated block comment", pos)
			}
			pos += 2 + end + 2
		default:
			return pos, nil
		}
	}
	return pos, nil
}

// scanQuoted reads one double-quoted string starting at s[pos], resolving
// escape sequences, and returns the value and the position just past the
// closing quote.
//
// Escapes are consumed two characters at a time in a single pass. This is the
// same requirement as unescapeAndroid: a literal backslash-n reaches this
// function as the three characters \ \ n, and any approach that resolves
// backslash-backslash separately from backslash-n will silently convert it
// into a real newline.
func scanQuoted(s string, pos int) (value string, next int, err error) {
	if s[pos] != '"' {
		return "", 0, fmt.Errorf("expected an opening quote")
	}
	pos++ // consume the opening quote

	var b strings.Builder
	for pos < len(s) {
		switch c := s[pos]; c {
		case '"':
			return b.String(), pos + 1, nil

		case '\\':
			if pos+1 >= len(s) {
				return "", 0, fmt.Errorf("string ends with a dangling escape")
			}
			pos++
			switch e := s[pos]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			case 'U', 'u':
				// \UXXXX — four hex digits, as emitted for control characters.
				if pos+4 >= len(s) {
					return "", 0, fmt.Errorf("truncated unicode escape")
				}
				var r rune
				if _, err := fmt.Sscanf(s[pos+1:pos+5], "%04x", &r); err != nil {
					return "", 0, fmt.Errorf("malformed unicode escape %q: %w",
						s[pos-1:pos+5], err)
				}
				b.WriteRune(r)
				pos += 4
			default:
				// Preserve unknown escapes verbatim rather than dropping the
				// backslash and silently altering the copy.
				b.WriteByte('\\')
				b.WriteByte(e)
			}
			pos++

		default:
			b.WriteByte(c)
			pos++
		}
	}
	return "", 0, fmt.Errorf("unterminated string")
}
