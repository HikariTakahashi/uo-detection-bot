package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

const defaultShadowConfigPath = "/etc/discord-bot/shadow.json"
const defaultShadowDataPath = "/var/lib/discord-bot/shadow"
const shadowDeadline = 10 * time.Second
const laughterPrompt = "laughter"

type shadowConfig struct {
	Mode                     string   `json:"mode"`
	UseDetectionChannels     bool     `json:"use_detection_channels"`
	ChannelIDs               []string `json:"channel_ids,omitempty"`
	Endpoint                 string   `json:"endpoint"`
	SampleNonCandidatesEvery int      `json:"sample_non_candidates_every"`
	CountLaughter            bool     `json:"count_laughter"`
}

func loadShadowConfig(path string) (shadowConfig, error) {
	c := shadowConfig{Mode: "off", Endpoint: "http://127.0.0.1:8766/classify", SampleNonCandidatesEvery: 10}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	// Keep absence distinct from an explicit false for existing installs.
	var file struct {
		*shadowConfig
		CountLaughter json.RawMessage `json:"count_laughter"`
	}
	file.shadowConfig = &c
	if err := decodeJSON(data, &file); err != nil {
		return c, err
	}
	// Existing installations that already run AI in the detection channels
	// can receive the live feature in a binary-only update. Explicit settings
	// still take precedence, and review-only channels are not opted in.
	if len(file.CountLaughter) == 0 {
		c.CountLaughter = c.Mode == "shadow" && c.UseDetectionChannels
	} else if bytes.Equal(file.CountLaughter, []byte("null")) || json.Unmarshal(file.CountLaughter, &c.CountLaughter) != nil {
		return c, errors.New("count_laughter must be a boolean")
	}
	if c.Mode != "off" && c.Mode != "shadow" {
		return c, errors.New("shadow mode must be off or shadow")
	}
	if c.SampleNonCandidatesEvery < 1 || c.SampleNonCandidatesEvery > 1000 {
		return c, errors.New("invalid shadow sampling interval")
	}
	if c.UseDetectionChannels && len(c.ChannelIDs) > 0 {
		return c, errors.New("choose inherited or explicit shadow channels")
	}
	if c.Mode == "shadow" && !c.UseDetectionChannels && len(c.ChannelIDs) == 0 {
		return c, errors.New("shadow channels required")
	}
	for _, id := range c.ChannelIDs {
		if !isSnowflake(id) {
			return c, errors.New("invalid shadow channel ID")
		}
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.ForceQuery || u.RawQuery != "" || u.Fragment != "" || u.EscapedPath() != "/classify" {
		return c, errors.New("shadow endpoint must be local HTTP /classify")
	}
	return c, nil
}

var shadowURL = regexp.MustCompile(`(?i)(https?://|www\.)[^\s<>"『』「」【】、。！？]+`)

func shadowClean(text string) string { return strings.TrimSpace(shadowURL.ReplaceAllString(text, " ")) }
func shadowNormalize(text string) string {
	return strings.ToLower(strings.Map(func(r rune) rune {
		if r >= 0xff01 && r <= 0xff5e {
			r -= 0xfee0
		}
		if r >= 0x30a1 && r <= 0x30f6 {
			r -= 0x60
		}
		return r
	}, text))
}
func validShadowLabel(s string) bool { return s == "mockery" || s == "other" || s == "uncertain" }

type shadowCollector struct {
	config          shadowConfig
	channels        map[string]bool
	words           []string
	exactWords      map[string]bool
	store           *shadowStore
	jobs            chan shadowRecord
	laughter        chan laughterJob
	detector        *detector
	client          *http.Client
	mu              sync.Mutex
	pending         map[string]shadowRecord
	cancelled       map[string]bool
	deleted         map[string]time.Time
	dropped         atomic.Uint64
	laughterDropped atomic.Uint64
}

func newShadow(configPath, dataPath string, d *detector) (*shadowCollector, error) {
	c, configErr := loadShadowConfig(configPath)
	if configErr != nil {
		c.Mode = "off"
		c.CountLaughter = false
	}
	store, storeErr := openShadowStore(dataPath, time.Now())
	if storeErr != nil && !c.CountLaughter {
		return nil, errors.Join(configErr, storeErr)
	}
	s := &shadowCollector{
		config: c, channels: make(map[string]bool), store: store, detector: d,
		jobs: make(chan shadowRecord, 10), laughter: make(chan laughterJob, 20),
		pending: make(map[string]shadowRecord), cancelled: make(map[string]bool),
		client: &http.Client{
			Timeout:       shadowDeadline,
			Transport:     &http.Transport{MaxConnsPerHost: 1},
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") },
		},
	}
	if storeErr != nil {
		s.config.Mode = "off"
	}
	s.exactWords = d.words // Configuration is immutable for the lifetime of the bot.
	if c.UseDetectionChannels {
		for id := range d.channels {
			s.channels[id] = true
		}
	} else {
		for _, id := range c.ChannelIDs {
			s.channels[id] = true
		}
	}
	for word := range d.words {
		s.words = append(s.words, shadowNormalize(word))
	}
	if c.Mode == "shadow" && len(s.words) == 0 {
		s.config.Mode = "off"
		configErr = errors.New("shadow requires detection words")
	}
	return s, errors.Join(configErr, storeErr)
}

// No network, disk writes, count mutations, or Discord sends on this path.
// Returns true when the message belongs exclusively to the live AI path.
func (s *shadowCollector) enqueue(event *discordgo.MessageCreate) bool {
	if s == nil || (!s.config.CountLaughter && s.config.Mode != "shadow") || event == nil || event.Message == nil {
		return false
	}
	m := event.Message
	if !isSnowflake(m.ID) || !isSnowflake(m.GuildID) || m.Author == nil || m.Author.Bot || m.WebhookID != "" || m.Content == "" {
		return false
	}
	// The normal detector owns configured uo/うお repetitions. Do not create a
	// second review record for a post that will receive its normal reply.
	if s.detector.repetitionCount(m.Content) > 0 {
		return false
	}
	canCount := s.config.CountLaughter && s.detector.eligibleMessage(event)
	canSample := s.config.Mode == "shadow" && s.channels[m.ChannelID]
	if !canCount && !canSample {
		return false
	}
	r := shadowRecord{MessageID: m.ID, GuildID: m.GuildID, ChannelID: m.ChannelID, CapturedAt: time.Now(), Text: m.Content, CleanedText: shadowClean(m.Content), Selection: "non_candidate_sample", SampleEvery: 1, LegacyMatch: s.exactWords[m.Content], BotVersion: version}
	normalized := shadowNormalize(r.CleanedText)
	var matchedWords string
	if canCount {
		matchedWords = laughterWords(normalized, s.words)
	}
	live := matchedWords != ""
	if !live && !canSample {
		return false
	}
	if utf8.RuneCountInString(m.Content) > 2000 {
		s.recordDrop(live)
		return live
	}
	if live {
		r.Selection = "laughter_candidate"
	} else {
		for _, word := range s.words {
			if strings.Contains(normalized, word) {
				r.Selection = "keyword_candidate"
				break
			}
		}
	}
	if r.Selection == "non_candidate_sample" {
		r.SampleEvery = s.config.SampleNonCandidatesEvery
		h := sha256.Sum256([]byte(m.ID))
		if binary.BigEndian.Uint64(h[:8])%uint64(s.config.SampleNonCandidatesEvery) != 0 {
			return false
		}
		if r.CleanedText == "" {
			r.Selection = "url_only_sample"
		}
	}
	// Only use an already supplied reply in the same channel. Never fetch history.
	ref := m.MessageReference
	if ref != nil && ref.ChannelID == m.ChannelID && (ref.GuildID == "" || ref.GuildID == m.GuildID) && m.ReferencedMessage != nil && m.ReferencedMessage.ID == ref.MessageID && utf8.RuneCountInString(m.Content)+utf8.RuneCountInString(m.ReferencedMessage.Content) <= 2000 {
		r.Context = m.ReferencedMessage.Content
		r.ContextMessageID = ref.MessageID
	}
	// Only best-effort evaluation samples are dropped on lock contention.
	if live {
		s.mu.Lock()
	} else if !s.mu.TryLock() {
		s.dropped.Add(1)
		return false
	}
	defer s.mu.Unlock()
	for id, until := range s.deleted {
		if !time.Now().Before(until) {
			delete(s.deleted, id)
		}
	}
	if _, ok := s.deleted[r.MessageID]; ok {
		return live
	}
	if _, ok := s.deleted[r.ContextMessageID]; ok {
		return live
	}
	if _, ok := s.pending[r.MessageID]; ok {
		return live
	}
	if live {
		select {
		case s.laughter <- laughterJob{record: r, authorID: m.Author.ID, words: matchedWords}:
			s.pending[r.MessageID] = r
		default:
			s.recordDrop(true)
		}
	} else {
		select {
		case s.jobs <- r:
			s.pending[r.MessageID] = r
		default:
			s.dropped.Add(1)
		}
	}
	return live
}

func (s *shadowCollector) predict(ctx context.Context, r shadowRecord) (shadowPrediction, error) {
	if r.CleanedText == "" {
		return shadowPrediction{Label: "other", Model: "url_filter"}, nil
	}
	return s.requestPrediction(ctx, s.config.Endpoint, map[string]string{"text": r.Text, "context": r.Context})
}

func (s *shadowCollector) requestPrediction(ctx context.Context, endpoint string, input map[string]string) (shadowPrediction, error) {
	data, _ := json.Marshal(input)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		return shadowPrediction{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return shadowPrediction{}, errClassifierUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusServiceUnavailable {
		return shadowPrediction{}, errClassifierUnavailable
	}
	if resp.StatusCode != http.StatusOK {
		return shadowPrediction{}, errors.New("classifier returned non-success")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(body) > 65536 {
		return shadowPrediction{}, errors.New("invalid classifier response size")
	}
	var p shadowPrediction
	if err := decodeJSON(body, &p); err != nil {
		return p, errors.New("invalid classifier response")
	}
	if !validShadowLabel(p.Label) || p.Model == "" || len(p.Model) > 128 || p.Revision == "" || len(p.Revision) > 128 || p.Prompt == "" || len(p.Prompt) > 64 || len(p.PromptSHA256) != 64 || p.Seconds < 0 || math.IsNaN(p.Seconds) || math.IsInf(p.Seconds, 0) {
		return p, errors.New("invalid classifier metadata")
	}
	if len(p.Probabilities) != 3 {
		return p, errors.New("missing classifier probabilities")
	}
	sum := 0.0
	for _, key := range []string{"A", "B", "C"} {
		v, ok := p.Probabilities[key]
		if !ok || v < 0 || v > 1 || math.IsNaN(v) {
			return p, errors.New("invalid classifier probabilities")
		}
		sum += v
	}
	if math.Abs(sum-1) > 0.02 {
		return p, errors.New("invalid classifier probability sum")
	}
	if p.Truncated {
		p.Label = "uncertain"
	}
	return p, nil
}

var errClassifierUnavailable = errors.New("classifier unavailable or timed out")

func (s *shadowCollector) finish(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
	delete(s.cancelled, id)
}

func (s *shadowCollector) process(ctx context.Context, r shadowRecord) {
	defer s.finish(r.MessageID)
	s.mu.Lock()
	cancelled := s.cancelled[r.MessageID]
	s.mu.Unlock()
	if cancelled || s.store.has(r.MessageID) {
		return
	}
	requestCtx, cancel := context.WithDeadline(ctx, r.CapturedAt.Add(shadowDeadline))
	defer cancel()
	p, err := s.predict(requestCtx, r)
	if err != nil {
		r.Error = "classifier_failed_or_timeout"
	} else {
		r.Prediction = p
	}
	if ctx.Err() != nil {
		return
	}
	// Serialize the final add with deletion, so a deleted queued message cannot reappear.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancelled[r.MessageID] {
		return
	}
	if err := s.store.add(r, time.Now()); err != nil {
		slog.Error("shadow save failed", "error", safeError(err))
	}
}

func (s *shadowCollector) run(ctx context.Context, sender messageClient) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		// Prioritize live decisions when both queues have work.
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.maintain()
			continue
		case job := <-s.laughter:
			s.processLaughter(ctx, sender, job)
			continue
		default:
		}
		select {
		case <-ctx.Done():
			return
		case r := <-s.jobs:
			s.process(ctx, r)
		case job := <-s.laughter:
			s.processLaughter(ctx, sender, job)
		case <-ticker.C:
			s.maintain()
		}
	}
}

func (s *shadowCollector) maintain() {
	if s.store != nil {
		if err := s.store.prune(time.Now()); err != nil {
			slog.Error("shadow retention cleanup failed", "error", safeError(err))
		}
	}
	if n := s.dropped.Swap(0); n > 0 {
		slog.Warn("shadow samples skipped", "count", n)
	}
	if n := s.laughterDropped.Swap(0); n > 0 {
		slog.Warn("laughter candidates skipped", "count", n)
	}
}

func (s *shadowCollector) remove(id, guild string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !isSnowflake(id) || !isSnowflake(guild) {
		return nil
	}
	if s.deleted == nil {
		s.deleted = make(map[string]time.Time)
	}
	for key, until := range s.deleted {
		if !time.Now().Before(until) {
			delete(s.deleted, key)
		}
	}
	// Cover asynchronous Gateway handler ordering without retaining message bodies.
	if len(s.deleted) < 10000 {
		s.deleted[id] = time.Now().Add(time.Minute)
	}
	for key, r := range s.pending {
		if r.GuildID == guild && (r.MessageID == id || r.ContextMessageID == id) {
			s.cancelled[key] = true
		}
	}
	if s.store != nil {
		return s.store.remove(id, guild)
	}
	return nil
}
