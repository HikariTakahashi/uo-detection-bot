package main

import (
	"context"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestPunctuatedReactionBoundaries(t *testing.T) {
	d := &detector{words: map[string]bool{"うお": true, "うぉ": true, "どわー": true, "uo": true}}
	for text, want := range map[string]uint64{
		"うぉ？": 1, "うお!": 1, "どわー！？！？": 1, "uo?": 1,
		"うおうお！！": 2, "uoＵＯウオ!?？！": 3,
		"うぉうぉ": 2, "うぉ うぉ": 2, "うぉ　うぉ？！": 2,
		"うお うぉuo！！": 3, "うおうぉ？": 2,
		"うお": 1, "うおうお": 2,
		"": 0, "！？": 0, "うぉ？本当？": 0, "うぉ？です": 0,
		"「うぉ？」": 0, "`うぉ？`": 0, "> うぉ？": 0,
		" うぉ？": 0, "うぉ？ ": 0, "うぉ？\n": 0, "うぉ ？": 0,
		"？うぉ": 0, "うぉ。": 0, "うぉ…？": 0, "うぉ‼": 0,
		"未登録？": 0, "うお？うお": 0, "うぉ うぉ ？": 0,
		"https://example.com/uo?": 0,
	} {
		t.Run(text, func(t *testing.T) {
			if got := d.literalCount(text); got != want {
				t.Fatalf("count = %d, want %d", got, want)
			}
		})
	}
}

func TestPunctuatedReactionCountsWithoutReview(t *testing.T) {
	for text, units := range map[string]uint64{"うぉ？": 1, "どわー！？！？": 1, "うおうお！！": 2, "うぉうぉ": 2, "うぉ うぉ": 2, "うぉ　うぉ？！": 2} {
		t.Run(text, func(t *testing.T) {
			d := testDetector(t)
			d.words["うぉ"] = true
			s := testShadow(t)
			s.detector = d
			m := testMessage(text)
			a, _, _ := reviewSetup(t)
			a.detector = d
			if owned, err := a.enqueue(m); err != nil || owned {
				t.Fatal("deterministic reaction entered adaptive review", owned, err)
			}
			client := &fakeMessages{}
			if err := d.handleIncoming(client, s, m); err != nil {
				t.Fatal(err)
			}
			counts, err := loadCounts(d.statePath)
			if err != nil || counts[testUser] != units || len(client.messages) != 1 || client.messages[0].Content != detectionMessage(testUser, units, units) {
				t.Fatal("incorrect count or reply", counts, client.messages, err)
			}
			if len(s.jobs) != 0 || len(s.laughter) != 0 || len(s.store.records) != 0 {
				t.Fatal("deterministic reaction also entered shadow/AI review")
			}
		})
	}
}

func TestPunctuatedHeldReactionResolvesOnce(t *testing.T) {
	for text, units := range map[string]uint64{
		"うぉ？": 1, "うおうお！！": 2,
		"%E3%81%86%E3%81%89%E3%81%86%E3%81%89":  2,
		"%E3%81%86%E3%81%89 %E3%81%86%E3%81%89": 2,
	} {
		t.Run(text, func(t *testing.T) {
			a, f, out := reviewSetup(t)
			a.detector.words["うぉ"] = true
			f.source.Content = text
			r, _ := a.snapshot(reviewID)
			r.Text, r.Revision = text, contentRevision(f.source)
			if err := a.save(r); err != nil {
				t.Fatal(err)
			}
			*out = adaptiveResult{Revision: r.Revision, Release: strings.Repeat("a", 64), Label: "uncertain"}
			runReview(t, a, f, out.Release)
			oldNotice := f.replies[0].Content
			a = reloadReview(t, a)
			view := fixedView(text, []string{"うお", "うぉ"})
			if view.LiteralUnits == 0 && len(view.Candidates) == 1 {
				view = fixedView(view.Candidates[0], []string{"うお", "うぉ"})
			}
			if view.LiteralUnits != units {
				t.Fatal("helper did not resolve held reaction", view)
			}
			*out = adaptiveResult{Revision: r.Revision, Release: strings.Repeat("b", 64), Label: "mockery", Units: view.LiteralUnits}
			f.failSend = true
			if err := a.process(context.Background(), f, reviewID, out.Release); err == nil {
				t.Fatal("lost response not simulated")
			}
			state, err := loadDetectionState(a.detector.statePath)
			if err != nil {
				t.Fatal(err)
			}
			a.detector.counts, a.detector.reviewReceipts = state.Counts, state.ReviewReceipts
			a = reloadReview(t, a)
			f.failSend = false
			runReview(t, a, f, out.Release)
			runReview(t, a, f, out.Release)
			if owned, err := a.enqueue(&discordgo.MessageCreate{Message: f.source}); !owned || err != nil {
				t.Fatal("resolved review lost ownership", owned, err)
			}
			if err := a.detector.handleMessage(f, &discordgo.MessageCreate{Message: f.source}); err != nil {
				t.Fatal(err)
			}
			r, _ = a.snapshot(reviewID)
			if r.State != "done" || r.Units != units || a.detector.counts[reviewUser] != 7+units || f.sends != 2 || f.edits != 0 || f.replies[0].Content != oldNotice || f.replies[1].Content != resultMessage("mockery", reviewUser, 7+units) {
				t.Fatal("held reaction not resolved exactly once", r, f)
			}
		})
	}
}
