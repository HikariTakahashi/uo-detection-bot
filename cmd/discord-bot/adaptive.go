package main

// Persistent review orchestration. Only this Bot owns Discord credentials and
// count mutations. The loopback decoder never receives IDs or credentials.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

type adaptiveConfig struct {
	Enabled bool `json:"enabled"`
}
type adaptiveRecord struct {
	CancelPending bool              `json:"cancel_pending"`
	Epoch         uint64            `json:"epoch"`
	ID            string            `json:"id"`
	Channel       string            `json:"channel"`
	Guild         string            `json:"guild"`
	Author        string            `json:"author"`
	Text          string            `json:"text"`
	Revision      string            `json:"revision"`
	Edited        string            `json:"edited"`
	ReplyID       string            `json:"reply_id"`
	ReplyHistory  []string          `json:"reply_history,omitempty"`
	NoticeVersion int               `json:"notice_version,omitempty"`
	Sending       bool              `json:"sending"` // persisted before POST; uncertain POSTs are reconciled, never blindly retried
	State         string            `json:"state"`   // queued, waiting, done, cancelled
	Release       string            `json:"release"`
	Families      []string          `json:"families"`
	Label         string            `json:"label"`
	Units         uint64            `json:"units"`
	Total         uint64            `json:"total"`
	Notice        string            `json:"notice"`
	NoticeDirty   bool              `json:"notice_dirty"`
	Updated       time.Time         `json:"updated"`
	Estimation    *estimationStatus `json:"-"`
}
type adaptiveResult struct {
	Revision   string            `json:"revision"`
	Release    string            `json:"release"`
	Unknown    bool              `json:"unknown"`
	Label      string            `json:"label"`
	Units      uint64            `json:"units"`
	Families   []string          `json:"families"`
	Estimation *estimationStatus `json:"estimation,omitempty"`
}
type adaptiveClient interface {
	messageClient
	ChannelMessage(string, string, ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageDelete(string, string, ...discordgo.RequestOption) error
	ChannelMessages(string, int, string, string, string, ...discordgo.RequestOption) ([]*discordgo.Message, error)
}
type adaptiveReviews struct {
	mu       sync.Mutex
	dir      string
	records  map[string]adaptiveRecord
	detector *detector
	http     *http.Client
	endpoint string
	botID    string
}

func contentRevision(m *discordgo.Message) string {
	edited := ""
	if m.EditedTimestamp != nil {
		edited = m.EditedTimestamp.UTC().Format(time.RFC3339Nano)
	}
	sum := sha256.Sum256([]byte(edited + "\x00" + m.Content))
	return hex.EncodeToString(sum[:])
}
func newAdaptive(config, dir string, d *detector) (*adaptiveReviews, error) {
	data, err := os.ReadFile(config)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c adaptiveConfig
	if err = decodeJSON(data, &c); err != nil {
		return nil, err
	}
	if !c.Enabled {
		return nil, nil
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	a := &adaptiveReviews{dir: dir, records: map[string]adaptiveRecord{}, detector: d, http: &http.Client{Timeout: 90 * time.Second}, endpoint: "http://127.0.0.1:8767"}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil, errors.New("invalid review file")
		}
		raw, e := os.ReadFile(filepath.Join(dir, entry.Name()))
		if e != nil {
			return nil, e
		}
		var r adaptiveRecord
		if len(raw) > 65536 || decodeJSON(raw, &r) != nil || !isSnowflake(r.ID) || entry.Name() != r.ID+".json" || !isSnowflake(r.Author) || !isSnowflake(r.Channel) || !isSnowflake(r.Guild) || len(r.Revision) != 64 {
			return nil, errors.New("invalid review state")
		}
		if r.State != "queued" && r.State != "waiting" && r.State != "done" && r.State != "cancelled" {
			return nil, errors.New("invalid review state")
		}
		if r.Estimation != nil && !r.Estimation.valid() {
			return nil, errors.New("invalid estimation status")
		}
		if err := a.loadEstimationCache(&r); err != nil {
			return nil, err
		}
		if r.NoticeVersion < 0 || r.NoticeVersion > 1 || (r.ReplyID != "" && !isSnowflake(r.ReplyID)) {
			return nil, errors.New("invalid review delivery state")
		}
		for _, id := range r.ReplyHistory {
			if !isSnowflake(id) {
				return nil, errors.New("invalid review reply history")
			}
		}
		// Existing completed notices stay delivered. An unfinished legacy edit
		// becomes a new reply without changing its already committed count.
		if r.NoticeVersion == 0 && r.NoticeDirty && r.ReplyID != "" {
			queueReviewNotice(&r, r.Notice)
			if e := a.save(r); e != nil {
				return nil, e
			}
		}
		a.records[r.ID] = r
	}
	return a, nil
}
func (a *adaptiveReviews) save(r adaptiveRecord) error {
	r.Updated = time.Now().UTC()
	if err := atomicShadowJSON(filepath.Join(a.dir, r.ID+".json"), r); err != nil {
		return err
	}
	a.records[r.ID] = r
	return a.saveEstimationCache(r)
}

// Durable ownership is established before existing AI paths can see the post.
func (a *adaptiveReviews) enqueue(m *discordgo.MessageCreate) (bool, error) {
	if a == nil || !a.detector.eligibleMessage(m) {
		return false, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.records[m.ID]; ok {
		return true, nil
	}
	if a.detector.literalCount(m.Content) > 0 {
		return false, nil
	}
	if !isSnowflake(m.ID) || m.Content == "" || utf8.RuneCountInString(m.Content) > 2000 {
		return true, nil
	}
	pending := 0
	for _, r := range a.records {
		if r.State == "queued" || r.State == "waiting" {
			pending++
		}
	}
	if pending >= 1000 {
		return true, errors.New("adaptive review capacity reached")
	}
	r := adaptiveRecord{ID: m.ID, Channel: m.ChannelID, Guild: m.GuildID, Author: m.Author.ID, Text: m.Content, Revision: contentRevision(m.Message), State: "queued"}
	return true, a.save(r)
}
func (a *adaptiveReviews) edit(m *discordgo.MessageUpdate) error {
	if a == nil || m == nil || m.Message == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.records[m.ID]
	if !ok || r.State == "cancelled" {
		return nil
	}
	// Gateway updates may omit content/author. Fetch the authoritative message in
	// the worker rather than treating an omitted content field as an empty edit.
	r.Epoch++
	r.State = "queued"
	// Retain the evaluated release so duplicate/partial Gateway updates can
	// be reconciled against the authoritative unchanged revision.
	return a.save(r)
}
func (a *adaptiveReviews) remove(id, guild string) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.records[id]
	if !ok || r.Guild != guild {
		return nil
	}
	if r.State != "cancelled" {
		r.CancelPending = r.State != "done"
	}
	r.State = "cancelled"
	r.Text = ""
	r.NoticeDirty = false
	return a.save(r)
}
func (a *adaptiveReviews) snapshot(id string) (adaptiveRecord, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.records[id]
	return r, ok
}
func unknownMessage(user string) string {
	return fmt.Sprintf("冷笑の疑いがあるメッセージを検知しました！\n<@%s>さんのメッセージは現在審査中です！", user)
}
func resultMessage(label, user string, total uint64) string {
	switch label {
	case "mockery":
		return fmt.Sprintf("**再判定完了**\n冷笑を検知しました！\n<@%s>さんの%d回目の冷笑です！", user, total)
	case "other":
		return "**再判定完了**\n冷笑ではありませんでした。"
	default:
		return "**審査継続**\n判定が保留されました！\n<@845282954636951583>、なんとかしろ！"
	}
}

// A persisted notice is immutable until its delivery is reconciled. The last
// completed reply bounds recovery so an earlier identical hold cannot match.
func queueReviewNotice(r *adaptiveRecord, content string) {
	if r.ReplyID != "" {
		r.ReplyHistory = append(r.ReplyHistory, r.ReplyID)
	}
	r.ReplyID = ""
	r.Sending = false
	r.NoticeVersion = 1
	r.Notice = content
	r.NoticeDirty = true
}

func reviewMentions(r adaptiveRecord) *discordgo.MessageAllowedMentions {
	users := []string{}
	if r.Label == "uncertain" {
		users = append(users, "845282954636951583")
	} else if r.Label == "mockery" || r.Label == "" {
		users = append(users, r.Author)
	}
	return &discordgo.MessageAllowedMentions{Users: users, RepliedUser: false}
}
func discordMissing(err error) bool {
	var e *discordgo.RESTError
	return errors.As(err, &e) && e.Message != nil && e.Message.Code == 10008
}
func (a *adaptiveReviews) analyze(ctx context.Context, r adaptiveRecord) (adaptiveResult, error) {
	var out adaptiveResult
	words := []string{}
	for w := range a.detector.words {
		words = append(words, w)
	}
	sort.Strings(words)
	body, _ := json.Marshal(map[string]any{"text": r.Text, "revision": r.Revision, "words": words, "channel_id": r.Channel})
	req, e := http.NewRequestWithContext(ctx, "POST", a.endpoint+"/analyze", bytes.NewReader(body))
	if e != nil {
		return out, e
	}
	req.Header.Set("Content-Type", "application/json")
	resp, e := a.http.Do(req)
	if e != nil {
		return out, errors.New("decoder unavailable")
	}
	defer resp.Body.Close()
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if e != nil || len(raw) > 65536 || resp.StatusCode != 200 {
		return out, errors.New("decoder failed")
	}
	if e = decodeJSON(raw, &out); e != nil {
		return out, errors.New("invalid decoder response")
	}
	if out.Revision != r.Revision || len(out.Release) != 64 || out.Units > 2000 || len(out.Families) > 64 {
		return out, errors.New("decoder response mismatch")
	}
	if out.Label != "mockery" && out.Label != "other" && out.Label != "uncertain" {
		return out, errors.New("invalid decoder label")
	}
	if out.Label != "mockery" && out.Units != 0 {
		return out, errors.New("invalid decoder count")
	}
	if out.Estimation != nil && (!out.Estimation.valid() || !out.Unknown || out.Units != 0) {
		return out, errors.New("invalid estimation result")
	}
	return out, nil
}
func (a *adaptiveReviews) release(ctx context.Context) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", a.endpoint+"/health", nil)
	resp, e := a.http.Do(req)
	if e != nil {
		return "", errors.New("decoder unavailable")
	}
	defer resp.Body.Close()
	var h struct {
		Status  string `json:"status"`
		Release string `json:"release"`
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if e != nil || resp.StatusCode != 200 || json.Unmarshal(raw, &h) != nil || h.Status != "ready" || len(h.Release) != 64 {
		return "", errors.New("decoder not ready")
	}
	return h.Release, nil
}

// If a POST's outcome is unknown, find the existing reply. Never issue a second
// POST merely because a timeout or process crash lost its response.
func (a *adaptiveReviews) reconcile(s adaptiveClient, r adaptiveRecord) (string, error) {
	boundary := r.ID
	if len(r.ReplyHistory) > 0 {
		boundary = r.ReplyHistory[len(r.ReplyHistory)-1]
	}
	before := ""
	for page := 0; page < 100; page++ {
		msgs, e := s.ChannelMessages(r.Channel, 100, before, "", "", discordgo.WithContext(context.Background()))
		if e != nil {
			return "", e
		}
		for _, m := range msgs {
			if len(m.ID) < len(boundary) || (len(m.ID) == len(boundary) && m.ID <= boundary) {
				return "", nil
			}
			if m.Author != nil && m.Author.ID == a.botID && m.MessageReference != nil && m.MessageReference.MessageID == r.ID && m.Content == r.Notice {
				return m.ID, nil
			}
		}
		if len(msgs) < 100 {
			return "", nil
		}
		before = msgs[len(msgs)-1].ID
	}
	return "", nil
}
func (a *adaptiveReviews) notify(s adaptiveClient, id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	r, ok := a.records[id]
	if !ok {
		return nil
	}
	if r.State == "cancelled" {
		// A count receipt may have committed immediately before a failed review
		// checkpoint. Pending cancellation withdraws that contribution as well.
		if r.CancelPending {
			if _, err := a.detector.applyReview(r.ID, r.Author, r.Revision, 0); err != nil {
				return err
			}
			r.CancelPending = false
			if err := a.save(r); err != nil {
				return err
			}
		}
		if r.ReplyID == "" && r.Sending {
			reply, e := a.reconcile(s, r)
			if e != nil {
				return e
			}
			if reply == "" {
				return errors.New("review reply delivery unresolved; operator reconciliation required")
			}
			r.ReplyID = reply
		}
		for len(r.ReplyHistory) > 0 {
			if e := s.ChannelMessageDelete(r.Channel, r.ReplyHistory[0]); e != nil && !discordMissing(e) {
				return e
			}
			r.ReplyHistory = r.ReplyHistory[1:]
			if e := a.save(r); e != nil {
				return e
			}
		}
		if r.ReplyID != "" {
			if e := s.ChannelMessageDelete(r.Channel, r.ReplyID); e != nil && !discordMissing(e) {
				return e
			}
			r.ReplyID = ""
			r.Sending = false
		}
		return a.save(r)
	}
	if !r.NoticeDirty {
		return nil
	}
	if r.ReplyID == "" {
		if r.Sending {
			reply, e := a.reconcile(s, r)
			if e != nil {
				return e
			}
			if reply == "" {
				return errors.New("review reply delivery unresolved; operator reconciliation required")
			}
			r.ReplyID = reply
		} else {
			r.Sending = true
			if e := a.save(r); e != nil {
				return e
			}
			msg, e := s.ChannelMessageSendComplex(r.Channel, &discordgo.MessageSend{Content: r.Notice, Reference: &discordgo.MessageReference{MessageID: r.ID, ChannelID: r.Channel, GuildID: r.Guild}, AllowedMentions: reviewMentions(r)})
			if e != nil {
				return e
			}
			r.ReplyID = msg.ID
			// Save the response ID before marking this notice delivered.
			if e = a.save(r); e != nil {
				return e
			}
		}
	}
	r.NoticeDirty = false
	return a.save(r)
}
func (a *adaptiveReviews) process(ctx context.Context, s adaptiveClient, id, release string) error {
	r, ok := a.snapshot(id)
	if !ok {
		return nil
	}
	if r.State == "cancelled" {
		return a.notify(s, id)
	}
	if r.NoticeDirty {
		if e := a.notify(s, id); e != nil {
			return e
		}
		r, _ = a.snapshot(id)
	}
	if r.State == "waiting" && r.Release == release && r.Estimation != nil {
		return a.refreshEstimation(ctx, s, r)
	}
	if r.State == "done" || (r.State == "waiting" && r.Release == release) {
		return nil
	}
	m, e := s.ChannelMessage(r.Channel, r.ID, discordgo.WithContext(ctx))
	if discordMissing(e) {
		if e = a.remove(id, r.Guild); e != nil {
			return e
		}
		return a.notify(s, id)
	}
	if e != nil {
		return e
	}
	if m.Author == nil || m.Author.ID != r.Author {
		return errors.New("review author changed")
	}
	if contentRevision(m) == r.Revision && r.State == "queued" && r.Release != "" {
		final := r.Label == "mockery" || r.Label == "other"
		if final || r.Release == release {
			a.mu.Lock()
			current := a.records[id]
			if current.State == "queued" && current.Epoch == r.Epoch && current.Revision == r.Revision {
				current.State = "waiting"
				if final {
					current.State = "done"
				}
				e = a.save(current)
			}
			a.mu.Unlock()
			return e
		}
	}
	if contentRevision(m) != r.Revision {
		a.mu.Lock()
		current := a.records[id]
		if current.State == "cancelled" {
			a.mu.Unlock()
			return nil
		}
		current.Text = m.Content
		current.Revision = contentRevision(m)
		current.Edited = ""
		if m.EditedTimestamp != nil {
			current.Edited = m.EditedTimestamp.UTC().Format(time.RFC3339Nano)
		}
		current.State = "queued"
		current.Release = ""
		// Retain the previous label until the new result commits, including
		// pre-upgrade holds that did not yet have a notice.
		current.Units = 0
		// An edited post is pending again; withdraw this review's prior contribution.
		_, e = a.detector.applyReview(id, current.Author, current.Revision, 0)
		if e == nil {
			e = a.save(current)
		}
		a.mu.Unlock()
		if e != nil {
			return e
		}
		r = current
	}
	if utf8.RuneCountInString(r.Text) > 2000 {
		return errors.New("review text outside limits")
	}
	result := adaptiveResult{Revision: r.Revision, Release: release, Label: "other"}
	if r.Text != "" {
		result, e = a.analyze(ctx, r)
	}
	if e != nil {
		return e
	}
	// Re-fetch immediately before applying: missed/offline edit/delete events must
	// not allow a stale inference result to modify the counter.
	latest, e := s.ChannelMessage(r.Channel, r.ID, discordgo.WithContext(ctx))
	if discordMissing(e) {
		if e = a.remove(id, r.Guild); e != nil {
			return e
		}
		return a.notify(s, id)
	}
	if e != nil {
		return e
	}
	if contentRevision(latest) != r.Revision {
		return nil
	}
	a.mu.Lock()
	current := a.records[id]
	if current.State == "cancelled" || current.Revision != r.Revision || current.Epoch != r.Epoch {
		a.mu.Unlock()
		return nil
	}
	// The first unknown result always creates the pending notice without a count.
	wasHeld := current.State == "waiting" || current.Label == "uncertain" || strings.Contains(current.Notice, "現在審査中") || strings.HasPrefix(current.Notice, "**審査継続**") || strings.HasPrefix(current.Notice, "**再判定完了**")
	current.Estimation = result.Estimation
	if result.Estimation != nil {
		current.State, current.Release, current.Label, current.Units = "waiting", result.Release, "uncertain", 0
		queueReviewNotice(&current, estimationMessage(*result.Estimation))
		e = a.save(current)
		a.mu.Unlock()
		if e != nil {
			return e
		}
		return a.notify(s, id)
	}
	if result.Unknown && !wasHeld {
		current.State = "waiting"
		current.Release = result.Release
		current.Families = result.Families
		current.Label = ""
		queueReviewNotice(&current, unknownMessage(r.Author))
	} else {
		units := result.Units
		if result.Unknown {
			units = 0
			result.Label = "uncertain"
		}
		total, err := a.detector.applyReview(id, r.Author, r.Revision, units)
		if err != nil {
			a.mu.Unlock()
			return err
		}
		current.Total = total
		current.Units = units
		current.Label = result.Label
		current.Release = result.Release
		current.Families = result.Families
		current.State = "done"
		if result.Label == "uncertain" {
			current.State = "waiting"
		}
		// Ordinary non-target posts have no review reply. They are not announced.
		if wasHeld || current.ReplyID != "" || result.Label == "mockery" || result.Label == "uncertain" {
			notice := resultMessage(result.Label, r.Author, total)
			if !wasHeld {
				notice = strings.TrimPrefix(notice, "**再判定完了**\n")
			}
			if !wasHeld && result.Label == "mockery" {
				notice = detectionMessage(r.Author, units, total)
			}
			queueReviewNotice(&current, notice)
		}
	}
	e = a.save(current)
	a.mu.Unlock()
	if e != nil {
		return e
	}
	return a.notify(s, id)
}
func (a *adaptiveReviews) run(ctx context.Context, s adaptiveClient) {
	timer := time.NewTicker(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		a.mu.Lock()
		cancelled := []string{}
		for id, r := range a.records {
			if r.State == "cancelled" && (r.ReplyID != "" || len(r.ReplyHistory) > 0 || r.Sending || r.CancelPending) {
				cancelled = append(cancelled, id)
			}
		}
		a.mu.Unlock()
		for _, id := range cancelled {
			if e := a.notify(s, id); e != nil {
				slog.Warn("adaptive cancellation deferred", "error", safeError(e))
			}
		}
		rev, err := a.release(ctx)
		if err != nil {
			continue
		}
		a.mu.Lock()
		ids := []string{}
		for id, r := range a.records {
			if r.State != "done" || r.NoticeDirty {
				ids = append(ids, id)
			}
		}
		a.mu.Unlock()
		sort.Strings(ids)
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			if e := a.process(ctx, s, id, rev); e != nil {
				slog.Warn("adaptive review deferred", "error", safeError(e))
			}
		}
	}
}
