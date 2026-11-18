package main

// This subprocess mode never opens Discord, state files, or credentials. Python
// supplies a graph node; the same fixed Go transforms provide data-only edges.
import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

type restorationView struct {
	Candidates   []string `json:"candidates"`
	LiteralUnits uint64   `json:"literal_units"`
}

func fixedView(text string, words []string) restorationView {
	d := &detector{words: map[string]bool{}}
	for _, w := range words {
		d.words[w] = true
	}
	out := restorationView{Candidates: []string{}, LiteralUnits: d.literalCount(text)}
	if out.LiteralUnits > 0 || shadowURL.MatchString(text) {
		return out
	}
	ascii := decodeASCII85Text(text)
	if strings.Contains(text, "`") && !validDecodedText(ascii) {
		return out
	}
	add := func(v string) {
		if v != text && validDecodedText(v) && utf8.RuneCountInString(v) <= 2000 && !shadowURL.MatchString(v) && !strings.Contains(v, "`") && !slices.Contains(out.Candidates, v) {
			out.Candidates = append(out.Candidates, v)
		}
	}
	if strings.Contains(text, "`") {
		add(ascii)
		return out
	}
	// Legacy word-aware numeric interpretations remain candidates, never votes.
	normalized := slices.Clone(words)
	for i := range normalized {
		normalized[i] = shadowNormalize(normalized[i])
	}
	add(transformedText(text, words))
	if encodedNumber.FindString(text) == text {
		for _, v := range numberCandidates(text, normalized) {
			add(v)
		}
		// A canonical UTF-8 hex layer may wrap a still encoded inner body.
		token := strings.NewReplacer(" ", "", "\t", "", "0x", "", "0X", "").Replace(text)
		if raw, err := hex.DecodeString(token); err == nil {
			add(string(raw))
		}
	}
	if v, err := url.PathUnescape(text); err == nil {
		add(v)
	}
	add(decodeUnicodeText(text))
	add(ascii)
	add(decodeShiftJISText(text))
	add(decodeNumbers(text, normalized))
	// Keep competing interpretations inside surrounding prose visible to the
	// graph as well; decodeNumbers alone deliberately suppresses ambiguity.
	for _, span := range encodedNumber.FindAllStringIndex(text, -1) {
		before, _ := utf8.DecodeLastRuneInString(text[:span[0]])
		after, _ := utf8.DecodeRuneInString(strings.TrimLeft(text[span[1]:], "wWｗＷ"))
		if wordCharacter(before) || wordCharacter(after) {
			continue
		}
		for _, v := range numberCandidates(text[span[0]:span[1]], normalized) {
			add(text[:span[0]] + v + text[span[1]:])
		}
	}
	return out
}

func restoreView(in io.Reader, out io.Writer) error {
	raw, err := io.ReadAll(io.LimitReader(in, 32769))
	if err != nil {
		return err
	}
	var request struct {
		Text  string   `json:"text"`
		Words []string `json:"words"`
	}
	if len(raw) > 32768 || decodeJSON(raw, &request) != nil || !utf8.ValidString(request.Text) || utf8.RuneCountInString(request.Text) > 2000 || len(request.Words) == 0 || len(request.Words) > 1024 {
		return errors.New("invalid restoration request")
	}
	for _, w := range request.Words {
		if w == "" || utf8.RuneCountInString(w) > 80 {
			return errors.New("invalid word")
		}
	}
	return json.NewEncoder(out).Encode(fixedView(request.Text, request.Words))
}
