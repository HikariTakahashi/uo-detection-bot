package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	youtubeNotification = "youtube"
	fridayNotification  = "friday"
	fridayMentionCount  = 50
)

var japanTime = time.FixedZone("JST", 9*60*60)

type notificationConfig struct {
	channelID        string
	userID           string
	youtubeChannelID string
}

func loadNotificationConfig() (notificationConfig, error) {
	cfg := notificationConfig{
		channelID:        strings.TrimSpace(os.Getenv("DISCORD_CHANNEL_ID")),
		userID:           strings.TrimSpace(os.Getenv("DISCORD_USER_ID")),
		youtubeChannelID: strings.TrimSpace(os.Getenv("YOUTUBE_CHANNEL_ID")),
	}
	if cfg.channelID == "" && cfg.userID == "" {
		return notificationConfig{}, nil
	}
	if !isSnowflake(cfg.channelID) || !isSnowflake(cfg.userID) {
		return notificationConfig{}, errors.New("scheduled notifications require numeric DISCORD_CHANNEL_ID and DISCORD_USER_ID")
	}
	if cfg.youtubeChannelID == "" {
		cfg.youtubeChannelID = defaultYouTubeChannelID
	}
	if !validYouTubeChannelID(cfg.youtubeChannelID) {
		return notificationConfig{}, errors.New("YOUTUBE_CHANNEL_ID must be a YouTube channel ID starting with UC")
	}
	return cfg, nil
}

func (cfg notificationConfig) enabled() bool {
	return cfg.channelID != ""
}

func nextNotification(now time.Time) (time.Time, string) {
	now = now.In(japanTime)
	year, month, day := now.Date()
	morning := time.Date(year, month, day, 6, 0, 0, 0, japanTime)
	if !morning.After(now) {
		morning = morning.AddDate(0, 0, 1)
	}
	friday := time.Date(year, month, day, 0, 0, 0, 0, japanTime).
		AddDate(0, 0, (int(time.Friday)-int(now.Weekday())+7)%7)
	if !friday.After(now) {
		friday = friday.AddDate(0, 0, 7)
	}
	if friday.Before(morning) {
		return friday, fridayNotification
	}
	return morning, youtubeNotification
}

func runScheduledNotifications(ctx context.Context, s messageClient, cfg notificationConfig) {
	youtube := &http.Client{Timeout: 15 * time.Second}
	var last time.Time
	for ctx.Err() == nil {
		now := time.Now()
		if last.After(now) {
			now = last
		}
		next, kind := nextNotification(now)
		for remaining := time.Until(next); remaining > 0; remaining = time.Until(next) {
			// Recheck wall time periodically in case the system clock is adjusted.
			if err := waitNotification(ctx, min(remaining, time.Minute)); err != nil {
				return
			}
		}
		last = next
		var err error
		switch kind {
		case youtubeNotification:
			err = sendYouTubeNotification(ctx, s, cfg, youtube)
		case fridayNotification:
			err = sendFridayMentions(ctx, s, cfg)
		}
		if err != nil && ctx.Err() == nil {
			slog.Error("scheduled notification failed", "notification", kind, "error", err)
		}
	}
}

func waitNotification(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func sendScheduledMessage(ctx context.Context, s messageClient, channelID, content string, users ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content:         content,
		AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}, Users: users},
	}, discordgo.WithContext(ctx), discordgo.WithRestRetries(0))
	if err != nil {
		return errors.New(safeError(err))
	}
	return nil
}

func sendFridayMentions(ctx context.Context, s messageClient, cfg notificationConfig) error {
	mention := "<@" + cfg.userID + ">"
	for i := range fridayMentionCount {
		if err := sendScheduledMessage(ctx, s, cfg.channelID, mention, cfg.userID); err != nil {
			return fmt.Errorf("send Friday mention %d/%d: %w", i+1, fridayMentionCount, err)
		}
		if err := waitNotification(ctx, time.Second); err != nil {
			return err
		}
	}
	return sendScheduledMessage(ctx, s, cfg.channelID, "今日なにするの")
}
