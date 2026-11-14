package main

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

const laughterDeadline = 30 * time.Second

type laughterJob struct {
	record   shadowRecord
	authorID string // Passed to the existing counter; not stored for review.
	words    string
}

func wordCharacter(r rune) bool {
	return unicode.Is(unicode.Latin, r) || unicode.IsDigit(r) || r == '_'
}

// text and words have been normalized and URLs have been masked. Japanese
// word boundaries and quotations are resolved by the classifier.
func laughterWords(text string, words []string) string {
	var matches []string
	for _, word := range words {
		if word == "" {
			continue
		}
		first, _ := utf8.DecodeRuneInString(word)
		pattern := word + "w"
		for start := 0; start < len(text); {
			i := strings.Index(text[start:], pattern)
			if i < 0 {
				break
			}
			i += start
			start = i + 1
			before, _ := utf8.DecodeLastRuneInString(text[:i])
			rest := strings.TrimLeft(text[i+len(word):], "w")
			after, _ := utf8.DecodeRuneInString(rest)
			if (wordCharacter(first) && wordCharacter(before)) || wordCharacter(after) {
				continue
			}
			matches = append(matches, word)
			break
		}
	}
	slices.Sort(matches)
	return strings.Join(slices.Compact(matches), "、")
}

func (s *shadowCollector) recordDrop(live bool) {
	if live {
		s.laughterDropped.Add(1)
	} else {
		s.dropped.Add(1)
	}
}

func (s *shadowCollector) predictLaughter(ctx context.Context, job laughterJob) (shadowPrediction, error) {
	endpoint := strings.TrimSuffix(s.config.Endpoint, "/classify") + "/classify/laughter"
	input := map[string]string{"text": job.record.Text, "context": job.record.Context, "target_words": job.words}
	for attempt := 0; ; attempt++ {
		p, err := s.requestPrediction(ctx, endpoint, input)
		if err == nil {
			if p.Prompt != laughterPrompt {
				return p, errors.New("unexpected laughter classifier prompt")
			}
			return p, nil
		}
		if attempt != 0 || !errors.Is(err, errClassifierUnavailable) {
			return p, err
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return p, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *shadowCollector) processLaughter(ctx context.Context, sender messageClient, job laughterJob) {
	r := job.record
	defer s.finish(r.MessageID)
	s.mu.Lock()
	cancelled := s.cancelled[r.MessageID]
	s.mu.Unlock()
	if cancelled || s.detector.alreadyAICounted(r.MessageID) {
		return
	}
	requestCtx, cancel := context.WithDeadline(ctx, r.CapturedAt.Add(laughterDeadline))
	defer cancel()
	p, err := s.predictLaughter(requestCtx, job)
	if err != nil {
		r.Error = "classifier_failed_or_timeout"
		slog.Warn("laughter classification failed", "message_id", r.MessageID, "error", err)
	} else {
		r.Prediction = p
	}
	if ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	if s.cancelled[r.MessageID] {
		s.mu.Unlock()
		return
	}
	if s.store != nil && (p.Label == "uncertain" && err == nil || s.config.Mode == "shadow" && s.channels[r.ChannelID]) {
		if storeErr := s.store.add(r, time.Now()); storeErr != nil {
			slog.Error("laughter review save failed", "error", safeError(storeErr))
		}
	}
	s.mu.Unlock()
	if err != nil || p.Label != "mockery" {
		return
	}
	ref := &discordgo.MessageReference{MessageID: r.MessageID, ChannelID: r.ChannelID, GuildID: r.GuildID}
	if err := s.detector.countAndReply(sender, job.authorID, ref, 1, true); err != nil {
		slog.Error("laughter count or reply failed", "message_id", r.MessageID, "error", err)
	}
}
