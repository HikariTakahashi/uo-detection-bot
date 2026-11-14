package main

import (
	"strings"
	"testing"
)

func TestRepetitionCandidateBoundaries(t *testing.T) {
	words := []string{"uo", "うお", "どわー", "ワオ", "♓", "うぉ", "u", "o"}
	for _, text := range []string{"uouo", "uououo", "うおうお", "uoＵＯウオ", strings.Repeat("uo", 1000)} {
		if !repetitionCandidate(text, words) {
			t.Errorf("missed candidate %q", text)
		}
	}
	for _, text := range []string{
		"", "uo", "うお", "uou", "uouou", "duououo", "uououox", "uououow", "uoどわー",
		"ワオワオ", "♓♓", "うぉうぉ", "ｳｵｳｵ", "uuoo", "uo uo", "uo\nuo", " uouo", "uouo\n",
		"uouo！", "uouoは入力例", "「uouo」", "`uouo`", "> uouo", "https://example.com/uouo",
		"uouo https://example.com", "uo「説明」uo", "uo\u200buo",
	} {
		if repetitionCandidate(text, words) {
			t.Errorf("unexpected candidate %q", text)
		}
	}
	for _, text := range []string{"uouo", "uoうお"} {
		if repetitionCandidate(text, []string{"うお"}) {
			t.Errorf("accepted unconfigured form %q", text)
		}
	}
	if !repetitionCandidate("ＵＯuo", []string{"ＵＯ"}) {
		t.Fatal("configured normalized form not recognized")
	}
}

func TestRepetitionUsesNormalDetectionOnceAndSkipsShadow(t *testing.T) {
	for _, text := range []string{"うおうお", "uououo", "uoうお"} {
		t.Run(text, func(t *testing.T) {
			d := testDetector(t)
			d.words["uo"] = true
			s := testShadow(t)
			s.detector = d
			s.words = append(s.words, "uo")
			sends := &fakeMessages{}
			m := testMessage(text)
			if err := d.handleIncoming(sends, s, m); err != nil {
				t.Fatal(err)
			}
			if len(sends.messages) != 1 || d.counts[testUser] != 1 {
				t.Fatal("repetition was not normally detected", sends.messages, d.counts)
			}
			if len(s.jobs) != 0 || len(s.laughter) != 0 || len(s.store.records) != 0 {
				t.Fatal("repetition was also collected as a shadow record")
			}
		})
	}
}

func TestRepetitionObeysNormalDetectionScope(t *testing.T) {
	for _, kind := range []string{"outside_channel", "bot", "webhook", "dm"} {
		t.Run(kind, func(t *testing.T) {
			d := testDetector(t)
			d.words["uo"] = true
			m := testMessage("うおうお")
			switch kind {
			case "outside_channel":
				m.ChannelID = testUser2
			case "bot":
				m.Author.Bot = true
			case "webhook":
				m.WebhookID = testUser
			case "dm":
				m.GuildID = ""
			}
			sends := &fakeMessages{}
			if err := d.handleMessage(sends, m); err != nil {
				t.Fatal(err)
			}
			if len(sends.messages) != 0 || len(d.counts) != 0 {
				t.Fatal("repetition bypassed normal scope", kind)
			}
		})
	}
}
