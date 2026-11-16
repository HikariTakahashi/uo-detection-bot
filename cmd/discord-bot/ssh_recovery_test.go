package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

func TestSSHRecoveryAuthorization(t *testing.T) {
	const operator = "123456789012345678"
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(operator)))
	event := &discordgo.Interaction{
		GuildID: "223456789012345678",
		Member:  &discordgo.Member{User: &discordgo.User{ID: operator}},
	}
	if !sshRecoveryAuthorized(event, digest) {
		t.Fatal("the designated operator was rejected")
	}
	event.Member.User.ID = "323456789012345678"
	if sshRecoveryAuthorized(event, digest) {
		t.Fatal("another Discord member was authorized")
	}
	event.GuildID = ""
	event.Member.User.ID = operator
	if sshRecoveryAuthorized(event, digest) {
		t.Fatal("a non-guild interaction was authorized")
	}
}

func TestSSHRecoveryEnrollmentURL(t *testing.T) {
	if !validSSHRecoveryURL("https://login.tailscale.com/a/123456") {
		t.Fatal("valid Tailscale enrollment URL was rejected")
	}
	for _, address := range []string{
		"http://login.tailscale.com/a/123456",
		"https://login.tailscale.com.example.net/a/123456",
		"https://login.tailscale.com@other.example/a/123456",
		"https://login.tailscale.com/a/123456> ",
	} {
		if validSSHRecoveryURL(address) {
			t.Errorf("invalid Tailscale enrollment URL accepted: %q", address)
		}
	}
}

func TestSSHRecoveryStopsExistingStreams(t *testing.T) {
	client, inbound := net.Pipe()
	outbound, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	deadline := time.Now().Add(2 * time.Second)
	_ = client.SetDeadline(deadline)
	_ = server.SetDeadline(deadline)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		proxySSH(ctx, inbound, outbound)
	}()
	for _, pair := range [][2]net.Conn{{client, server}, {server, client}} {
		if _, err := io.WriteString(pair[0], "SSH-2.0-test"); err != nil {
			t.Fatal(err)
		}
		buf := make([]byte, len("SSH-2.0-test"))
		if _, err := io.ReadFull(pair[1], buf); err != nil || string(buf) != "SSH-2.0-test" {
			t.Fatalf("forwarded %q, error %v", buf, err)
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("SSH stream survived cancellation")
	}
}
