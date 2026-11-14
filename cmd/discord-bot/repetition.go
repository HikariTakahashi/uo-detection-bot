package main

// repetitionCount counts consecutive configured uo/うお forms, after the same
// case, width and kana normalization used by shadow collection. Both forms are
// two runes long, so fixed-size tokens suffice; arbitrary dictionary word
// segmentation would broaden the accepted language unnecessarily.
// Keep the entire original body: stripping quotes, URLs or whitespace could
// turn an explanation or a different message into a bare reaction.
func repetitionCount(text string, words []string) uint64 {
	runes := []rune(shadowNormalize(text))
	if len(runes) < 4 || len(runes)%2 != 0 {
		return 0
	}
	allowed := make(map[string]bool)
	for _, word := range words {
		word = shadowNormalize(word)
		if word == "uo" || word == "うお" {
			allowed[word] = true
		}
	}
	for i := 0; i < len(runes); i += 2 {
		if !allowed[string(runes[i:i+2])] {
			return 0
		}
	}
	return uint64(len(runes) / 2)
}
