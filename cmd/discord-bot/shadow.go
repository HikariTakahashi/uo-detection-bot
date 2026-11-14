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

type shadowConfig struct {
	Mode                     string   `json:"mode"`
	UseDetectionChannels     bool     `json:"use_detection_channels"`
	ChannelIDs               []string `json:"channel_ids,omitempty"`
	Endpoint                 string   `json:"endpoint"`
	SampleNonCandidatesEvery int      `json:"sample_non_candidates_every"`
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
	if err := decodeJSON(data, &c); err != nil {
		return c, err
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
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/classify" {
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
	config     shadowConfig
	channels   map[string]bool
	words      []string
	exactWords map[string]bool
	store      *shadowStore
	jobs       chan shadowRecord
	client     *http.Client
	mu         sync.Mutex
	pending    map[string]shadowRecord
	cancelled  map[string]bool
	deleted    map[string]time.Time
	dropped    atomic.Uint64
}

func newShadow(configPath, dataPath string, d *detector) (*shadowCollector, error) {
	store, err := openShadowStore(dataPath, time.Now())
	if err != nil {
		return nil, err
	}
	c, configErr := loadShadowConfig(configPath)
	if configErr != nil {
		c.Mode = "off"
	}
	s := &shadowCollector{config: c, channels: make(map[string]bool), store: store, jobs: make(chan shadowRecord, 10), pending: make(map[string]shadowRecord), cancelled: make(map[string]bool), client: &http.Client{Timeout: shadowDeadline, Transport: &http.Transport{Proxy: nil, MaxConnsPerHost: 1}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}}
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
	return s, configErr
}

// No network, disk writes, count mutations, or Discord sends on this path.
func (s *shadowCollector) enqueue(event *discordgo.MessageCreate) {
	if s == nil || s.config.Mode != "shadow" || event == nil || event.Message == nil {
		return
	}
	m := event.Message
	if !isSnowflake(m.ID) || !isSnowflake(m.GuildID) || !s.channels[m.ChannelID] || m.Author == nil || m.Author.Bot || m.WebhookID != "" || m.Content == "" {
		return
	}
	if utf8.RuneCountInString(m.Content) > 2000 {
		s.dropped.Add(1)
		return
	}
	r := shadowRecord{MessageID: m.ID, GuildID: m.GuildID, ChannelID: m.ChannelID, CapturedAt: time.Now(), Text: m.Content, CleanedText: shadowClean(m.Content), Selection: "non_candidate_sample", SampleEvery: 1, LegacyMatch: s.exactWords[m.Content], BotVersion: version}
	normalized := shadowNormalize(r.CleanedText)
	for _, word := range s.words {
		if strings.Contains(normalized, word) {
			r.Selection = "keyword_candidate"
			break
		}
	}
	if r.Selection == "non_candidate_sample" {
		r.SampleEvery = s.config.SampleNonCandidatesEvery
		h := sha256.Sum256([]byte(m.ID))
		if binary.BigEndian.Uint64(h[:8])%uint64(s.config.SampleNonCandidatesEvery) != 0 {
			return
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
	// Persistence/deletion may be slow. Never accumulate blocked Gateway handlers.
	if !s.mu.TryLock() {
		s.dropped.Add(1)
		return
	}
	defer s.mu.Unlock()
	for id, until := range s.deleted {
		if !time.Now().Before(until) {
			delete(s.deleted, id)
		}
	}
	if _, ok := s.deleted[r.MessageID]; ok {
		return
	}
	if _, ok := s.deleted[r.ContextMessageID]; ok {
		return
	}
	if _, ok := s.pending[r.MessageID]; ok {
		return
	}
	select {
	case s.jobs <- r:
		s.pending[r.MessageID] = r
	default:
		s.dropped.Add(1)
	}
}

func (s *shadowCollector) predict(ctx context.Context, r shadowRecord) (shadowPrediction, error) {
	if r.CleanedText == "" {
		return shadowPrediction{Label: "other", Model: "url_filter"}, nil
	}
	data, _ := json.Marshal(map[string]string{"text": r.Text, "context": r.Context})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.config.Endpoint, bytes.NewReader(data))
	if err != nil {
		return shadowPrediction{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return shadowPrediction{}, errors.New("classifier unavailable or timed out")
	}
	defer resp.Body.Close()
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

func (s *shadowCollector) process(ctx context.Context, r shadowRecord) {
	defer func() { s.mu.Lock(); delete(s.pending, r.MessageID); delete(s.cancelled, r.MessageID); s.mu.Unlock() }()
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

func (s *shadowCollector) run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-s.jobs:
			s.process(ctx, r)
		case <-ticker.C:
			if err := s.store.prune(time.Now()); err != nil {
				slog.Error("shadow retention cleanup failed", "error", safeError(err))
			}
			if n := s.dropped.Swap(0); n > 0 {
				slog.Warn("shadow samples skipped", "count", n)
			}
		}
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
	return s.store.remove(id, guild)
}
