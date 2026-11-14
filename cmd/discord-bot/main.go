package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
)

var version = "development"

const pingDescription = "Discord Gatewayの現在のレイテンシを表示します"

func main() {
	showVersion := flag.Bool("version", false, "print build commit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("bot stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	token := strings.TrimSpace(os.Getenv("DISCORD_TOKEN"))
	if token == "" {
		return errors.New("DISCORD_TOKEN is required")
	}
	guild := strings.TrimSpace(os.Getenv("DISCORD_GUILD_ID"))
	if guild != "" && !isSnowflake(guild) {
		return errors.New("DISCORD_GUILD_ID must be a numeric server ID")
	}
	notifications, err := loadNotificationConfig()
	if err != nil {
		return err
	}
	detection, err := newDetector(
		envPath("DETECTION_CONFIG_PATH", defaultDetectionConfigPath),
		envPath("DETECTION_STATE_PATH", defaultDetectionStatePath),
	)
	if err != nil {
		return err
	}
	shadow, shadowErr := newShadow(envPath("SHADOW_CONFIG_PATH", defaultShadowConfigPath), envPath("SHADOW_DATA_PATH", defaultShadowDataPath), detection)
	if shadowErr != nil {
		slog.Error("shadow collection disabled or unavailable", "error", safeError(shadowErr))
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	defer stopWorker()
	if shadow != nil {
		go shadow.run(workerCtx)
	}
	reviews := newReview(shadow)
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		return errors.New("cannot create Discord session")
	}
	// Keep /ping usable without privileged intents when detection is disabled.
	s.Identify.Intents = 0
	if detection.enabled() {
		s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent
		s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
			if err := detection.handleMessage(s, m); err != nil {
				slog.Error("message detection failed", "error", err)
			}
		})
	}
	if shadow != nil && shadow.config.Mode == "shadow" {
		s.Identify.Intents |= discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent
		s.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageCreate) { shadow.enqueue(m) })
	}
	if shadow != nil {
		s.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageDelete) {
			if m != nil && m.Message != nil {
				if err := shadow.remove(m.ID, m.GuildID); err != nil {
					slog.Error("shadow delete failed", "error", safeError(err))
				}
			}
		})
		s.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageDeleteBulk) {
			if m != nil {
				for _, id := range m.Messages {
					if err := shadow.remove(id, m.GuildID); err != nil {
						slog.Error("shadow bulk delete failed", "error", safeError(err))
					}
				}
			}
		})
	}
	s.Client.Timeout = 20 * time.Second
	s.AddHandler(func(s *discordgo.Session, i *discordgo.InteractionCreate) {
		if handled, err := reviews.handle(s, i); handled {
			if err != nil {
				slog.Error("review interaction failed", "error", safeError(err))
			}
			return
		}
		if i == nil || i.Interaction == nil || i.Type != discordgo.InteractionApplicationCommand {
			return
		}
		if i.ApplicationCommandData().Name != "ping" {
			return
		}
		err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: formatPingResponse(s.HeartbeatLatency()), AllowedMentions: &discordgo.MessageAllowedMentions{}},
		})
		if err != nil {
			slog.Error("failed to respond to /ping", "error", safeError(err))
		}
	})
	if err := s.Open(); err != nil {
		return fmt.Errorf("open Discord gateway: %s", safeError(err))
	}
	defer s.Close()
	if s.State == nil || s.State.User == nil {
		return errors.New("Discord READY did not contain a bot user")
	}
	if err := ensurePingCommand(s, s.State.User.ID, guild); err != nil {
		return err
	}
	if err := ensureReviewCommand(s, s.State.User.ID, guild); err != nil {
		slog.Error("review command registration failed", "error", err)
	}
	if err := notify("READY=1\nSTATUS=Discord connected; /ping registered; commit " + version); err != nil {
		return fmt.Errorf("notify systemd: %w", err)
	}
	scope := "global"
	if guild != "" {
		scope = "guild"
	}
	slog.Info("bot ready", "version", version, "command_scope", scope,
		"detection_enabled", detection.enabled(), "scheduled_notifications_enabled", notifications.enabled())
	if notifications.enabled() {
		go runScheduledNotifications(ctx, s, notifications)
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = notify("STOPPING=1")
			return nil
		case <-ticker.C:
			s.RLock()
			ack := s.LastHeartbeatAck
			s.RUnlock()
			// A dead Gateway connection must not look healthy indefinitely.
			if !ack.IsZero() && time.Since(ack) < 90*time.Second {
				_ = notify("WATCHDOG=1")
			}
		}
	}
}

type commandClient interface {
	ApplicationCommands(string, string, ...discordgo.RequestOption) ([]*discordgo.ApplicationCommand, error)
	ApplicationCommandCreate(string, string, *discordgo.ApplicationCommand, ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error)
	ApplicationCommandEdit(string, string, string, *discordgo.ApplicationCommand, ...discordgo.RequestOption) (*discordgo.ApplicationCommand, error)
}

func ensurePingCommand(s commandClient, app, guild string) error {
	commands, err := s.ApplicationCommands(app, guild)
	if err != nil {
		return fmt.Errorf("list commands: %s", safeError(err))
	}
	desired := &discordgo.ApplicationCommand{Name: "ping", Description: pingDescription, Type: discordgo.ChatApplicationCommand}
	for _, c := range commands {
		if c.Name == "ping" && c.Type == discordgo.ChatApplicationCommand {
			if c.Description == desired.Description && len(c.Options) == 0 {
				return nil
			}
			desired.Options = []*discordgo.ApplicationCommandOption{}
			_, err = s.ApplicationCommandEdit(app, guild, c.ID, desired)
			if err != nil {
				return fmt.Errorf("update /ping: %s", safeError(err))
			}
			return nil
		}
	}
	_, err = s.ApplicationCommandCreate(app, guild, desired)
	if err != nil {
		return fmt.Errorf("register /ping: %s", safeError(err))
	}
	return nil
}

func formatPingResponse(latency time.Duration) string {
	if latency < 0 {
		latency = 0
	}
	return fmt.Sprintf("🏓 Pong! %d ms", latency.Round(time.Millisecond).Milliseconds())
}

func isSnowflake(s string) bool {
	if len(s) < 17 || len(s) > 20 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Discord errors can contain request URLs with interaction tokens. Log only status codes.
func safeError(err error) string {
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Response != nil {
		return fmt.Sprintf("Discord HTTP %d", rest.Response.StatusCode)
	}
	return fmt.Sprintf("%T (details omitted to protect credentials)", err)
}

func notify(message string) error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return err
	}
	defer conn.Close()
	_, err = conn.Write([]byte(message))
	return err
}
