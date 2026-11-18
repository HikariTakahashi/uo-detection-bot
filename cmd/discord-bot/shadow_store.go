package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const shadowRetention = 7 * 24 * time.Hour
const shadowMaxRecords = 1000

var errReviewUnavailable = errors.New("review item expired, removed, or already reviewed")

type shadowPrediction struct {
	Label         string             `json:"label"`
	Model         string             `json:"model"`
	Revision      string             `json:"revision"`
	Prompt        string             `json:"prompt"`
	PromptSHA256  string             `json:"prompt_sha256"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Seconds       float64            `json:"seconds"`
	Truncated     bool               `json:"truncated"`
}

type shadowRecord struct {
	MessageID        string           `json:"message_id"`
	GuildID          string           `json:"guild_id"`
	ChannelID        string           `json:"channel_id"`
	CapturedAt       time.Time        `json:"captured_at"`
	Text             string           `json:"text"`
	CleanedText      string           `json:"cleaned_text"`
	DecodedText      string           `json:"decoded_text,omitempty"`
	Context          string           `json:"context,omitempty"`
	ContextMessageID string           `json:"context_message_id,omitempty"`
	Selection        string           `json:"selection"`
	SampleEvery      int              `json:"sample_every"`
	LegacyMatch      bool             `json:"legacy_match"`
	BotVersion       string           `json:"bot_version"`
	Prediction       shadowPrediction `json:"prediction"`
	Error            string           `json:"error,omitempty"`
	HumanLabel       string           `json:"human_label,omitempty"`
	Reason           string           `json:"reason,omitempty"`
	ReviewerID       string           `json:"reviewer_id,omitempty"`
	ReviewedAt       time.Time        `json:"reviewed_at,omitempty"`
}

type shadowStore struct {
	mu      sync.Mutex
	dir     string
	records map[string]shadowRecord
}

func atomicShadowJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".shadow-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	if directory, err := os.Open(filepath.Dir(path)); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func openShadowStore(dir string, now time.Time) (*shadowStore, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	s := &shadowStore{dir: dir, records: make(map[string]shadowRecord)}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".shadow-") && entry.Type().IsRegular() {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return nil, err
			}
			continue
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > 65536 {
			return nil, errors.New("invalid shadow record file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var r shadowRecord
		if err := decodeJSON(data, &r); err != nil {
			return nil, err
		}
		if !isSnowflake(r.MessageID) || !isSnowflake(r.GuildID) || !isSnowflake(r.ChannelID) || entry.Name() != r.MessageID+".json" || r.CapturedAt.IsZero() || r.CapturedAt.After(now.Add(time.Minute)) {
			return nil, errors.New("invalid shadow record")
		}
		if !now.Before(r.CapturedAt.Add(shadowRetention)) {
			if err := os.Remove(path); err != nil {
				return nil, err
			}
			continue
		}
		if err := os.Chmod(path, 0600); err != nil {
			return nil, err
		}
		s.records[r.MessageID] = r
	}
	if len(s.records) > shadowMaxRecords {
		return nil, errors.New("too many shadow records")
	}
	return s, nil
}

func (s *shadowStore) add(r shadowRecord, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.records[r.MessageID]; exists {
		return nil
	}
	if len(s.records) >= shadowMaxRecords {
		return errors.New("shadow storage full")
	}
	if !now.Before(r.CapturedAt.Add(shadowRetention)) {
		return nil
	}
	if err := atomicShadowJSON(filepath.Join(s.dir, r.MessageID+".json"), r); err != nil {
		return err
	}
	s.records[r.MessageID] = r
	return nil
}

func (s *shadowStore) has(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.records[id]
	return ok
}

func (s *shadowStore) next(guild string, excluded map[string]bool, now time.Time) (shadowRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items := make([]shadowRecord, 0)
	for _, r := range s.records {
		if r.GuildID == guild && r.HumanLabel == "" && r.Error == "" && !excluded[r.MessageID] && now.Before(r.CapturedAt.Add(shadowRetention)) {
			items = append(items, r)
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CapturedAt.Equal(items[j].CapturedAt) {
			return items[i].MessageID < items[j].MessageID
		}
		return items[i].CapturedAt.Before(items[j].CapturedAt)
	})
	if len(items) == 0 {
		return shadowRecord{}, false
	}
	return items[0], true
}

func (s *shadowStore) label(id, guild, reviewer, label, reason string, now time.Time) (shadowRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if !ok || r.GuildID != guild || r.HumanLabel != "" || r.Error != "" || !now.Before(r.CapturedAt.Add(shadowRetention)) {
		return shadowRecord{}, errReviewUnavailable
	}
	if !validShadowLabel(label) {
		return shadowRecord{}, errors.New("invalid label")
	}
	r.HumanLabel, r.Reason, r.ReviewerID, r.ReviewedAt = label, reason, reviewer, now
	if err := atomicShadowJSON(filepath.Join(s.dir, id+".json"), r); err != nil {
		return shadowRecord{}, err
	}
	s.records[id] = r
	return r, nil
}

func (s *shadowStore) remove(id, guild string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, r := range s.records {
		if r.GuildID == guild && (r.MessageID == id || r.ContextMessageID == id) {
			if err := os.Remove(filepath.Join(s.dir, key+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			delete(s.records, key)
		}
	}
	return nil
}

func (s *shadowStore) prune(now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, r := range s.records {
		if !now.Before(r.CapturedAt.Add(shadowRetention)) {
			if err := os.Remove(filepath.Join(s.dir, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			delete(s.records, id)
		}
	}
	return nil
}
