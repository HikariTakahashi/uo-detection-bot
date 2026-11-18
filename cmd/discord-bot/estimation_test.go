package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestEstimationNoticesRecoverAndNeverCount(t *testing.T) {
	a, f, out := reviewSetup(t)
	status := estimationStatus{ID: "estimate-" + strings.Repeat("c", 64), State: "queued", Reason: "external_disabled", Version: strings.Repeat("d", 64)}
	out.Estimation = &status
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(status)
			return
		}
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || body["channel_id"] != reviewChannel {
			t.Error("channel scope missing")
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()
	a.endpoint = server.URL
	runReview(t, a, f, out.Release)
	if f.sends != 1 || !strings.Contains(f.replies[0].Content, "未送信") || !strings.Contains(f.replies[0].Content, "許可されていません") {
		t.Fatal("queued notice claims submission")
	}
	states := []string{"awaiting_reply", "unable", "validating", "unverified", "awaiting_adoption", "complete", "failed"}
	for index, state := range states {
		status.State, status.Reason, status.Version = state, "test", fmt.Sprintf("%064x", index+1)
		a = reloadReview(t, a)
		a.endpoint = server.URL
		runReview(t, a, f, out.Release)
		runReview(t, a, f, out.Release)
		if f.sends != index+2 || a.detector.counts[reviewUser] != 7 {
			t.Fatal("status replay duplicated notice or count", state, f.sends)
		}
		if !strings.Contains(f.replies[len(f.replies)-1].Content, estimationLabels[state]) {
			t.Fatal("notice does not match persisted state", state)
		}
	}
	r, _ := a.snapshot(reviewID)
	if r.Units != 0 || r.State != "waiting" || f.edits != 0 {
		t.Fatal("candidate leaked into counting path", r)
	}
}

func TestEstimationStatusIdentityCannotChange(t *testing.T) {
	a, f, out := reviewSetup(t)
	status := estimationStatus{ID: "estimate-" + strings.Repeat("c", 64), State: "queued", Reason: "test", Version: strings.Repeat("d", 64)}
	out.Estimation = &status
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			changed := status
			changed.ID = "estimate-" + strings.Repeat("e", 64)
			json.NewEncoder(w).Encode(changed)
			return
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()
	a.endpoint = server.URL
	runReview(t, a, f, out.Release)
	if a.process(context.Background(), f, reviewID, out.Release) == nil {
		t.Fatal("foreign estimation accepted")
	}
	if f.sends != 1 || a.detector.counts[reviewUser] != 7 {
		t.Fatal("foreign status was announced")
	}
}

func TestEstimationCommandExplicitInputIsScopedAndEphemeral(t *testing.T) {
	a, _, _ := reviewSetup(t)
	var received map[string]any
	status := estimationStatus{ID: "estimate-" + strings.Repeat("c", 64), State: "queued", Reason: "external_disabled", Version: strings.Repeat("d", 64)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&received)
		json.NewEncoder(w).Encode(status)
	}))
	defer server.Close()
	a.endpoint = server.URL
	event := reviewEvent(discordgo.InteractionApplicationCommand, "review")
	data := event.Interaction.Data.(discordgo.ApplicationCommandInteractionData)
	data.Name = "estimate"
	data.Options = []*discordgo.ApplicationCommandInteractionDataOption{{Name: "input", Type: discordgo.ApplicationCommandOptionString, Value: "wおう"}}
	event.Interaction.Data = data
	event.ChannelID = reviewChannel
	client := &fakeReview{}
	handled, err := a.handleEstimation(client, event)
	if err != nil || !handled {
		t.Fatal(handled, err)
	}
	if received["text"] != "wおう" || received["channel_id"] != event.ChannelID || received["start"] != float64(0) || received["end"] != float64(3) {
		t.Fatal("explicit selection lost", received)
	}
	if len(client.responses) != 1 || client.responses[0].Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
		t.Fatal("explicit response was public")
	}
	for _, key := range []string{"author", "author_id", "context", "message_id", "source"} {
		if _, ok := received[key]; ok {
			t.Fatal("unnecessary metadata forwarded", key)
		}
	}
}

func TestEstimationCommandPermissionsAndChannelBeforeHTTP(t *testing.T) {
	for _, bad := range []string{"role", "channel", "dm"} {
		t.Run(bad, func(t *testing.T) {
			a, _, _ := reviewSetup(t)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; http.Error(w, "must not call", 500) }))
			defer server.Close()
			a.endpoint = server.URL
			event := reviewEvent(discordgo.InteractionApplicationCommand, "estimate")
			event.ChannelID = reviewChannel
			event.Interaction.Data = discordgo.ApplicationCommandInteractionData{Name: "estimate", Options: []*discordgo.ApplicationCommandInteractionDataOption{{Name: "input", Type: discordgo.ApplicationCommandOptionString, Value: "wおう"}}}
			switch bad {
			case "role":
				event.Member.Roles = nil
			case "channel":
				event.ChannelID = "111111111111111111"
			case "dm":
				event.GuildID = ""
			}
			client := &fakeReview{}
			handled, err := a.handleEstimation(client, event)
			if !handled || err != nil || calls != 0 || len(client.responses) != 1 || client.responses[0].Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
				t.Fatal("permission check leaked a request", handled, err, calls)
			}
		})
	}
}

func TestEstimationUnknownPostResultDoesNotClaimDelivery(t *testing.T) {
	status := estimationStatus{ID: "estimate-" + strings.Repeat("c", 64), State: "awaiting_reply", Reason: "post_response_unknown", Version: strings.Repeat("d", 64)}
	message := estimationMessage(status)
	if strings.Contains(message, "依頼しました") || !strings.Contains(message, "送信結果を確認中") || !strings.Contains(message, "再送しません") {
		t.Fatal(message)
	}
}

func TestEstimationCacheRecoveryKeepsLegacyJSONAndNoDuplicateRequest(t *testing.T) {
	for _, checkpointFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(checkpointFailure), func(t *testing.T) {
			a, f, out := reviewSetup(t)
			status := estimationStatus{ID: "estimate-" + strings.Repeat("c", 64), State: "queued", Reason: "external_disabled", Version: strings.Repeat("d", 64)}
			out.Estimation = &status
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					json.NewEncoder(w).Encode(status)
					return
				}
				posts++
				json.NewEncoder(w).Encode(out)
			}))
			defer server.Close()
			a.endpoint = server.URL
			cacheDir := filepath.Join(a.dir, "estimation-status")
			if checkpointFailure {
				if err := os.WriteFile(cacheDir, []byte("block optional cache"), 0600); err != nil {
					t.Fatal(err)
				}
				if a.process(context.Background(), f, reviewID, out.Release) == nil {
					t.Fatal("cache checkpoint error missing")
				}
				if err := os.Remove(cacheDir); err != nil {
					t.Fatal(err)
				}
			} else {
				runReview(t, a, f, out.Release)
				if err := os.Remove(filepath.Join(cacheDir, reviewID+".json")); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := os.ReadFile(filepath.Join(a.dir, reviewID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var legacy map[string]any
			if err = json.Unmarshal(raw, &legacy); err != nil {
				t.Fatal(err)
			}
			if _, ok := legacy["estimation"]; ok {
				t.Fatal("new field broke legacy strict reader")
			}
			a = reloadReview(t, a)
			runReview(t, a, f, out.Release)
			runReview(t, a, f, out.Release)
			if posts != 1 || f.sends != 1 || a.detector.counts[reviewUser] != 7 {
				t.Fatal("recovery repeated analysis or notice", posts, f.sends)
			}
			r, _ := a.snapshot(reviewID)
			if r.Estimation == nil || r.Estimation.Version != status.Version {
				t.Fatal("authoritative state not recovered")
			}
		})
	}
}
