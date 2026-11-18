package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

const productionApprover = "845282954636951583"
const decoderRepository = "HikariTakahashi/uo-decoder-evaluation"

var approvalID = regexp.MustCompile(`^[a-f0-9]{32}$`)
var commitID = regexp.MustCompile(`^[a-f0-9]{40}$`)
var checksumID = regexp.MustCompile(`^[a-f0-9]{64}$`)

type releasePin struct {
	Repository string `json:"repository"`
	Commit     string `json:"commit"`
	IndexSHA   string `json:"index_sha256"`
}
type releaseRequest struct {
	Schema   int        `json:"schema"`
	ID       string     `json:"id"`
	Approver string     `json:"approver"`
	Approval releasePin `json:"approval"`
	Base     releasePin `json:"base"`
	Created  int64      `json:"created_at"`
	Expires  int64      `json:"expires_at"`
	Families []string   `json:"families"`
	Bundles  int        `json:"bundles"`
	Checks   int        `json:"checks"`
}
type releaseDecision struct {
	ID      string `json:"request_id"`
	Hash    string `json:"request_sha256"`
	Actor   string `json:"actor"`
	Action  string `json:"action"`
	At      int64  `json:"decided_at"`
	Channel string `json:"channel_id"`
	Message string `json:"message_id"`
}
type approvalDelivery struct {
	Hash    string `json:"request_sha256"`
	ID      string `json:"id"`
	Channel string `json:"channel"`
	Message string `json:"message"`
	Sending bool   `json:"sending"`
	Blocked bool   `json:"blocked"`
	Display string `json:"display"`
}
type approvalDiscord interface {
	UserChannelCreate(string, ...discordgo.RequestOption) (*discordgo.Channel, error)
	ChannelMessageSendComplex(string, *discordgo.MessageSend, ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessageEditComplex(*discordgo.MessageEdit, ...discordgo.RequestOption) (*discordgo.Message, error)
	ChannelMessages(string, int, string, string, string, ...discordgo.RequestOption) ([]*discordgo.Message, error)
	InteractionRespond(*discordgo.Interaction, *discordgo.InteractionResponse, ...discordgo.RequestOption) error
}
type releaseApprovals struct {
	mu                 sync.Mutex
	shared, dir, botID string
	now                func() time.Time
}

func newReleaseApprovals(config, shared, dir string) (*releaseApprovals, error) {
	var c struct {
		Enabled    bool   `json:"enabled"`
		Approver   string `json:"approver"`
		Repository string `json:"repository"`
	}
	raw, e := os.ReadFile(config)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if e = decodeJSON(raw, &c); e != nil {
		return nil, e
	}
	if !c.Enabled {
		return nil, nil
	}
	if c.Approver != productionApprover || c.Repository != decoderRepository {
		return nil, errors.New("invalid production approval policy")
	}
	if e = os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	return &releaseApprovals{shared: shared, dir: dir, now: time.Now}, nil
}
func approvalRead(path string, v any) ([]byte, error) {
	info, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 || info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("untrusted approval file")
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	return raw, decodeJSON(raw, v)
}
func (a *releaseApprovals) request(id string) (releaseRequest, string, error) {
	var r releaseRequest
	if !approvalID.MatchString(id) {
		return r, "", errors.New("invalid approval id")
	}
	raw, e := approvalRead(filepath.Join(a.shared, "requests", id+".json"), &r)
	if e != nil {
		return r, "", e
	}
	validPin := func(p releasePin) bool {
		return p.Repository == decoderRepository && commitID.MatchString(p.Commit) && checksumID.MatchString(p.IndexSHA)
	}
	if r.Schema != 1 || r.ID != id || r.Approver != productionApprover || !validPin(r.Approval) || !validPin(r.Base) || r.Created <= 0 || r.Expires-r.Created != 7*86400 || r.Bundles < 1 || r.Checks < 1 {
		return r, "", errors.New("invalid approval request")
	}
	sum := sha256.Sum256(raw)
	return r, hex.EncodeToString(sum[:]), nil
}
func (a *releaseApprovals) selected() (string, error) {
	var p struct {
		ID string `json:"id"`
	}
	_, e := approvalRead(filepath.Join(a.shared, "requests", "current.json"), &p)
	if e != nil {
		return "", e
	}
	if !approvalID.MatchString(p.ID) {
		return "", errors.New("invalid current approval")
	}
	return p.ID, nil
}
func (a *releaseApprovals) delivery(id string) (approvalDelivery, error) {
	d := approvalDelivery{ID: id}
	_, e := approvalRead(filepath.Join(a.dir, id+".json"), &d)
	if errors.Is(e, os.ErrNotExist) {
		e = nil
	}
	if d.ID != id {
		return d, errors.New("invalid delivery")
	}
	return d, e
}
func (a *releaseApprovals) save(d approvalDelivery) error {
	return atomicShadowJSON(filepath.Join(a.dir, d.ID+".json"), d)
}
func (a *releaseApprovals) state(r releaseRequest) (string, error) {
	var result struct {
		State   string `json:"state"`
		Updated int64  `json:"updated_at"`
	}
	_, e := approvalRead(filepath.Join(a.shared, "results", r.ID+".json"), &result)
	if e != nil {
		return "", e
	}
	switch result.State {
	case "pending", "applying", "active", "rejected", "expired", "superseded", "failed":
	default:
		return "", errors.New("invalid adoption result")
	}
	if result.State == "pending" {
		var d releaseDecision
		if _, e := approvalRead(filepath.Join(a.shared, "decisions", r.ID+".json"), &d); e == nil {
			if d.Action == "approve" {
				return "approved", nil
			}
			return "rejected", nil
		} else if !errors.Is(e, os.ErrNotExist) {
			return "", e
		}
		if a.now().Unix() >= r.Expires {
			return "expired", nil
		}
	}
	return result.State, nil
}
func approvalText(r releaseRequest, state string) string {
	titles := map[string]string{"pending": "復元機能の本番採用の承認依頼", "approved": "承認済み・反映待ち", "applying": "承認済み・反映中", "active": "本番反映完了", "rejected": "本番採用を却下しました", "expired": "承認期限が切れました", "superseded": "候補または本番の版が変更されたため失効しました", "failed": "本番反映失敗・現在の版を維持しています"}
	families := []rune(strings.Join(r.Families, ", "))
	if len(families) > 400 {
		families = families[:400]
	}
	body := fmt.Sprintf("**%s**\n対応形式: %s\n検証: 復元器%d件・%d検証通過\nCommit: `%s`\nhttps://github.com/%s/releases/tag/evaluation-%s\n承認期限: <t:%d:F>\n", titles[state], string(families), r.Bundles, r.Checks, r.Approval.Commit, decoderRepository, r.Approval.Commit, r.Expires)
	if state == "pending" {
		body += "検証済みの復元機能を本番環境へ反映しますか？\n承認すると、Piで反映後に保留投稿を自動再判定し、冷笑と確定した分を本番カウントへ反映します。\n"
	}
	return body + "[採用依頼:" + r.ID + "]"
}
func approvalButtons(id string, disabled bool) []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: "本番採用を許可", Style: discordgo.SuccessButton, CustomID: "adopt:" + id + ":approve", Disabled: disabled},
		discordgo.Button{Label: "却下", Style: discordgo.DangerButton, CustomID: "adopt:" + id + ":reject", Disabled: disabled}}}}
}
func (a *releaseApprovals) notify(s approvalDiscord, r releaseRequest) error {
	d, e := a.delivery(r.ID)
	if e != nil {
		return e
	}
	_, hash, e := a.request(r.ID)
	if e != nil {
		return e
	}
	if d.Hash == "" && d.Channel == "" && !d.Sending {
		d.Hash = hash
		if e = a.save(d); e != nil {
			return e
		}
	}
	if d.Hash != hash {
		return errors.New("approval request changed after notification")
	}
	state, e := a.state(r)
	if e != nil {
		return e
	}
	if d.Blocked {
		return nil
	}
	if d.Message == "" && !d.Sending && state != "pending" {
		return nil
	}
	if d.Channel == "" {
		channel, e := s.UserChannelCreate(productionApprover)
		if e != nil {
			d.Blocked = true
			if err := a.save(d); err != nil {
				return err
			}
			return e
		}
		d.Channel = channel.ID
		if e = a.save(d); e != nil {
			return e
		}
	}
	if d.Message == "" {
		if d.Sending {
			before := ""
			for page := 0; page < 100; page++ {
				messages, e := s.ChannelMessages(d.Channel, 100, before, "", "")
				if e != nil {
					return e
				}
				for _, m := range messages {
					if m.Author != nil && m.Author.ID == a.botID && strings.Contains(m.Content, "[採用依頼:"+r.ID+"]") {
						d.Message = m.ID
						break
					}
				}
				if d.Message != "" || len(messages) < 100 {
					break
				}
				before = messages[len(messages)-1].ID
			}
			if d.Message == "" {
				return errors.New("approval DM delivery unresolved; no duplicate POST")
			}
			if e = a.save(d); e != nil {
				return e
			}
		} else {
			d.Sending = true
			if e = a.save(d); e != nil {
				return e
			}
			m, e := s.ChannelMessageSendComplex(d.Channel, &discordgo.MessageSend{Content: approvalText(r, state), Components: approvalButtons(r.ID, false), AllowedMentions: noReviewMentions()})
			if e != nil {
				return e
			}
			d.Message = m.ID
			d.Display = state
			return a.save(d)
		}
	}
	if d.Display == state {
		return nil
	}
	content := approvalText(r, state)
	components := approvalButtons(r.ID, state != "pending")
	_, e = s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: d.Message, Channel: d.Channel, Content: &content, Components: &components, AllowedMentions: noReviewMentions()})
	if e != nil {
		return e
	}
	d.Display = state
	return a.save(d)
}
func (a *releaseApprovals) tick(s approvalDiscord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, e := a.selected()
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	ids := map[string]bool{}
	if current != "" {
		ids[current] = true
	}
	entries, e := os.ReadDir(a.dir)
	if e != nil {
		return e
	}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if approvalID.MatchString(id) {
			ids[id] = true
		}
	}
	var first error
	for id := range ids {
		r, _, e := a.request(id)
		if e == nil {
			e = a.notify(s, r)
		}
		if e != nil && first == nil {
			first = e
		}
	}
	return first
}
func sharedDecision(path string, value releaseDecision) error {
	raw, e := json.Marshal(value)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".decision-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if e = f.Chmod(0640); e != nil {
		return e
	}
	if _, e = f.Write(append(raw, '\n')); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	// A hard link is an atomic create-if-absent, never replace an earlier answer.
	if e = os.Link(f.Name(), path); e != nil {
		return e
	}
	if dir, e := os.Open(filepath.Dir(path)); e == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
func (a *releaseApprovals) handle(s approvalDiscord, event *discordgo.InteractionCreate) (bool, error) {
	if a == nil || event == nil || event.Interaction == nil || event.Type != discordgo.InteractionMessageComponent {
		return false, nil
	}
	i := event.Interaction
	id := i.MessageComponentData().CustomID
	if !strings.HasPrefix(id, "adopt:") {
		return false, nil
	}
	deny := func() (bool, error) {
		return true, s.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Content: "この承認依頼は操作できません。本人・対象・期限・処理状態を確認してください。", Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noReviewMentions()}})
	}
	if !a.mu.TryLock() {
		return deny()
	}
	defer a.mu.Unlock()
	parts := strings.Split(id, ":")
	if len(parts) != 3 || !approvalID.MatchString(parts[1]) || (parts[2] != "approve" && parts[2] != "reject") || i.GuildID != "" || i.User == nil || i.User.ID != productionApprover || i.Message == nil {
		return deny()
	}
	r, hash, e := a.request(parts[1])
	if e != nil {
		return deny()
	}
	d, e := a.delivery(r.ID)
	if e != nil {
		return deny()
	}
	selected, e := a.selected()
	if e != nil {
		return deny()
	}
	state, e := a.state(r)
	if e != nil || state != "pending" || d.Hash != hash || selected != r.ID || i.ChannelID != d.Channel || i.Message.ID != d.Message || a.now().Unix() < r.Created || a.now().Unix() >= r.Expires {
		return deny()
	}
	// ACK before any disk commit or deployment work; ordinary Bot edits carry
	// later results without depending on the short-lived interaction token.
	if e = s.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate}); e != nil {
		return true, e
	}
	decision := releaseDecision{ID: r.ID, Hash: hash, Actor: i.User.ID, Action: parts[2], At: a.now().Unix(), Channel: d.Channel, Message: d.Message}
	if e = sharedDecision(filepath.Join(a.shared, "decisions", r.ID+".json"), decision); e != nil {
		return true, e
	}
	return true, nil
}
func (a *releaseApprovals) run(ctx context.Context, s approvalDiscord) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		if e := a.tick(s); e != nil {
			slog.Warn("production approval deferred", "error", safeError(e))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
