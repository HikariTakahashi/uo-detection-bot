package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestReadableJapaneseNoNoticeAndHeldResolution(t *testing.T) {
	raw, err := os.ReadFile("../../scripts/fixtures/readable-japanese.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err = json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	// Optional results from the isolated Pi run connect actual restoration and
	// Laya judgments to fake Discord delivery and temporary counter files.
	var responses map[string]adaptiveResult
	if path := os.Getenv("UO_READABLE_RESPONSES"); path != "" {
		raw, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(raw, &responses); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range cases {
		for _, held := range []bool{false, true} {
			name := tc.ID + map[bool]string{false: "/new", true: "/previously-held"}[held]
			t.Run(name, func(t *testing.T) {
				a, f, out := reviewSetup(t)
				f.source.Content = tc.Text
				r, _ := a.snapshot(reviewID)
				r.Text, r.Revision = tc.Text, contentRevision(f.source)
				if err := a.save(r); err != nil {
					t.Fatal(err)
				}
				out.Revision = r.Revision
				if held {
					runReview(t, a, f, out.Release)
					if f.sends != 1 || f.replies[0].Content != unknownMessage(reviewUser) {
						t.Fatal("missing original hold notice")
					}
					a = reloadReview(t, a)
				}
				result := adaptiveResult{Label: "other"}
				if responses != nil {
					var ok bool
					result, ok = responses[tc.ID]
					if !ok || result.Unknown || result.Label != "other" || result.Units != 0 {
						t.Fatal("isolated Pi did not dismiss ordinary Japanese", tc.ID, result)
					}
				}
				result.Revision, result.Release = r.Revision, strings.Repeat("b", 64)
				*out = result
				runReview(t, a, f, out.Release)
				a = reloadReview(t, a)
				runReview(t, a, f, out.Release)
				r, _ = a.snapshot(reviewID)
				if r.State != "done" || r.Label != "other" || r.Units != 0 || a.detector.counts[reviewUser] != 7 || f.edits != 0 {
					t.Fatal("ordinary Japanese did not finish without changing the count", r)
				}
				if !held && f.sends != 0 {
					t.Fatal("ordinary new post received a review notice")
				}
				if held && (f.sends != 2 || f.replies[0].Content != unknownMessage(reviewUser) || f.replies[1].Content != resultMessage("other", reviewUser, 7)) {
					t.Fatal("old hold was not resolved once with its history preserved")
				}
			})
		}
	}
}
