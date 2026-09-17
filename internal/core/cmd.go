package gidbig

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/bwmarrin/discordgo"
	humanize "github.com/dustin/go-humanize"
	"github.com/toksikk/gidbig/internal/admin"
	"github.com/toksikk/gidbig/internal/bot"
	"github.com/toksikk/gidbig/internal/cfg"
	"github.com/toksikk/gidbig/internal/coffee"
	"github.com/toksikk/gidbig/internal/eso"
	"github.com/toksikk/gidbig/internal/gamerstatus"
	"github.com/toksikk/gidbig/internal/gippity"
	"github.com/toksikk/gidbig/internal/leetoclock"
	"github.com/toksikk/gidbig/internal/llm"
	"github.com/toksikk/gidbig/internal/stoll"
	"github.com/toksikk/gidbig/internal/wardogs"
	"github.com/toksikk/gidbig/internal/wttrin"
)

var (
	// discordgo session
	discord *discordgo.Session

	// Config struct to pass around
	conf *cfg.Config

	// mutex for checking if voice connection already exists
	mutex = &sync.Mutex{}

	// Start time for uptime calculation
	startTime = time.Now()

	// Eso module instance for web server access
	esoMod *eso.Module
)

func onReady(s *discordgo.Session, event *discordgo.Ready) {
	slog.Info("Discord READY", "user", event.User.String(), "guilds", len(event.Guilds))
}

func onConnect(s *discordgo.Session, event *discordgo.Connect) {
	slog.Info("Discord WebSocket connected",
		"shard_id", s.ShardID,
		"shard_count", s.ShardCount,
		"reconnect_enabled", s.ShouldReconnectOnError,
	)
}

func onDisconnect(s *discordgo.Session, event *discordgo.Disconnect) {
	// Callbacks run asynchronously and can arrive after the next Connect.
	// Do not wait for the session lock or claim this is the current state.
	slog.Warn("Discord WebSocket disconnect event received; automatic reconnect enabled",
		"shard_id", s.ShardID,
		"shard_count", s.ShardCount,
		"reconnect_enabled", s.ShouldReconnectOnError,
	)
}

func onResumed(s *discordgo.Session, event *discordgo.Resumed) {
	slog.Info("Discord session resumed")
}

func scontains(key string, options ...string) bool {
	for _, item := range options {
		if item == key {
			return true
		}
	}
	return false
}

func buildBotStatsMessage(s *discordgo.Session) string {
	stats := runtime.MemStats{}
	runtime.ReadMemStats(&stats)

	users := 0
	servers := 0
	if s.State != nil {
		servers = len(s.State.Guilds)
		for _, guild := range s.State.Guilds {
			users += len(guild.Members)
		}
	}

	uptime := time.Since(startTime).Round(time.Second)
	startDateTime := startTime.Format("2006-01-02 15:04:05")

	msg := fmt.Sprintf(`Version:         %s
Discordgo:       %s
Go:              %s

Memory:
  Alloc:         %s
  Sys:           %s
  TotalAlloc:    %s

Live Memory Objects:
  Malloc:        %s
  Frees:         %s

Heap:
  Alloc:         %s
  InUse:         %s
  Sys:           %s

Heap Returnable:
  HeapIdle:      %s
  HeapReleased:  %s

Stack:
  InUse:         %s
  Sys:           %s

Pointer Lookups: %d
Tasks:           %d
Servers:         %d
Users:           %d

Uptime:          %s (since %s)
`, currentVersion(), discordgo.VERSION, runtime.Version(),
		humanize.Bytes(stats.Alloc), humanize.Bytes(stats.Sys), humanize.Bytes(stats.TotalAlloc),
		humanize.Bytes(stats.Mallocs), humanize.Bytes(stats.Frees),
		humanize.Bytes(stats.HeapAlloc), humanize.Bytes(stats.HeapInuse), humanize.Bytes(stats.HeapSys),
		humanize.Bytes(stats.HeapIdle), humanize.Bytes(stats.HeapReleased),
		humanize.Bytes(stats.StackInuse), humanize.Bytes(stats.StackSys),
		stats.Lookups, runtime.NumGoroutine(), servers, users, uptime, startDateTime)

	return msg
}

// statusInteractionResponse builds the ephemeral interaction response for /status.
// Owner gets the stats block; non-owners get a denial. buildStats is injectable for testing.
func statusInteractionResponse(userID, ownerID string, buildStats func() string) *discordgo.InteractionResponse {
	if userID != ownerID {
		return &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content: "Access denied.",
				Flags:   discordgo.MessageFlagsEphemeral,
			},
		}
	}
	return &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: "```" + buildStats() + "```",
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}
}

func onStatusInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	if i.ApplicationCommandData().Name != "status" {
		return
	}

	var userID string
	if i.Member != nil {
		userID = i.Member.User.ID
	} else if i.User != nil {
		userID = i.User.ID
	}

	resp := statusInteractionResponse(userID, conf.Discord.OwnerID, func() string {
		return buildBotStatsMessage(s)
	})
	if err := s.InteractionRespond(i.Interaction, resp); err != nil {
		slog.Error("could not respond to /status interaction", "error", err)
	}
}

func onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Content == "ping" || m.Content == "pong" {
		// If the message is "ping" reply with "Pong!"
		if m.Content == "ping" {
			msg, err := s.ChannelMessageSend(m.ChannelID, "Pong!")
			if err != nil {
				slog.Error("could not send channel message", "message", msg, "error", err)
			}
		}

		// If the message is "pong" reply with "Ping!"
		if m.Content == "pong" {
			msg, err := s.ChannelMessageSend(m.ChannelID, "Ping!")
			if err != nil {
				slog.Error("could not send channel message", "message", msg, "error", err)
			}
		}

		// Updating bot status
		err := s.UpdateGameStatus(0, "Ping Pong with "+m.Author.Username)
		if err != nil {
			slog.Error("could not set game status", "error", err)
		}
	}
}

func setStartedStatus() {
	err := discord.UpdateCustomStatus(startedStatus())
	if err != nil {
		slog.Warn("Failed to set custom status", "error", err)
	}
}

func startedStatus() string {
	return "I just started! " + currentVersion() + " (" + builddate + ")"
}

func setupLogging(config *cfg.Config) {
	opts := &slog.HandlerOptions{}
	var logger *slog.Logger
	if config.DevMode {
		opts.Level = slog.LevelDebug
		opts.AddSource = true
		slog.Debug("Dev Mode", "devMode", config.DevMode)
		logger = slog.New(slog.NewTextHandler(os.Stdout, opts))
	} else {
		logger = slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}

	slog.SetDefault(logger)
}

// StartGidbig obviously
func StartGidbig() {
	conf = cfg.GetConfig()
	setupLogging(conf)
	LogVersion()
	var err error

	// create SoundCollections by scanning the audio folder
	createCollections()
	SetMaxQueueSize(conf.Soundboard.QueueMaxDepth)

	// Preload all the sounds
	slog.Info("Preloading sounds...")
	for _, coll := range COLLECTIONS {
		coll.Load()
	}

	// Create a discord session
	slog.Info("Starting discord session...")
	discord, err = discordgo.New("Bot " + conf.Discord.Token)
	if err != nil {
		slog.Error("Failed to create discord session", "error", err)
		os.Exit(1)
		return
	}

	// The pinned discordgo fork sets no read/write deadlines on its
	// websockets, so a degraded network can hang Open() (waiting for the
	// Hello packet while holding the session lock) or the heartbeat write
	// (holding wsMutex) forever, silently stalling the reconnect loop.
	// Bound every read/write at the transport level so stalls turn into
	// errors and discordgo reconnects on its own. See wsdeadline.go.
	discord.Dialer = newDeadlineDialer()

	// Capture gateway reconnect attempts and failures in the same structured
	// log stream as the application. Dev mode additionally enables payload and
	// heartbeat-level diagnostics.
	configureDiscordgoLogging(discord, conf.DevMode)

	// Set sharding info
	discord.ShardID = conf.Discord.ShardID
	discord.ShardCount = conf.Discord.ShardCount

	if discord.ShardCount <= 0 {
		discord.ShardCount = 1
	}

	discord.AddHandler(onReady)
	discord.AddHandler(onConnect)
	discord.AddHandler(onDisconnect)
	discord.AddHandler(onResumed)
	discord.AddHandler(onMessageCreate)
	discord.AddHandler(onStatusInteractionCreate)
	discord.AddHandler(onCoreSlashInteractionCreate)

	watchdogCtx, stopWatchdog := context.WithCancel(context.Background())
	defer stopWatchdog()
	go runDiscordWatchdog(watchdogCtx, discord, discordWatchdogInterval, discordWatchdogTimeout, func() {
		os.Exit(1)
	})

	err = discord.Open()
	if err != nil {
		slog.Error("Failed to create discord websocket connection", "error", err)
		os.Exit(1)
		return
	}

	if err := llm.Initialize(
		conf.LLM.Provider,
		conf.LLM.Model,
		conf.LLM.VisionModel,
		conf.LLM.BaseURL,
		conf.LLM.HTTPReferer,
		conf.LLM.Title,
	); err != nil {
		slog.Error("llm: init failed", "error", err)
		_ = discord.Close()
		os.Exit(1)
		return
	}
	llm.ResolvePersonality(conf.LLM.Personality, conf.LLM.Preset)
	coffeeMod := coffee.New()
	coffeeReady := false
	if err := coffeeMod.Init(bot.Deps{Session: discord, Config: conf}); err != nil {
		slog.Error("coffee: init failed", "error", err)
	} else {
		coffeeReady = true
		for _, l := range coffeeMod.Listeners() {
			discord.AddHandler(l)
		}
	}
	admin.RegisterProvider(coffeeMod)
	admin.Start(discord, conf.Discord.OwnerID, buildBotStatsMessage)
	esoMod = eso.New()
	if err := esoMod.Init(bot.Deps{Session: discord, OwnerID: conf.Discord.OwnerID}); err != nil {
		slog.Error("eso: init failed", "error", err)
	} else {
		for _, l := range esoMod.Listeners() {
			discord.AddHandler(l)
		}
	}
	bgCtx, bgCancel := context.WithCancel(context.Background())
	bgSupervisor := bot.NewSupervisor()
	if coffeeReady {
		bgSupervisor.Start(bgCtx, coffeeMod.Background()...)
	}
	gamerstatusMod := gamerstatus.New()
	if err := gamerstatusMod.Init(bot.Deps{Session: discord, OwnerID: conf.Discord.OwnerID}); err != nil {
		slog.Error("gamerstatus: init failed", "error", err)
	} else {
		bgSupervisor.Start(bgCtx, gamerstatusMod.Background()...)
	}
	gippity.Start(discord, conf.Gippity.RateLimitMessagesPerHour)
	leetoMod := leetoclock.New()
	leetoReady := false
	if err := leetoMod.Init(bot.Deps{Session: discord, Config: conf}); err != nil {
		slog.Error("leetoclock: init failed", "error", err)
	} else {
		leetoReady = true
		for _, l := range leetoMod.Listeners() {
			discord.AddHandler(l)
		}
		bgSupervisor.Start(bgCtx, leetoMod.Background()...)
	}
	stollMod := stoll.New()
	if err := stollMod.Init(bot.Deps{Session: discord, OwnerID: conf.Discord.OwnerID}); err != nil {
		slog.Error("stoll: init failed", "error", err)
	} else {
		for _, l := range stollMod.Listeners() {
			discord.AddHandler(l)
		}
	}
	wardogsMod := wardogs.New()
	if err := wardogsMod.Init(bot.Deps{Session: discord, OwnerID: conf.Discord.OwnerID}); err != nil {
		slog.Error("wardogs: init failed", "error", err)
	} else {
		for _, l := range wardogsMod.Listeners() {
			discord.AddHandler(l)
		}
	}
	wttrinMod := wttrin.New()
	if err := wttrinMod.Init(bot.Deps{Session: discord, OwnerID: conf.Discord.OwnerID, LLM: llm.GetClient()}); err != nil {
		slog.Error("wttrin: init failed", "error", err)
	} else {
		for _, l := range wttrinMod.Listeners() {
			discord.AddHandler(l)
		}
	}

	cmds := []*discordgo.ApplicationCommand{
		{Name: "status", Description: "Show bot runtime status (owner only)"},
	}
	cmds = append(cmds, coreSlashCommands()...)
	cmds = append(cmds, admin.Commands()...)
	cmds = append(cmds, coffeeMod.Commands()...)
	cmds = append(cmds, esoMod.Commands()...)
	cmds = append(cmds, gippity.Commands()...)
	cmds = append(cmds, stollMod.Commands()...)
	cmds = append(cmds, wardogsMod.Commands()...)
	cmds = append(cmds, wttrinMod.Commands()...)
	if _, err := discord.ApplicationCommandBulkOverwrite(discord.State.User.ID, "", cmds); err != nil {
		slog.Error("Failed to register slash commands", "error", err)
	}

	// Start Webserver if a valid port is provided and if ClientID and ClientSecret are set
	if conf.Web.Port != 0 && conf.Web.Port >= 1 && conf.Web.Oauth.ClientID != "" && conf.Web.Oauth.ClientSecret != "" && conf.Web.Oauth.RedirectURI != "" {
		slog.Info("Starting web server", "port", conf.Web.Port)
		go startWebServer(conf)
	} else {
		slog.Info("Required web server arguments missing or invalid. Skipping web server start.")
	}

	Banner(nil)

	slog.Info("Gidbig is ready. Quit with CTRL-C.")
	setStartedStatus()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	<-c

	slog.Info("shutting down")

	stopWatchdog()
	bgCancel()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		// Background tasks can also be blocked on the Discord session lock.
		// Include their wait in the shutdown deadline.
		bgSupervisor.Wait()
		if err := discord.Close(); err != nil {
			slog.Error("error closing discord session", "error", err)
		}
		gippity.Shutdown()
		gippity.CloseDB()
		if leetoReady {
			if err := leetoMod.Shutdown(); err != nil {
				slog.Error("leetoclock: shutdown failed", "error", err)
			}
		}
		if err := coffeeMod.Shutdown(); err != nil {
			slog.Error("coffee: shutdown failed", "error", err)
		}
	}()

	select {
	case <-shutdownDone:
		slog.Info("shutdown complete")
	case <-time.After(10 * time.Second):
		slog.Warn("shutdown timed out after 10s, forcing exit")
		logGoroutineStacks()
	}
}
