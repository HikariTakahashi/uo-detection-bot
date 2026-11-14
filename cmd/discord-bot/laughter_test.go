package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaughterCandidates(t *testing.T) {
	words := []string{"うお", "どわー", "uo", "うお"}
	for _, tc := range []struct{ text, want string }{
		{"うおw", "うお"}, {"ウオＷｗw！", "うお"}, {"どわーWW それは無理", "どわー"},
		{"先頭に文章、ｕｏｗｗ", "uo"}, {"『うおw』は例です。どわーw", "うお、どわー"},
		{"『うおw』と書きます", "うお"}, {"うおwそれは無理", "うお"},
		{"うお", ""}, {"うお w", ""}, {"うおのあとでw", ""}, {"うおworld", ""},
		{"duowww", ""}, {"uoworld、uo", ""}, {"https://example.com/うおw", ""},
		{"うおhttps://example.com w", ""}, {"https://example.com/uo。うおｗｗ", "うお"},
	} {
		if got := laughterWords(shadowNormalize(shadowClean(tc.text)), words); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.text, got, tc.want)
		}
	}
}

func TestLaughterCountsOnlyValidatedAIResultsAndDeduplicatesAfterRestart(t *testing.T) {
	dir := t.TempDir()
	detectionPath := filepath.Join(dir, "detection.json")
	statePath := filepath.Join(dir, "counts.json")
	shadowPath := filepath.Join(dir, "shadow.json")
	dataPath := filepath.Join(dir, "review")
	writeTestFile(t, detectionPath, `{"words":["うお","どわー","うおw"],"channel_ids":["`+testChannel+`"]}`)
	writeTestFile(t, statePath, `{"`+testUser+`":7}`)
	calls := make(chan string, 30)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var input map[string]string
		if err := json.NewDecoder(req.Body).Decode(&input); err != nil || req.URL.Path != "/classify/laughter" || input["target_words"] == "" {
			t.Error("live request did not use the dedicated classifier with configured words", req.URL.Path, input, err)
		}
		body := input["text"]
		calls <- body
		p := samplePrediction()
		p.Prompt = laughterPrompt
		switch body {
		case "『うおw』は表記の説明です":
			p.Label = "other"
		case "うおｗ判断困難":
			p.Label = "uncertain"
		case "うおｗ切り捨て":
			p.Truncated = true
		case "うおｗ旧版":
			p.Prompt = "baseline"
		case "うおｗ停止中":
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(p)
	}))
	defer server.Close()
	writeTestFile(t, shadowPath, fmt.Sprintf(`{"count_laughter":true,"endpoint":%q}`, server.URL+"/classify"))
	reload := func() (*detector, *shadowCollector) {
		t.Helper()
		d, err := newDetector(detectionPath, statePath)
		if err != nil {
			t.Fatal(err)
		}
		s, err := newShadow(shadowPath, dataPath, d)
		if err != nil {
			t.Fatal(err)
		}
		return d, s
	}
	d, s := reload()
	sends := &fakeMessages{}
	ctx := context.Background()
	if err := d.handleIncoming(sends, s, testMessage("うお")); err != nil {
		t.Fatal(err)
	}
	for i, body := range []string{"うおw", "ウオｗｗ！うおw", "『うおw』は表記の説明です", "うおｗ判断困難", "うおｗ切り捨て", "うおｗ旧版", "うおｗ停止中"} {
		m := testMessage(body)
		m.ID = fmt.Sprintf("%018d", i+1000)
		before := len(sends.messages)
		for range 2 {
			if err := d.handleIncoming(sends, s, m); err != nil {
				t.Fatal(err)
			}
		}
		if len(s.laughter) != 1 || len(s.jobs) != 0 || len(sends.messages) != before {
			t.Fatalf("%q: expected exactly one AI job and no fallback to exact match", body)
		}
		s.processLaughter(ctx, sends, <-s.laughter)
		attempts := 1
		if strings.Contains(body, "停止中") {
			attempts = 2
		}
		if len(calls) != attempts {
			t.Fatalf("%q: expected %d attempts, got %d", body, attempts, len(calls))
		}
		for range attempts {
			if <-calls != body {
				t.Fatal("wrong body sent to model")
			}
		}
		if i < 2 {
			if len(sends.messages) != before+1 || sends.messages[before].Reference.MessageID != m.ID || !strings.Contains(sends.messages[before].Content, fmt.Sprintf("%d回目", 9+i)) {
				t.Fatalf("%q: expected one reply with the new cumulative count", body)
			}
		} else if len(sends.messages) != before {
			t.Fatalf("%q: non-mockery result triggered a reply", body)
		}
		stored, hasReview := s.store.records[m.ID]
		if hasReview != (i == 3 || i == 4) || hasReview && stored.Prediction.Label != "uncertain" {
			t.Fatalf("%q: only uncertain results should be reviewed with shadow off", body)
		}
	}
	d, s = reload()
	for i, body := range []string{"うおw", "ウオｗｗ！うおw"} {
		m := testMessage(body)
		m.ID = fmt.Sprintf("%018d", i+1000)
		if err := d.handleIncoming(sends, s, m); err != nil || len(s.laughter) != 1 {
			t.Fatal("AI job fell back to exact match after restart", err)
		}
		s.processLaughter(ctx, sends, <-s.laughter)
	}
	if d.counts[testUser] != 10 || len(sends.messages) != 3 || len(calls) != 0 {
		t.Fatal("replayed messages were processed again", d.counts, len(sends.messages), len(calls))
	}
}

func TestLaughterPersistenceFailureDoesNotLeaveDedupMarker(t *testing.T) {
	d, sends, m := testDetector(t), &fakeMessages{}, testMessage("うおw")
	if err := os.MkdirAll(d.statePath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.countAndReply(sends, m.Author.ID, m.Reference(), true); err == nil || len(sends.messages) != 0 || d.alreadyAICounted(m.ID) || d.counts[testUser] != 0 {
		t.Fatal("failed save changed count, left a deduplication marker, or sent a reply", err)
	}
	if err := os.Remove(d.statePath); err != nil {
		t.Fatal(err)
	}
	if err := d.countAndReply(sends, m.Author.ID, m.Reference(), true); err != nil || len(sends.messages) != 1 || !d.alreadyAICounted(m.ID) || d.counts[testUser] != 1 {
		t.Fatal("retry after failed persistence did not count and reply", err)
	}
}

func TestLaughterOnlyRunsInDetectionChannelsWhenEnabled(t *testing.T) {
	s := testShadow(t)
	s.channels[testUser2] = true // An extra channel reserved for review sampling.
	for _, tc := range []struct {
		enabled bool
		channel string
		live    bool
	}{
		{false, testChannel, false}, {true, testUser2, false}, {true, testThread, true},
	} {
		s.config.CountLaughter = tc.enabled
		m := testMessage("うおw")
		m.ChannelID = tc.channel
		if live := s.enqueue(m); live != tc.live {
			t.Fatal("incorrect route", tc, live)
		}
		if tc.live {
			<-s.laughter
		} else {
			<-s.jobs
		}
		s.finish(m.ID)
	}
}
