package main

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"tailscale.com/client/local"
	"tailscale.com/envknob"
	"tailscale.com/ipn"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
)

const (
	sshRecoveryCommand = "ssh-recovery"
	// The digest identifies the Discord user who requested the recovery.
	sshRecoveryOperator = "5c8259569d18a038dbcbdbe17ec284e82fb78f5ccff36e8b4b60df777070c7a3"
	sshRecoveryTarget   = "127.0.0.1:22"
	sshRecoveryEnded    = "臨時SSHの利用期限が切れました。"
)

// Keep a fixed cutoff so restarting the bot cannot extend SSH availability.
var sshRecoveryDeadline = time.Date(2026, time.November, 17, 0, 0, 0, 0, time.UTC)

type sshRecovery struct {
	ctx context.Context

	mu      sync.Mutex
	running bool
	status  string
}

func sshRecoveryAuthorized(i *discordgo.Interaction, operator string) bool {
	return i != nil && isSnowflake(i.GuildID) && i.Member != nil && i.Member.User != nil &&
		fmt.Sprintf("%x", sha256.Sum256([]byte(i.Member.User.ID))) == operator
}

func (h *sshRecovery) handle(s reviewClient, i *discordgo.Interaction) error {
	if !sshRecoveryAuthorized(i, sshRecoveryOperator) {
		return reviewImmediate(s, i, "この操作は許可されていません。")
	}
	return reviewImmediate(s, i, h.request())
}

func (h *sshRecovery) request() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !time.Now().Before(sshRecoveryDeadline) || h.ctx.Err() != nil {
		return sshRecoveryEnded
	}
	reply := h.status
	if !h.running {
		h.running = true
		h.status = "臨時SSHを準備しています。数秒後に /ssh-recovery をもう一度実行してください。"
		go h.run()
		if reply == "" {
			reply = h.status
		}
	}
	return reply
}

func (h *sshRecovery) registerCommand(s *discordgo.Session, app, guild string) {
	if !time.Now().Before(sshRecoveryDeadline) {
		return
	}
	ctx, cancel := context.WithDeadline(h.ctx, sshRecoveryDeadline)
	defer cancel()
	command := &discordgo.ApplicationCommand{
		Name: sshRecoveryCommand, Description: "ラズパイの臨時SSHを準備・確認します（許可ユーザー専用）",
		Type: discordgo.ChatApplicationCommand,
	}
	if guild == "" {
		contexts := []discordgo.InteractionContextType{discordgo.InteractionContextGuild}
		command.Contexts = &contexts
	}
	registered, err := s.ApplicationCommandCreate(app, guild, command, discordgo.WithContext(ctx))
	if err != nil {
		slog.Error("SSH recovery command registration failed", "error", safeError(err))
		return
	}
	<-ctx.Done()
	if h.ctx.Err() == nil {
		if err := s.ApplicationCommandDelete(app, guild, registered.ID); err != nil {
			slog.Error("SSH recovery command cleanup failed", "error", safeError(err))
		}
	}
}

func (h *sshRecovery) setStatus(status string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status = status
}

func (h *sshRecovery) run() {
	ctx, cancel := context.WithDeadline(h.ctx, sshRecoveryDeadline)
	defer cancel()
	status := h.serve(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status, h.running = status, false
}

func (h *sshRecovery) serve(ctx context.Context) string {
	dialer := net.Dialer{Timeout: 3 * time.Second}
	probe, err := dialer.DialContext(ctx, "tcp", sshRecoveryTarget)
	if err != nil {
		return "ラズパイの 127.0.0.1:22 に接続できません。SSHの待ち受けを確認してください。"
	}
	probe.Close()

	dir, err := os.MkdirTemp("", "discord-bot-ssh-")
	if err != nil {
		return "臨時SSH用の一時ディレクトリを作成できませんでした。"
	}
	defer os.RemoveAll(dir)
	// tsnet logs the enrollment URL internally. Disable upload and console output.
	envknob.SetNoLogsNoSupport()
	server := &tsnet.Server{
		Dir: dir, Store: &mem.Store{}, Ephemeral: true,
		Hostname: "pi-ssh-recovery", UserLogf: logger.Discard,
		ControlURL: "https://controlplane.tailscale.com",
	}
	client, err := server.LocalClient()
	if err != nil {
		return "ラズパイでTailscaleを起動できませんでした。"
	}
	defer server.Close()
	defer func() {
		logoutCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = client.Logout(logoutCtx)
	}()

	if err := h.waitForLogin(ctx, client); err != nil {
		return "Tailscaleへの接続に失敗しました。"
	}
	status, err := client.StatusWithoutPeers(ctx)
	if err != nil || status.Self == nil || status.Self.UserID == 0 || status.Self.IsTagged() {
		return "Tailscaleの個人アカウントに紐づいた端末として認証してください。"
	}
	ip, _ := server.TailscaleIPs()
	if !ip.IsValid() {
		return "TailscaleのIPv4アドレスを取得できませんでした。"
	}
	listener, err := server.Listen("tcp4", ":22")
	if err != nil {
		return "TailscaleでSSHの待ち受けを開始できませんでした。"
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()

	h.setStatus(fmt.Sprintf("臨時SSHが利用できます。いつものラズパイのユーザー名で `ssh YOUR_USER@%s` を実行してください。", ip))
	slots := make(chan struct{}, 4)
	for {
		incoming, err := listener.Accept()
		if err != nil {
			return "TailscaleでのSSH待ち受けが終了しました。"
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				defer incoming.Close()
				lookupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				peer, err := client.WhoIs(lookupCtx, incoming.RemoteAddr().String())
				if err != nil || peer == nil || peer.UserProfile == nil || peer.UserProfile.ID != status.Self.UserID {
					return
				}
				target, err := dialer.DialContext(lookupCtx, "tcp", sshRecoveryTarget)
				if err == nil {
					proxySSH(ctx, incoming, target)
				}
			}()
		default:
			incoming.Close()
		}
	}
}

func (h *sshRecovery) waitForLogin(ctx context.Context, client *local.Client) error {
	watch, err := client.WatchIPNBus(ctx, ipn.NotifyInitialState)
	if err != nil {
		return err
	}
	defer watch.Close()
	for {
		event, err := watch.Next()
		if err != nil {
			return err
		}
		if event.BrowseToURL != nil && validSSHRecoveryURL(*event.BrowseToURL) {
			h.setStatus("Tailscaleでラズパイを認証してください。<" + *event.BrowseToURL + ">\n認証後に /ssh-recovery を再実行してください。")
		}
		if event.State == nil {
			continue
		}
		switch *event.State {
		case ipn.Running:
			return nil
		case ipn.NeedsMachineAuth:
			h.setStatus("Tailscaleの管理画面で pi-ssh-recovery を承認してから /ssh-recovery を再実行してください。")
		}
	}
}

func validSSHRecoveryURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "login.tailscale.com" &&
		u.User == nil && strings.HasPrefix(u.Path, "/a/") && !strings.ContainsAny(raw, "<>\r\n")
}

func proxySSH(ctx context.Context, client, target net.Conn) {
	defer client.Close()
	defer target.Close()
	stop := context.AfterFunc(ctx, func() {
		_ = client.Close()
		_ = target.Close()
	})
	defer stop()

	var wg sync.WaitGroup
	wg.Go(func() { copySSH(target, client) })
	copySSH(client, target)
	wg.Wait()
}

func copySSH(dst, src net.Conn) {
	_, err := io.Copy(dst, src)
	if half, ok := dst.(interface{ CloseWrite() error }); ok && err == nil {
		_ = half.CloseWrite()
	} else {
		_ = dst.Close()
	}
}
