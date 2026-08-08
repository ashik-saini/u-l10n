package parse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// JSON parses a Flutter localization file: a flat object of string values,
// 4-space indented, in Lokalise's (non-alphabetical) key order.
//
// Decoding goes through encoding/json's tokeniser rather than a hand-rolled
// scanner. Unescaping is the one part of JSON that the standard library gets
// exactly right, including the \n versus \\n distinction this project cannot
// afford to lose. What the standard library cannot do is ENCODE the way
// Lokalise does — it will not escape '/' as '\/' — so pkg/export hand-rolls
// the writer. Decode with the stdlib, encode by hand: still two independent
// implementations, and the round-trip remains a real check.
//
// json.Unmarshal into a map would lose key order, so the tokeniser is stepped
// manually.
func JSON(r io.Reader) (*File, error) {
	dec := json.NewDecoder(r)
	// Reject numbers silently losing precision if a value is ever non-string.
	dec.UseNumber()

	tok, err := dec.Token()
	if err != nil {
		return nil, fmt.Errorf("read opening token: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, fmt.Errorf("expected a JSON object, got %v", tok)
	}

	file := &File{Entries: make([]Entry, 0, 6500)}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("read key: %w", err)
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("expected a string key, got %T", keyTok)
		}

		valTok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("read value for %q: %w", key, err)
		}
		value, ok := valTok.(string)
		if !ok {
			// Nested objects and arrays are not part of the Flutter format.
			// Failing loudly beats silently dropping a key.
			return nil, fmt.Errorf("value for %q is %T, expected string", key, valTok)
		}

		file.Entries = append(file.Entries, Entry{
			Key:        key,
			Value:      value,
			RenderHint: RenderHintPlain,
		})
	}

	if tok, err = dec.Token(); err != nil {
		return nil, fmt.Errorf("read closing token: %w", err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '}' {
		return nil, fmt.Errorf("expected object close, got %v", tok)
	}

	return file, nil
}

// JSONBytes is a convenience wrapper around JSON.
func JSONBytes(data []byte) (*File, error) {
	return JSON(bytes.NewReader(data))
}
