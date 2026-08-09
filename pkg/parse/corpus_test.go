package parse

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Phase 2 gate: every committed u-mobile localization file parses, at an
// exact entry count.
//
// The counts below were measured independently (Python: json.load for the
// Flutter files, ElementTree for the Android files, a regex scan for the iOS
// files) against the u-mobile working tree on 2026-08-08. They are a tripwire,
// not a specification: if u-mobile is re-pulled these will fail, and the right
// response is to re-measure deliberately and update them in the same commit
// that explains why they moved.
//
// The suite skips when u-mobile is not checked out alongside this repo. Point
// U_MOBILE_PATH at it to run elsewhere.
const envUMobilePath = "U_MOBILE_PATH"

func uMobileRoot(t *testing.T) string {
	t.Helper()

	root := os.Getenv(envUMobilePath)
	if root == "" {
		// pkg/parse -> repo root -> BE -> YouTrip -> FE/u-mobile
		root = filepath.Join("..", "..", "..", "..", "FE", "u-mobile")
	}
	if _, err := os.Stat(filepath.Join(root, "assets", "langs")); err != nil {
		t.Skipf("u-mobile not available at %s (set %s): %v", root, envUMobilePath, err)
	}
	return root
}

func TestCorpusFlutterJSON(t *testing.T) {
	root := uMobileRoot(t)

	// locale -> statements in the file, distinct keys, statements whose value is "".
	// The empty count is per STATEMENT, so a duplicated empty key counts twice.
	// entries > unique means the file contains duplicate keys; see
	// TestCorpusDuplicateKeys.
	// Re-measured 2026-08-09 (python json.load with object_pairs_hook, which
	// keeps duplicate statements) after the u-mobile checkout moved to
	// 0e2fe9190: only the six Flutter files changed; the Android and iOS
	// counts below were unaffected, and the duplicate-key counts in
	// TestCorpusDuplicateKeys still match.
	want := map[string]struct{ entries, unique, empty int }{
		"en-SG": {5940, 5917, 339},
		"en-MY": {6456, 6454, 3649},
		"en-AU": {6154, 6142, 3078},
		"ms-MY": {6393, 6393, 3642},
		"th-TH": {5383, 5383, 3826},
		"en-TH": {5382, 5382, 3800},
	}

	for locale, w := range want {
		t.Run(locale, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, "assets", "langs", locale+".json"))
			require.NoError(t, err)

			file, err := JSONBytes(data)
			require.NoError(t, err)

			assert.Equal(t, w.entries, file.Len(), "statements in the file")
			assert.Equal(t, w.unique, file.UniqueLen(), "distinct keys")

			var empty int
			for _, e := range file.Entries {
				if e.Value == "" {
					empty++
				}
			}
			assert.Equal(t, w.empty, empty,
				"explicitly-empty values must be counted as PRESENT, not dropped")

		})
	}
}

// TestCorpusPerLocaleKeyPresenceDiffers is the evidence for the schema's
// three-state rule. If key presence were uniform across locales, "absent"
// could safely mean "empty" and the whole distinction would be unnecessary.
func TestCorpusPerLocaleKeyPresenceDiffers(t *testing.T) {
	root := uMobileRoot(t)

	load := func(locale string) *File {
		data, err := os.ReadFile(filepath.Join(root, "assets", "langs", locale+".json"))
		require.NoError(t, err)
		file, err := JSONBytes(data)
		require.NoError(t, err)
		return file
	}

	enSG, enMY := load("en-SG"), load("en-MY")
	assert.Greater(t, enMY.Len(), enSG.Len(),
		"en-MY genuinely has MORE keys than en-SG; per-locale presence is real data")

	inSG := make(map[string]bool, enSG.Len())
	for _, e := range enSG.Entries {
		inSG[e.Key] = true
	}
	var onlyInMY int
	for _, e := range enMY.Entries {
		if !inSG[e.Key] {
			onlyInMY++
		}
	}
	assert.Positive(t, onlyInMY, "keys exist in en-MY and not in en-SG")
}

func TestCorpusAndroidXML(t *testing.T) {
	root := uMobileRoot(t)

	want := map[string]int{
		"values":        3199,
		"values-ab":     3103,
		"values-am":     5002,
		"values-au":     4774,
		"values-en":     3149,
		"values-en-rMY": 5001,
		"values-ms":     5005,
		"values-th":     2603,
	}

	for dir, count := range want {
		t.Run(dir, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(
				root, "android", "app", "src", "main", "res", dir, "strings.xml"))
			require.NoError(t, err)

			file, err := XMLBytes(data)
			require.NoError(t, err)

			// This assertion also guards against silent loss: encoding/xml
			// ignores element types absent from the target struct, so an
			// unexpected <plurals> or <string-array> would vanish without
			// error. The count is what catches it.
			assert.Equal(t, count, file.Len(), "<string> count")
			assert.Equal(t, count, file.UniqueLen(),
				"the Android files are duplicate-free, unlike the Flutter and iOS ones")
		})
	}
}

// TestCorpusAndroidCDATA pins the five CDATA-wrapped values. They are a
// per-value render hint that the exporter must reproduce exactly.
func TestCorpusAndroidCDATA(t *testing.T) {
	root := uMobileRoot(t)

	data, err := os.ReadFile(filepath.Join(
		root, "android", "app", "src", "main", "res", "values", "strings.xml"))
	require.NoError(t, err)

	file, err := XMLBytes(data)
	require.NoError(t, err)

	var wrapped []string
	for _, e := range file.Entries {
		if e.RenderHint == RenderHintCDATA {
			wrapped = append(wrapped, e.Key)
		}
	}

	assert.ElementsMatch(t, []string{
		"frame_1775__dollar_copy_34__50",
		"remittance_landing_transferrablebal_info",
		"remittance_transferrablebal_info_bottomsheet_desc",
		"rewards01__0_pending__b_completed",
		"ytfam_accountsuspensionparentend_txt",
	}, wrapped, "exactly these five values are CDATA-wrapped")
}

// TestCorpusAndroidKeyTransform documents that Android files carry TRANSFORMED
// key names. Reversing the transform is the importer's job; the parser reports
// what the file says.
func TestCorpusAndroidKeyTransform(t *testing.T) {
	root := uMobileRoot(t)

	data, err := os.ReadFile(filepath.Join(
		root, "android", "app", "src", "main", "res", "values", "strings.xml"))
	require.NoError(t, err)

	file, err := XMLBytes(data)
	require.NoError(t, err)

	// Canonical key '01-menu-help__category_title__17' appears in the Android
	// file with its leading digits stripped and hyphens replaced.
	_, found := file.Lookup("_menu_help__category_title__17")
	assert.True(t, found, "Android names are transformed: leading digits stripped, [^A-Za-z0-9_] -> _")

	_, found = file.Lookup("01-menu-help__category_title__17")
	assert.False(t, found, "the canonical name does not appear in the Android file")
}

func TestCorpusIOSStrings(t *testing.T) {
	root := uMobileRoot(t)

	want := map[string]struct{ entries, unique int }{
		"Localizable.strings":             {929, 900},
		"Base.lproj/Localizable.strings":  {4897, 4897},
		"en.lproj/Localizable.strings":    {3227, 3227},
		"en-MY.lproj/Localizable.strings": {5080, 5078},
		"en-TH.lproj/Localizable.strings": {3240, 3238},
		"en-AU.lproj/Localizable.strings": {4973, 4973},
		"ms.lproj/Localizable.strings":    {5079, 5079},
		"th.lproj/Localizable.strings":    {3150, 3149},
	}

	for rel, w := range want {
		t.Run(rel, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, "ios", "Runner", rel))
			require.NoError(t, err)

			file, err := Strings(data)
			require.NoError(t, err)

			assert.Equal(t, w.entries, file.Len(), "statements in the file")
			assert.Equal(t, w.unique, file.UniqueLen(), "distinct keys")
		})
	}
}

// TestCorpusIOSSurrogatePairEmoji is the live proof for the surrogate-pair
// decoder: the committed corpus really does carry emoji as paired \uXXXX
// escapes, and they must decode to the flag, never to U+FFFD runs.
func TestCorpusIOSSurrogatePairEmoji(t *testing.T) {
	root := uMobileRoot(t)

	for rel, key := range map[string]string{
		"Base.lproj/Localizable.strings":  "CrossMarketReferralCarouselBannerMsg",
		"en-AU.lproj/Localizable.strings": "CrossMarketReferralCarouselBannerMsg",
		"en-MY.lproj/Localizable.strings": "nudge_referralmy_desc",
		"ms.lproj/Localizable.strings":    "nudge_referralmy_desc",
	} {
		t.Run(rel, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(root, "ios", "Runner", rel))
			require.NoError(t, err)

			file, err := Strings(data)
			require.NoError(t, err)

			got, ok := file.Lookup(key)
			require.True(t, ok, key)
			assert.Contains(t, got, "\U0001F1F2\U0001F1FE",
				"the escaped surrogate pairs must decode to the MY flag emoji")
			assert.NotContains(t, got, string(rune(0xFFFD)),
				"a surrogate half must never become U+FFFD")
		})
	}
}

// ---------------------------------------------------------------------------
// Raw-text tripwires.
//
// The parser makes silent assumptions about what the Android corpus can
// contain — encoding/xml and the hand-rolled entity decoding would not fail
// loudly if they were violated. These tests turn each assumption into a
// verified, pinned fact about the committed files. When one fails after a
// corpus re-pull, re-measure and update the pinned facts in the same commit
// that explains why they moved.
// ---------------------------------------------------------------------------

// androidCorpusDirs lists the eight committed Android resource directories.
var androidCorpusDirs = []string{
	"values", "values-ab", "values-am", "values-au",
	"values-en", "values-en-rMY", "values-ms", "values-th",
}

func readAndroidCorpus(t *testing.T, root string) map[string]string {
	t.Helper()
	files := make(map[string]string, len(androidCorpusDirs))
	for _, dir := range androidCorpusDirs {
		data, err := os.ReadFile(filepath.Join(
			root, "android", "app", "src", "main", "res", dir, "strings.xml"))
		require.NoError(t, err)
		files[dir] = string(data)
	}
	return files
}

var corpusCDATASection = regexp.MustCompile(`(?s)<!\[CDATA\[.*?\]\]>`)

// TestCorpusAndroidRawTripwires scans the raw file text — deliberately not the
// parser's output, which cannot distinguish an escaped quote from a bare one
// once unescaping has run.
func TestCorpusAndroidRawTripwires(t *testing.T) {
	root := uMobileRoot(t)
	files := readAndroidCorpus(t, root)

	t.Run("no escaped inline markup", func(t *testing.T) {
		// &lt;b&gt; as TEXT would mean the copy carries markup one escaping
		// layer deeper than the parser's carve-out for raw <b>/<u>. Zero
		// occurrences today.
		re := regexp.MustCompile(`&lt;/?[bu]&gt;`)
		for dir, data := range files {
			assert.Empty(t, re.FindAllString(data, -1), "%s", dir)
		}
	})

	t.Run("non-b/u inline tags appear only inside CDATA", func(t *testing.T) {
		// The plain-path exporter only carves out <b> and <u>; every other
		// inline tag the corpus holds (<link>, <A>, <B>) lives inside CDATA,
		// where no escaping applies. A non-b/u tag OUTSIDE CDATA would be
		// re-escaped on export and change what the app renders.
		tag := regexp.MustCompile(`</?[A-Za-z][^>]*>`)
		stringOpen := regexp.MustCompile(`^<string name="[^"]*">$`)
		allowed := map[string]bool{
			"<resources>": true, "</resources>": true, "</string>": true,
			"<b>": true, "</b>": true, "<u>": true, "</u>": true,
		}
		for dir, data := range files {
			outside := corpusCDATASection.ReplaceAllString(data, "")
			for _, m := range tag.FindAllString(outside, -1) {
				if !allowed[m] && !stringOpen.MatchString(m) {
					t.Errorf("%s: unexpected tag %q outside CDATA", dir, m)
				}
			}
		}

		// Pin the tags that DO exist, so this test is known to be looking at
		// real data rather than vacuously passing.
		inCDATA := map[string]int{}
		for _, data := range files {
			for _, section := range corpusCDATASection.FindAllString(data, -1) {
				for _, m := range tag.FindAllString(section, -1) {
					if !allowed[m] {
						inCDATA[m]++
					}
				}
			}
		}
		assert.Equal(t, map[string]int{
			"<link>": 3, "</link>": 3, "<A>": 4, "<B>": 4,
		}, inCDATA, "the non-b/u inline tags, all inside CDATA")
	})

	t.Run("bare quotes pinned to the one known instance", func(t *testing.T) {
		// aapt gives an unescaped '"' quote-section semantics (whitespace
		// inside is preserved, the quotes themselves vanish). The corpus has
		// exactly ONE value relying on that — a DATA question deliberately
		// not handled in code — so a second instance must trip this wire
		// before it ships.
		body := regexp.MustCompile(`(?s)<string name="([^"]+)">(.*?)</string>`)
		type hit struct {
			dir, key string
			count    int
		}
		var hits []hit
		for dir, data := range files {
			for _, m := range body.FindAllStringSubmatch(data, -1) {
				key, text := m[1], corpusCDATASection.ReplaceAllString(m[2], "")
				var bare int
				for i := 0; i < len(text); i++ {
					if text[i] == '"' && (i == 0 || text[i-1] != '\\') {
						bare++
					}
				}
				if bare > 0 {
					hits = append(hits, hit{dir, key, bare})
				}
			}
		}
		assert.Equal(t, []hit{{"values-th", "TopUpMaxInfoMsg", 2}}, hits,
			"exactly one value in the whole Android corpus contains bare quotes")
	})

	t.Run("no numeric character references", func(t *testing.T) {
		// The entity decoder resolves &#dd; and &#xhh; — but the committed
		// corpus contains ZERO numeric references of any kind, not even the
		// &#39;/&#34; forms the decoder originally special-cased. If one
		// appears, decoding becomes load-bearing: re-verify it round-trips.
		re := regexp.MustCompile(`&#[0-9]+;|&#[xX][0-9a-fA-F]+;`)
		for dir, data := range files {
			assert.Empty(t, re.FindAllString(data, -1), "%s", dir)
		}
	})
}

// TestCorpusIOSLineEndingsAreMixed records the state of the tree, because it
// is the reason the .strings line ending is a serializer OPTION rather than a
// constant. Only ios/Runner/Localizable.strings carries carriage returns.
func TestCorpusIOSLineEndingsAreMixed(t *testing.T) {
	root := uMobileRoot(t)

	countCR := func(rel string) int {
		data, err := os.ReadFile(filepath.Join(root, "ios", "Runner", rel))
		require.NoError(t, err)
		var n int
		for _, b := range data {
			if b == '\r' {
				n++
			}
		}
		return n
	}

	assert.Equal(t, 959, countCR("Localizable.strings"),
		"the root file is CRLF — committed content, not a checkout artefact")
	for _, rel := range []string{
		"Base.lproj/Localizable.strings",
		"en.lproj/Localizable.strings",
		"en-MY.lproj/Localizable.strings",
	} {
		assert.Zero(t, countCR(rel), "%s is pure LF", rel)
	}
}

// TestCorpusNewlineFormsCoexist proves both newline forms are present in real
// data, in quantity, and that the parser keeps them distinct.
func TestCorpusNewlineFormsCoexist(t *testing.T) {
	root := uMobileRoot(t)

	data, err := os.ReadFile(filepath.Join(root, "assets", "langs", "en-SG.json"))
	require.NoError(t, err)

	file, err := JSONBytes(data)
	require.NoError(t, err)

	var realNewline, literalBackslashN int
	for _, e := range file.Entries {
		for i := 0; i < len(e.Value); i++ {
			switch {
			case e.Value[i] == '\n':
				realNewline++
			case e.Value[i] == '\\' && i+1 < len(e.Value) && e.Value[i+1] == 'n':
				literalBackslashN++
				i++
			}
		}
	}

	assert.Positive(t, realNewline, "values containing real newlines exist")
	assert.Positive(t, literalBackslashN, "values containing a literal backslash-n exist")
	t.Logf("en-SG: %d real newlines, %d literal backslash-n — both must survive export",
		realNewline, literalBackslashN)
}

// TestCorpusDuplicateKeys pins a real data-quality problem in the committed
// tree, discovered while building this parser.
//
// Several files declare the same key more than once, and a few give it
// DIFFERENT values. Both consumers take the last occurrence, so the file is
// not ambiguous at runtime — but it cannot be represented in the database,
// where an active key name is unique. The importer must therefore collapse
// duplicates last-wins AND record a warning, and the drift-reconciliation
// report at cutover should list them for an owner to sign off.
//
// This also bounds the Phase 3 round-trip gate: a file containing duplicates
// cannot be reproduced byte-for-byte from a store that can only hold one value
// per key. Gate R must run against a fresh Lokalise export — where keys are
// unique by construction — not against these committed files.
func TestCorpusDuplicateKeys(t *testing.T) {
	root := uMobileRoot(t)

	t.Run("flutter", func(t *testing.T) {
		want := map[string]int{
			"en-SG": 23, "en-AU": 12, "en-MY": 2,
			"ms-MY": 0, "th-TH": 0, "en-TH": 0,
		}
		for locale, count := range want {
			data, err := os.ReadFile(filepath.Join(root, "assets", "langs", locale+".json"))
			require.NoError(t, err)
			file, err := JSONBytes(data)
			require.NoError(t, err)
			assert.Len(t, file.Duplicates(), count, "%s duplicate keys", locale)
		}
	})

	t.Run("ios", func(t *testing.T) {
		want := map[string]int{
			"Localizable.strings":             29,
			"en-MY.lproj/Localizable.strings": 2,
			"en-TH.lproj/Localizable.strings": 2,
			"th.lproj/Localizable.strings":    1,
			"Base.lproj/Localizable.strings":  0,
			"en.lproj/Localizable.strings":    0,
			"en-AU.lproj/Localizable.strings": 0,
			"ms.lproj/Localizable.strings":    0,
		}
		for rel, count := range want {
			data, err := os.ReadFile(filepath.Join(root, "ios", "Runner", rel))
			require.NoError(t, err)
			file, err := Strings(data)
			require.NoError(t, err)
			assert.Len(t, file.Duplicates(), count, "%s duplicate keys", rel)
		}
	})

	// The dangerous subset: same key, different values. Collapsing these
	// silently would change what the app displays.
	t.Run("conflicting values", func(t *testing.T) {
		conflicting := func(f *File) int {
			var n int
			for _, values := range f.Duplicates() {
				first := values[0]
				for _, v := range values[1:] {
					if v != first {
						n++
						break
					}
				}
			}
			return n
		}

		for rel, want := range map[string]int{
			"Localizable.strings":             0, // 29 duplicates, all identical
			"en-MY.lproj/Localizable.strings": 1,
			"en-TH.lproj/Localizable.strings": 2,
			"th.lproj/Localizable.strings":    1,
		} {
			data, err := os.ReadFile(filepath.Join(root, "ios", "Runner", rel))
			require.NoError(t, err)
			file, err := Strings(data)
			require.NoError(t, err)
			assert.Equal(t, want, conflicting(file),
				"%s: keys duplicated with DIFFERENT values", rel)
		}
	})
}
