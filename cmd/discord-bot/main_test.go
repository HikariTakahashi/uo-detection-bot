package main

import (
	"errors"
	"github.com/bwmarrin/discordgo"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecoveredPingResponse(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Second, "🏓 Pong! 0 ms"}, {0, "🏓 Pong! 0 ms"},
		{1499 * time.Microsecond, "🏓 Pong! 1 ms"}, {1500 * time.Microsecond, "🏓 Pong! 2 ms"},
		{42 * time.Millisecond, "🏓 Pong! 42 ms"},
	} {
		if got := formatPingResponse(tc.d); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.d, got, tc.want)
		}
	}
}

type fakeCommands struct {
	commands        []*discordgo.ApplicationCommand
	fail            error
	created, edited int
	editedID        string
}

func (f *fakeCommands) ApplicationCommands(string, string, ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error) {
	return f.commands, f.fail
}
func (f *fakeCommands) ApplicationCommandCreate(_ string, _ string, c *discordgo.ApplicationCommand, _ ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error) {
	f.created++
	return c, nil
}
func (f *fakeCommands) ApplicationCommandEdit(_ string, _ string, id string, c *discordgo.ApplicationCommand, _ ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error) {
	f.edited++
	f.editedID = id
	return c, nil
}
func TestRegisterOnlyPing(t *testing.T) {
	f := &fakeCommands{commands: []*discordgo.ApplicationCommand{{ID: "other", Name: "other", Type: 1}}}
	if err := ensurePingCommand(f, "app", "guild"); err != nil {
		t.Fatal(err)
	}
	if f.created != 1 || f.edited != 0 {
		t.Fatal("unexpected changes")
	}
	f = &fakeCommands{commands: []*discordgo.ApplicationCommand{{ID: "ping-id", Name: "ping", Description: "old", Type: 1}, {ID: "other", Name: "other", Type: 1}}}
	if err := ensurePingCommand(f, "app", "guild"); err != nil {
		t.Fatal(err)
	}
	if f.editedID != "ping-id" || f.created != 0 {
		t.Fatal("modified wrong command")
	}
	f.commands[0].Description = pingDescription
	f.edited = 0
	if err := ensurePingCommand(f, "app", "guild"); err != nil {
		t.Fatal(err)
	}
	if f.edited != 0 || f.created != 0 {
		t.Fatal("unchanged command was modified")
	}
	f.fail = errors.New("secret request URL")
	if err := ensurePingCommand(f, "app", "guild"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatal("error missing or sensitive details leaked")
	}
}
func TestSafeError(t *testing.T) {
	err := &discordgo.RESTError{Response: &http.Response{StatusCode: 403}}
	if got := safeError(err); got != "Discord HTTP 403" {
		t.Fatal(got)
	}
}
func TestNotify(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notify.sock")
	socket, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	t.Setenv("NOTIFY_SOCKET", path)
	if err := notify("READY=1"); err != nil {
		t.Fatal(err)
	}
	_ = socket.SetReadDeadline(time.Now().Add(time.Second))
	b := make([]byte, 100)
	n, _, err := socket.ReadFromUnix(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:n]) != "READY=1" {
		t.Fatal(string(b[:n]))
	}
}
