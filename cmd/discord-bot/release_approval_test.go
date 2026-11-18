package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

type adoptionFake struct {
	sent, edits, responses int
	failSend, failDM       bool
	messages               []*discordgo.Message
	last                   *discordgo.MessageSend
}

func (f *adoptionFake) UserChannelCreate(string, ...discordgo.RequestOption) (*discordgo.Channel, error) {
	if f.failDM {
		return nil, errors.New("DM denied")
	}
	return &discordgo.Channel{ID: "111111111111111111"}, nil
}
func (f *adoptionFake) ChannelMessageSendComplex(_ string, m *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.sent++
	f.last = m
	v := &discordgo.Message{ID: "222222222222222222", Author: &discordgo.User{ID: "444444444444444444"}, Content: m.Content}
	f.messages = append(f.messages, v)
	if f.failSend {
		return nil, errors.New("lost response")
	}
	return v, nil
}
func (f *adoptionFake) ChannelMessageEditComplex(*discordgo.MessageEdit, ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.edits++
	return nil, nil
}
func (f *adoptionFake) ChannelMessages(string, int, string, string, string, ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	return f.messages, nil
}
func (f *adoptionFake) InteractionRespond(*discordgo.Interaction, *discordgo.InteractionResponse, ...discordgo.RequestOption) error {
	f.responses++
	return nil
}
func adoptionSetup(t *testing.T) (*releaseApprovals, *adoptionFake, releaseRequest) {
	t.Helper()
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	dir := filepath.Join(root, "delivery")
	for _, p := range []string{dir, filepath.Join(shared, "requests"), filepath.Join(shared, "decisions"), filepath.Join(shared, "results")} {
		if e := os.MkdirAll(p, 0750); e != nil {
			t.Fatal(e)
		}
	}
	pin := releasePin{Repository: decoderRepository, Commit: strings.Repeat("a", 40), IndexSHA: strings.Repeat("b", 64)}
	r := releaseRequest{Schema: 1, ID: strings.Repeat("c", 32), Approver: productionApprover, Approval: pin, Base: pin, Created: 1000, Expires: 1000 + 7*86400, Families: []string{"synthetic"}, Bundles: 2, Checks: 10}
	if e := atomicShadowJSON(filepath.Join(shared, "requests", r.ID+".json"), r); e != nil {
		t.Fatal(e)
	}
	if e := atomicShadowJSON(filepath.Join(shared, "requests", "current.json"), map[string]string{"id": r.ID}); e != nil {
		t.Fatal(e)
	}
	if e := atomicShadowJSON(filepath.Join(shared, "results", r.ID+".json"), map[string]any{"state": "pending", "updated_at": 1000}); e != nil {
		t.Fatal(e)
	}
	return &releaseApprovals{shared: shared, dir: dir, botID: "444444444444444444", now: func() time.Time { return time.Unix(1001, 0) }}, &adoptionFake{}, r
}
func adoptionEvent(r releaseRequest) *discordgo.InteractionCreate {
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{Type: discordgo.InteractionMessageComponent, User: &discordgo.User{ID: productionApprover}, ChannelID: "111111111111111111", Message: &discordgo.Message{ID: "222222222222222222"}, Data: discordgo.MessageComponentInteractionData{CustomID: "adopt:" + r.ID + ":approve"}}}
}
func TestAdoptionOneDMAndOneDecision(t *testing.T) {
	a, s, r := adoptionSetup(t)
	if e := a.tick(s); e != nil {
		t.Fatal(e)
	}
	if e := a.tick(s); e != nil {
		t.Fatal(e)
	}
	if s.sent != 1 {
		t.Fatal(s.sent)
	}
	if !strings.Contains(s.last.Content, "本番カウント") || len(s.last.Components) != 1 {
		t.Fatal("approval impact missing")
	}
	event := adoptionEvent(r)
	if handled, e := a.handle(s, event); !handled || e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(a.shared, "decisions", r.ID+".json")
	before, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.handle(s, event); e != nil {
		t.Fatal(e)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("answer overwritten")
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0640 {
		t.Fatal(info.Mode())
	}
	if e = a.tick(s); e != nil {
		t.Fatal(e)
	}
	if s.edits != 1 {
		t.Fatal("approval not reflected")
	}
}
func TestAdoptionInvalidActorChannelMessageGuildExpiry(t *testing.T) {
	for _, kind := range []string{"actor", "channel", "message", "guild", "expiry", "superseded", "busy"} {
		t.Run(kind, func(t *testing.T) {
			a, s, r := adoptionSetup(t)
			if e := a.tick(s); e != nil {
				t.Fatal(e)
			}
			i := adoptionEvent(r)
			switch kind {
			case "actor":
				i.User.ID = "333333333333333333"
			case "channel":
				i.ChannelID = "333333333333333333"
			case "message":
				i.Message.ID = "333333333333333333"
			case "guild":
				i.GuildID = "333333333333333333"
			case "expiry":
				a.now = func() time.Time { return time.Unix(r.Expires, 0) }
			case "superseded":
				_ = atomicShadowJSON(filepath.Join(a.shared, "requests", "current.json"), map[string]string{"id": strings.Repeat("d", 32)})
			case "busy":
				a.mu.Lock()
				defer a.mu.Unlock()
			}
			if handled, e := a.handle(s, i); !handled || e != nil {
				t.Fatal(e)
			}
			if _, e := os.Stat(filepath.Join(a.shared, "decisions", r.ID+".json")); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("unauthorized receipt")
			}
		})
	}
}
func TestAdoptionLostPostReconcilesAfterRestart(t *testing.T) {
	a, s, _ := adoptionSetup(t)
	s.failSend = true
	if a.tick(s) == nil {
		t.Fatal("expected lost POST")
	}
	b := &releaseApprovals{shared: a.shared, dir: a.dir, botID: a.botID, now: a.now}
	if e := b.tick(s); e != nil {
		t.Fatal(e)
	}
	if s.sent != 1 {
		t.Fatal("duplicate DM")
	}
}
func TestAdoptionUnresolvedPostDoesNotResend(t *testing.T) {
	a, s, _ := adoptionSetup(t)
	s.failSend = true
	_ = a.tick(s)
	s.messages = nil
	if a.tick(s) == nil {
		t.Fatal("expected unresolved delivery")
	}
	if s.sent != 1 {
		t.Fatal("duplicate DM")
	}
}
func TestAdoptionDMRefusalNoFallback(t *testing.T) {
	a, s, _ := adoptionSetup(t)
	s.failDM = true
	if a.tick(s) == nil {
		t.Fatal("expected refusal")
	}
	s.failDM = false
	if e := a.tick(s); e != nil {
		t.Fatal(e)
	}
	if s.sent != 0 {
		t.Fatal("unexpected retry")
	}
}
func TestAdoptionExpiryAndCompletionEditOriginal(t *testing.T) {
	a, s, r := adoptionSetup(t)
	_ = a.tick(s)
	a.now = func() time.Time { return time.Unix(r.Expires, 0) }
	if e := a.tick(s); e != nil {
		t.Fatal(e)
	}
	if s.sent != 1 || s.edits != 1 {
		t.Fatal("expiry duplicated DM")
	}
}

func TestAdoptionChangedRequestCannotBeApproved(t *testing.T) {
	a, s, r := adoptionSetup(t)
	if e := a.tick(s); e != nil {
		t.Fatal(e)
	}
	r.Approval.Commit = strings.Repeat("e", 40)
	if e := atomicShadowJSON(filepath.Join(a.shared, "requests", r.ID+".json"), r); e != nil {
		t.Fatal(e)
	}
	if _, e := a.handle(s, adoptionEvent(r)); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(a.shared, "decisions", r.ID+".json")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("changed target approved")
	}
}
func TestAdoptionPythonReceiptAndPendingRejudge(t *testing.T) {
	if _, e := exec.LookPath("python3"); e != nil {
		t.Fatal(e)
	}
	a, s, r := adoptionSetup(t)
	r.Base.Commit = strings.Repeat("f", 40)
	if e := atomicShadowJSON(filepath.Join(a.shared, "requests", r.ID+".json"), r); e != nil {
		t.Fatal(e)
	}
	review, discord, out := reviewSetup(t)
	runReview(t, review, discord, out.Release)
	if e := a.tick(s); e != nil {
		t.Fatal(e)
	}
	if _, e := a.handle(s, adoptionEvent(r)); e != nil {
		t.Fatal(e)
	}
	root, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	code := `import sys,os
from pathlib import Path
sys.path.insert(0,str(Path(sys.argv[1])/'services/decoder'))
import approval
from adaptive.common import atomic,read
shared=Path(sys.argv[2]);state=shared.parent/'decoder-state'
r=read(shared/'requests'/(read(shared/'requests/current.json')['id']+'.json'))
atomic(state/'current/active-production.json',r['base'])
atomic(state/'publication-pushed.json',r['approval'])
approval.ci_success=lambda candidate: None
def activate(state,pin,image,expected_base):
 assert approval.current(state)==expected_base
 atomic(state/'current/active-production.json',pin)
approval.advance(state,shared,'test-image',now=1002,bot_uid=os.getuid(),activate=activate)
assert read(shared/'results'/(r['id']+'.json'))['state']=='active'
`
	if data, e := exec.Command("python3", "-c", code, root, a.shared).CombinedOutput(); e != nil {
		t.Fatalf("Python receipt failed: %v %s", e, data)
	}
	if e := a.tick(s); e != nil {
		t.Fatal(e)
	}
	var result struct {
		State   string `json:"state"`
		Updated int64  `json:"updated_at"`
	}
	raw, e := os.ReadFile(filepath.Join(a.shared, "results", r.ID+".json"))
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(raw, &result); e != nil {
		t.Fatal(e)
	}
	if result.State != "active" {
		t.Fatal("no release adoption")
	}
	out.Release = strings.Repeat("b", 64)
	out.Unknown = false
	out.Label = "mockery"
	out.Units = 2
	runReview(t, review, discord, out.Release)
	runReview(t, review, discord, out.Release)
	if s.sent != 1 || s.edits != 1 || discord.sends != 2 || discord.edits != 0 || discord.replies[0].Content != unknownMessage(reviewUser) || discord.replies[1].Content != resultMessage("mockery", reviewUser, 9) || review.detector.counts[reviewUser] != 9 {
		t.Fatal("duplicate approval, reply, or count")
	}
}
