package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// The optional response file comes from the isolated Pi acceptance run using
// the production-approved decoder in Docker and the actual Laya classifier.
func TestUnifiedInitialDetectionAndRestart(t *testing.T) {
	a, f, out := reviewSetup(t)
	f.source.Content = "E4F25FE4F28318"
	r, _ := a.snapshot(reviewID)
	r.Text = f.source.Content
	r.Revision = contentRevision(f.source)
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	*out = adaptiveResult{Revision: r.Revision, Release: strings.Repeat("a", 64), Label: "mockery", Units: 1}
	if path := os.Getenv("UO_UNIFIED_RESPONSE"); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, out); err != nil {
			t.Fatal(err)
		}
		out.Revision = r.Revision
		if out.Unknown || out.Label != "mockery" || out.Units != 1 {
			t.Fatal("isolated Laya did not confirm mockery", out)
		}
	}
	f.failSend = true
	if err := a.process(context.Background(), f, reviewID, out.Release); err == nil {
		t.Fatal("lost send response was not simulated")
	}
	if a.detector.counts[reviewUser] != 8 || f.sends != 1 {
		t.Fatal("initial result not counted once")
	}
	state, err := loadDetectionState(a.detector.statePath)
	if err != nil {
		t.Fatal(err)
	}
	a.detector.counts = state.Counts
	a.detector.reviewReceipts = state.ReviewReceipts
	a = reloadReview(t, a)
	f.failSend = false
	if owned, err := a.enqueue(&discordgo.MessageCreate{Message: f.source}); !owned || err != nil {
		t.Fatal(owned, err)
	}
	runReview(t, a, f, out.Release)
	runReview(t, a, f, out.Release)
	if f.sends != 1 || f.edits != 0 || a.detector.counts[reviewUser] != 8 || f.replies[0].Content != detectionMessage(reviewUser, 1, 8) {
		t.Fatalf("duplicate or rejudge wording: %+v", f)
	}
	if strings.Contains(f.replies[0].Content, "再判定") {
		t.Fatal("first pass called rejudgment")
	}
	// Another post reuses the same restoration without duplicating the first.
	f.source.ID = "123456789012345679"
	out.Revision = contentRevision(f.source)
	if owned, err := a.enqueue(&discordgo.MessageCreate{Message: f.source}); !owned || err != nil {
		t.Fatal(owned, err)
	}
	if err := a.process(context.Background(), f, f.source.ID, out.Release); err != nil {
		t.Fatal(err)
	}
	if f.sends != 2 || a.detector.counts[reviewUser] != 9 || f.replies[1].Content != detectionMessage(reviewUser, 1, 9) {
		t.Fatal("decoder reuse failed")
	}
}

func TestUnifiedInitialHoldThenRejudgment(t *testing.T) {
	a, f, out := reviewSetup(t)
	out.Unknown = false
	out.Label = "uncertain"
	runReview(t, a, f, out.Release)
	if f.sends != 1 || a.detector.counts[reviewUser] != 7 {
		t.Fatal("hold counted or missing notice")
	}
	old := f.replies[0].Content
	a = reloadReview(t, a)
	out.Release = strings.Repeat("b", 64)
	out.Label = "mockery"
	out.Units = 1
	runReview(t, a, f, out.Release)
	a = reloadReview(t, a)
	runReview(t, a, f, out.Release)
	if f.sends != 2 || f.edits != 0 || f.replies[0].Content != old || f.replies[1].Content != resultMessage("mockery", reviewUser, 8) || a.detector.counts[reviewUser] != 8 {
		t.Fatal("held result delivery incorrect")
	}
}

func TestUnifiedIntakeKeepsOriginalRulesAndOwnsFixedTransform(t *testing.T) {
	a, f, _ := reviewSetup(t)
	for _, text := range []string{"うお", "うおうお"} {
		f.source.ID = "123456789012345679"
		f.source.Content = text
		if owned, err := a.enqueue(&discordgo.MessageCreate{Message: f.source}); owned || err != nil {
			t.Fatal("original literal path changed", text)
		}
	}
	f.source.Content = "e38186e3818a"
	if owned, err := a.enqueue(&discordgo.MessageCreate{Message: f.source}); !owned || err != nil {
		t.Fatal("fixed transform bypassed common flow")
	}
}

func TestUnifiedDuplicatePartialGatewayEditDoesNotReplyAgain(t *testing.T) {
	for _, held := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "held"}[held], func(t *testing.T) {
			a, f, out := reviewSetup(t)
			if !held {
				out.Unknown = false
				out.Label = "mockery"
				out.Units = 1
			}
			runReview(t, a, f, out.Release)
			before := a.detector.counts[reviewUser]
			// Partial update does not contain authoritative content/edited timestamp.
			if err := a.edit(&discordgo.MessageUpdate{Message: &discordgo.Message{ID: reviewID}}); err != nil {
				t.Fatal(err)
			}
			a = reloadReview(t, a)
			runReview(t, a, f, out.Release)
			if f.sends != 1 || a.detector.counts[reviewUser] != before {
				t.Fatal("duplicate Gateway update reapplied")
			}
			if held {
				out.Release = strings.Repeat("c", 64)
				out.Unknown = false
				out.Label = "mockery"
				out.Units = 1
				runReview(t, a, f, out.Release)
				if f.sends != 2 || f.replies[1].Content != resultMessage("mockery", reviewUser, 8) {
					t.Fatal("hold was not resumed")
				}
			}
		})
	}
}
