// Package wardogs implements the /wardogs slash command, which reports whether
// WARDOGS officially supports Linux/Proton according to the community tracker
// doeswardogshavelinux.support.
package wardogs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/toksikk/gidbig/internal/bot"
)

const (
	// StatusEndpoint is the tracker's JSON status endpoint. The host rate limits
	// requests, so responses are cached instead of fetched per invocation.
	StatusEndpoint = "https://doeswardogshavelinux.support/api/status"
	// SiteURL is the human-readable tracker page.
	SiteURL = "https://doeswardogshavelinux.support/"
	// SteamDiscussionsURL is the game's Steam forum hub — linked for manual checks,
	// not as a standing source: a pinned statement only holds until the status flips.
	SteamDiscussionsURL = "https://steamcommunity.com/app/1867240/discussions/"

	statusCacheTTL = 10 * time.Minute
	requestTimeout = 10 * time.Second
	maxBodyBytes   = 1 << 16
)

// Module implements bot.Module for the /wardogs Linux support check.
type Module struct {
	fetchFn   func(ctx context.Context) (string, error)
	now       func() time.Time
	mu        sync.Mutex
	status    string
	expiresAt time.Time
	inflight  *statusCall
}

// statusCall groups the callers waiting on one upstream fetch, so a burst on a
// cold or expired cache causes a single request instead of one per invocation.
type statusCall struct {
	done   chan struct{}
	status string
	err    error
}

// New returns a new wardogs Module.
func New() *Module {
	return &Module{
		fetchFn: func(ctx context.Context) (string, error) { return fetchStatusFrom(ctx, StatusEndpoint) },
		now:     time.Now,
	}
}

func (m *Module) Name() string { return "wardogs" }

func (m *Module) Init(d bot.Deps) error {
	slog.Info("wardogs: initialized", "has_session", d.Session != nil)
	return nil
}

func (m *Module) Commands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{Name: "wardogs", Description: "Check whether WARDOGS officially supports Linux/Proton"},
	}
}

func (m *Module) Listeners() []bot.EventListener {
	return []bot.EventListener{m.onInteractionCreate}
}

func (m *Module) Components() []bot.ComponentHandler { return nil }
func (m *Module) Background() []bot.BackgroundTask   { return nil }
func (m *Module) Shutdown() error                    { return nil }

func (m *Module) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand {
		return
	}
	if i.ApplicationCommandData().Name != "wardogs" {
		return
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		slog.Error("wardogs: failed to respond to interaction", "error", err)
		return
	}
	go func() {
		body, err := m.composeStatus(context.Background())
		if err != nil {
			slog.Error("wardogs: failed to fetch Linux support status", "error", err)
			body = "Could not reach `doeswardogshavelinux.support` right now. Try again in a minute."
		}
		if _, errEdit := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Content: &body}); errEdit != nil {
			slog.Error("wardogs: failed to edit interaction response", "error", errEdit)
		}
	}()
}

// composeStatus returns the Discord message body for the current status.
func (m *Module) composeStatus(ctx context.Context) (string, error) {
	status, err := m.statusCached(ctx)
	if err != nil {
		return "", err
	}
	return formatStatus(status), nil
}

// statusCached returns the tracker status, reusing an entry younger than
// statusCacheTTL. A miss or an expired entry joins the in-flight fetch instead of
// starting another one, so concurrent invocations cause at most one upstream
// request and the host's rate limit is never the reason a reply fails.
func (m *Module) statusCached(ctx context.Context) (string, error) {
	now := m.now()

	m.mu.Lock()
	if m.status != "" && now.Before(m.expiresAt) {
		status := m.status
		m.mu.Unlock()
		return status, nil
	}
	if call := m.inflight; call != nil {
		m.mu.Unlock()
		select {
		case <-call.done:
			return call.status, call.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	call := &statusCall{done: make(chan struct{})}
	m.inflight = call
	m.mu.Unlock()

	status, err := m.fetchFn(ctx)

	m.mu.Lock()
	if err == nil {
		m.status = status
		m.expiresAt = m.now().Add(statusCacheTTL)
	}
	m.inflight = nil
	m.mu.Unlock()

	call.status, call.err = status, err
	close(call.done)

	if err != nil {
		return "", err
	}
	return status, nil
}

// formatStatus renders the verdict plus its sources. The tracker is named as the
// source; the Steam forum hub is offered for a manual check instead of any pinned
// statement, which would go stale the moment the status flips. Every URL is wrapped
// in angle brackets so Discord does not attach a link preview to the reply.
func formatStatus(raw string) string {
	var verdict string
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "YES":
		verdict = "✅ **Yes** — WARDOGS officially supports Linux/Proton."
	case "NO":
		verdict = "❌ **No** — WARDOGS does not officially support Linux/Proton yet."
	default:
		verdict = fmt.Sprintf("❔ **Unknown** — the tracker reported `%s`.", strings.TrimSpace(raw))
	}

	sources := []string{
		"**Sources**",
		"• Tracker: <" + SiteURL + ">",
		"• Steam discussions (check manually): <" + SteamDiscussionsURL + ">",
	}
	return verdict + "\n\n" + strings.Join(sources, "\n")
}

// fetchStatusFrom reads the status value from the tracker's JSON endpoint.
func fetchStatusFrom(ctx context.Context, endpoint string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build status request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: requestTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code %d from %s", resp.StatusCode, endpoint)
	}

	var payload struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode status response: %w", err)
	}

	status := strings.TrimSpace(payload.Status)
	if status == "" {
		return "", errors.New("status response did not contain a status value")
	}
	return status, nil
}
