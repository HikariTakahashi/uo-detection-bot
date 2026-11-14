package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultYouTubeChannelID = "UCiTxQj4a82_B6Y0DvVAE8Ww"
	youtubeFeedURL          = "https://www.youtube.com/feeds/videos.xml"
	maxYouTubeFeedSize      = 1 << 20
)

type youtubeUpload struct {
	published time.Time
	link      string
}

func validYouTubeChannelID(id string) bool {
	if len(id) != 24 || !strings.HasPrefix(id, "UC") {
		return false
	}
	for _, c := range id[2:] {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func fetchYouTubeUpload(ctx context.Context, client *http.Client, channelID string) (youtubeUpload, error) {
	feedURL := youtubeFeedURL + "?" + url.Values{"channel_id": {channelID}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feedURL, nil)
	if err != nil {
		return youtubeUpload{}, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; YouTubeLastUploadBot/1.0)")
	resp, err := client.Do(req)
	if err != nil {
		return youtubeUpload{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return youtubeUpload{}, fmt.Errorf("YouTube feed returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxYouTubeFeedSize+1))
	if err != nil {
		return youtubeUpload{}, err
	}
	if len(data) > maxYouTubeFeedSize {
		return youtubeUpload{}, errors.New("YouTube feed exceeds the size limit")
	}
	return parseYouTubeUpload(data)
}

func parseYouTubeUpload(data []byte) (youtubeUpload, error) {
	var feed struct {
		XMLName xml.Name `xml:"http://www.w3.org/2005/Atom feed"`
		Entries []struct {
			Published string `xml:"http://www.w3.org/2005/Atom published"`
			Links     []struct {
				Rel  string `xml:"rel,attr"`
				Href string `xml:"href,attr"`
			} `xml:"http://www.w3.org/2005/Atom link"`
		} `xml:"http://www.w3.org/2005/Atom entry"`
	}
	if err := xml.Unmarshal(data, &feed); err != nil {
		return youtubeUpload{}, fmt.Errorf("decode YouTube feed: %w", err)
	}
	if len(feed.Entries) == 0 {
		return youtubeUpload{}, errors.New("YouTube feed has no uploads")
	}
	// Like the original script, use the first video and its publish time.
	entry := feed.Entries[0]
	published, err := time.Parse(time.RFC3339, strings.TrimSpace(entry.Published))
	if err != nil {
		return youtubeUpload{}, errors.New("YouTube feed has no valid publish time for the latest upload")
	}
	var link string
	for _, candidate := range entry.Links {
		href := strings.TrimSpace(candidate.Href)
		u, err := url.Parse(href)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(candidate.Rel), "alternate") {
			link = href
			break
		}
		if link == "" {
			link = href
		}
	}
	if link == "" {
		return youtubeUpload{}, errors.New("YouTube feed has no video link for the latest upload")
	}
	return youtubeUpload{published: published, link: link}, nil
}

func sendYouTubeNotification(ctx context.Context, s messageClient, cfg notificationConfig, client *http.Client) error {
	upload, err := fetchYouTubeUpload(ctx, client, cfg.youtubeChannelID)
	mention := "<@" + cfg.userID + ">"
	message := mention + "、おはよう1319!\n"
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		slog.Warn("failed to fetch latest YouTube upload", "error", err)
		message += "YouTubeからの取得に失敗しました！\nなんとかしろ！、 " + mention
	} else {
		days := int(math.Floor(time.Since(upload.published).Hours() / 24))
		message += fmt.Sprintf("最後の動画投稿から%d 日経過しました！\n最後に投稿した動画： %s", days, upload.link)
	}
	return sendScheduledMessage(ctx, s, cfg.channelID, message, cfg.userID)
}
