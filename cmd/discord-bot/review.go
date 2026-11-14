package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

const reviewDescription = "shadow収集した投稿に正解ラベルを付けます（管理者専用）"

func ensureReviewCommand(s commandClient, app, guild string) error {
	commands, err := s.ApplicationCommands(app, guild)
	if err != nil {
		return fmt.Errorf("list review commands: %s", safeError(err))
	}
	permissions := int64(discordgo.PermissionAdministrator)
	dm := false
	contexts := []discordgo.InteractionContextType{discordgo.InteractionContextGuild}
	desired := &discordgo.ApplicationCommand{Name: "review", Description: reviewDescription, Type: discordgo.ChatApplicationCommand, DefaultMemberPermissions: &permissions, Options: []*discordgo.ApplicationCommandOption{}}
	if guild == "" {
		desired.DMPermission, desired.Contexts = &dm, &contexts
	}
	for _, c := range commands {
		if c.Name != "review" || c.Type != discordgo.ChatApplicationCommand {
			continue
		}
		if c.Description == desired.Description && len(c.Options) == 0 && reflect.DeepEqual(c.DefaultMemberPermissions, desired.DefaultMemberPermissions) && (guild != "" || reflect.DeepEqual(c.Contexts, desired.Contexts)) {
			return nil
		}
		if _, err = s.ApplicationCommandEdit(app, guild, c.ID, desired); err != nil {
			return fmt.Errorf("edit /review: %s", safeError(err))
		}
		return nil
	}
	if _, err = s.ApplicationCommandCreate(app, guild, desired); err != nil {
		return fmt.Errorf("register /review: %s", safeError(err))
	}
	return nil
}

type reviewClient interface {
	InteractionRespond(*discordgo.Interaction, *discordgo.InteractionResponse, ...discordgo.RequestOption) error
	InteractionResponseEdit(*discordgo.Interaction, *discordgo.WebhookEdit, ...discordgo.RequestOption) (*discordgo.Message, error)
}

type reviewLease struct {
	MessageID, GuildID, UserID, ChannelID string
	Expires                               time.Time
}

type reviewHandler struct {
	shadow  *shadowCollector
	mu      sync.Mutex
	leases  map[string]reviewLease
	skipped map[string]map[string]time.Time
	now     func() time.Time
}

func newReview(s *shadowCollector) *reviewHandler {
	return &reviewHandler{shadow: s, leases: make(map[string]reviewLease), skipped: make(map[string]map[string]time.Time), now: time.Now}
}

func reviewAdmin(i *discordgo.Interaction) bool {
	return i != nil && isSnowflake(i.GuildID) && i.Member != nil && i.Member.User != nil && isSnowflake(i.Member.User.ID) && i.Member.Permissions&discordgo.PermissionAdministrator != 0
}

func noReviewMentions() *discordgo.MessageAllowedMentions {
	return &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}
}

func reviewImmediate(s reviewClient, i *discordgo.Interaction, text string) error {
	return s.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noReviewMentions()}})
}

func reviewDefer(s reviewClient, i *discordgo.Interaction) error {
	return s.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral}})
}

func reviewEdit(s reviewClient, i *discordgo.Interaction, text string, embeds []*discordgo.MessageEmbed, components []discordgo.MessageComponent) error {
	if embeds == nil {
		embeds = []*discordgo.MessageEmbed{}
	}
	if components == nil {
		components = []discordgo.MessageComponent{}
	}
	_, err := s.InteractionResponseEdit(i, &discordgo.WebhookEdit{Content: &text, Embeds: &embeds, Components: &components, AllowedMentions: noReviewMentions()})
	return err
}

func (h *reviewHandler) expireLocked(now time.Time) {
	for key, l := range h.leases {
		if !now.Before(l.Expires) {
			delete(h.leases, key)
		}
	}
	for user, ids := range h.skipped {
		for id, until := range ids {
			if !now.Before(until) {
				delete(ids, id)
			}
		}
		if len(ids) == 0 {
			delete(h.skipped, user)
		}
	}
}

func (h *reviewHandler) acquire(i *discordgo.Interaction) (shadowRecord, string, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.expireLocked(now)
	excluded := make(map[string]bool)
	// One active item per reviewer. Starting /review again releases their old lease.
	for key, l := range h.leases {
		if l.UserID == i.Member.User.ID && l.GuildID == i.GuildID {
			delete(h.leases, key)
		} else {
			excluded[l.MessageID] = true
		}
	}
	for id := range h.skipped[i.Member.User.ID] {
		excluded[id] = true
	}
	r, ok := h.shadow.store.next(i.GuildID, excluded, now)
	if !ok {
		return r, "", false, nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return r, "", false, err
	}
	key := hex.EncodeToString(nonce[:])
	h.leases[key] = reviewLease{MessageID: r.MessageID, GuildID: i.GuildID, UserID: i.Member.User.ID, ChannelID: i.ChannelID, Expires: now.Add(15 * time.Minute)}
	return r, key, true, nil
}

func (h *reviewHandler) leaseLocked(i *discordgo.Interaction, key string) (reviewLease, bool) {
	h.expireLocked(h.now())
	l, ok := h.leases[key]
	return l, ok && l.UserID == i.Member.User.ID && l.GuildID == i.GuildID && l.ChannelID == i.ChannelID
}

func reviewQuote(s string) string { return "```\n" + strings.ReplaceAll(s, "`", "ˋ") + "\n```" }
func reviewButtons(key string) []discordgo.MessageComponent {
	labels := []struct{ value, title string }{{"mockery", "冷笑"}, {"other", "対象外"}, {"uncertain", "判断困難"}, {"skip", "今回はスキップ"}, {"delete", "データを削除"}}
	buttons := []discordgo.MessageComponent{}
	for _, item := range labels {
		style := discordgo.SecondaryButton
		if item.value == "delete" {
			style = discordgo.DangerButton
		}
		buttons = append(buttons, discordgo.Button{Label: item.title, Style: style, CustomID: "review:" + key + ":" + item.value})
	}
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: buttons}}
}
func reviewNextButton() []discordgo.MessageComponent {
	return []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.Button{Label: "次の未レビュー", Style: discordgo.SecondaryButton, CustomID: "review:next"}}}}
}

func (h *reviewHandler) show(s reviewClient, i *discordgo.Interaction) error {
	if err := reviewDefer(s, i); err != nil {
		return err
	}
	r, key, ok, err := h.acquire(i)
	if err != nil {
		return reviewEdit(s, i, "レビュー画面を準備できませんでした。", nil, nil)
	}
	if !ok {
		return reviewEdit(s, i, "未レビューの投稿はありません。処理待ち・判定エラー・他の管理者が確認中・スキップ中の投稿は表示されません。", nil, reviewNextButton())
	}
	embeds := []*discordgo.MessageEmbed{{Title: "投稿本文", Description: reviewQuote(r.Text)}}
	if r.Context != "" {
		embeds = append(embeds, &discordgo.MessageEmbed{Title: "返信元（同じチャンネル）", Description: reviewQuote(r.Context)})
	}
	text := fmt.Sprintf("相手を見下す・嘲る使い方なら「冷笑」、単なる引用・説明・驚き・語の一部なら「対象外」、文脈不足なら「判断困難」を選んでください。AIの答えは保存後に表示します。\nチャンネル: <#%s> / 投稿ID: %s\n通知・カウントは変更されません。画面の有効期限は15分です。", r.ChannelID, r.MessageID)
	return reviewEdit(s, i, text, embeds, reviewButtons(key))
}

func (h *reviewHandler) handle(s reviewClient, event *discordgo.InteractionCreate) (bool, error) {
	if event == nil || event.Interaction == nil {
		return false, nil
	}
	i := event.Interaction
	var action, key string
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		if i.ApplicationCommandData().Name != "review" {
			return false, nil
		}
		action = "next"
	case discordgo.InteractionMessageComponent:
		id := i.MessageComponentData().CustomID
		if !strings.HasPrefix(id, "review:") {
			return false, nil
		}
		if id == "review:next" {
			action = "next"
		} else {
			p := strings.Split(id, ":")
			if len(p) == 3 {
				key, action = p[1], p[2]
			}
		}
	case discordgo.InteractionModalSubmit:
		id := i.ModalSubmitData().CustomID
		if !strings.HasPrefix(id, "review-submit:") {
			return false, nil
		}
		p := strings.Split(id, ":")
		if len(p) == 3 {
			key, action = p[1], p[2]
		}
	default:
		return false, nil
	}
	// Command defaults are not sufficient: re-check every component and modal.
	if !reviewAdmin(i) {
		return true, reviewImmediate(s, i, "この操作はサーバーの管理者だけが利用できます。")
	}
	if h.shadow == nil {
		return true, reviewImmediate(s, i, "shadow保存先を利用できません。管理者がBotのログを確認してください。")
	}
	if action == "next" {
		return true, h.show(s, i)
	}
	h.mu.Lock()
	lease, ok := h.leaseLocked(i, key)
	h.mu.Unlock()
	if !ok {
		return true, reviewImmediate(s, i, "この画面は期限切れ、または別の利用者の画面です。/review を開き直してください。")
	}
	if i.Type == discordgo.InteractionMessageComponent && validShadowLabel(action) {
		return true, s.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseModal, Data: &discordgo.InteractionResponseData{Title: "正解ラベルを保存", CustomID: "review-submit:" + key + ":" + action, Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{discordgo.TextInput{CustomID: "reason", Label: "理由（任意・200文字まで）", Style: discordgo.TextInputParagraph, Required: false, MaxLength: 200}}}}}})
	}
	if err := reviewDefer(s, i); err != nil {
		return true, err
	}
	if action == "skip" && i.Type == discordgo.InteractionMessageComponent {
		h.mu.Lock()
		if _, ok := h.leaseLocked(i, key); !ok {
			h.mu.Unlock()
			return true, reviewEdit(s, i, "この画面は期限切れです。", nil, nil)
		}
		delete(h.leases, key)
		if h.skipped[i.Member.User.ID] == nil {
			h.skipped[i.Member.User.ID] = make(map[string]time.Time)
		}
		h.skipped[i.Member.User.ID][lease.MessageID] = h.now().Add(15 * time.Minute)
		h.mu.Unlock()
		return true, reviewEdit(s, i, "この投稿を15分間スキップしました。", nil, reviewNextButton())
	}
	if action == "delete" && i.Type == discordgo.InteractionMessageComponent {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.leaseLocked(i, key); !ok {
			return true, reviewEdit(s, i, "この画面は期限切れです。", nil, nil)
		}
		if err := h.shadow.remove(lease.MessageID, i.GuildID); err != nil {
			return true, reviewEdit(s, i, "削除に失敗しました。保存先を確認してください。", nil, nil)
		}
		delete(h.leases, key)
		return true, reviewEdit(s, i, "評価データを削除しました。Discordの投稿やカウントは変更していません。", nil, reviewNextButton())
	}
	if i.Type != discordgo.InteractionModalSubmit || !validShadowLabel(action) {
		return true, reviewEdit(s, i, "操作が不正です。/review を開き直してください。", nil, nil)
	}
	reason, err := reviewReason(i.ModalSubmitData())
	if err != nil {
		return true, reviewEdit(s, i, "理由の形式が不正です。", nil, nil)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.leaseLocked(i, key); !ok {
		return true, reviewEdit(s, i, "この画面は期限切れです。", nil, nil)
	}
	r, err := h.shadow.store.label(lease.MessageID, i.GuildID, i.Member.User.ID, action, reason, h.now())
	if err != nil {
		return true, reviewEdit(s, i, "保存できませんでした。期限切れ・削除済み・レビュー済み、または保存エラーです。", nil, nil)
	}
	delete(h.leases, key)
	titles := map[string]string{"mockery": "冷笑", "other": "対象外", "uncertain": "判断困難"}
	text := fmt.Sprintf("正解「%s」を保存しました。\nAIの判定: %s\n通知・カウントは変更していません。", titles[action], titles[r.Prediction.Label])
	return true, reviewEdit(s, i, text, nil, reviewNextButton())
}

func reviewReason(data discordgo.ModalSubmitInteractionData) (string, error) {
	if len(data.Components) != 1 {
		return "", errors.New("invalid modal")
	}
	row, ok := data.Components[0].(*discordgo.ActionsRow)
	if !ok || len(row.Components) != 1 {
		return "", errors.New("invalid modal row")
	}
	input, ok := row.Components[0].(*discordgo.TextInput)
	if !ok || input.CustomID != "reason" || utf8.RuneCountInString(input.Value) > 200 {
		return "", errors.New("invalid reason")
	}
	return strings.TrimSpace(input.Value), nil
}
