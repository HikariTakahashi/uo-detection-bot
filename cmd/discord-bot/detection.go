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
	"slices"
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

type reviewReceipt struct {
	UserID   string `json:"user_id"`
	Revision string `json:"revision"`
	Units    uint64 `json:"units"`
	Total    uint64 `json:"total"`
}

type detectionState struct {
	ReviewReceipts map[string]reviewReceipt
	Counts         map[string]uint64
	AICounted      map[string]bool // Discord message IDs, never message contents.
}

func loadDetectionState(path string) (detectionState, error) {
	state := detectionState{Counts: make(map[string]uint64), AICounted: make(map[string]bool), ReviewReceipts: make(map[string]reviewReceipt)}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, fmt.Errorf("read detection counts: %w", err)
	}
	var stored map[string]*uint64
	var fields map[string]json.RawMessage
	if err := decodeJSON(data, &fields); err != nil {
		return state, fmt.Errorf("decode detection counts: %w", err)
	}
	if _, versioned := fields["version"]; versioned {
		var file struct {
			ReviewReceipts map[string]reviewReceipt `json:"review_receipts,omitempty"`
			Version        int                      `json:"version"`
			Counts         map[string]*uint64       `json:"counts"`
			AICounted      []string                 `json:"ai_counted_message_ids"`
		}
		if err := decodeJSON(data, &file); err != nil || (file.Version != 1 && file.Version != 2) || file.AICounted == nil {
			return state, errors.New("invalid detection state format")
		}
		for _, id := range file.AICounted {
			if !isSnowflake(id) {
				return state, errors.New("invalid AI-counted message ID")
			}
			state.AICounted[id] = true
		}
		for id, receipt := range file.ReviewReceipts {
			if !isSnowflake(id) || !isSnowflake(receipt.UserID) || len(receipt.Revision) != 64 || receipt.Units > 2000 {
				return state, errors.New("invalid review receipt")
			}
			state.ReviewReceipts[id] = receipt
		}
		stored = file.Counts
	} else if err := decodeJSON(data, &stored); err != nil {
		return state, fmt.Errorf("decode detection counts: %w", err)
	}
	if stored == nil {
		return state, errors.New("detection counts must be a JSON object")
	}
	for id, count := range stored {
		if !isSnowflake(id) || count == nil {
			return state, errors.New("detection counts require user IDs and unsigned integer values")
		}
		state.Counts[id] = *count
	}
	return state, nil
}

// Write in the destination directory so rename atomically replaces the old file.
// Until rename succeeds, every error leaves the previous counts intact.
func saveDetectionState(path string, counts map[string]uint64, aiCounted map[string]bool, reviews ...map[string]reviewReceipt) error {
	var value any = counts
	if len(aiCounted) > 0 {
		ids := make([]string, 0, len(aiCounted))
		for id := range aiCounted {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		value = struct {
			Version   int               `json:"version"`
			Counts    map[string]uint64 `json:"counts"`
			AICounted []string          `json:"ai_counted_message_ids"`
		}{1, counts, ids}
	}
	if len(reviews) > 0 && len(reviews[0]) > 0 {
		ids := []string{}
		for id := range aiCounted {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		value = struct {
			Version   int                      `json:"version"`
			Counts    map[string]uint64        `json:"counts"`
			AICounted []string                 `json:"ai_counted_message_ids"`
			Receipts  map[string]reviewReceipt `json:"review_receipts"`
		}{2, counts, ids, reviews[0]}
	}
	data, err := json.Marshal(value)
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
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	// The rename is the commit point. Directory fsync is best effort here: an
	// error after the rename must not roll back only the in-memory count.
	if directory, err := os.Open(dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

type messageClient interface {
	ChannelMessageSendComplex(string, *discordgo.MessageSend, ...discordgo.RequestOption) (*discordgo.Message, error)
}

type detector struct {
	mu             sync.Mutex // Serializes increment, persistence, and notification.
	words          map[string]bool
	channels       map[string]bool
	counts         map[string]uint64
	aiCounted      map[string]bool
	statePath      string
	reviewReceipts map[string]reviewReceipt
}

func newDetector(configPath, statePath string) (*detector, error) {
	config, err := loadDetectionConfig(configPath)
	if err != nil {
		return nil, err
	}
	state, err := loadDetectionState(statePath)
	if err != nil {
		return nil, err
	}
	d := &detector{words: make(map[string]bool), channels: make(map[string]bool), counts: state.Counts, aiCounted: state.AICounted, statePath: statePath, reviewReceipts: state.ReviewReceipts}
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

func (d *detector) eligibleMessage(event *discordgo.MessageCreate) bool {
	if event == nil || event.Message == nil {
		return false
	}
	m := event.Message
	return d.channels[m.ChannelID] && m.GuildID != "" && m.Author != nil && !m.Author.Bot && m.WebhookID == "" && isSnowflake(m.Author.ID)
}

func (d *detector) handleMessage(s messageClient, event *discordgo.MessageCreate) error {
	if !d.eligibleMessage(event) {
		return nil
	}
	increment := d.literalCount(event.Content)
	if increment == 0 {
		increment = d.literalCount(d.transformedText(event.Content))
	}
	if increment == 0 {
		return nil
	}
	return d.countAndReply(s, event.Author.ID, event.Reference(), increment, false)
}

// repetitionCount only accepts a whole message made of two or more
// configured uo/うお forms. It deliberately does not split arbitrary detection
// words, so a word sequence cannot turn ordinary prose into a detection.
func (d *detector) repetitionCount(content string) uint64 {
	words := make([]string, 0, len(d.words))
	for word := range d.words {
		words = append(words, word)
	}
	return repetitionCount(content, words)
}

func (d *detector) handleIncoming(s messageClient, ai *shadowCollector, event *discordgo.MessageCreate) error {
	if ai.enqueue(event) {
		return nil
	}
	return d.handleMessage(s, event)
}

func (d *detector) alreadyAICounted(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.aiCounted[id]
}

func (d *detector) countAndReply(s messageClient, id string, ref *discordgo.MessageReference, increment uint64, fromAI bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, reviewed := d.reviewReceipts[ref.MessageID]; reviewed || d.aiCounted[ref.MessageID] {
		return nil
	}
	previous, existed := d.counts[id]
	if increment > math.MaxUint64-previous {
		return errors.New("detection count overflow")
	}
	count := previous + increment
	d.counts[id] = count
	if fromAI {
		d.aiCounted[ref.MessageID] = true
	}
	if err := saveDetectionState(d.statePath, d.counts, d.aiCounted, d.reviewReceipts); err != nil {
		if fromAI {
			delete(d.aiCounted, ref.MessageID)
		}
		if existed {
			d.counts[id] = previous
		} else {
			delete(d.counts, id)
		}
		return fmt.Errorf("save detection counts: %w", err)
	}
	_, err := s.ChannelMessageSendComplex(ref.ChannelID, &discordgo.MessageSend{
		Content:         detectionMessage(id, increment, count),
		AllowedMentions: &discordgo.MessageAllowedMentions{Users: []string{id}},
		Reference:       ref,
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

// The count and receipt commit together. Repeating a result returns its original
// total; changing a message revision replaces only this review's contribution.
func (d *detector) applyReview(id, user, revision string, units uint64) (uint64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !isSnowflake(id) || !isSnowflake(user) || len(revision) != 64 || units > 2000 {
		return 0, errors.New("invalid review result")
	}
	if d.reviewReceipts == nil {
		d.reviewReceipts = make(map[string]reviewReceipt)
	}
	old, exists := d.reviewReceipts[id]
	if exists && old.UserID != user {
		return 0, errors.New("review author mismatch")
	}
	if exists && old.Revision == revision && old.Units == units {
		return old.Total, nil
	}
	if d.aiCounted[id] && !exists {
		return 0, errors.New("already owned by legacy AI")
	}
	previous := d.counts[user]
	if previous < old.Units || math.MaxUint64-(previous-old.Units) < units {
		return 0, errors.New("invalid count adjustment")
	}
	total := previous - old.Units + units
	d.counts[user] = total
	d.reviewReceipts[id] = reviewReceipt{user, revision, units, total}
	if err := saveDetectionState(d.statePath, d.counts, d.aiCounted, d.reviewReceipts); err != nil {
		d.counts[user] = previous
		if exists {
			d.reviewReceipts[id] = old
		} else {
			delete(d.reviewReceipts, id)
		}
		return 0, err
	}
	return total, nil
}

func detectionMessage(user string, units, total uint64) string {
	notice := "冷笑を検知しました！"
	if units > 1 {
		notice = fmt.Sprintf("冷笑を%d回検知しました！", units)
	}
	return fmt.Sprintf("%s\n<@%s>さんの%d回目の冷笑です！", notice, user, total)
}
