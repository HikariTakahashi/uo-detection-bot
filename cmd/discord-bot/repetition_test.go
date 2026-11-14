package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestRepetitionCountBoundaries(t *testing.T) {
	words := []string{"uo", "うお", "どわー", "ワオ", "♓", "うぉ", "u", "o"}
	for text, want := range map[string]uint64{"uouo": 2, "uououo": 3, "うおうお": 2, "uoＵＯウオ": 3, strings.Repeat("uo", 1000): 1000} {
		if got := repetitionCount(text, words); got != want {
			t.Errorf("%q: counted %d words, want %d", text, got, want)
		}
	}
	for _, text := range []string{
		"", "uo", "うお", "uou", "uouou", "duououo", "uououox", "uououow", "uoどわー",
		"ワオワオ", "♓♓", "うぉうぉ", "ｳｵｳｵ", "uuoo", "uo uo", "uo\nuo", " uouo", "uouo\n",
		"uouo！", "uouoは入力例", "「uouo」", "`uouo`", "> uouo", "https://example.com/uouo",
		"uouo https://example.com", "uo「説明」uo", "uo\u200buo",
	} {
		if repetitionCount(text, words) != 0 {
			t.Errorf("unexpected candidate %q", text)
		}
	}
	for _, text := range []string{"uouo", "uoうお"} {
		if repetitionCount(text, []string{"うお"}) != 0 {
			t.Errorf("accepted unconfigured form %q", text)
		}
	}
	if repetitionCount("ＵＯuo", []string{"ＵＯ"}) != 2 {
		t.Fatal("configured normalized form not recognized")
	}
}

func TestRepetitionAddsEveryWordAndRepliesOnce(t *testing.T) {
	for text, increment := range map[string]uint64{"うおうお": 2, "uououo": 3, "uoうお": 2, "うおうおうおうお": 4} {
		t.Run(text, func(t *testing.T) {
			d := testDetector(t)
			d.words["uo"] = true
			d.words["うおうお"] = true // A full match must not hide its component words.
			d.counts[testUser] = 7
			s := testShadow(t)
			s.detector = d
			s.words = append(s.words, "uo")
			sends := &fakeMessages{}
			m := testMessage(text)
			if err := d.handleIncoming(sends, s, m); err != nil {
				t.Fatal(err)
			}
			want := 7 + increment
			if len(sends.messages) != 1 || d.counts[testUser] != want {
				t.Fatal("wrong reply count or total", len(sends.messages), d.counts)
			}
			if !strings.Contains(sends.messages[0].Content, fmt.Sprintf("%d回目", want)) {
				t.Fatalf("reply does not show updated total: %q", sends.messages[0].Content)
			}
			if counts, err := loadCounts(d.statePath); err != nil || counts[testUser] != want {
				t.Fatalf("wrong persisted total: %v, %v", counts, err)
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
