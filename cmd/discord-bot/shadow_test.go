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
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func testShadow(t *testing.T) *shadowCollector {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "shadow.json")
	writeTestFile(t, config, `{"mode":"shadow","use_detection_channels":true,"sample_non_candidates_every":1}`)
	s, err := newShadow(config, filepath.Join(dir, "records"), testDetector(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func samplePrediction() shadowPrediction {
	return shadowPrediction{Label: "mockery", Model: "laya", Revision: "revision", Prompt: "baseline", PromptSHA256: strings.Repeat("a", 64), Probabilities: map[string]float64{"A": 0.7, "B": 0.2, "C": 0.1}}
}
func sampleRecord() shadowRecord {
	m := testMessage("うお")
	return shadowRecord{MessageID: m.ID, GuildID: m.GuildID, ChannelID: m.ChannelID, Text: m.Content, CleanedText: m.Content, CapturedAt: time.Now(), Selection: "keyword_candidate", Prediction: samplePrediction()}
}
func TestShadowConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shadow.json")
	c, err := loadShadowConfig(path)
	if err != nil || c.Mode != "off" {
		t.Fatal(c, err)
	}
	for _, input := range []string{`{"mode":"active"}`, `{"mode":"shadow"}`, `{"endpoint":"http://example.com/classify"}`, `{"endpoint":"http://127.0.0.1/classify?secret=x"}`, `{"sample_non_candidates_every":0}`, `{"use_detection_channels":true,"channel_ids":["333333333333333333"]}`} {
		writeTestFile(t, path, input)
		if _, err = loadShadowConfig(path); err == nil {
			t.Fatal("accepted", input)
		}
	}
	s := testShadow(t)
	if !s.channels[testThread] || !s.channels[testChannel] {
		t.Fatal("channels not inherited")
	}
	writeTestFile(t, path, `{"mode":"active"}`)
	disabled, err := newShadow(path, filepath.Join(t.TempDir(), "data"), testDetector(t))
	if err == nil || disabled == nil || disabled.config.Mode != "off" {
		t.Fatal("invalid config must fail open for legacy bot")
	}
}
func TestShadowSelectionAndIsolation(t *testing.T) {
	for _, tc := range []struct{ text, selection, clean string }{{"ちゅううおう", "keyword_candidate", "ちゅううおう"}, {"https://example.com/uo", "url_only_sample", ""}, {"ＵＯ ウオ！", "keyword_candidate", "ＵＯ ウオ！"}, {"こんにちは", "non_candidate_sample", "こんにちは"}} {
		s := testShadow(t)
		s.enqueue(testMessage(tc.text))
		select {
		case r := <-s.jobs:
			if r.Selection != tc.selection || r.CleanedText != tc.clean {
				t.Fatal(r)
			}
		default:
			t.Fatal("missing sample")
		}
	}
	s := testShadow(t)
	for _, change := range []func(*discordgo.MessageCreate){func(m *discordgo.MessageCreate) { m.Author.Bot = true }, func(m *discordgo.MessageCreate) { m.WebhookID = testUser }, func(m *discordgo.MessageCreate) { m.GuildID = "" }, func(m *discordgo.MessageCreate) { m.ChannelID = testUser }, func(m *discordgo.MessageCreate) { m.Content = strings.Repeat("あ", 2001) }} {
		m := testMessage("うお")
		change(m)
		s.enqueue(m)
	}
	if len(s.jobs) != 0 {
		t.Fatal("excluded message collected")
	}
	for n := 0; n < 20; n++ {
		m := testMessage("うお")
		m.ID = fmt.Sprintf("%018d", 1000+n)
		s.enqueue(m)
	}
	if len(s.jobs) != 10 || len(s.pending) != 10 {
		t.Fatal("queue is not bounded")
	}
	// The exact same legacy inputs still produce exactly the same sends/counts,
	// even if shadow inference fails or collects embedded words and URLs.
	d := testDetector(t)
	sends := &fakeMessages{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	s = testShadow(t)
	s.config.Endpoint = server.URL + "/classify"
	for n, body := range []string{"うお", "ちゅううおう", "https://example.com/uo", "どわー"} {
		m := testMessage(body)
		m.ID = fmt.Sprintf("%018d", 2000+n)
		if err := d.handleMessage(sends, m); err != nil {
			t.Fatal(err)
		}
		s.enqueue(m)
		s.process(context.Background(), <-s.jobs)
	}
	if d.counts[testUser] != 2 || len(sends.messages) != 2 {
		t.Fatal("legacy behavior changed")
	}
	if len(s.store.records) != 4 {
		t.Fatal("missing shadow errors/results")
	}
}
func TestShadowPredictionValidationAndDeadline(t *testing.T) {
	for _, kind := range []string{"valid", "truncated", "label", "probabilities", "metadata", "huge", "extra"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "huge" {
					fmt.Fprint(w, strings.Repeat("x", 65537))
					return
				}
				if kind == "extra" {
					fmt.Fprint(w, `{"unknown":true}`)
					return
				}
				p := samplePrediction()
				switch kind {
				case "truncated":
					p.Truncated = true
				case "label":
					p.Label = "yes"
				case "probabilities":
					p.Probabilities["A"] = 3
				case "metadata":
					p.PromptSHA256 = ""
				}
				json.NewEncoder(w).Encode(p)
			}))
			defer server.Close()
			s := testShadow(t)
			s.config.Endpoint = server.URL + "/classify"
			p, err := s.predict(context.Background(), sampleRecord())
			valid := kind == "valid" || kind == "truncated"
			if (err == nil) != valid {
				t.Fatal(kind, p, err)
			}
			if kind == "truncated" && p.Label != "uncertain" {
				t.Fatal(p)
			}
		})
	}
	s := testShadow(t)
	r := sampleRecord()
	r.CapturedAt = time.Now().Add(-time.Minute)
	s.process(context.Background(), r)
	if s.store.records[r.MessageID].Error == "" {
		t.Fatal("expired queue item had no error")
	}
}
func TestShadowDeleteDuringInference(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		json.NewEncoder(w).Encode(samplePrediction())
	}))
	defer server.Close()
	s := testShadow(t)
	s.config.Endpoint = server.URL + "/classify"
	s.enqueue(testMessage("うお"))
	r := <-s.jobs
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.process(context.Background(), r) }()
	<-started
	if err := s.remove(r.MessageID, r.GuildID); err != nil {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	if s.store.has(r.MessageID) {
		t.Fatal("deleted record reappeared")
	}
	s.enqueue(testMessage("うお"))
	if len(s.jobs) != 0 {
		t.Fatal("late create resurrected deleted record")
	}
}
func TestShadowStoreRetentionAndAtomicReview(t *testing.T) {
	s := testShadow(t)
	r := sampleRecord()
	now := time.Now()
	if err := s.store.add(r, now); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{s.store.dir: 0700, filepath.Join(s.store.dir, r.MessageID+".json"): 0600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != want {
			t.Fatal(path, info, err)
		}
	}
	if _, err := s.store.label(r.MessageID, testUser, testUser, "other", "", now); err == nil {
		t.Fatal("cross guild label accepted")
	}
	if _, err := s.store.label(r.MessageID, r.GuildID, testUser, "other", "理由", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.label(r.MessageID, r.GuildID, testUser, "mockery", "", now); err == nil {
		t.Fatal("duplicate label accepted")
	}
	reloaded, err := openShadowStore(s.store.dir, now)
	if err != nil || reloaded.records[r.MessageID].HumanLabel != "other" {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(s.store.dir, ".shadow-crashed"), "private body")
	reloaded, err = openShadowStore(s.store.dir, now.Add(shadowRetention+time.Second))
	if err != nil || len(reloaded.records) != 0 {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(s.store.dir)
	if len(entries) != 0 {
		t.Fatal("expired/temp data left on disk")
	}
	r = sampleRecord()
	r.MessageID = "888888888888888888"
	if err := s.store.add(r, now); err != nil {
		t.Fatal(err)
	}
	oldDir := s.store.dir
	s.store.dir = filepath.Join(oldDir, "missing")
	if _, err := s.store.label(r.MessageID, r.GuildID, testUser, "other", "", now); err == nil {
		t.Fatal("save failure ignored")
	}
	if s.store.records[r.MessageID].HumanLabel != "" {
		t.Fatal("failed write mutated memory")
	}
}
func TestShadowReplyDeletionAndContextBound(t *testing.T) {
	s := testShadow(t)
	m := testMessage("うお")
	m.MessageReference = &discordgo.MessageReference{MessageID: testUser2, ChannelID: testChannel, GuildID: m.GuildID}
	m.ReferencedMessage = &discordgo.Message{ID: testUser2, Content: "返信元"}
	s.enqueue(m)
	r := <-s.jobs
	if r.Context != "返信元" {
		t.Fatal(r)
	}
	if err := s.store.add(r, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.remove(testUser2, r.GuildID); err != nil {
		t.Fatal(err)
	}
	if s.store.has(r.MessageID) || !s.cancelled[r.MessageID] {
		t.Fatal("reply data not removed")
	}
	s = testShadow(t)
	m.MessageReference.ChannelID = testThread
	s.enqueue(m)
	r = <-s.jobs
	if r.Context != "" {
		t.Fatal("cross-channel reply retained")
	}
}

func TestShadowCapacityDedupAndSampling(t *testing.T) {
	s := testShadow(t)
	r := sampleRecord()
	if err := s.store.add(r, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.Text = "duplicate must not overwrite"
	if err := s.store.add(r, time.Now()); err != nil {
		t.Fatal(err)
	}
	if s.store.records[r.MessageID].Text == r.Text {
		t.Fatal("duplicate overwritten")
	}
	for n := 0; n < shadowMaxRecords; n++ {
		id := fmt.Sprintf("%018d", 10000+n)
		s.store.records[id] = shadowRecord{MessageID: id}
	}
	r.MessageID = "777777777777777777"
	if err := s.store.add(r, time.Now()); err == nil || s.store.has(r.MessageID) {
		t.Fatal("capacity exceeded")
	}
	a, b := testShadow(t), testShadow(t)
	a.config.SampleNonCandidatesEvery, b.config.SampleNonCandidatesEvery = 10, 10
	selected := 0
	for n := 0; n < 100; n++ {
		m := testMessage("普通の文章")
		m.ID = fmt.Sprintf("%018d", 30000+n)
		a.enqueue(m)
		b.enqueue(m)
		if len(a.jobs) != len(b.jobs) {
			t.Fatal("sampling not deterministic")
		}
		if len(a.jobs) > 0 {
			<-a.jobs
			<-b.jobs
			selected++
		}
	}
	if selected == 0 || selected == 100 {
		t.Fatal("invalid sampling", selected)
	}
}

func TestShadowBusyStoreDoesNotBlockGateway(t *testing.T) {
	s := testShadow(t)
	s.mu.Lock()
	done := make(chan struct{})
	go func() { s.enqueue(testMessage("うお")); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		s.mu.Unlock()
		t.Fatal("enqueue waited for persistence lock")
	}
	s.mu.Unlock()
	if len(s.jobs) != 0 || s.dropped.Load() != 1 {
		t.Fatal("busy sample was not dropped")
	}
}
