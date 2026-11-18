package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bwmarrin/discordgo"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const reviewID = "123456789012345678"
const reviewUser = "234567890123456789"
const reviewChannel = "345678901234567890"
const reviewGuild = "456789012345678901"

type reviewFake struct {
	source                      *discordgo.Message
	replies                     []*discordgo.Message
	sends, edits, deletes       int
	failSend, failEdit, missing bool
	onFetch                     func(int)
	fetches                     int
}

func (f *reviewFake) ChannelMessage(c, id string, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.fetches++
	if f.onFetch != nil {
		f.onFetch(f.fetches)
	}
	if f.missing {
		return nil, &discordgo.RESTError{Message: &discordgo.APIErrorMessage{Code: 10008}}
	}
	m := *f.source
	return &m, nil
}
func (f *reviewFake) ChannelMessageSendComplex(c string, m *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.sends++
	r := &discordgo.Message{ID: fmt.Sprint(567890123456789011 + f.sends), Content: m.Content, Author: &discordgo.User{ID: "999999999999999999"}, MessageReference: m.Reference}
	f.replies = append(f.replies, r)
	if f.failSend {
		return nil, errors.New("response lost")
	}
	return r, nil
}
func (f *reviewFake) ChannelMessageEditComplex(m *discordgo.MessageEdit, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	f.edits++
	if f.failEdit {
		return nil, errors.New("edit failed")
	}
	f.replies[0].Content = *m.Content
	return f.replies[0], nil
}
func (f *reviewFake) ChannelMessageDelete(c, id string, _ ...discordgo.RequestOption) error {
	f.deletes++
	return nil
}
func (f *reviewFake) ChannelMessages(string, int, string, string, string, ...discordgo.RequestOption) ([]*discordgo.Message, error) {
	out := append([]*discordgo.Message(nil), f.replies...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
func reviewSetup(t *testing.T) (*adaptiveReviews, *reviewFake, *adaptiveResult) {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	os.WriteFile(config, []byte(`{"enabled":true}`), 0600)
	d := &detector{counts: map[string]uint64{reviewUser: 7}, aiCounted: map[string]bool{}, reviewReceipts: map[string]reviewReceipt{}, channels: map[string]bool{reviewChannel: true}, words: map[string]bool{"うお": true}, statePath: filepath.Join(dir, "counts.json")}
	a, e := newAdaptive(config, filepath.Join(dir, "review"), d)
	if e != nil {
		t.Fatal(e)
	}
	a.botID = "999999999999999999"
	f := &reviewFake{source: &discordgo.Message{ID: reviewID, ChannelID: reviewChannel, GuildID: reviewGuild, Content: "novel:encoded", Author: &discordgo.User{ID: reviewUser}}}
	out := &adaptiveResult{Revision: contentRevision(f.source), Release: strings.Repeat("a", 64), Unknown: true, Label: "uncertain"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(out) }))
	t.Cleanup(server.Close)
	a.endpoint = server.URL
	if owned, e := a.enqueue(&discordgo.MessageCreate{Message: f.source}); !owned || e != nil {
		t.Fatalf("enqueue %v %v", owned, e)
	}
	return a, f, out
}
func runReview(t *testing.T, a *adaptiveReviews, f *reviewFake, rev string) {
	t.Helper()
	if e := a.process(context.Background(), f, reviewID, rev); e != nil {
		t.Fatal(e)
	}
}
func TestAdaptivePendingReleaseExactlyOnce(t *testing.T) {
	a, f, out := reviewSetup(t)
	runReview(t, a, f, out.Release)
	if f.sends != 1 || a.detector.counts[reviewUser] != 7 || f.replies[0].Content != unknownMessage(reviewUser) {
		t.Fatal("pending must not count")
	}
	runReview(t, a, f, out.Release)
	if f.sends != 1 {
		t.Fatal("duplicate reply")
	}
	out.Release = strings.Repeat("b", 64)
	out.Unknown = false
	out.Label = "mockery"
	out.Units = 2
	runReview(t, a, f, out.Release)
	runReview(t, a, f, out.Release)
	if f.sends != 2 || f.edits != 0 || a.detector.counts[reviewUser] != 9 || f.replies[0].Content != unknownMessage(reviewUser) || f.replies[1].Content != resultMessage("mockery", reviewUser, 9) {
		t.Fatal("wrong final count/notice")
	}
	state, e := loadDetectionState(a.detector.statePath)
	if e != nil {
		t.Fatal(e)
	}
	a.detector.counts = state.Counts
	a.detector.reviewReceipts = state.ReviewReceipts
	total, e := a.detector.applyReview(reviewID, reviewUser, out.Revision, 2)
	if e != nil || total != 9 || a.detector.counts[reviewUser] != 9 {
		t.Fatal("restart duplicate")
	}
}
func TestAdaptiveAmbiguousSendReconciles(t *testing.T) {
	a, f, out := reviewSetup(t)
	f.failSend = true
	if a.process(context.Background(), f, reviewID, out.Release) == nil {
		t.Fatal("expected lost response")
	}
	f.failSend = false
	runReview(t, a, f, out.Release)
	if f.sends != 1 || a.records[reviewID].ReplyID == "" {
		t.Fatal("must find existing reply")
	}
}
func TestAdaptiveResultSendFailureDoesNotRecount(t *testing.T) {
	a, f, out := reviewSetup(t)
	runReview(t, a, f, out.Release)
	out.Release = strings.Repeat("b", 64)
	out.Unknown = false
	out.Label = "mockery"
	out.Units = 1
	f.failSend = true
	if a.process(context.Background(), f, reviewID, out.Release) == nil {
		t.Fatal("expected send failure")
	}
	f.failSend = false
	runReview(t, a, f, out.Release)
	if a.detector.counts[reviewUser] != 8 || f.sends != 2 || f.edits != 0 {
		t.Fatal("duplicate count or reply")
	}
}
func TestAdaptiveEditedDuringInferenceIsNotApplied(t *testing.T) {
	a, f, out := reviewSetup(t)
	out.Unknown = false
	out.Label = "mockery"
	out.Units = 1
	f.onFetch = func(n int) {
		if n == 2 {
			f.source.Content = "ありがとう"
			now := time.Now()
			f.source.EditedTimestamp = &now
		}
	}
	runReview(t, a, f, out.Release)
	if a.detector.counts[reviewUser] != 7 || f.sends != 0 {
		t.Fatal("stale applied")
	}
}
func TestAdaptiveDeleteCancelsAndDeletesReply(t *testing.T) {
	a, f, out := reviewSetup(t)
	runReview(t, a, f, out.Release)
	if e := a.remove(reviewID, reviewGuild); e != nil {
		t.Fatal(e)
	}
	runReview(t, a, f, out.Release)
	if f.deletes != 1 || a.records[reviewID].Text != "" || a.detector.counts[reviewUser] != 7 {
		t.Fatal("not cancelled")
	}
}
func TestAdaptiveOtherAndHoldRelease(t *testing.T) {
	a, f, out := reviewSetup(t)
	runReview(t, a, f, out.Release)
	out.Release = strings.Repeat("b", 64)
	out.Unknown = false
	out.Label = "uncertain"
	runReview(t, a, f, out.Release)
	if a.records[reviewID].State != "waiting" || f.replies[1].Content != resultMessage("uncertain", reviewUser, 7) {
		t.Fatal("hold not retained")
	}
	out.Release = strings.Repeat("c", 64)
	out.Label = "other"
	runReview(t, a, f, out.Release)
	if a.records[reviewID].State != "done" || a.detector.counts[reviewUser] != 7 || f.sends != 3 || f.edits != 0 {
		t.Fatal("other wrong")
	}
}
func TestAdaptiveReceiptWriteFailureRollsBack(t *testing.T) {
	a, _, out := reviewSetup(t)
	a.detector.statePath = filepath.Join(a.detector.statePath, "invalid")
	os.WriteFile(filepath.Dir(a.detector.statePath), []byte("x"), 0600)
	_, e := a.detector.applyReview(reviewID, reviewUser, out.Revision, 1)
	if e == nil || a.detector.counts[reviewUser] != 7 || len(a.detector.reviewReceipts) != 0 {
		t.Fatal("non-atomic failed write")
	}
}
func TestAdaptiveNewRevisionReplacesContribution(t *testing.T) {
	a, _, out := reviewSetup(t)
	a.detector.applyReview(reviewID, reviewUser, out.Revision, 2)
	total, e := a.detector.applyReview(reviewID, reviewUser, strings.Repeat("f", 64), 1)
	if e != nil || total != 8 {
		t.Fatal("not a replacement")
	}
}

func TestAdaptiveReceiptSurvivesReviewCheckpointFailure(t *testing.T) {
	a, f, out := reviewSetup(t)
	runReview(t, a, f, out.Release)
	out.Release = strings.Repeat("b", 64)
	out.Unknown = false
	out.Label = "mockery"
	out.Units = 1
	original := a.dir
	a.dir = filepath.Join(t.TempDir(), "missing")
	if a.process(context.Background(), f, reviewID, out.Release) == nil {
		t.Fatal("expected record checkpoint failure")
	}
	if a.detector.counts[reviewUser] != 8 {
		t.Fatal("count must be committed")
	}
	a.dir = original
	runReview(t, a, f, out.Release)
	if a.detector.counts[reviewUser] != 8 || f.sends != 2 || f.edits != 0 {
		t.Fatal("replayed checkpoint added again")
	}
}
func TestAdaptiveDeletedWhileClassifyingDoesNotCount(t *testing.T) {
	a, f, out := reviewSetup(t)
	out.Unknown = false
	out.Label = "mockery"
	out.Units = 1
	f.onFetch = func(n int) {
		if n == 2 {
			a.remove(reviewID, reviewGuild)
		}
	}
	runReview(t, a, f, out.Release)
	if a.detector.counts[reviewUser] != 7 || f.sends != 0 {
		t.Fatal("deleted message counted")
	}
}
func TestAdaptivePartialEditInvalidatesInFlightResult(t *testing.T) {
	a, f, out := reviewSetup(t)
	out.Unknown = false
	out.Label = "mockery"
	out.Units = 1
	f.onFetch = func(n int) {
		if n == 2 {
			a.edit(&discordgo.MessageUpdate{Message: &discordgo.Message{ID: reviewID}})
		}
	}
	runReview(t, a, f, out.Release)
	if a.detector.counts[reviewUser] != 7 || f.sends != 0 {
		t.Fatal("edit event lost")
	}
}
func TestAdaptiveSchemaRequiresReceiptVersion(t *testing.T) {
	a, _, out := reviewSetup(t)
	a.detector.applyReview(reviewID, reviewUser, out.Revision, 1)
	// A legacy direct count must preserve the receipt, including after restart.
	if e := saveDetectionState(a.detector.statePath, a.detector.counts, a.detector.aiCounted, a.detector.reviewReceipts); e != nil {
		t.Fatal(e)
	}
	state, e := loadDetectionState(a.detector.statePath)
	if e != nil || state.ReviewReceipts[reviewID].Total != 8 {
		t.Fatal("receipt lost")
	}
}

func TestAdaptiveCancellationAfterFailedCheckpointWithdrawsReceipt(t *testing.T) {
	a, f, out := reviewSetup(t)
	runReview(t, a, f, out.Release)
	out.Release = strings.Repeat("b", 64)
	out.Unknown = false
	out.Label = "mockery"
	out.Units = 1
	original := a.dir
	a.dir = filepath.Join(t.TempDir(), "missing")
	if a.process(context.Background(), f, reviewID, out.Release) == nil {
		t.Fatal("expected checkpoint failure")
	}
	a.dir = original
	if err := a.remove(reviewID, reviewGuild); err != nil {
		t.Fatal(err)
	}
	runReview(t, a, f, out.Release)
	if a.detector.counts[reviewUser] != 7 || a.records[reviewID].CancelPending {
		t.Fatal("cancelled pending count survived")
	}
}

func reloadReview(t *testing.T, a *adaptiveReviews) *adaptiveReviews {
	t.Helper()
	config := filepath.Join(filepath.Dir(a.dir), "config.json")
	b, err := newAdaptive(config, a.dir, a.detector)
	if err != nil {
		t.Fatal(err)
	}
	b.endpoint, b.botID = a.endpoint, a.botID
	return b
}

func TestAdaptiveRepeatedHoldRecoveryUsesLatestNotification(t *testing.T) {
	a, f, out := reviewSetup(t)
	runReview(t, a, f, out.Release)
	out.Release = strings.Repeat("b", 64)
	runReview(t, a, f, out.Release)
	previous := a.records[reviewID].ReplyID
	out.Release = strings.Repeat("c", 64)
	f.failSend = true
	if a.process(context.Background(), f, reviewID, out.Release) == nil {
		t.Fatal("expected lost response")
	}
	a = reloadReview(t, a)
	f.failSend = false
	runReview(t, a, f, out.Release)
	if f.sends != 3 || f.edits != 0 || a.records[reviewID].ReplyID == previous || a.records[reviewID].ReplyID != f.replies[2].ID {
		t.Fatal("matched an earlier identical hold")
	}
	a = reloadReview(t, a)
	runReview(t, a, f, out.Release)
	if f.sends != 3 || a.detector.counts[reviewUser] != 7 {
		t.Fatal("restart duplicated notification/count")
	}
	if err := a.remove(reviewID, reviewGuild); err != nil {
		t.Fatal(err)
	}
	runReview(t, a, f, out.Release)
	if f.deletes != 3 || len(a.records[reviewID].ReplyHistory) != 0 {
		t.Fatal("cancellation left notices")
	}
}

func TestAdaptiveMissingAmbiguousResultDoesNotReusePendingOrResend(t *testing.T) {
	a, f, out := reviewSetup(t)
	runReview(t, a, f, out.Release)
	out.Release = strings.Repeat("b", 64)
	out.Unknown = false
	out.Label = "mockery"
	out.Units = 1
	f.failSend = true
	if a.process(context.Background(), f, reviewID, out.Release) == nil {
		t.Fatal("expected lost response")
	}
	f.replies = f.replies[:1] // Server did not retain the attempted result.
	f.failSend = false
	a = reloadReview(t, a)
	if a.process(context.Background(), f, reviewID, out.Release) == nil {
		t.Fatal("uncertain delivery must remain blocked")
	}
	if f.sends != 2 || a.detector.counts[reviewUser] != 8 || !a.records[reviewID].NoticeDirty {
		t.Fatal("blind retry or duplicate count")
	}
}

func TestAdaptiveLegacyDeliveredNoticeNotReplayed(t *testing.T) {
	a, f, out := reviewSetup(t)
	runReview(t, a, f, out.Release)
	r := a.records[reviewID]
	r.NoticeVersion = 0
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	a = reloadReview(t, a)
	runReview(t, a, f, out.Release)
	if f.sends != 1 {
		t.Fatal("legacy notice replayed")
	}
	out.Release = strings.Repeat("b", 64)
	out.Unknown = false
	out.Label = "other"
	runReview(t, a, f, out.Release)
	if f.sends != 2 || f.edits != 0 || f.replies[0].Content != unknownMessage(reviewUser) {
		t.Fatal("legacy pending was edited")
	}
}

func TestAdaptiveLegacyUnfinishedEditBecomesNewReply(t *testing.T) {
	a, f, out := reviewSetup(t)
	runReview(t, a, f, out.Release)
	r := a.records[reviewID]
	r.NoticeVersion = 0
	r.Notice = resultMessage("other", reviewUser, 7)
	r.NoticeDirty = true
	r.State = "done"
	r.Label = "other"
	if err := a.save(r); err != nil {
		t.Fatal(err)
	}
	a = reloadReview(t, a)
	runReview(t, a, f, out.Release)
	a = reloadReview(t, a)
	runReview(t, a, f, out.Release)
	if f.sends != 2 || f.edits != 0 || f.replies[0].Content != unknownMessage(reviewUser) || f.replies[1].Content != r.Notice {
		t.Fatal("legacy unfinished edit was not delivered once")
	}
}

func TestAdaptiveEditedPostGetsNewResultWithoutDuplicatingContribution(t *testing.T) {
	a, f, out := reviewSetup(t)
	runReview(t, a, f, out.Release)
	out.Release = strings.Repeat("b", 64)
	out.Unknown = false
	out.Label = "mockery"
	out.Units = 2
	runReview(t, a, f, out.Release)
	oldNotice := f.replies[1].Content
	f.source.Content = "novel:edited"
	now := time.Now()
	f.source.EditedTimestamp = &now
	out.Revision = contentRevision(f.source)
	out.Units = 1
	if err := a.edit(&discordgo.MessageUpdate{Message: f.source}); err != nil {
		t.Fatal(err)
	}
	runReview(t, a, f, out.Release)
	a = reloadReview(t, a)
	runReview(t, a, f, out.Release)
	if f.sends != 3 || f.edits != 0 || a.detector.counts[reviewUser] != 8 || f.replies[1].Content != oldNotice || f.replies[2].Content != resultMessage("mockery", reviewUser, 8) {
		t.Fatal("edited post was not separately announced with replacement count")
	}
}
