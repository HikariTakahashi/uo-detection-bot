package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	"github.com/bwmarrin/discordgo"
)

const (
	defaultDetectionConfigPath = "/etc/discord-bot/config.json"
	defaultDetectionStatePath  = "/var/lib/discord-bot/counts.json"
)

type detectionConfig struct {
	Words      []string `json:"words"`
	ChannelIDs []string `json:"channel_ids"`
}

func decodeJSON(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected a single JSON value")
	}
	return nil
}

func loadDetectionConfig(path string) (detectionConfig, error) {
	var config detectionConfig
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return config, fmt.Errorf("read detection config: %w", err)
	}
	if err := decodeJSON(data, &config); err != nil {
		return config, fmt.Errorf("decode detection config: %w", err)
	}
	if len(config.Words) == 0 || config.ChannelIDs == nil {
		return config, errors.New("detection config requires nonempty words and a channel_ids array")
	}
	for _, word := range config.Words {
		if word == "" {
			return config, errors.New("detection words must not be empty")
		}
	}
	for _, id := range config.ChannelIDs {
		if !isSnowflake(id) {
			return config, errors.New("detection channel_ids must contain numeric Discord channel IDs")
		}
	}
	return config, nil
}

func loadCounts(path string) (map[string]uint64, error) {
	counts := make(map[string]uint64)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return counts, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read detection counts: %w", err)
	}
	var stored map[string]*uint64
	if err := decodeJSON(data, &stored); err != nil {
		return nil, fmt.Errorf("decode detection counts: %w", err)
	}
	if stored == nil {
		return nil, errors.New("detection counts must be a JSON object")
	}
	for id, count := range stored {
		if !isSnowflake(id) || count == nil {
			return nil, errors.New("detection counts require user IDs and unsigned integer values")
		}
		counts[id] = *count
	}
	return counts, nil
}

// Write in the destination directory so rename atomically replaces the old file.
// Until rename succeeds, every error leaves the previous counts intact.
func saveCounts(path string, counts map[string]uint64) error {
	data, err := json.Marshal(counts)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".counts-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

type messageClient interface {
	ChannelMessageSendComplex(string, *discordgo.MessageSend, ...discordgo.RequestOption) (*discordgo.Message, error)
}

type detector struct {
	mu        sync.Mutex // Serializes increment, persistence, and notification.
	words     map[string]bool
	channels  map[string]bool
	counts    map[string]uint64
	statePath string
}

func newDetector(configPath, statePath string) (*detector, error) {
	config, err := loadDetectionConfig(configPath)
	if err != nil {
		return nil, err
	}
	counts, err := loadCounts(statePath)
	if err != nil {
		return nil, err
	}
	d := &detector{words: make(map[string]bool), channels: make(map[string]bool), counts: counts, statePath: statePath}
	for _, word := range config.Words {
		d.words[word] = true
	}
	for _, id := range config.ChannelIDs {
		d.channels[id] = true
	}
	return d, nil
}

func (d *detector) enabled() bool {
	return len(d.words) > 0 && len(d.channels) > 0
}

func (d *detector) handleMessage(s messageClient, event *discordgo.MessageCreate) error {
	if event == nil || event.Message == nil {
		return nil
	}
	m := event.Message
	if m.GuildID == "" || m.Author == nil || m.Author.Bot || m.WebhookID != "" || !isSnowflake(m.Author.ID) {
		return nil
	}
	if !d.channels[m.ChannelID] || !d.words[m.Content] {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	id := m.Author.ID
	previous, existed := d.counts[id]
	if previous == math.MaxUint64 {
		return errors.New("detection count overflow")
	}
	count := previous + 1
	d.counts[id] = count
	if err := saveCounts(d.statePath, d.counts); err != nil {
		if existed {
			d.counts[id] = previous
		} else {
			delete(d.counts, id)
		}
		return fmt.Errorf("save detection counts: %w", err)
	}
	_, err := s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Content:         fmt.Sprintf("冷笑を検知しました！\n<@%s>さんの%d回目の冷笑です！", id, count),
		AllowedMentions: &discordgo.MessageAllowedMentions{Users: []string{id}},
	})
	if err != nil {
		// The detection is still counted even when Discord rejects the notification.
		return fmt.Errorf("send detection notification: %s", safeError(err))
	}
	return nil
}

func envPath(name, fallback string) string {
	if path := os.Getenv(name); path != "" {
		return path
	}
	return fallback
}
