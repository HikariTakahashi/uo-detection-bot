package main

// repetitionCandidate recognizes only configured uo/うお forms, after the same
// case, width and kana normalization used by shadow collection. Both forms are
// two runes long, so fixed-size tokens suffice; arbitrary dictionary word
// segmentation would broaden the accepted language unnecessarily.
// Keep the entire original body: stripping quotes, URLs or whitespace could
// turn an explanation or a different message into a bare reaction.
func repetitionCandidate(text string, words []string) bool {
	allowed := make(map[string]bool)
	for _, word := range words {
		word = shadowNormalize(word)
		if word == "uo" || word == "うお" {
			allowed[word] = true
		}
	}
	runes := []rune(shadowNormalize(text))
	if len(runes) < 4 || len(runes)%2 != 0 {
		return false
	}
	for i := 0; i < len(runes); i += 2 {
		if !allowed[string(runes[i:i+2])] {
			return false
		}
	}
	return true
}
