package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

type estimationStatus struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Reason  string `json:"reason"`
	Version string `json:"version"`
}

var estimationID = regexp.MustCompile(`^estimate-[a-f0-9]{64}$`)
var estimationVersion = regexp.MustCompile(`^[a-f0-9]{64}$`)
var estimationNoticeID = regexp.MustCompile(`(?m)^依頼ID: (estimate-[a-f0-9]{64})$`)
var estimationLabels = map[string]string{
	"queued":            "未知方式の推定依頼待ちです（未送信）。",
	"awaiting_reply":    "未知方式の推定を依頼しました。Lunaの回答待ちです。",
	"unable":            "根拠が足りず、方式を推定できませんでした。",
	"validating":        "推定候補を隔離環境で検証中です。加算しません。",
	"unverified":        "推定候補はありますが、正しさは未確認です。加算しません。",
	"awaiting_adoption": "独立検証済みの候補です。公開・本番採用の承認待ちです。加算しません。",
	"complete":          "承認済み復元器の本番採用が完了しました。投稿の判定は改めて行います。",
	"failed":            "推定依頼が失敗しました。自動再送はしません。",
}

type estimationCache struct {
	Revision string           `json:"revision"`
	Epoch    uint64           `json:"epoch"`
	Status   estimationStatus `json:"status"`
}

func (a *adaptiveReviews) saveEstimationCache(r adaptiveRecord) error {
	path := filepath.Join(a.dir, "estimation-status", r.ID+".json")
	if r.Estimation == nil {
		err := os.Remove(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return atomicShadowJSON(path, estimationCache{Revision: r.Revision, Epoch: r.Epoch, Status: *r.Estimation})
}

func (a *adaptiveReviews) loadEstimationCache(r *adaptiveRecord) error {
	match := estimationNoticeID.FindStringSubmatch(r.Notice)
	if len(match) != 2 || r.Label != "uncertain" || r.State == "cancelled" {
		return nil
	}
	// The ordinary notice is the durable journal. If the optional cache was not
	// committed, fetch authoritative state again instead of sending the POST.
	r.Estimation = &estimationStatus{ID: match[1], State: "queued", Reason: "recovery", Version: strings.Repeat("0", 64)}
	raw, err := os.ReadFile(filepath.Join(a.dir, "estimation-status", r.ID+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var cache estimationCache
	if len(raw) > 4096 || decodeJSON(raw, &cache) != nil || !cache.Status.valid() {
		return errors.New("invalid estimation cache")
	}
	if cache.Revision == r.Revision && cache.Epoch == r.Epoch && cache.Status.ID == match[1] {
		r.Estimation = &cache.Status
	}
	return nil
}

func (e estimationStatus) valid() bool {
	return estimationID.MatchString(e.ID) && estimationVersion.MatchString(e.Version) && estimationLabels[e.State] != "" && len(e.Reason) <= 80
}
func estimationMessage(e estimationStatus) string {
	text := estimationLabels[e.State]
	if e.State == "queued" {
		switch e.Reason {
		case "external_disabled", "channel_not_allowed", "scope_not_allowed", "identifiers_or_mentions", "input_too_long":
			text += " 外部送信は許可されていません。"
		case "daily_limit", "batch_limit":
			text += " 依頼上限に達しています。"
		case "connection_failed", "relay_disabled":
			text += " 接続を利用できません。"
		}
	}
	if e.Reason == "public_synthetic_reproduction_required" {
		text = "独立検証済みですが、公開可能な合成入力での再現確認待ちです。加算しません。"
	}
	if e.State == "awaiting_reply" && e.Reason == "post_response_unknown" {
		text = "送信結果を確認中です。同じ依頼IDの状態だけを照会し、再送しません。"
	}
	if e.State == "awaiting_reply" && e.Reason == "poll_failed" {
		text = "回答の状態を確認できません。同じ依頼IDで再確認し、再送しません。"
	}
	if e.State == "unverified" && e.Reason == "independent_validation_failed" {
		text = "候補が独立検証の正解に一致しませんでした。採用・加算はしません。"
	}
	if e.State == "unverified" && e.Reason == "sandbox_failed" {
		text = "推定候補の隔離実行に失敗しました。採用・加算はしません。"
	}
	return text + "\n依頼ID: " + e.ID
}

func (a *adaptiveReviews) estimationCall(ctx context.Context, method, path string, value any) (estimationStatus, error) {
	var status estimationStatus
	var body []byte
	if value != nil {
		body, _ = json.Marshal(value)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return status, err
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := a.http.Do(req)
	if err != nil {
		return status, errors.New("estimation service unavailable")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(raw) > 4096 || response.StatusCode != 200 || decodeJSON(raw, &status) != nil || !status.valid() {
		return status, errors.New("invalid estimation response")
	}
	return status, nil
}

func (a *adaptiveReviews) refreshEstimation(ctx context.Context, s adaptiveClient, old adaptiveRecord) error {
	status, err := a.estimationCall(ctx, "GET", "/estimates/"+old.Estimation.ID, nil)
	if err != nil {
		return err
	}
	if status.ID != old.Estimation.ID {
		return errors.New("estimation identity changed")
	}
	if status.Version == old.Estimation.Version {
		return nil
	}
	a.mu.Lock()
	current := a.records[old.ID]
	if current.State != "waiting" || current.Epoch != old.Epoch || current.Revision != old.Revision || current.Estimation == nil || current.Estimation.ID != status.ID {
		a.mu.Unlock()
		return nil
	}
	current.Estimation = &status
	message := estimationMessage(status)
	if current.Notice != message {
		queueReviewNotice(&current, message)
	}
	err = a.save(current)
	a.mu.Unlock()
	if err != nil {
		return err
	}
	return a.notify(s, old.ID)
}

type estimationClient interface {
	reviewClient
}

func estimationCommand() *discordgo.ApplicationCommand {
	permissions := int64(discordgo.PermissionUseApplicationCommands)
	return &discordgo.ApplicationCommand{Name: "estimate", Description: "未知の符号化方式を推定キューへ登録・状態確認します（指定ロール専用）", Type: discordgo.ChatApplicationCommand, DefaultMemberPermissions: &permissions,
		Options: []*discordgo.ApplicationCommandOption{
			{Type: discordgo.ApplicationCommandOptionString, Name: "input", Description: "推定対象の符号列だけを指定（400文字以内）。メンションや文脈を含めないでください", MaxLength: 400},
			{Type: discordgo.ApplicationCommandOptionString, Name: "request_id", Description: "既存の推定依頼IDを指定すると現在の状態を表示します", MaxLength: 100},
		}}
}

func ensureEstimationCommand(s commandClient, app, guild string) error {
	commands, err := s.ApplicationCommands(app, guild)
	if err != nil {
		return err
	}
	desired := estimationCommand()
	if guild == "" {
		dm := false
		desired.DMPermission = &dm
		contexts := []discordgo.InteractionContextType{discordgo.InteractionContextGuild}
		desired.Contexts = &contexts
	}
	for _, command := range commands {
		if command.Name == desired.Name && command.Type == desired.Type {
			if command.Description == desired.Description && reflect.DeepEqual(command.Options, desired.Options) && reflect.DeepEqual(command.DefaultMemberPermissions, desired.DefaultMemberPermissions) && (guild != "" || reflect.DeepEqual(command.Contexts, desired.Contexts)) {
				return nil
			}
			_, err = s.ApplicationCommandEdit(app, guild, command.ID, desired)
			return err
		}
	}
	_, err = s.ApplicationCommandCreate(app, guild, desired)
	return err
}

func (a *adaptiveReviews) handleEstimation(s estimationClient, event *discordgo.InteractionCreate) (bool, error) {
	if event == nil || event.Interaction == nil || event.Type != discordgo.InteractionApplicationCommand || event.ApplicationCommandData().Name != "estimate" {
		return false, nil
	}
	i := event.Interaction
	if !canReview(i) {
		return true, reviewImmediate(s, i, "この操作は指定のレビューロール専用です。")
	}
	if a == nil || !a.detector.channels[i.ChannelID] {
		return true, reviewImmediate(s, i, "このチャンネルでは推定依頼を利用できません。")
	}
	input, id := "", ""
	for _, option := range i.ApplicationCommandData().Options {
		if option.Type != discordgo.ApplicationCommandOptionString {
			return true, reviewImmediate(s, i, "入力形式が不正です。")
		}
		switch option.Name {
		case "input":
			input = option.StringValue()
		case "request_id":
			id = option.StringValue()
		}
	}
	if (input == "") == (id == "") || utf8.RuneCountInString(input) > 400 || (id != "" && !estimationID.MatchString(id)) {
		return true, reviewImmediate(s, i, "input または request_id のどちらか一方を指定してください。")
	}
	if err := reviewDefer(s, i); err != nil {
		return true, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), a.http.Timeout)
	defer cancel()
	var status estimationStatus
	var err error
	if id != "" {
		status, err = a.estimationCall(ctx, "GET", "/estimates/"+id, nil)
	} else {
		revision := contentRevision(&discordgo.Message{Content: input})
		status, err = a.estimationCall(ctx, "POST", "/estimate", map[string]any{"text": input, "revision": revision, "channel_id": i.ChannelID, "start": 0, "end": utf8.RuneCountInString(input)})
	}
	if err != nil {
		return true, reviewEdit(s, i, "推定キューを利用できませんでした。入力や状態を確認してください。", nil, nil)
	}
	return true, reviewEdit(s, i, strings.TrimSpace(estimationMessage(status)), nil, nil)
}
