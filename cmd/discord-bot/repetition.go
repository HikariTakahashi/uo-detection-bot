package main

// repetitionCount counts configured uo/うお/うぉ forms, after the same
// case, width and kana normalization used by shadow collection. All forms are
// two runes long, so fixed-size tokens suffice; arbitrary dictionary word
// segmentation would broaden the accepted language unnecessarily.
// Only spaces between complete words are allowed. Keep leading/trailing space,
// quotes and prose significant rather than stripping them into a reaction.
func repetitionCount(text string, words []string) uint64 {
	runes := []rune(shadowNormalize(text))
	if len(runes) < 4 {
		return 0
	}
	allowed := make(map[string]bool)
	for _, word := range words {
		word = shadowNormalize(word)
		if word == "uo" || word == "うお" || word == "うぉ" {
			allowed[word] = true
		}
	}
	var count uint64
	for i := 0; i < len(runes); {
		if i+2 > len(runes) || !allowed[string(runes[i:i+2])] {
			return 0
		}
		count++
		i += 2
		if i == len(runes) {
			break
		}
		for i < len(runes) && (runes[i] == ' ' || runes[i] == '\u3000') {
			i++
		}
		if i == len(runes) {
			return 0
		}
	}
	if count < 2 {
		return 0
	}
	return count
}
