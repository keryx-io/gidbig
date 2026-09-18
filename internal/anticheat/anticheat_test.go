package anticheat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/toksikk/gidbig/internal/bot"
)

// sampleCSV mirrors the shape of the GamingOnLinux export: a UTF-8 BOM, a
// quoted multi-line note, and a multi-value anti-cheat cell.
const sampleCSV = "\ufeff\"Game Title\",Status,\"Anti-Cheat Software\",\"Single Player Only\"," +
	"\"Works with Proton\",\"Works with Native Linux\",Notes\n" +
	"2XKO,Broken,Vanguard,,No,No,\n" +
	"\"7 Days to Die\",Works,\"Easy Anti-Cheat\",,Yes,Yes,\n" +
	"\"Halo Infinite\",Works,\"Easy Anti-Cheat\",,Yes,No,\"Needs Proton Experimental.\nGE-Proton 10-30 also works.\"\n" +
	"\"Broken Arrow\",Broken,\"Broken Arrow Anti-Cheat System\n, Easy Anti-Cheat\",,No,No,\n"

func newTestModule(fetch func(context.Context) ([]byte, error)) *Module {
	m := New()
	m.fetchFn = fetch
	return m
}

func staticFetch(body string) func(context.Context) ([]byte, error) {
	return func(context.Context) ([]byte, error) { return []byte(body), nil }
}

func TestParseDataset_ReadsAllColumns(t *testing.T) {
	entries, err := parseDataset(strings.NewReader(sampleCSV))
	if err != nil {
		t.Fatalf("parseDataset() error = %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4", len(entries))
	}

	halo := entries[2]
	if halo.Game != "Halo Infinite" || halo.Status != "Works" || halo.AntiCheat != "Easy Anti-Cheat" {
		t.Errorf("unexpected entry: %#v", halo)
	}
	if halo.WorksWithProton != "Yes" || halo.WorksWithNativeLinux != "No" {
		t.Errorf("proton/native flags = %q/%q, want Yes/No", halo.WorksWithProton, halo.WorksWithNativeLinux)
	}
	if want := "Needs Proton Experimental., GE-Proton 10-30 also works."; halo.Notes != want {
		t.Errorf("notes = %q, want %q", halo.Notes, want)
	}

	brokenArrow := entries[3]
	if want := "Broken Arrow Anti-Cheat System, Easy Anti-Cheat"; brokenArrow.AntiCheat != want {
		t.Errorf("multi-value anti-cheat = %q, want %q", brokenArrow.AntiCheat, want)
	}
	if strings.ContainsAny(brokenArrow.AntiCheat, "\r\n") {
		t.Errorf("anti-cheat must be single line, got %q", brokenArrow.AntiCheat)
	}

	if entries[0].Notes != "" || entries[0].SinglePlayerOnly != "" {
		t.Errorf("empty cells should stay empty: %#v", entries[0])
	}
}

func TestParseDataset_HeaderOrderAndExtraColumns(t *testing.T) {
	body := "Status,Notes,\"Game Title\",Unexpected Column\n" +
		"Broken,Some note,Some Game,ignored\n"

	entries, err := parseDataset(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseDataset() error = %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].Game != "Some Game" || entries[0].Status != "Broken" || entries[0].Notes != "Some note" {
		t.Errorf("unexpected entry: %#v", entries[0])
	}
	if entries[0].AntiCheat != "" {
		t.Errorf("missing column should yield empty value, got %q", entries[0].AntiCheat)
	}
}

func TestParseDataset_SkipsRowsWithoutGameTitle(t *testing.T) {
	body := "\"Game Title\",Status\n,Works\nGame,Works\n,\n"
	entries, err := parseDataset(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parseDataset() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Game != "Game" {
		t.Fatalf("unexpected entries: %#v", entries)
	}
}

func TestParseDataset_Errors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"header only", "\"Game Title\",Status\n"},
		{"missing game title column", "Status,Notes\nWorks,note\n"},
		{"missing status column", "\"Game Title\",Notes\nGame,note\n"},
		{"ragged record", "\"Game Title\",Status\n\"Game\",\"Works\"\n\"Broken\",\"unterminated\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseDataset(strings.NewReader(tt.body)); err == nil {
				t.Fatal("expected an error, got nil")
			}
		})
	}
}

func TestSanitizeField_CollapsesWhitespaceAndEmptyParts(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Easy Anti-Cheat", "Easy Anti-Cheat"},
		{"  Easy   Anti-Cheat  ", "Easy Anti-Cheat"},
		{"Arbiter, Easy Anti-Cheat", "Arbiter, Easy Anti-Cheat"},
		{"Broken Arrow Anti-Cheat System\n, Easy Anti-Cheat", "Broken Arrow Anti-Cheat System, Easy Anti-Cheat"},
		{"line one\r\nline two", "line one, line two"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := sanitizeField(tt.in); got != tt.want {
			t.Errorf("sanitizeField(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSanitizeField_RewritesNotesMarkup(t *testing.T) {
	// Cells like this come straight out of the export; a bare URL in a Discord
	// message would attach a link preview, hence the angle brackets.
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "anchor becomes text plus bracketed url",
			in:   `Playable if you <a href="https://example.com/guide/">manually update PunkBuster</a>.`,
			want: `Playable if you manually update PunkBuster (<https://example.com/guide/>).`,
		},
		{
			name: "anchor with extra attributes and comma in text",
			in:   `<a class="x" href="https://example.com/a" target="_blank">reads well, mostly</a>`,
			want: `reads well, mostly (<https://example.com/a>)`,
		},
		{
			name: "bare url is angle bracketed",
			in:   "See https://example.com/notes for details.",
			want: "See <https://example.com/notes> for details.",
		},
		{
			name: "surrounding tags are dropped",
			in:   `<p>&amp; then <b>some</b> text</p>`,
			want: `& then some text`,
		},
		{
			name: "plain hyphens and ampersands survive",
			in:   "Boot Protection - Requires both Secure Boot & TPM 2.0 as well.",
			want: "Boot Protection - Requires both Secure Boot & TPM 2.0 as well.",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeField(tt.in)
			if got != tt.want {
				t.Errorf("sanitizeField(%q) = %q, want %q", tt.in, got, tt.want)
			}
			if strings.Contains(got, "<a ") || strings.Contains(got, "</a>") {
				t.Errorf("anchor markup survived: %q", got)
			}
			if strings.Contains(got, "<<") || strings.Contains(got, ">>") {
				t.Errorf("url wrapping produced nested angle brackets: %q", got)
			}
		})
	}
}

func TestNeutralizeText_DefusesMentionsAndBackticks(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		forbidden   []string
		wantContain []string
	}{
		{
			name:        "@everyone",
			in:          "@everyone",
			forbidden:   []string{"@everyone"},
			wantContain: []string{"@" + zeroWidthSpace + "everyone"},
		},
		{
			name:        "user mention token",
			in:          "ping <@123456789012345678> now",
			forbidden:   []string{"<@123456789012345678>"},
			wantContain: []string{"@", "123456789012345678"},
		},
		{
			name:        "role mention token",
			in:          "<@&987654321098765432>",
			forbidden:   []string{"<@&987654321098765432>"},
			wantContain: []string{"987654321098765432"},
		},
		{
			name:        "backtick ends the code span",
			in:          "x` @everyone `y",
			forbidden:   []string{"`", "@everyone"},
			wantContain: []string{"'", "everyone"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := neutralizeText(tt.in)
			for _, forbidden := range tt.forbidden {
				if strings.Contains(got, forbidden) {
					t.Errorf("neutralizeText(%q) = %q, still contains %q", tt.in, got, forbidden)
				}
			}
			for _, want := range tt.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("neutralizeText(%q) = %q, missing %q", tt.in, got, want)
				}
			}
		})
	}
}

func TestFormatResults_NeutralizesUntrustedText(t *testing.T) {
	entries := []entry{
		{
			Game:      "Sneaky @everyone Game",
			Status:    "Works",
			AntiCheat: "<@112233445566778899> Guard",
			// Data reaches the renderer through parseDataset, which sanitizes
			// each cell; keep that step in the fixture so the assertions cover
			// the real pipeline.
			Notes: sanitizeField("Read https://example.com/evil and ping @here"),
		},
	}

	tests := []struct {
		name  string
		query string
	}{
		{name: "query matches the entry", query: "sneaky"},
		{name: "no match, query is reflected", query: "nothing @everyone `tick"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := formatResults(entries, tt.query)
			for _, forbidden := range []string{"@everyone", "@here", "<@112233445566778899>"} {
				if strings.Contains(out, forbidden) {
					t.Errorf("reply contains live token %q:\n%s", forbidden, out)
				}
			}
			if strings.Count(out, "`")%2 != 0 {
				t.Errorf("reply has an unbalanced code span:\n%s", out)
			}
			if strings.Contains(out, "https://example.com/evil") && !strings.Contains(out, "<https://example.com/evil>") {
				t.Errorf("bare url survived:\n%s", out)
			}
		})
	}
}

func searchFixture() []entry {
	return []entry{
		{Game: "Halo", Status: "Works"},
		{Game: "Halo Infinite", Status: "Works"},
		{Game: "Combat Halo", Status: "Broken"},
		{Game: "Exhalo", Status: "Broken"},
		{Game: "Halo Wars 2", Status: "Reports Needed"},
		{Game: "Unrelated", Status: "Broken", AntiCheat: "Halo Guard"},
	}
}

func TestSearchEntries_RanksTitleMatchesAboveVendorMatches(t *testing.T) {
	got := searchEntries(searchFixture(), "HALO")
	var names []string
	for _, e := range got {
		names = append(names, e.Game)
	}
	want := []string{"Halo", "Halo Infinite", "Halo Wars 2", "Combat Halo", "Exhalo", "Unrelated"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Errorf("search order = %v, want %v", names, want)
	}
}

func TestSearchEntries_NormalizesWhitespaceAndCase(t *testing.T) {
	fixture := []entry{{Game: "  Deep   Rock  Galactic ", Status: "Works"}}
	if got := searchEntries(fixture, "deep rock"); len(got) != 1 {
		t.Errorf("search with collapsed whitespace returned %d matches, want 1", len(got))
	}
	if got := searchEntries(fixture, "deep galactic"); len(got) != 0 {
		t.Errorf("non-substring query returned %d matches, want 0", len(got))
	}
}

func TestSearchEntries_NoMatchAndEmptyQuery(t *testing.T) {
	if got := searchEntries(searchFixture(), "nothing here"); len(got) != 0 {
		t.Errorf("expected no matches, got %#v", got)
	}
	if got := searchEntries(searchFixture(), "   "); got != nil {
		t.Errorf("blank query should return nil, got %#v", got)
	}
}

func TestSearchEntries_ReturnsEveryMatch(t *testing.T) {
	fixture := make([]entry, 12)
	for i := range fixture {
		fixture[i] = entry{Game: fmt.Sprintf("Game %02d", i), Status: "Works"}
	}
	if got := searchEntries(fixture, "game"); len(got) != 12 {
		t.Errorf("got %d matches, want all 12 (limiting is the renderer's job)", len(got))
	}
}

func TestFormatResults_IncludesFieldsAndSourceLink(t *testing.T) {
	entries, err := parseDataset(strings.NewReader(sampleCSV))
	if err != nil {
		t.Fatal(err)
	}

	out := formatResults(entries, "7 days to die")

	for _, want := range []string{
		"1 match(es) for `7 days to die`",
		"**7 Days to Die** — Works",
		"Anti-cheat: Easy Anti-Cheat",
		"Works with Proton: Yes",
		"Works with native Linux: Yes",
		"<https://www.gamingonlinux.com/anticheat/>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reply is missing %q:\n%s", want, out)
		}
	}
}

func TestFormatResults_NoMatchMentionsQueryAndSource(t *testing.T) {
	entries, _ := parseDataset(strings.NewReader(sampleCSV))
	out := formatResults(entries, "half-life")
	if !strings.Contains(out, "No entry for `half-life`") {
		t.Errorf("unexpected no-match reply:\n%s", out)
	}
	if !strings.Contains(out, "<https://www.gamingonlinux.com/anticheat/>") {
		t.Errorf("no-match reply should still link the source:\n%s", out)
	}
}

func TestFormatResults_LimitsListedMatches(t *testing.T) {
	fixture := make([]entry, 12)
	for i := range fixture {
		fixture[i] = entry{Game: fmt.Sprintf("Game %02d", i), Status: "Works"}
	}

	out := formatResults(fixture, "game")

	if got := strings.Count(out, "**Game "); got != maxResults {
		t.Errorf("listed %d entries, want %d:\n%s", got, maxResults, out)
	}
	if !strings.Contains(out, "…and 7 more matches.") {
		t.Errorf("missing remainder line:\n%s", out)
	}
}

func TestFormatResults_StaysWithinDiscordLimit(t *testing.T) {
	notes := strings.Repeat("long note ", 100)
	fixture := make([]entry, 40)
	for i := range fixture {
		fixture[i] = entry{Game: fmt.Sprintf("Game %02d %s", i, strings.Repeat("x", 60)), Status: "Works", Notes: notes}
	}

	out := formatResults(fixture, "game")

	if got := len([]rune(out)); got > messageLimit {
		t.Errorf("reply length = %d runes, want <= %d", got, messageLimit)
	}
}

func TestGetDataset_SharesOneFetchBetweenConcurrentCallers(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	m := newTestModule(func(context.Context) ([]byte, error) {
		calls.Add(1)
		once.Do(func() { close(started) })
		<-release
		return []byte(sampleCSV), nil
	})

	const workers = 8
	results := make(chan int, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entries, err := m.getDataset(context.Background())
			if err != nil {
				results <- -1
				return
			}
			results <- len(entries)
		}()
	}

	<-started
	close(release)
	wg.Wait()
	close(results)

	if got := calls.Load(); got != 1 {
		t.Errorf("dataset fetched %d times, want 1 (single-flight)", got)
	}
	for got := range results {
		if got != 4 {
			t.Errorf("caller got %d entries, want 4", got)
		}
	}
}

func TestGetDataset_CachesUntilTTLExpires(t *testing.T) {
	var calls atomic.Int32
	m := newTestModule(func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte(sampleCSV), nil
	})
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }

	if _, err := m.getDataset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := m.getDataset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("dataset fetched %d times within TTL, want 1", got)
	}

	now = now.Add(datasetCacheTTL + time.Second)
	if _, err := m.getDataset(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("dataset fetched %d times after TTL expiry, want 2", got)
	}
}

func TestGetDataset_DoesNotCacheFailures(t *testing.T) {
	var calls atomic.Int32
	m := newTestModule(func(context.Context) ([]byte, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("upstream down")
		}
		return []byte(sampleCSV), nil
	})

	if _, err := m.getDataset(context.Background()); err == nil {
		t.Fatal("expected the first call to fail")
	}
	if m.cachedDataset() != nil {
		t.Error("a failed fetch must not populate the cache")
	}
	entries, err := m.getDataset(context.Background())
	if err != nil {
		t.Fatalf("retry should succeed, got %v", err)
	}
	if len(entries) != 4 {
		t.Errorf("retry returned %d entries, want 4", len(entries))
	}
}

func TestGetDataset_WaiterHonoursContextCancellation(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var once sync.Once
	m := newTestModule(func(context.Context) ([]byte, error) {
		once.Do(func() { close(started) })
		<-release
		return []byte(sampleCSV), nil
	})

	leader := make(chan int, 1)
	go func() {
		entries, err := m.getDataset(context.Background())
		if err != nil {
			leader <- -1
			return
		}
		leader <- len(entries)
	}()

	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.getDataset(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("waiting caller error = %v, want context.DeadlineExceeded", err)
	}

	close(release)
	if got := <-leader; got != 4 {
		t.Errorf("leading caller got %d entries, want 4", got)
	}
}

func TestRefreshLoop_FetchesAndStopsWithContext(t *testing.T) {
	var calls atomic.Int32
	m := newTestModule(func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte(sampleCSV), nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.refreshLoop(ctx)
		close(done)
	}()

	deadline := time.After(time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for the initial dataset fetch")
		case <-time.After(time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refreshLoop did not stop after context cancellation")
	}
}

func TestCommandsMetadata(t *testing.T) {
	commands := New().Commands()
	if len(commands) != 1 {
		t.Fatalf("got %d commands, want 1", len(commands))
	}
	command := commands[0]
	if command.Name != commandName {
		t.Errorf("command name = %q, want %q", command.Name, commandName)
	}
	if len(command.Options) != 1 {
		t.Fatalf("got %d options, want 1", len(command.Options))
	}
	option := command.Options[0]
	if option.Name != optionName || !option.Required || !option.Autocomplete || option.MaxLength != 100 {
		t.Errorf("option metadata = %#v", option)
	}
}

func TestInit_NeedsNoDependencies(t *testing.T) {
	if err := New().Init(bot.Deps{}); err != nil {
		t.Errorf("Init() error = %v, want nil", err)
	}
}

func TestModuleMetadata(t *testing.T) {
	m := New()
	if m.Name() != "anticheat" {
		t.Errorf("Name() = %q, want %q", m.Name(), "anticheat")
	}
	if len(m.Listeners()) != 1 {
		t.Errorf("got %d listeners, want 1", len(m.Listeners()))
	}
	tasks := m.Background()
	if len(tasks) != 1 || tasks[0].Name == "" || tasks[0].Run == nil {
		t.Errorf("background tasks = %#v, want one named task", tasks)
	}
	if m.Components() != nil || m.Shutdown() != nil {
		t.Error("Components() and Shutdown() should be no-ops")
	}
}

type discordRequest struct {
	method string
	path   string
	body   []byte
}

func anticheatDiscordSession(t *testing.T) (*discordgo.Session, <-chan discordRequest) {
	t.Helper()
	requests := make(chan discordRequest, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requests <- discordRequest{method: r.Method, path: r.URL.Path, body: body}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	origAPI, origWebhooks := discordgo.EndpointAPI, discordgo.EndpointWebhooks
	discordgo.EndpointAPI = server.URL + "/"
	discordgo.EndpointWebhooks = server.URL + "/webhooks/"
	t.Cleanup(func() {
		discordgo.EndpointAPI = origAPI
		discordgo.EndpointWebhooks = origWebhooks
	})
	s, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	return s, requests
}

func anticheatInteraction(interactionType discordgo.InteractionType, name string, query *string) *discordgo.InteractionCreate {
	data := discordgo.ApplicationCommandInteractionData{Name: name}
	if query != nil {
		data.Options = []*discordgo.ApplicationCommandInteractionDataOption{{Name: optionName, Value: *query}}
	}
	return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		ID: "interaction", AppID: "app", Token: "token", Type: interactionType,
		ChannelID: "channel", Data: data,
	}}
}

func awaitRequest(t *testing.T, requests <-chan discordRequest) discordRequest {
	t.Helper()
	select {
	case req := <-requests:
		return req
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a Discord request")
		return discordRequest{}
	}
}

func TestLookupHandler_DefersThenEditsWithResults(t *testing.T) {
	m := newTestModule(staticFetch(sampleCSV))
	s, requests := anticheatDiscordSession(t)

	query := "7 days to die"
	m.onInteractionCreate(s, anticheatInteraction(discordgo.InteractionApplicationCommand, commandName, &query))

	deferReq := awaitRequest(t, requests)
	var deferred discordgo.InteractionResponse
	if err := json.Unmarshal(deferReq.body, &deferred); err != nil {
		t.Fatal(err)
	}
	if deferred.Type != discordgo.InteractionResponseDeferredChannelMessageWithSource {
		t.Errorf("first response type = %v, want deferred", deferred.Type)
	}
	if deferReq.method != http.MethodPost {
		t.Errorf("first request method = %s, want POST", deferReq.method)
	}

	editReq := awaitRequest(t, requests)
	var edit struct {
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(editReq.body, &edit); err != nil {
		t.Fatal(err)
	}
	if editReq.method != http.MethodPatch {
		t.Errorf("second request method = %s, want PATCH", editReq.method)
	}
	if edit.Content == nil || !strings.Contains(*edit.Content, "**7 Days to Die** — Works") {
		t.Errorf("edited content = %v, want the matching game", edit.Content)
	}
}

func TestLookupHandler_ReportsFetchFailure(t *testing.T) {
	m := newTestModule(func(context.Context) ([]byte, error) { return nil, errors.New("offline") })
	s, requests := anticheatDiscordSession(t)

	query := "halo"
	m.onInteractionCreate(s, anticheatInteraction(discordgo.InteractionApplicationCommand, commandName, &query))
	awaitRequest(t, requests) // deferred

	editReq := awaitRequest(t, requests)
	var edit struct {
		Content *string `json:"content"`
	}
	if err := json.Unmarshal(editReq.body, &edit); err != nil {
		t.Fatal(err)
	}
	if edit.Content == nil || !strings.Contains(*edit.Content, "Failed to reach the GamingOnLinux Anti-Cheat list") {
		t.Errorf("edited content = %v, want the failure notice", edit.Content)
	}
}

func TestLookupHandler_IgnoresOtherCommands(t *testing.T) {
	m := newTestModule(staticFetch(sampleCSV))
	s, requests := anticheatDiscordSession(t)

	m.onInteractionCreate(s, anticheatInteraction(discordgo.InteractionApplicationCommand, "wttr", nil))

	select {
	case req := <-requests:
		t.Errorf("unexpected Discord request for a foreign command: %s %s", req.method, req.path)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestLookupHandler_SuppressesMentionsAndBackticks(t *testing.T) {
	m := newTestModule(staticFetch(sampleCSV))
	s, requests := anticheatDiscordSession(t)

	query := "nothing ` @everyone"
	m.onInteractionCreate(s, anticheatInteraction(discordgo.InteractionApplicationCommand, commandName, &query))
	awaitRequest(t, requests) // deferred

	editReq := awaitRequest(t, requests)
	body := string(editReq.body)
	if !strings.Contains(body, `"allowed_mentions":{"parse":[]`) {
		t.Errorf("edit payload does not disable mention parsing: %s", body)
	}
	if strings.Contains(body, "@everyone") {
		t.Errorf("reflected query kept a live mention token: %s", body)
	}
	if strings.Contains(body, "` ") && !strings.Contains(body, "' ") {
		t.Errorf("reflected query kept a raw backtick: %s", body)
	}
}

func TestLookupHandler_HintReplyDisablesMentions(t *testing.T) {
	m := newTestModule(staticFetch(sampleCSV))
	s, requests := anticheatDiscordSession(t)

	query := "   "
	m.onInteractionCreate(s, anticheatInteraction(discordgo.InteractionApplicationCommand, commandName, &query))

	req := awaitRequest(t, requests)
	body := string(req.body)
	if !strings.Contains(body, "Provide a game title or anti-cheat vendor") {
		t.Errorf("unexpected hint reply: %s", body)
	}
	if !strings.Contains(body, `"allowed_mentions":{"parse":[]`) {
		t.Errorf("hint reply does not disable mention parsing: %s", body)
	}
}

func TestAutocompleteHandler_OffersRankedChoicesFromCache(t *testing.T) {
	m := newTestModule(staticFetch(sampleCSV))
	if _, err := m.getDataset(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, requests := anticheatDiscordSession(t)

	query := "7 days"
	m.onInteractionCreate(s, anticheatInteraction(discordgo.InteractionApplicationCommandAutocomplete, commandName, &query))

	req := awaitRequest(t, requests)
	var response struct {
		Type int `json:"type"`
		Data struct {
			Choices []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"choices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(req.body, &response); err != nil {
		t.Fatal(err)
	}
	if discordgo.InteractionResponseType(response.Type) != discordgo.InteractionApplicationCommandAutocompleteResult {
		t.Errorf("response type = %d, want the autocomplete result type", response.Type)
	}
	if len(response.Data.Choices) == 0 {
		t.Fatal("expected autocomplete choices, got none")
	}
	if got := response.Data.Choices[0].Name; got != "7 Days to Die — Works" {
		t.Errorf("first choice = %q, want %q", got, "7 Days to Die — Works")
	}
	if got := response.Data.Choices[0].Value; got != "7 Days to Die" {
		t.Errorf("first choice value = %q, want the plain game title", got)
	}
}

func TestAutocompleteHandler_ColdCacheDoesNotFetch(t *testing.T) {
	var calls atomic.Int32
	m := newTestModule(func(context.Context) ([]byte, error) {
		calls.Add(1)
		return []byte(sampleCSV), nil
	})
	s, requests := anticheatDiscordSession(t)

	query := "halo"
	m.onInteractionCreate(s, anticheatInteraction(discordgo.InteractionApplicationCommandAutocomplete, commandName, &query))

	req := awaitRequest(t, requests)
	var response struct {
		Data struct {
			Choices []json.RawMessage `json:"choices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(req.body, &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data.Choices) != 0 {
		t.Errorf("cold cache should answer with no choices, got %d", len(response.Data.Choices))
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("autocomplete fetched the dataset %d times, want 0 (3 s window)", got)
	}
}
