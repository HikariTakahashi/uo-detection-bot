package main

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/bwmarrin/discordgo"
)

const (
	testNotificationChannel = "123456789012345678"
	testNotificationUser    = "234567890123456789"
)

func TestLoadNotificationConfig(t *testing.T) {
	t.Setenv("DISCORD_CHANNEL_ID", "")
	t.Setenv("DISCORD_USER_ID", "")
	t.Setenv("YOUTUBE_CHANNEL_ID", "")
	if cfg, err := loadNotificationConfig(); err != nil || cfg.enabled() {
		t.Fatalf("unconfigured notifications: %+v, %v", cfg, err)
	}
	t.Setenv("DISCORD_CHANNEL_ID", testNotificationChannel)
	if _, err := loadNotificationConfig(); err == nil {
		t.Fatal("accepted a channel ID without a user ID")
	}
	t.Setenv("DISCORD_USER_ID", " "+testNotificationUser+" ")
	if cfg, err := loadNotificationConfig(); err != nil || !cfg.enabled() ||
		cfg.userID != testNotificationUser || cfg.youtubeChannelID != defaultYouTubeChannelID {
		t.Fatalf("configured notifications: %+v, %v", cfg, err)
	}
	t.Setenv("YOUTUBE_CHANNEL_ID", "https://www.youtube.com/@example")
	if _, err := loadNotificationConfig(); err == nil {
		t.Fatal("accepted a URL instead of a YouTube channel ID")
	}
}

func TestNextNotification(t *testing.T) {
	for _, tc := range []struct {
		now, want string
		kind      string
	}{
		{"2026-11-12T14:59:59Z", "2026-11-12T15:00:00Z", fridayNotification},
		{"2026-11-12T15:00:00Z", "2026-11-12T21:00:00Z", youtubeNotification},
		{"2026-11-12T20:59:59Z", "2026-11-12T21:00:00Z", youtubeNotification},
		{"2026-11-12T21:00:00Z", "2026-11-13T21:00:00Z", youtubeNotification},
		{"2026-12-31T14:59:59Z", "2026-12-31T15:00:00Z", fridayNotification},
	} {
		now, err := time.Parse(time.RFC3339, tc.now)
		if err != nil {
			t.Fatal(err)
		}
		next, kind := nextNotification(now)
		if got := next.UTC().Format(time.RFC3339); got != tc.want || kind != tc.kind {
			t.Errorf("after %s: got %s %s, want %s %s", tc.now, got, kind, tc.want, tc.kind)
		}
	}
}

type recordedNotification struct {
	when    time.Time
	channel string
	message *discordgo.MessageSend
}

type recordingNotifications []recordedNotification

func (r *recordingNotifications) ChannelMessageSendComplex(channel string, message *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	*r = append(*r, recordedNotification{time.Now(), channel, message})
	return &discordgo.Message{}, nil
}

func checkNotification(t *testing.T, n recordedNotification, content string, users ...string) {
	t.Helper()
	allowed := n.message.AllowedMentions
	if n.channel != testNotificationChannel || n.message.Content != content ||
		allowed == nil || len(allowed.Parse) != 0 || len(allowed.Roles) != 0 ||
		allowed.RepliedUser || !slices.Equal(allowed.Users, users) {
		t.Errorf("unexpected notification: channel %s, message %+v, allowed mentions %+v", n.channel, n.message, allowed)
	}
}

func TestFridayMentions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := notificationConfig{channelID: testNotificationChannel, userID: testNotificationUser}
		var sent recordingNotifications
		start := time.Now()
		if err := sendFridayMentions(t.Context(), &sent, cfg); err != nil {
			t.Fatal(err)
		}
		if len(sent) != 51 {
			t.Fatalf("sent %d messages, want 51", len(sent))
		}
		for i, n := range sent[:50] {
			checkNotification(t, n, "<@"+testNotificationUser+">", testNotificationUser)
			if got := n.when.Sub(start); got != time.Duration(i)*time.Second {
				t.Errorf("mention %d sent after %v", i+1, got)
			}
		}
		checkNotification(t, sent[50], "今日なにするの")
		if got := sent[50].when.Sub(start); got != 50*time.Second {
			t.Errorf("closing message sent after %v", got)
		}
	})
}

func TestFridayMentionsStopOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := notificationConfig{channelID: testNotificationChannel, userID: testNotificationUser}
		var sent recordingNotifications
		ctx, cancel := context.WithTimeout(t.Context(), 2500*time.Millisecond)
		defer cancel()
		if err := sendFridayMentions(ctx, &sent, cfg); err != context.DeadlineExceeded || len(sent) != 3 {
			t.Fatalf("got %v after sending %d messages, want deadline after 3", err, len(sent))
		}
	})
}

type notificationRoundTripper func(*http.Request) (*http.Response, error)

func (f notificationRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestYouTubeNotification(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{
			"latest upload", http.StatusOK,
			`<feed xmlns="http://www.w3.org/2005/Atom">
				<entry>
					<published>1999-12-30T10:00:00+09:00</published>
					<updated>2000-01-01T00:00:00Z</updated>
					<link rel="self" href="https://www.youtube.com/feeds/example"/>
					<link rel="alternate" href="https://www.youtube.com/watch?v=latest"/>
				</entry>
				<entry><published>1999-12-15T00:00:00Z</published></entry>
			</feed>`,
			"最後の動画投稿から1 日経過しました！\n最後に投稿した動画： https://www.youtube.com/watch?v=latest",
		},
		{
			"empty feed", http.StatusOK, `<feed xmlns="http://www.w3.org/2005/Atom"></feed>`,
			"YouTubeからの取得に失敗しました！\nなんとかしろ！、 <@" + testNotificationUser + ">",
		},
		{
			"HTTP error", http.StatusServiceUnavailable, "",
			"YouTubeからの取得に失敗しました！\nなんとかしろ！、 <@" + testNotificationUser + ">",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				client := &http.Client{Transport: notificationRoundTripper(func(r *http.Request) (*http.Response, error) {
					if r.Method != http.MethodGet || r.URL.Host != "www.youtube.com" ||
						r.URL.Path != "/feeds/videos.xml" || r.URL.Query().Get("channel_id") != defaultYouTubeChannelID {
						t.Errorf("unexpected feed request: %s %s", r.Method, r.URL)
					}
					return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}, nil
				})}
				cfg := notificationConfig{channelID: testNotificationChannel, userID: testNotificationUser, youtubeChannelID: defaultYouTubeChannelID}
				var sent recordingNotifications
				if err := sendYouTubeNotification(t.Context(), &sent, cfg, client); err != nil {
					t.Fatal(err)
				}
				if len(sent) != 1 {
					t.Fatalf("sent %d notifications, want 1", len(sent))
				}
				checkNotification(t, sent[0], "<@"+testNotificationUser+">、おはよう1319!\n"+tc.want, testNotificationUser)
			})
		})
	}
}
