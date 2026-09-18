// Package anticheat implements the /anticheat slash command, which searches
// GamingOnLinux's Linux/Steam Deck anti-cheat compatibility list for a game
// title or an anti-cheat vendor.
//
// The list lives at https://www.gamingonlinux.com/anticheat/ (CC BY 4.0) and is
// published as CSV under https://www.gamingonlinux.com/anticheat/csv/. The CSV
// is fetched at most once per datasetCacheTTL: concurrent callers and the
// background refresh share a single in-flight request, and failures are not
// cached so a later call can retry.
package anticheat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/toksikk/gidbig/internal/bot"
)

const (
	commandName = "anticheat"
	optionName  = "query"

	// datasetURL is the CSV download of the compatibility list. The human
	// readable page is https://www.gamingonlinux.com/anticheat/.
	datasetURL = "https://www.gamingonlinux.com/anticheat/csv/"
	sourceLine = "Source: GamingOnLinux Anti-Cheat list (CC BY 4.0): <https://www.gamingonlinux.com/anticheat/>"

	datasetCacheTTL      = time.Hour
	refreshCheckInterval = 15 * time.Minute
	fetchTimeout         = 15 * time.Second
	maxDatasetBytes      = 4 << 20

	maxResults          = 5
	messageLimit        = 1900
	noteMaxRunes        = 200
	autocompleteLimit   = 25
	choiceLabelMaxRunes = 100

	userAgent = "gidbig (+https://github.com/toksikk/gidbig)"
)

// Ranks decide the result order; lower is a better match.
const (
	rankExact = iota
	rankPrefix
	rankWord
	rankSubstring
	rankAntiCheat
	rankNone
)

// entry is one row of the GamingOnLinux list.
type entry struct {
	Game                 string
	Status               string
	AntiCheat            string
	SinglePlayerOnly     string
	WorksWithProton      string
	WorksWithNativeLinux string
	Notes                string
}

// utf8BOM is the byte-order mark the CSV export starts with.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

var datasetHTTPClient = &http.Client{Timeout: fetchTimeout}

// Module implements bot.Module for the anti-cheat lookup command.
type Module struct {
	fetchFn func(ctx context.Context) ([]byte, error)
	now     func() time.Time

	cacheMu   sync.Mutex
	entries   []entry
	expiresAt time.Time
	inflight  *datasetCall
}

// datasetCall is the shared result of one in-flight dataset fetch.
type datasetCall struct {
	done    chan struct{}
	entries []entry
	err     error
}

var _ bot.Module = (*Module)(nil)

// noMentions tells Discord to parse no mention type in the bot's own replies.
// The slice is non-nil on purpose: discordgo marshals a zero-value
// MessageAllowedMentions as "parse": null, while "parse": [] is what actually
// disables mention parsing.
var noMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}}

// New returns a new anticheat Module.
func New() *Module {
	return &Module{
		fetchFn: fetchDataset,
		now:     time.Now,
	}
}

func (m *Module) Name() string { return "anticheat" }

// Init satisfies bot.Module. The module stores no session: every handler
// receives it per event.
func (m *Module) Init(_ bot.Deps) error {
	slog.Info("anticheat: initialized")
	return nil
}

func (m *Module) Commands() []*discordgo.ApplicationCommand {
	return []*discordgo.ApplicationCommand{
		{
			Name:        commandName,
			Description: "Search GamingOnLinux's Linux/Steam Deck anti-cheat list",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:         discordgo.ApplicationCommandOptionString,
					Name:         optionName,
					Description:  "Game title or anti-cheat vendor",
					Required:     true,
					MaxLength:    100,
					Autocomplete: true,
				},
			},
		},
	}
}

func (m *Module) Listeners() []bot.EventListener {
	return []bot.EventListener{m.onInteractionCreate}
}

func (m *Module) Components() []bot.ComponentHandler { return nil }

// Background keeps the dataset warm so lookups and autocomplete work without a
// cold fetch in the interaction handler.
func (m *Module) Background() []bot.BackgroundTask {
	return []bot.BackgroundTask{{Name: "anticheat-dataset", Run: m.refreshLoop}}
}

func (m *Module) Shutdown() error { return nil }

func (m *Module) onInteractionCreate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.ApplicationCommandData().Name != commandName {
		return
	}
	switch i.Type {
	case discordgo.InteractionApplicationCommand:
		m.handleLookup(s, i)
	case discordgo.InteractionApplicationCommandAutocomplete:
		m.handleAutocomplete(s, i)
	}
}

func (m *Module) handleLookup(s *discordgo.Session, i *discordgo.InteractionCreate) {
	query := strings.TrimSpace(stringOption(i, optionName))
	if query == "" {
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
			Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{
				Content:         "Provide a game title or anti-cheat vendor to search for, e.g. `Battlefield 6` or `BattlEye`.",
				AllowedMentions: noMentions,
			},
		}); err != nil {
			slog.Error("anticheat: failed to respond to interaction", "error", err)
		}
		return
	}

	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	}); err != nil {
		slog.Error("anticheat: failed to respond to interaction", "error", err)
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()

		entries, err := m.getDataset(ctx)
		body := formatResults(entries, query)
		if err != nil {
			slog.Error("anticheat: failed to load dataset", "query", query, "error", err)
			body = "Failed to reach the GamingOnLinux Anti-Cheat list, try again later.\n" + sourceLine
		}
		if _, errEdit := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{
			Content:         &body,
			AllowedMentions: noMentions,
		}); errEdit != nil {
			slog.Error("anticheat: failed to edit interaction response", "error", errEdit)
		}
	}()
}

func (m *Module) handleAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	query := strings.TrimSpace(stringOption(i, optionName))
	var choices []*discordgo.ApplicationCommandOptionChoice
	// Autocomplete must answer within Discord's 3 s window, so it only reads the
	// warm cache (kept fresh by Background) and never fetches.
	for _, e := range searchEntries(m.cachedDataset(), query) {
		if len(choices) == autocompleteLimit {
			break
		}
		label := e.Game
		if e.Status != "" {
			label += " — " + e.Status
		}
		choices = append(choices, &discordgo.ApplicationCommandOptionChoice{
			Name:  truncateRunes(label, choiceLabelMaxRunes),
			Value: truncateRunes(e.Game, choiceLabelMaxRunes),
		})
	}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionApplicationCommandAutocompleteResult,
		Data: &discordgo.InteractionResponseData{Choices: choices},
	}); err != nil {
		slog.Error("anticheat: failed to respond to autocomplete", "error", err)
	}
}

// refreshLoop warms the cache at startup and re-checks it periodically, so a
// failed fetch is retried instead of leaving the cache cold until the next
// command.
func (m *Module) refreshLoop(ctx context.Context) {
	m.refresh(ctx)
	ticker := time.NewTicker(refreshCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refresh(ctx)
		}
	}
}

func (m *Module) refresh(ctx context.Context) {
	entries, err := m.getDataset(ctx)
	if err != nil {
		slog.Warn("anticheat: dataset refresh failed", "error", err)
		return
	}
	slog.Info("anticheat: dataset loaded", "entries", len(entries))
}

// getDataset returns the parsed list, fetching it when the cache is empty or
// expired. Concurrent callers share a single fetch; a failed fetch is not
// cached.
func (m *Module) getDataset(ctx context.Context) ([]entry, error) {
	m.cacheMu.Lock()
	if m.entries != nil && m.now().Before(m.expiresAt) {
		entries := m.entries
		m.cacheMu.Unlock()
		return entries, nil
	}
	if call := m.inflight; call != nil {
		m.cacheMu.Unlock()
		select {
		case <-call.done:
			return call.entries, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &datasetCall{done: make(chan struct{})}
	m.inflight = call
	m.cacheMu.Unlock()

	entries, err := m.load(ctx)

	m.cacheMu.Lock()
	m.inflight = nil
	if err == nil {
		m.entries = entries
		m.expiresAt = m.now().Add(datasetCacheTTL)
	}
	m.cacheMu.Unlock()

	call.entries, call.err = entries, err
	close(call.done)

	return entries, err
}

// cachedDataset returns the current cache contents without fetching. The slice
// is never mutated after it is stored, so callers may read it directly while a
// refresh swaps in a new one.
func (m *Module) cachedDataset() []entry {
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	return m.entries
}

func (m *Module) load(ctx context.Context) ([]entry, error) {
	body, err := m.fetchFn(ctx)
	if err != nil {
		return nil, err
	}
	return parseDataset(bytes.NewReader(body))
}

func fetchDataset(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, datasetURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := datasetHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDatasetBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDatasetBytes {
		return nil, errors.New("dataset exceeds size limit")
	}
	return body, nil
}

// parseDataset reads the CSV export. Columns are looked up by header name so a
// reordered or extended export keeps working; rows without a game title are
// skipped.
func parseDataset(r io.Reader) ([]entry, error) {
	buffered := bufio.NewReader(r)
	// The export starts with a UTF-8 BOM, which encoding/csv does not strip:
	// without removing it here, the quoted first header cell ("Game Title") is
	// rejected as a bare quote in a non-quoted field.
	if prefix, err := buffered.Peek(len(utf8BOM)); err == nil && bytes.Equal(prefix, utf8BOM) {
		_, _ = buffered.Discard(len(utf8BOM))
	}

	reader := csv.NewReader(buffered)
	reader.FieldsPerRecord = -1

	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	columns := make(map[string]int, len(header))
	for i, name := range header {
		columns[normalizeKey(name)] = i
	}
	// column returns -1 for a column the export does not have, so optional
	// fields stay empty instead of silently reading another column.
	column := func(name string) int {
		if index, ok := columns[name]; ok {
			return index
		}
		return -1
	}

	gameCol := column("game title")
	if gameCol < 0 {
		return nil, errors.New("dataset has no \"Game Title\" column")
	}
	statusCol := column("status")
	if statusCol < 0 {
		return nil, errors.New("dataset has no \"Status\" column")
	}
	antiCheatCol := column("anti-cheat software")
	singlePlayerCol := column("single player only")
	protonCol := column("works with proton")
	nativeCol := column("works with native linux")
	notesCol := column("notes")

	value := func(record []string, col int) string {
		if col < 0 || col >= len(record) {
			return ""
		}
		return sanitizeField(record[col])
	}

	var entries []entry
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read record %d: %w", len(entries)+2, err)
		}
		game := value(record, gameCol)
		if game == "" {
			continue
		}
		entries = append(entries, entry{
			Game:                 game,
			Status:               value(record, statusCol),
			AntiCheat:            value(record, antiCheatCol),
			SinglePlayerOnly:     value(record, singlePlayerCol),
			WorksWithProton:      value(record, protonCol),
			WorksWithNativeLinux: value(record, nativeCol),
			Notes:                value(record, notesCol),
		})
	}
	if len(entries) == 0 {
		return nil, errors.New("dataset has no entries")
	}
	return entries, nil
}

// searchEntries returns the entries matching query, best match first. A game
// title match always ranks above a match on the anti-cheat field, so searching
// for a vendor only surfaces vendor games when no title matches.
func searchEntries(entries []entry, query string) []entry {
	q := normalizeKey(query)
	if q == "" {
		return nil
	}
	type ranked struct {
		entry entry
		rank  int
	}
	var matches []ranked
	for _, e := range entries {
		rank := matchRank(e, q)
		if rank == rankNone {
			continue
		}
		matches = append(matches, ranked{entry: e, rank: rank})
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].rank != matches[j].rank {
			return matches[i].rank < matches[j].rank
		}
		return strings.ToLower(matches[i].entry.Game) < strings.ToLower(matches[j].entry.Game)
	})

	result := make([]entry, 0, len(matches))
	for _, m := range matches {
		result = append(result, m.entry)
	}
	return result
}

func matchRank(e entry, query string) int {
	title := normalizeKey(e.Game)
	if title == "" {
		return rankNone
	}
	switch {
	case title == query:
		return rankExact
	case strings.HasPrefix(title, query):
		return rankPrefix
	case hasWordPrefix(title, query):
		return rankWord
	case strings.Contains(title, query):
		return rankSubstring
	case query != "" && strings.Contains(normalizeKey(e.AntiCheat), query):
		return rankAntiCheat
	}
	return rankNone
}

// hasWordPrefix reports whether query starts a word inside the normalized title.
func hasWordPrefix(title, query string) bool {
	for offset := 0; offset < len(title); {
		r, size := utf8.DecodeRuneInString(title[offset:])
		wordStart := offset == 0 || !unicode.IsLetter(r)
		if wordStart {
			index := strings.Index(title[offset:], query)
			if index == 0 {
				return true
			}
			if index > 0 {
				// Only count a hit that starts at a word boundary.
				before, _ := utf8.DecodeLastRuneInString(title[offset : offset+index])
				if !unicode.IsLetter(before) {
					return true
				}
			}
		}
		if unicode.IsLetter(r) {
			offset += size
			continue
		}
		offset += size
	}
	return false
}

// formatResults renders the answer, keeping the whole reply inside Discord's
// 2000 character message limit. Everything that comes from the query or the CSV
// export is neutralized before it reaches the message body.
func formatResults(entries []entry, query string) string {
	display := neutralizeText(sanitizeField(query))
	matches := searchEntries(entries, query)
	if len(matches) == 0 {
		return truncateRunes("No entry for `"+display+"` in the GamingOnLinux Anti-Cheat list.\n\n"+sourceLine, messageLimit)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) for `%s` in the GamingOnLinux Anti-Cheat list:\n", len(matches), display)
	shown := matches
	if len(shown) > maxResults {
		shown = shown[:maxResults]
	}
	for _, e := range shown {
		b.WriteString("\n**" + neutralizeText(e.Game) + "**")
		if e.Status != "" {
			b.WriteString(" — " + neutralizeText(e.Status))
		}
		b.WriteString("\n")
		field := func(label, value string) {
			if value != "" {
				b.WriteString(label + ": " + neutralizeText(value) + "\n")
			}
		}
		field("Anti-cheat", e.AntiCheat)
		field("Works with Proton", e.WorksWithProton)
		field("Works with native Linux", e.WorksWithNativeLinux)
		field("Single player only", e.SinglePlayerOnly)
		if e.Notes != "" {
			b.WriteString("Note: " + neutralizeText(truncateRunes(e.Notes, noteMaxRunes)) + "\n")
		}
	}
	if len(matches) > len(shown) {
		fmt.Fprintf(&b, "\n…and %d more matches.\n", len(matches)-len(shown))
	}
	b.WriteString("\n" + sourceLine)

	return truncateRunes(b.String(), messageLimit)
}

// markupAnchor matches the HTML anchor tags the export embeds in notes.
var markupAnchor = regexp.MustCompile(`<a\s[^>]*href="([^"]*)"[^>]*>(.*?)</a>`)

// markupTag matches any remaining HTML tag.
var markupTag = regexp.MustCompile(`</?[a-zA-Z][^>]*>`)

// markupBareURL matches a URL that is not already angle-bracketed (or protected
// by the \x00 placeholder used for anchors).
var markupBareURL = regexp.MustCompile(`(^|[^<\x00])(https?://[^\s<>]+)`)

// sanitizeField turns a CSV cell into a single line suitable for a Discord
// message: embedded newlines and stray commas in multi-value cells are joined
// with ", ", HTML anchors become "text (<url>)", every other URL is wrapped in
// angle brackets so Discord attaches no link preview, and remaining markup is
// dropped.
func sanitizeField(value string) string {
	// Anchors are rewritten through placeholder bytes first: the tag strip below
	// would otherwise swallow the anchor and its URL.
	value = markupAnchor.ReplaceAllString(value, "$2 \x00$1\x01")
	value = markupTag.ReplaceAllString(value, "")
	value = html.UnescapeString(value)
	value = markupBareURL.ReplaceAllString(value, "$1<$2>")
	value = strings.NewReplacer("\x00", "(<", "\x01", ">)").Replace(value)

	var parts []string
	for _, line := range strings.FieldsFunc(value, func(r rune) bool { return r == '\n' || r == '\r' }) {
		for _, piece := range strings.Split(line, ",") {
			if trimmed := strings.Join(strings.Fields(piece), " "); trimmed != "" {
				parts = append(parts, trimmed)
			}
		}
	}
	return strings.Join(parts, ", ")
}

// zeroWidthSpace separates the parts of a mention token without being visible.
const zeroWidthSpace = "\u200b"

// neutralizeText defuses anything Discord would act on inside untrusted text
// that reaches a message body: a mention token (@everyone, <@id>, <@&id>) stops
// being a token when its "@" is split, and backticks — which would end the
// inline code span the query is wrapped in — become apostrophes.
func neutralizeText(value string) string {
	value = strings.ReplaceAll(value, "@", "@"+zeroWidthSpace)
	return strings.ReplaceAll(value, "`", "'")
}

// normalizeKey lowercases and collapses whitespace for matching and for header
// lookup; a UTF-8 BOM on the first header cell is dropped.
func normalizeKey(value string) string {
	return strings.ToLower(strings.Trim(strings.Join(strings.Fields(strings.TrimPrefix(value, "\ufeff")), " "), " "))
}

func truncateRunes(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	if max <= 1 {
		return string(runes[:max])
	}
	return strings.TrimRight(string(runes[:max-1]), " ") + "…"
}

// stringOption reads a string option from an interaction, tolerating absent
// options and unexpected types.
func stringOption(i *discordgo.InteractionCreate, name string) string {
	opt := i.ApplicationCommandData().GetOption(name)
	if opt == nil {
		return ""
	}
	value, _ := opt.Value.(string)
	return value
}
