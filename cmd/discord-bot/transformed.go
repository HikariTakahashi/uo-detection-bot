package main

// Transformations are data-only and deliberately finite: one outer decoding,
// then hex/binary interpretation, optionally undoing a one-bit circular shift.
// No recursive decoding, substring searches inside numbers, or code execution.
import (
	"encoding/ascii85"
	"math/bits"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/japanese"
)

const transformedMaxRunes = 2000

var encodedNumber = regexp.MustCompile(`(?i)(?:0x[0-9a-f]|[0-9a-f])+(?:[ \t]+(?:0x[0-9a-f]|[0-9a-f])+)*`)
var encodedUnicode = regexp.MustCompile(`\\(?:u[0-9a-fA-F]{4}|U[0-9a-fA-F]{8})`)

func validDecodedText(text string) bool {
	if text == "" || !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if r == utf8.RuneError || unicode.IsControl(r) && r != '\n' && r != '\t' && r != '\r' {
			return false
		}
	}
	return true
}

// Only candidate selection uses this count; it never becomes a counter delta.
// Latin fragments in identifiers are excluded. Japanese usage stays with AI.
func transformedWordHits(text string, words []string) int {
	text = shadowNormalize(text)
	if n := repetitionCount(text, words); n > 0 {
		return int(n)
	}
	hits := 0
	for _, word := range words {
		if word == "" {
			continue
		}
		first, _ := utf8.DecodeRuneInString(word)
		last, _ := utf8.DecodeLastRuneInString(word)
		for start := 0; start < len(text); {
			i := strings.Index(text[start:], word)
			if i < 0 {
				break
			}
			i += start
			start = i + len(word)
			before, _ := utf8.DecodeLastRuneInString(text[:i])
			// A laughter suffix is part of the reaction, not an identifier.
			after, _ := utf8.DecodeRuneInString(strings.TrimLeft(text[start:], "w"))
			if wordCharacter(first) && wordCharacter(before) || wordCharacter(last) && wordCharacter(after) {
				continue
			}
			hits++
		}
	}
	return hits
}

func decodeUnicodeText(text string) string {
	valid := true
	decoded := encodedUnicode.ReplaceAllStringFunc(text, func(token string) string {
		n, err := strconv.ParseUint(token[2:], 16, 32)
		if err != nil || !utf8.ValidRune(rune(n)) {
			valid = false
			return token
		}
		return string(rune(n))
	})
	// Do not partially salvage malformed escape sequences in the same input.
	if !valid || strings.Contains(decoded, `\u`) || strings.Contains(decoded, `\U`) {
		return ""
	}
	return decoded
}

func decodeASCII85Text(text string) string {
	// Python's a85decode default accepts unframed Ascii85, not <~ ... ~>.
	// The standard decoder permits extra controls and uint32 overflow. Reject
	// both before decoding, including overflow in a padded final group.
	var value uint64
	digits := 0
	for _, r := range text {
		switch {
		case strings.ContainsRune(" \t\n\r\v", r):
			continue
		case r == 'z' && digits == 0:
			continue
		case r >= '!' && r <= 'u':
			value = value*85 + uint64(r-'!')
			digits++
			if digits == 5 {
				if value > 0xffffffff {
					return ""
				}
				value, digits = 0, 0
			}
		default:
			return ""
		}
	}
	if digits == 1 {
		return ""
	}
	if digits > 1 {
		for ; digits < 5; digits++ {
			value = value*85 + 84
		}
		if value > 0xffffffff {
			return ""
		}
	}
	// A 'z' expands to four bytes. Also leave room for the decoder's minimum
	// four-byte destination even for a short final group such as encoded "uo".
	raw := make([]byte, 4*len(text))
	n, consumed, err := ascii85.Decode(raw, []byte(text), true)
	if err != nil || consumed != len(text) {
		return ""
	}
	return string(raw[:n])
}

func decodeShiftJISText(text string) string {
	raw := make([]byte, 0, len(text))
	nonASCII := false
	for _, r := range text {
		if r > 255 {
			return ""
		}
		nonASCII = nonASCII || r > 127
		raw = append(raw, byte(r))
	}
	if !nonASCII {
		return ""
	}
	decoded, err := japanese.ShiftJIS.NewDecoder().Bytes(raw)
	if err != nil {
		return ""
	}
	return string(decoded)
}

func numberCandidates(token string, words []string) []string {
	token = strings.NewReplacer(" ", "", "\t", "", "0x", "", "0X", "").Replace(token)
	var candidates []string
	for _, base := range []int{16, 2} {
		for _, width := range []int{8, 16} {
			digits := width
			if base == 16 {
				digits /= 4
			}
			if len(token)%digits != 0 {
				continue
			}
			for _, rotation := range []int{0, -1, 1} {
				var decoded strings.Builder
				valid := true
				for i := 0; i < len(token); i += digits {
					n, err := strconv.ParseUint(token[i:i+digits], base, width)
					if err != nil {
						valid = false
						break
					}
					if width == 8 {
						decoded.WriteByte(bits.RotateLeft8(uint8(n), rotation))
					} else {
						r := rune(bits.RotateLeft16(uint16(n), rotation))
						if !utf8.ValidRune(r) {
							valid = false
							break
						}
						decoded.WriteRune(r)
					}
				}
				value := decoded.String()
				if valid && validDecodedText(value) && transformedWordHits(value, words) > 0 && !slices.Contains(candidates, value) {
					candidates = append(candidates, value)
				}
			}
		}
	}
	return candidates
}

func decodeNumber(token string, words []string) string {
	candidates := numberCandidates(token, words)
	if len(candidates) == 1 {
		return candidates[0]
	}
	return "" // Ambiguous interpretations must never multiply counts or AI jobs.
}

func decodeNumbers(text string, words []string) string {
	var out strings.Builder
	end := 0
	for _, span := range encodedNumber.FindAllStringIndex(text, -1) {
		before, _ := utf8.DecodeLastRuneInString(text[:span[0]])
		after, _ := utf8.DecodeRuneInString(strings.TrimLeft(text[span[1]:], "wWｗＷ"))
		if wordCharacter(before) || wordCharacter(after) {
			continue
		}
		decoded := decodeNumber(text[span[0]:span[1]], words)
		if decoded == "" {
			continue
		}
		out.WriteString(text[end:span[0]])
		out.WriteString(decoded)
		end = span[1]
	}
	out.WriteString(text[end:])
	return out.String()
}

// Returns one unambiguous full-message view containing a newly recovered
// configured word. Keep surrounding prose/quotes for the semantic classifier.
// URL/code-containing messages are conservatively left to the original path.
func transformedText(text string, words []string) string {
	if len(text) > transformedMaxRunes*utf8.UTFMax || utf8.RuneCountInString(text) > transformedMaxRunes || shadowURL.MatchString(text) {
		return ""
	}
	ascii := decodeASCII85Text(text)
	// Backticks are also legal Ascii85 digits. Only a valid whole Ascii85
	// payload may use that alphabet; do not decode numbers in code snippets.
	if strings.Contains(text, "`") && !validDecodedText(ascii) {
		return ""
	}
	words = slices.Clone(words)
	for i := range words {
		words[i] = shadowNormalize(words[i])
	}
	slices.Sort(words)
	words = slices.Compact(words)
	unquoted, err := url.PathUnescape(text)
	if err != nil {
		unquoted = ""
	}
	views := []string{text, unquoted, decodeUnicodeText(text), ascii, decodeShiftJISText(text)}
	if strings.Contains(text, "`") {
		views = []string{ascii}
	}
	var candidates []string
	for i, view := range views {
		// The raw Latin-1 mojibake can contain C1 controls. Only decoded views
		// must be valid text; never pass those controls through to the model.
		if i != 0 && !validDecodedText(view) {
			continue
		}
		view = decodeNumbers(view, words)
		if view == text || !validDecodedText(view) || strings.Contains(view, "`") || shadowURL.MatchString(view) || transformedWordHits(view, words) <= transformedWordHits(text, words) {
			continue
		}
		if !slices.Contains(candidates, view) {
			candidates = append(candidates, view)
		}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	return ""
}

func (d *detector) transformedText(text string) string {
	words := make([]string, 0, len(d.words))
	for word := range d.words {
		words = append(words, word)
	}
	return transformedText(text, words)
}

func (d *detector) literalCount(text string) uint64 {
	// Keep existing whole-message matches, including explicitly configured
	// punctuation. Otherwise allow only a trailing run of question/exclamation
	// marks; whitespace, quotations and surrounding prose remain significant.
	if n := d.bareLiteralCount(text); n > 0 {
		return n
	}
	return d.bareLiteralCount(strings.TrimRight(text, "?!？！"))
}

func (d *detector) bareLiteralCount(text string) uint64 {
	if n := d.repetitionCount(text); n > 0 {
		return n
	}
	if d.words[text] {
		return 1
	}
	return 0
}
