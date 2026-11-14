package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
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

func repetitionShadow(t *testing.T) *shadowCollector {
	t.Helper()
	s := testShadow(t)
	s.words = append(s.words, "uo")
	s.detector.words["uo"] = true
	s.config.SampleNonCandidatesEvery = 1000
	return s
}

func TestRepetitionWithoutContextIsUncertainAndNeverCallsAI(t *testing.T) {
	for _, reply := range []string{"", " \n", "https://example.com/uo"} {
		s := repetitionShadow(t)
		s.config.Endpoint = ":invalid" // Any accidental request fails the test.
		m := testMessage("uououo")
		m.MessageReference = &discordgo.MessageReference{MessageID: testUser2, ChannelID: m.ChannelID}
		m.ReferencedMessage = &discordgo.Message{ID: testUser2, Content: reply}
		sends := &fakeMessages{}
		for range 2 {
			if err := s.detector.handleIncoming(sends, s, m); err != nil {
				t.Fatal(err)
			}
		}
		if len(s.jobs) != 1 || len(s.laughter) != 0 {
			t.Fatal("candidate sampled out, duplicated, or routed to live AI")
		}
		r := <-s.jobs
		if r.Selection != "repetition_candidate" || r.SampleEvery != 1 {
			t.Fatal("incorrect candidate selection", r)
		}
		s.process(context.Background(), r)
		stored, ok := s.store.records[m.ID]
		if !ok || stored.Error != "" || stored.Prediction.Label != "uncertain" || stored.Prediction.Model != "repetition_filter" {
			t.Fatal("bare repetition must be reviewable without AI", stored)
		}
		if len(sends.messages) != 0 || len(s.detector.counts) != 0 {
			t.Fatal("repetition changed live notifications or counts")
		}
		// A persisted uncertain item survives restart and can be human-reviewed.
		reloaded, err := openShadowStore(s.store.dir, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := reloaded.next(m.GuildID, nil, time.Now()); !ok {
			t.Fatal("repetition not available for review after restart")
		}
	}
}

func TestRepetitionWithContextIsNeverSentToAI(t *testing.T) {
	s := repetitionShadow(t)
	s.config.Endpoint = ":invalid" // Any accidental request fails the test.
	m := testMessage("uououo")
	m.MessageReference = &discordgo.MessageReference{MessageID: testUser2, ChannelID: m.ChannelID}
	m.ReferencedMessage = &discordgo.Message{ID: testUser2, Content: "大会で優勝した！"}
	sends := &fakeMessages{}
	if err := s.detector.handleIncoming(sends, s, m); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs) != 1 || len(s.laughter) != 0 {
		t.Fatal("missing review-only candidate")
	}
	s.process(context.Background(), <-s.jobs)
	r := s.store.records[m.ID]
	if r.Error != "" || r.Context != "大会で優勝した！" || r.Prediction.Label != "uncertain" || r.Prediction.Model != "repetition_filter" {
		t.Fatal("contextual repetition was sent to AI or stored incorrectly", r)
	}
	if len(sends.messages) != 0 || len(s.detector.counts) != 0 || len(s.detector.aiCounted) != 0 {
		t.Fatal("review candidate changed live state")
	}
}

func TestRepetitionCannotUseUnavailableOrUnrelatedContext(t *testing.T) {
	for _, kind := range []string{"missing", "other_channel", "other_guild", "wrong_message", "too_long"} {
		t.Run(kind, func(t *testing.T) {
			s := repetitionShadow(t)
			s.config.Endpoint = ":invalid"
			m := testMessage("uououo")
			m.MessageReference = &discordgo.MessageReference{MessageID: testUser2, ChannelID: m.ChannelID, GuildID: m.GuildID}
			m.ReferencedMessage = &discordgo.Message{ID: testUser2, Content: "参考"}
			switch kind {
			case "missing":
				m.ReferencedMessage = nil
			case "other_channel":
				m.MessageReference.ChannelID = testUser
			case "other_guild":
				m.MessageReference.GuildID = testUser
			case "wrong_message":
				m.ReferencedMessage.ID = testUser
			case "too_long":
				m.ReferencedMessage.Content = strings.Repeat("あ", 2000)
			}
			s.enqueue(m)
			if len(s.jobs) != 1 {
				t.Fatal("missing candidate")
			}
			r := <-s.jobs
			if r.Context != "" {
				t.Fatal("unusable context included")
			}
			s.process(context.Background(), r)
			if got := s.store.records[m.ID]; got.Error != "" || got.Prediction.Label != "uncertain" {
				t.Fatal("missing context was not held for review", got)
			}
		})
	}
}

func TestRepetitionHonorsShadowScopeAndExactConfiguration(t *testing.T) {
	for _, kind := range []string{"off", "outside_channel", "bot", "webhook", "dm", "too_long"} {
		t.Run(kind, func(t *testing.T) {
			s := repetitionShadow(t)
			m := testMessage("uououo")
			switch kind {
			case "off":
				s.config.Mode = "off"
			case "outside_channel":
				m.ChannelID = testUser
			case "bot":
				m.Author.Bot = true
			case "webhook":
				m.WebhookID = testUser
			case "dm":
				m.GuildID = ""
			case "too_long":
				m.Content = strings.Repeat("uo", 1001)
			}
			if s.enqueue(m) || len(s.jobs) != 0 || len(s.laughter) != 0 {
				t.Fatal("collected outside review scope")
			}
		})
	}
	s := repetitionShadow(t)
	s.detector.words["uouo"] = true
	s.words = append(s.words, "uouo")
	sends := &fakeMessages{}
	if err := s.detector.handleIncoming(sends, s, testMessage("uouo")); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs) != 1 || (<-s.jobs).Selection != "keyword_candidate" || len(sends.messages) != 1 {
		t.Fatal("explicit exact-match configuration changed")
	}
}
