package main

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
)

const (
	testUser    = "111111111111111111"
	testUser2   = "222222222222222222"
	testChannel = "333333333333333333"
	testThread  = "444444444444444444"
)

type fakeMessages struct {
	messages []*discordgo.MessageSend
	channels []string
	fail     error
	before   func(*discordgo.MessageSend)
}

func (f *fakeMessages) ChannelMessageSendComplex(channel string, message *discordgo.MessageSend, _ ...discordgo.RequestOption) (*discordgo.Message, error) {
	if f.before != nil {
		f.before(message)
	}
	f.channels = append(f.channels, channel)
	f.messages = append(f.messages, message)
	return nil, f.fail
}

func writeTestFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}

func testDetector(t *testing.T) *detector {
	t.Helper()
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	writeTestFile(t, config, `{"words":["うお","どわー"],"channel_ids":["`+testChannel+`","`+testThread+`"]}`)
	d, err := newDetector(config, filepath.Join(dir, "state", "counts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func testMessage(word string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{
		ID: "999999999999999999", GuildID: "555555555555555555", ChannelID: testChannel,
		Content: word, Author: &discordgo.User{ID: testUser},
	}}
}

func loadCounts(path string) (map[string]uint64, error) {
	state, err := loadDetectionState(path)
	return state.Counts, err
}

func TestDetectionConfig(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		valid      bool
	}{
		{"enabled", `{"words":["うお","どわー"],"channel_ids":["` + testChannel + `"]}`, true},
		{"disabled", `{"words":["うお"],"channel_ids":[]}`, true},
		{"malformed", `{`, false},
		{"null", `null`, false},
		{"missing fields", `{}`, false},
		{"empty words", `{"words":[],"channel_ids":[]}`, false},
		{"empty word", `{"words":[""],"channel_ids":[]}`, false},
		{"null word", `{"words":[null],"channel_ids":[]}`, false},
		{"missing channels", `{"words":["うお"]}`, false},
		{"null channels", `{"words":["うお"],"channel_ids":null}`, false},
		{"bad channel", `{"words":["うお"],"channel_ids":["invalid"]}`, false},
		{"numeric channel", `{"words":["うお"],"channel_ids":[333333333333333333]}`, false},
		{"unknown field", `{"words":["うお"],"channel_ids":[],"channels":[]}`, false},
		{"trailing JSON", `{"words":["うお"],"channel_ids":[]} {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			writeTestFile(t, path, tc.data)
			_, err := loadDetectionConfig(path)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
	for _, missing := range []bool{true, false} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.json")
		if !missing {
			writeTestFile(t, path, `{"words":["うお"],"channel_ids":[]}`)
		}
		d, err := newDetector(path, filepath.Join(dir, "counts.json"))
		if err != nil || d.enabled() {
			t.Fatalf("expected disabled detection: %v", err)
		}
	}
	if _, err := loadDetectionConfig(t.TempDir()); err == nil {
		t.Fatal("read error was ignored")
	}
}

func TestDetectionExactMatchAndScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func(*discordgo.MessageCreate)
		match  bool
	}{
		{"uo", func(m *discordgo.MessageCreate) {}, true},
		{"dowa", func(m *discordgo.MessageCreate) { m.Content = "どわー" }, true},
		{"punctuation", func(m *discordgo.MessageCreate) { m.Content = "うお！" }, false},
		{"leading space", func(m *discordgo.MessageCreate) { m.Content = " うお" }, false},
		{"trailing space", func(m *discordgo.MessageCreate) { m.Content = "うお " }, false},
		{"newline", func(m *discordgo.MessageCreate) { m.Content = "うお\n" }, false},
		{"substring", func(m *discordgo.MessageCreate) { m.Content = "これはうおです" }, false},
		{"empty", func(m *discordgo.MessageCreate) { m.Content = "" }, false},
		{"unlisted channel or thread", func(m *discordgo.MessageCreate) { m.ChannelID = "666666666666666666" }, false},
		{"listed thread", func(m *discordgo.MessageCreate) { m.ChannelID = testThread }, true},
		{"reply", func(m *discordgo.MessageCreate) {
			m.Type = discordgo.MessageTypeReply
			m.MessageReference = &discordgo.MessageReference{MessageID: "101010101010101010", ChannelID: m.ChannelID}
		}, true},
		{"bot", func(m *discordgo.MessageCreate) { m.Author.Bot = true }, false},
		{"webhook", func(m *discordgo.MessageCreate) { m.WebhookID = "777777777777777777" }, false},
		{"DM", func(m *discordgo.MessageCreate) { m.GuildID = "" }, false},
		{"missing author", func(m *discordgo.MessageCreate) { m.Author = nil }, false},
		{"missing message", func(m *discordgo.MessageCreate) { m.Message = nil }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, client, m := testDetector(t), &fakeMessages{}, testMessage("うお")
			tc.modify(m)
			if err := d.handleMessage(client, m); err != nil {
				t.Fatal(err)
			}
			if (len(client.messages) == 1) != tc.match || (d.counts[testUser] == 1) != tc.match {
				t.Fatalf("messages=%d, counts=%v", len(client.messages), d.counts)
			}
			if tc.match {
				want := "冷笑を検知しました！\n<@" + testUser + ">さんの1回目の冷笑です！"
				if client.messages[0].Content != want || client.channels[0] != m.ChannelID {
					t.Fatalf("wrong notification: %+v", client.messages[0])
				}
				ref := client.messages[0].Reference
				if ref == nil || ref.MessageID != m.ID || ref.ChannelID != m.ChannelID || ref.GuildID != m.GuildID {
					t.Fatalf("wrong reply reference: %+v", ref)
				}
				if !reflect.DeepEqual(client.messages[0].AllowedMentions, &discordgo.MessageAllowedMentions{Users: []string{testUser}}) {
					t.Fatal("mentions are not restricted to the author")
				}
			} else if _, err := os.Stat(d.statePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("ignored message wrote state")
			}
		})
	}
	if err := testDetector(t).handleMessage(&fakeMessages{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDetectionGlobalCountsAndReload(t *testing.T) {
	d, client := testDetector(t), &fakeMessages{}
	for i := 0; i < 3; i++ {
		m := testMessage("うお")
		if i == 1 {
			m.Content, m.GuildID, m.ChannelID = "どわー", "888888888888888888", testThread
		}
		if i == 2 {
			m.Author.ID = testUser2
		}
		if err := d.handleMessage(client, m); err != nil {
			t.Fatal(err)
		}
	}
	counts, err := loadCounts(d.statePath)
	if err != nil || counts[testUser] != 2 || counts[testUser2] != 1 {
		t.Fatalf("counts=%v, error=%v", counts, err)
	}
	reloaded, err := newDetector(filepath.Join(filepath.Dir(filepath.Dir(d.statePath)), "config.json"), d.statePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := reloaded.handleMessage(client, testMessage("うお")); err != nil {
		t.Fatal(err)
	}
	if reloaded.counts[testUser] != 3 || !strings.Contains(client.messages[3].Content, "3回目") {
		t.Fatal("count did not survive reload")
	}
}

func TestDetectionConcurrentMessages(t *testing.T) {
	d, client := testDetector(t), &fakeMessages{}
	client.before = func(_ *discordgo.MessageSend) {
		counts, err := loadCounts(d.statePath)
		if err != nil || counts[testUser] != uint64(len(client.messages)+1) {
			t.Errorf("count must be persisted before notification: %v, %v", counts, err)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Go(func() {
			if err := d.handleMessage(client, testMessage("うお")); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if d.counts[testUser] != 30 || len(client.messages) != 30 {
		t.Fatalf("lost increments: %v", d.counts)
	}
	for i, message := range client.messages {
		if !strings.Contains(message.Content, fmt.Sprintf("さんの%d回目", i+1)) {
			t.Fatalf("notifications out of sequence: %q", message.Content)
		}
	}
}

func TestDetectionFailures(t *testing.T) {
	d, client := testDetector(t), &fakeMessages{}
	// A directory at the destination forces rename to fail, even when tests run as root.
	if err := os.MkdirAll(d.statePath, 0700); err != nil {
		t.Fatal(err)
	}
	for _, existing := range []bool{false, true} {
		if existing {
			d.counts[testUser] = 7
		}
		if err := d.handleMessage(client, testMessage("うお")); err == nil {
			t.Fatal("expected persistence failure")
		}
		count, exists := d.counts[testUser]
		if exists != existing || (existing && count != 7) || len(client.messages) != 0 {
			t.Fatal("failed save committed a count or sent a notification")
		}
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(d.statePath), ".counts-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatal("temporary state was not cleaned up")
	}
	if err := os.Remove(d.statePath); err != nil {
		t.Fatal(err)
	}
	client.fail = errors.New("secret Discord URL")
	if err := d.handleMessage(client, testMessage("うお")); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("notification error missing or exposed sensitive data")
	}
	counts, err := loadCounts(d.statePath)
	if err != nil || counts[testUser] != 8 || d.counts[testUser] != 8 {
		t.Fatal("notification failure must keep the saved count")
	}
	d.counts[testUser] = math.MaxUint64
	if err := d.handleMessage(client, testMessage("うお")); err == nil || d.counts[testUser] != math.MaxUint64 {
		t.Fatal("count overflow was not rejected")
	}
	d.counts[testUser] = math.MaxUint64 - 1
	replies := len(client.messages)
	if err := d.handleMessage(client, testMessage("うおうお")); err == nil || d.counts[testUser] != math.MaxUint64-1 || len(client.messages) != replies {
		t.Fatal("overflow from a multi-word increment changed the count or sent a reply")
	}
}

func TestCorruptCountsStopStartup(t *testing.T) {
	for _, data := range []string{"", "{", "null", "[]", `{"invalid":1}`, `{"` + testUser + `":null}`, `{"` + testUser + `":-1}`, `{"` + testUser + `":1.5}`, `{"` + testUser + `":18446744073709551616}`, "{} {}"} {
		t.Run(data, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "counts.json")
			writeTestFile(t, path, data)
			if _, err := newDetector(filepath.Join(dir, "absent-config.json"), path); err == nil {
				t.Fatal("corrupt counts allowed startup")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != data {
				t.Fatal("corrupt state was overwritten")
			}
		})
	}
}
