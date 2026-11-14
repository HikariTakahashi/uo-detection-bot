package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type fakeReview struct {
	responses []*discordgo.InteractionResponse
	edits     []*discordgo.WebhookEdit
}

func (f *fakeReview) InteractionRespond(_ *discordgo.Interaction, r *discordgo.InteractionResponse, _ ...discordgo.RequestOption) error {
	f.responses = append(f.responses, r)
	return nil
}
func (f *fakeReview) InteractionResponseEdit(_ *discordgo.Interaction, r *discordgo.WebhookEdit, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.edits = append(f.edits, r)
	return nil, nil
}
func reviewEvent(kind discordgo.InteractionType, id string) *discordgo.InteractionCreate {
	i := &discordgo.Interaction{GuildID: "555555555555555555", ChannelID: testChannel, Member: &discordgo.Member{User: &discordgo.User{ID: testUser}, Roles: []string{"1023596331392053388"}}, Type: kind}
	switch kind {
	case discordgo.InteractionApplicationCommand:
		i.Data = discordgo.ApplicationCommandInteractionData{Name: id}
	case discordgo.InteractionMessageComponent:
		i.Data = discordgo.MessageComponentInteractionData{CustomID: id}
	case discordgo.InteractionModalSubmit:
		i.Data = discordgo.ModalSubmitInteractionData{CustomID: id, Components: []discordgo.MessageComponent{&discordgo.ActionsRow{Components: []discordgo.MessageComponent{&discordgo.TextInput{CustomID: "reason", Value: "引用なので対象外"}}}}}
	}
	return &discordgo.InteractionCreate{Interaction: i}
}
func callReview(t *testing.T, h *reviewHandler, i *discordgo.InteractionCreate) *fakeReview {
	t.Helper()
	f := &fakeReview{}
	handled, err := h.handle(f, i)
	if !handled || err != nil {
		t.Fatal(handled, err)
	}
	for _, r := range f.responses {
		if r.Type != discordgo.InteractionResponseModal && r.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
			t.Fatal("public response")
		}
	}
	for _, e := range f.edits {
		if e.AllowedMentions == nil || len(e.AllowedMentions.Parse) > 0 {
			t.Fatal("mentions allowed")
		}
	}
	return f
}
func beginReview(t *testing.T, h *reviewHandler) string {
	t.Helper()
	f := callReview(t, h, reviewEvent(discordgo.InteractionApplicationCommand, "review"))
	raw, _ := json.Marshal(f.edits)
	if strings.Contains(string(raw), "AIの判定:") || strings.Contains(string(raw), "0.7") {
		t.Fatal("prediction revealed before review")
	}
	for key := range h.leases {
		return key
	}
	t.Fatal("no lease")
	return ""
}
func TestReviewPermissionsAndBinding(t *testing.T) {
	for _, kind := range []discordgo.InteractionType{discordgo.InteractionApplicationCommand, discordgo.InteractionMessageComponent, discordgo.InteractionModalSubmit} {
		for _, bad := range []string{"norole", "adminwithoutrole", "dm", "otheruser", "otherguild", "otherchannel", "expired"} {
			t.Run(string(rune(kind))+bad, func(t *testing.T) {
				s := testShadow(t)
				r := sampleRecord()
				if err := s.store.add(r, time.Now()); err != nil {
					t.Fatal(err)
				}
				h := newReview(s)
				key := beginReview(t, h)
				id := "review"
				if kind == discordgo.InteractionMessageComponent {
					id = "review:" + key + ":other"
				}
				if kind == discordgo.InteractionModalSubmit {
					id = "review-submit:" + key + ":other"
				}
				i := reviewEvent(kind, id)
				switch bad {
				case "norole":
					i.Member.Roles = nil
				case "adminwithoutrole":
					i.Member.Roles = []string{"1023596331392053389"}
					i.Member.Permissions = discordgo.PermissionAdministrator
				case "dm":
					i.GuildID = ""
				case "otheruser":
					i.Member.User.ID = testUser2
				case "otherguild":
					i.GuildID = testUser2
				case "otherchannel":
					i.ChannelID = testThread
				case "expired":
					h.now = func() time.Time { return time.Now().Add(time.Hour) }
				}
				f := callReview(t, h, i)
				if s.store.records[r.MessageID].HumanLabel != "" {
					t.Fatal("unauthorized mutation")
				}
				if kind != discordgo.InteractionApplicationCommand || bad == "norole" || bad == "adminwithoutrole" || bad == "dm" {
					if len(f.edits) != 0 || f.responses[0].Type == discordgo.InteractionResponseModal {
						t.Fatal("invalid access accepted")
					}
				}
			})
		}
	}
}
func TestReviewBlindSaveAndReplay(t *testing.T) {
	s := testShadow(t)
	r := sampleRecord()
	r.Text = "引用: うお @everyone ```"
	if err := s.store.add(r, time.Now()); err != nil {
		t.Fatal(err)
	}
	h := newReview(s)
	key := beginReview(t, h)
	f := callReview(t, h, reviewEvent(discordgo.InteractionMessageComponent, "review:"+key+":other"))
	if f.responses[0].Type != discordgo.InteractionResponseModal || s.store.records[r.MessageID].HumanLabel != "" {
		t.Fatal("modal changed label early")
	}
	f = callReview(t, h, reviewEvent(discordgo.InteractionModalSubmit, "review-submit:"+key+":other"))
	saved := s.store.records[r.MessageID]
	if saved.HumanLabel != "other" || saved.Reason != "引用なので対象外" || saved.ReviewerID != testUser {
		t.Fatal(saved)
	}
	if !strings.Contains(*f.edits[0].Content, "AIの判定: 冷笑") {
		t.Fatal("no post-save prediction")
	}
	callReview(t, h, reviewEvent(discordgo.InteractionModalSubmit, "review-submit:"+key+":mockery"))
	if s.store.records[r.MessageID].HumanLabel != "other" {
		t.Fatal("replay overwrote label")
	}
}

func TestReviewConcurrentSkipDeleteAndMissing(t *testing.T) {
	s := testShadow(t)
	r := sampleRecord()
	if err := s.store.add(r, time.Now()); err != nil {
		t.Fatal(err)
	}
	h := newReview(s)
	key := beginReview(t, h)
	other := reviewEvent(discordgo.InteractionApplicationCommand, "review")
	other.Member.User.ID = testUser2
	f := callReview(t, h, other)
	if !strings.Contains(*f.edits[0].Content, "未レビューの投稿はありません") {
		t.Fatal("double lease")
	}
	callReview(t, h, reviewEvent(discordgo.InteractionMessageComponent, "review:"+key+":skip"))
	if !s.store.has(r.MessageID) || len(h.skipped[testUser]) != 1 {
		t.Fatal("skip deleted data")
	}
	callReview(t, h, other)
	for k := range h.leases {
		key = k
	}
	del := reviewEvent(discordgo.InteractionMessageComponent, "review:"+key+":delete")
	del.Member.User.ID = testUser2
	callReview(t, h, del)
	if s.store.has(r.MessageID) {
		t.Fatal("delete failed")
	}
	callReview(t, newReview(nil), reviewEvent(discordgo.InteractionApplicationCommand, "review"))
}

type captureReviewCommand struct {
	fakeCommands
	desired *discordgo.ApplicationCommand
}

func (f *captureReviewCommand) ApplicationCommandCreate(_ string, _ string, c *discordgo.ApplicationCommand, _ ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error) {
	f.desired = c
	return c, nil
}
func (f *captureReviewCommand) ApplicationCommandEdit(app, guild, id string, c *discordgo.ApplicationCommand, opts ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error) {
	f.desired = c
	return f.fakeCommands.ApplicationCommandEdit(app, guild, id, c, opts...)
}
func TestReviewRegistration(t *testing.T) {
	f := &captureReviewCommand{}
	f.commands = []*discordgo.ApplicationCommand{{Name: "ping", Type: discordgo.ChatApplicationCommand}}
	if err := ensureReviewCommand(f, "app", ""); err != nil {
		t.Fatal(err)
	}
	c := f.desired
	if c.Name != "review" || c.DefaultMemberPermissions == nil || *c.DefaultMemberPermissions != discordgo.PermissionUseApplicationCommands || c.Contexts == nil || len(*c.Contexts) != 1 || (*c.Contexts)[0] != discordgo.InteractionContextGuild {
		t.Fatal(c)
	}
	if f.edited != 0 {
		t.Fatal("modified ping")
	}
	f.commands = append(f.commands, c)
	f.desired = nil
	if err := ensureReviewCommand(f, "app", ""); err != nil || f.desired != nil || f.edited != 0 {
		t.Fatal("non-idempotent registration", err)
	}
	guild := &captureReviewCommand{}
	if err := ensureReviewCommand(guild, "app", "guild"); err != nil {
		t.Fatal(err)
	}
	if guild.desired.Contexts != nil || guild.desired.DMPermission != nil {
		t.Fatal("global-only fields on guild command")
	}
	// An existing registration must receive a new mask, not an omitted permissions field.
	old := *c
	old.ID = "review-id"
	admin := int64(discordgo.PermissionAdministrator)
	old.DefaultMemberPermissions = &admin
	f.commands[1] = &old
	if err := ensureReviewCommand(f, "app", ""); err != nil {
		t.Fatal(err)
	}
	if f.edited != 1 || f.editedID != old.ID || f.desired == nil || f.desired.DefaultMemberPermissions == nil || *f.desired.DefaultMemberPermissions != discordgo.PermissionUseApplicationCommands {
		t.Fatal("existing administrator-only registration was not migrated")
	}
}
