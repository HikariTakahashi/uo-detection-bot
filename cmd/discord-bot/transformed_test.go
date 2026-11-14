package main

import (
	"context"
	"encoding/ascii85"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/bits"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

var transformedTestWords = []string{"うお", "どわー", "uo"}

func testASCII85(text string) string {
	out := make([]byte, ascii85.MaxEncodedLen(len(text)))
	n := ascii85.Encode(out, []byte(text))
	return string(out[:n])
}

func TestTransformedLiteralExamples(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"e38186e3818a", "うお"},
		{"0xE3 0X81 0x86\t0xe3 0x81 0x8a", "うお"},
		{"0xE30X810x860xe30x810x8a", "うお"},
		{"f1c043f1c045", "うお"}, // UTF-8 bytes rotated right one bit.
		{"c7030dc70315", "うお"}, // UTF-8 bytes rotated left one bit.
		{"3046304a", "うお"},
		{"111000111000000110000110111000111000000110001010", "うお"},
		{"e381a9e38081e381a9e3828fe383bc", "ど、どわー"},
		{"f1c0d4f140c0f1c0d4f141c7f1c15e", "ど、どわー"},
		{"c70353c70103c70353c7051fc70779", "ど、どわー"},
		{"306930013069308f30fc", "ど、どわー"},
		{"111000111000000110101001111000111000000010000001111000111000000110101001111000111000001010001111111000111000001110111100", "ど、どわー"},
		{"%E3%81%86%E3%81%8A", "うお"},
		{`\u3046\u304a`, "うお"},
		{`\U00003069\u3001\u3069\u308f\u30fc`, "ど、どわー"},
		{"j+2sFJVj", "うお"},
		{testASCII85("uo"), "uo"},
		{"j+42iJ:Za3WT/Skj+Fu", "ど、どわー"},
		{"\u0082¤\u0082¨", "うお"},
		{"\u0082Ç\u0081A\u0082Ç\u0082í\u0081[", "ど、どわー"},
		{"反応: e38186e3818awww！", "反応: うおwww！"},
		{"「e38186e3818a」は説明です", "「うお」は説明です"},
	} {
		if got := transformedText(tc.input, transformedTestWords); got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestTransformedNumericFormatsAndOuterCompositions(t *testing.T) {
	config, err := loadDetectionConfig("../../detection.config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, word := range []string{"うお", "ど、どわー"} {
		for _, width := range []int{8, 16} {
			for _, base := range []int{16, 2} {
				for _, rotation := range []int{0, -1, 1} {
					var units []string
					values := []rune(word)
					if width == 8 {
						values = nil
						for _, b := range []byte(word) {
							values = append(values, rune(b))
						}
					}
					for _, v := range values {
						if width == 8 {
							v = rune(bits.RotateLeft8(uint8(v), rotation))
						} else {
							v = rune(bits.RotateLeft16(uint16(v), rotation))
						}
						if base == 16 {
							units = append(units, fmt.Sprintf("%0*x", width/4, v))
						} else {
							units = append(units, fmt.Sprintf("%0*b", width, v))
						}
					}
					for _, separator := range []string{"", " ", "\t"} {
						encoded := strings.Join(units, separator)
						var escaped, percent strings.Builder
						for _, r := range encoded {
							fmt.Fprintf(&escaped, `\u%04x`, r)
							fmt.Fprintf(&percent, "%%%02x", r)
						}
						for _, outer := range []string{encoded, percent.String(), escaped.String(), testASCII85(encoded)} {
							if got := transformedText(outer, config.Words); got != word {
								t.Errorf("width=%d base=%d rotation=%d %q: got %q, want %q", width, base, rotation, outer, got, word)
							}
						}
					}
				}
			}
		}
	}
}

func TestTransformedRejectsMalformedUnrelatedAndUnboundedInput(t *testing.T) {
	for _, text := range []string{
		"うお", "うおw", "uo", "ど、どわー", "hello", "deadbeef", "123456789012345678",
		"id_e38186e3818a", "e38186e3818a_world", "e38186e3818aworld",
		"https://example.com/%E3%81%86%E3%81%8A", "www.example.com/e38186e3818a",
		"`e38186e3818a`", "```\ne38186e3818a\n```", "うお %20",
		"e38186e3818", "e38186e3818aff", "11100011100000011000011011100011100000011000101",
		"d8003046304a", "00003046304a", "\u0082¤\u0082", "%E3%81%86%E3%81%8A%ZZ",
		`\u3046\u304a\uZZZZ`, `\u3046\u304a\U00110000`, `\u3046\u304a\ud800`,
		"%25E3%2581%2586%25E3%2581%258A", // No recursive outer decoding.
		"<~j+2sFJVj~>", "j+2sFJVj!", "j+2sFJVj\x00", "uuuuu", "zj+2sFJVj",
		testASCII85("うおaa") + "z",
		strings.Repeat(" ", transformedMaxRunes) + "e38186e3818a",
	} {
		if got := transformedText(text, transformedTestWords); got != "" {
			t.Errorf("unexpected recovery of %q as %q", text, got)
		}
	}
	if got := transformedText("e38186e3818a", []string{"unconfigured"}); got != "" {
		t.Fatal("ignored configured words", got)
	}
	// Two valid interpretations of the very same hex span are not two uses.
	if got := transformedText("3046304a", []string{"うお", "0F0J"}); got != "" {
		t.Fatal("ambiguous numeric interpretation was accepted", got)
	}
}

func TestTransformedExactAndRepetitionCountsWithoutAI(t *testing.T) {
	for _, tc := range []struct {
		text string
		want uint64
	}{
		{"e38186e3818a", 1}, {"e38186e3818ae38186e3818a", 2},
		{"うおe38186e3818a", 2},
		{"75756f", 0}, {"756f756f756f", 3}, {`\u3046\u304a`, 1},
		{"%E3%81%86%E3%81%8A", 1}, {"うお e38186e3818a", 0},
		{"e381a9e38081e381a9e3828fe383bc", 0}, // Context needs AI, not substring counting.
		{"e38186e3818aw", 0}, {"「e38186e3818a」", 0},
	} {
		d := testDetector(t)
		d.words["uo"] = true
		client := &fakeMessages{}
		if err := d.handleIncoming(client, nil, testMessage(tc.text)); err != nil || d.counts[testUser] != tc.want {
			t.Errorf("%q: count=%d want=%d err=%v", tc.text, d.counts[testUser], tc.want, err)
		}
		if tc.want > 0 && len(client.messages) != 1 {
			t.Fatal("expected one reply per message")
		}
	}
}

func TestTransformedAIIntegration(t *testing.T) {
	for _, tc := range []struct {
		decoded, path, label, prompt string
		status                       int
		truncated                    bool
		want                         uint64
	}{
		{"ど、どわー", "/classify", "mockery", "baseline", 200, false, 1},
		{"うおw、うおw", "/classify/laughter", "mockery", "laughter", 200, false, 1},
		{"「うおw」は説明です", "/classify/laughter", "other", "laughter", 200, false, 0},
		{"ど、どわー", "/classify", "uncertain", "baseline", 200, false, 0},
		{"ど、どわー", "/classify", "mockery", "baseline", 200, true, 0},
		{"ど、どわー", "/classify", "mockery", "laughter", 200, false, 0},
		{"ど、どわー", "/classify", "mockery", "baseline", 503, false, 0},
		{"ど、どわー", "/classify", "mockery", "baseline", 400, false, 0},
	} {
		t.Run(fmt.Sprintf("%s-%s-%s-%d-%v", tc.decoded, tc.label, tc.prompt, tc.status, tc.truncated), func(t *testing.T) {
			s := testShadow(t)
			s.detector.words["どわー"] = true
			s.words = append(s.words, "どわー")
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls++
				var input map[string]string
				if err := json.NewDecoder(req.Body).Decode(&input); err != nil || input["text"] != tc.decoded || req.URL.Path != tc.path {
					t.Error("wrong decoded input or classifier", input, req.URL.Path, err)
				}
				if tc.path == "/classify" && input["target_words"] != "" || tc.path == "/classify/laughter" && input["target_words"] == "" {
					t.Error("wrong target words", input)
				}
				w.WriteHeader(tc.status)
				p := samplePrediction()
				p.Label, p.Prompt, p.Truncated = tc.label, tc.prompt, tc.truncated
				json.NewEncoder(w).Encode(p)
			}))
			defer server.Close()
			s.config.Endpoint = server.URL + "/classify"
			m := testMessage(hex.EncodeToString([]byte(tc.decoded)))
			client := &fakeMessages{}
			for range 2 {
				if err := s.detector.handleIncoming(client, s, m); err != nil {
					t.Fatal(err)
				}
			}
			if len(s.laughter) != 1 || len(s.jobs) != 0 || len(client.messages) != 0 {
				t.Fatal("must enqueue one exclusive AI decision")
			}
			job := <-s.laughter
			if job.record.Text != m.Content || job.record.DecodedText != tc.decoded {
				t.Fatal("lost raw/decoded review evidence", job.record)
			}
			s.processLaughter(context.Background(), client, job)
			wantCalls := 1
			if tc.status == 503 {
				wantCalls = 2
			}
			if calls != wantCalls || s.detector.counts[testUser] != tc.want || len(client.messages) != int(tc.want) {
				t.Fatal("wrong AI attempts, count or notifications", calls, s.detector.counts, len(client.messages))
			}
			if tc.want > 0 {
				state, err := loadDetectionState(s.detector.statePath)
				if err != nil || !state.AICounted[m.ID] {
					t.Fatal("deduplication not persisted", err)
				}
				s.detector.counts, s.detector.aiCounted = state.Counts, state.AICounted
				s.processLaughter(context.Background(), client, job)
				if calls != wantCalls || s.detector.counts[testUser] != tc.want {
					t.Fatal("replay after reload counted again")
				}
			}
		})
	}
}

func TestTransformedAIAvailabilityAndEligibility(t *testing.T) {
	for _, reason := range []string{"disabled", "channel", "bot", "webhook", "dm", "full", "deleted", "cancelled", "expired"} {
		t.Run(reason, func(t *testing.T) {
			s := testShadow(t)
			m := testMessage(url.PathEscape("うおw"))
			s.config.Mode = "off"
			s.config.CountLaughter = true
			switch reason {
			case "disabled":
				s.config.CountLaughter = false
			case "channel":
				m.ChannelID = testUser
			case "bot":
				m.Author.Bot = true
			case "webhook":
				m.WebhookID = testUser
			case "dm":
				m.GuildID = ""
			case "full":
				for len(s.laughter) < cap(s.laughter) {
					s.laughter <- laughterJob{}
				}
			case "deleted":
				s.remove(m.ID, m.GuildID)
			}
			client := &fakeMessages{}
			if err := s.detector.handleIncoming(client, s, m); err != nil {
				t.Fatal(err)
			}
			if reason == "cancelled" || reason == "expired" {
				job := <-s.laughter
				if reason == "cancelled" {
					s.remove(m.ID, m.GuildID)
				} else {
					job.record.CapturedAt = time.Now().Add(-time.Minute)
				}
				s.processLaughter(context.Background(), client, job)
			} else if reason != "full" && len(s.laughter) != 0 {
				t.Fatal("ineligible message queued")
			}
			if len(client.messages) != 0 || s.detector.counts[testUser] != 0 {
				t.Fatal("unavailable AI fell back to counting")
			}
		})
	}
}

func TestTransformedShadowKeepsRawAndDecodedReviewEvidence(t *testing.T) {
	s := testShadow(t)
	s.config.CountLaughter = false
	raw := hex.EncodeToString([]byte("うおw"))
	m := testMessage(raw)
	m.MessageReference = &discordgo.MessageReference{MessageID: testUser2, ChannelID: m.ChannelID, GuildID: m.GuildID}
	m.ReferencedMessage = &discordgo.Message{ID: testUser2, Content: "返信元の文脈"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var input map[string]string
		if err := json.NewDecoder(req.Body).Decode(&input); err != nil || req.URL.Path != "/classify" || input["text"] != "うおw" || input["context"] != m.ReferencedMessage.Content {
			t.Error("shadow did not classify decoded text with its original context", input, err)
		}
		json.NewEncoder(w).Encode(samplePrediction())
	}))
	defer server.Close()
	s.config.Endpoint = server.URL + "/classify"
	client := &fakeMessages{}
	if err := s.detector.handleIncoming(client, s, m); err != nil || len(s.jobs) != 1 || len(s.laughter) != 0 {
		t.Fatal("review-only mode used live detection", err)
	}
	s.process(context.Background(), <-s.jobs)
	if len(client.messages) != 0 || s.detector.counts[testUser] != 0 {
		t.Fatal("shadow classification counted a message")
	}
	store, err := openShadowStore(s.store.dir, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.store = store
	r := s.store.records[m.ID]
	if r.Text != raw || r.DecodedText != "うおw" || r.Prediction.Label != "mockery" {
		t.Fatal("review evidence did not survive reload", r)
	}
	f := callReview(t, newReview(s), reviewEvent(discordgo.InteractionApplicationCommand, "review"))
	if len(f.edits) != 1 || f.edits[0].Embeds == nil || len(*f.edits[0].Embeds) != 3 {
		t.Fatal("review must show raw text, decoded text and context")
	}
	embeds := *f.edits[0].Embeds
	if !strings.Contains(embeds[0].Description, raw) || !strings.Contains(embeds[1].Description, "うおw") {
		t.Fatal("review omitted raw or decoded text")
	}
}
