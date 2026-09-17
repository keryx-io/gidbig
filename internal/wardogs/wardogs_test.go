package wardogs

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestModule() *Module {
	return &Module{
		fetchFn: func(context.Context) (string, error) { return "NO", nil },
		now:     time.Now,
	}
}

func TestFormatStatus(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		contains string
	}{
		{name: "yes", raw: "YES", contains: "✅"},
		{name: "no", raw: "NO", contains: "❌"},
		{name: "lowercase and padded", raw: "  yes ", contains: "✅"},
		{name: "unknown", raw: "MAYBE", contains: "Unknown"},
		{name: "empty", raw: "", contains: "Unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatStatus(tt.raw)
			if !strings.Contains(got, tt.contains) {
				t.Errorf("formatStatus(%q) = %q, want it to contain %q", tt.raw, got, tt.contains)
			}
		})
	}
}

func TestFormatStatusListsSourcesWithoutPreviews(t *testing.T) {
	got := formatStatus("NO")

	for _, url := range []string{SiteURL, SteamDiscussionsURL} {
		if !strings.Contains(got, "<"+url+">") {
			t.Errorf("formatStatus = %q, want %q wrapped in angle brackets", got, url)
		}
	}

	// Angle brackets around every URL are what suppresses Discord's link preview.
	if bare, wrapped := strings.Count(got, "https://"), strings.Count(got, "<https://"); bare != wrapped {
		t.Errorf("formatStatus = %q, want all %d URLs wrapped in angle brackets", got, bare)
	}

	// The pinned Linux statement must not be cited as a standing source: it goes
	// stale as soon as the verdict flips to YES.
	if strings.Contains(got, "588436698284962639") {
		t.Errorf("formatStatus = %q, want no pinned statement link", got)
	}
	if !strings.Contains(got, "check manually") {
		t.Errorf("formatStatus = %q, want the forum link labelled for a manual check", got)
	}
}

func TestFormatStatusUnknownEchoesRawValue(t *testing.T) {
	got := formatStatus("MAYBE")

	if !strings.Contains(got, "`MAYBE`") {
		t.Errorf("formatStatus = %q, want the raw tracker value", got)
	}
}

func TestStatusCachedUsesCacheWithinTTL(t *testing.T) {
	m := newTestModule()
	now := time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	calls := 0
	m.fetchFn = func(context.Context) (string, error) {
		calls++
		return "NO", nil
	}

	for i := 0; i < 3; i++ {
		got, err := m.statusCached(context.Background())
		if err != nil {
			t.Fatalf("statusCached returned error: %v", err)
		}
		if got != "NO" {
			t.Fatalf("statusCached = %q, want %q", got, "NO")
		}
	}

	if calls != 1 {
		t.Errorf("fetch called %d times, want 1 within the cache TTL", calls)
	}
}

func TestStatusCachedRefetchesAfterTTL(t *testing.T) {
	m := newTestModule()
	now := time.Date(2026, 9, 17, 8, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	calls := 0
	m.fetchFn = func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "NO", nil
		}
		return "YES", nil
	}

	if _, err := m.statusCached(context.Background()); err != nil {
		t.Fatalf("first statusCached returned error: %v", err)
	}
	now = now.Add(statusCacheTTL + time.Second)
	got, err := m.statusCached(context.Background())
	if err != nil {
		t.Fatalf("second statusCached returned error: %v", err)
	}
	if got != "YES" {
		t.Errorf("statusCached after TTL = %q, want %q", got, "YES")
	}
	if calls != 2 {
		t.Errorf("fetch called %d times, want 2 after the cache expired", calls)
	}
}

func TestStatusCachedDoesNotCacheErrors(t *testing.T) {
	m := newTestModule()
	calls := 0
	m.fetchFn = func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "", errors.New("boom")
		}
		return "NO", nil
	}

	if _, err := m.statusCached(context.Background()); err == nil {
		t.Fatal("expected an error from the first statusCached call")
	}
	got, err := m.statusCached(context.Background())
	if err != nil {
		t.Fatalf("second statusCached returned error: %v", err)
	}
	if got != "NO" {
		t.Errorf("statusCached = %q, want %q", got, "NO")
	}
}

func TestStatusCachedSingleFlightOnColdCache(t *testing.T) {
	m := newTestModule()
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	m.fetchFn = func(context.Context) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return "NO", nil
	}

	const callers = 12
	results := make([]string, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = m.statusCached(context.Background())
		}(i)
	}

	waitForFetchStart(t, &mu, &calls)
	close(release)
	wg.Wait()

	if calls != 1 {
		t.Errorf("fetch called %d times for %d concurrent cold-cache callers, want 1", calls, callers)
	}
	for i := range results {
		if errs[i] != nil {
			t.Errorf("caller %d returned error: %v", i, errs[i])
		}
		if results[i] != "NO" {
			t.Errorf("caller %d got %q, want %q", i, results[i], "NO")
		}
	}
}

func TestStatusCachedSingleFlightSharesErrorAndRetries(t *testing.T) {
	m := newTestModule()
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	m.fetchFn = func(context.Context) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return "", errors.New("boom")
	}

	const callers = 6
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, errs[idx] = m.statusCached(context.Background())
		}(i)
	}

	waitForFetchStart(t, &mu, &calls)
	close(release)
	wg.Wait()

	if calls != 1 {
		t.Errorf("fetch called %d times, want 1", calls)
	}
	for i := range errs {
		if errs[i] == nil {
			t.Errorf("caller %d expected the shared fetch error", i)
		}
	}

	// A failed fetch is not cached, and the failed call must not stay in flight.
	m.fetchFn = func(context.Context) (string, error) { return "NO", nil }
	got, err := m.statusCached(context.Background())
	if err != nil {
		t.Fatalf("retry after a failed fetch returned error: %v", err)
	}
	if got != "NO" {
		t.Errorf("retry after a failed fetch = %q, want %q", got, "NO")
	}
}

func TestStatusCachedWaiterHonoursContext(t *testing.T) {
	m := newTestModule()
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	m.fetchFn = func(context.Context) (string, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return "NO", nil
	}

	fetching := make(chan struct{})
	go func() {
		defer close(fetching)
		_, _ = m.statusCached(context.Background())
	}()

	waitForFetchStart(t, &mu, &calls)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.statusCached(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("waiter error = %v, want context.Canceled", err)
	}

	close(release)
	<-fetching

	m.mu.Lock()
	inflight := m.inflight
	m.mu.Unlock()
	if inflight != nil {
		t.Error("in-flight call was not cleared")
	}
}

// waitForFetchStart blocks until the injected fetch has been entered, so the tests
// can be sure the callers they add afterwards join an in-flight fetch.
func waitForFetchStart(t *testing.T, mu *sync.Mutex, calls *int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := *calls
		mu.Unlock()
		if n >= 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the injected fetch was never entered")
}

func TestComposeStatusPropagatesError(t *testing.T) {
	m := newTestModule()
	m.fetchFn = func(context.Context) (string, error) { return "", errors.New("boom") }

	if _, err := m.composeStatus(context.Background()); err == nil {
		t.Fatal("expected composeStatus to propagate the fetch error")
	}
}

func TestComposeStatusFormatsFetchResult(t *testing.T) {
	m := newTestModule()

	got, err := m.composeStatus(context.Background())
	if err != nil {
		t.Fatalf("composeStatus returned error: %v", err)
	}
	if !strings.Contains(got, "❌") {
		t.Errorf("composeStatus = %q, want a negative verdict", got)
	}
}

func TestFetchStatusFromParsesJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept header = %q, want application/json", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"NO"}`))
	}))
	defer server.Close()

	got, err := fetchStatusFrom(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("fetchStatusFrom returned error: %v", err)
	}
	if got != "NO" {
		t.Errorf("fetchStatusFrom = %q, want %q", got, "NO")
	}
}

func TestFetchStatusFromErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{name: "server error", status: http.StatusInternalServerError, body: `{"status":"NO"}`, wantErr: "unexpected status code 500"},
		{name: "malformed json", status: http.StatusOK, body: `{"status":`, wantErr: "decode status response"},
		{name: "empty status", status: http.StatusOK, body: `{"status":"  "}`, wantErr: "did not contain a status value"},
		{name: "html body", status: http.StatusOK, body: `<html></html>`, wantErr: "decode status response"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			_, err := fetchStatusFrom(context.Background(), server.URL)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestFetchStatusFromHonoursContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"NO"}`))
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := fetchStatusFrom(ctx, server.URL); err == nil {
		t.Fatal("expected a cancelled context to fail the request")
	}
}

func TestFetchStatusFromRejectsUnreachableEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	if _, err := fetchStatusFrom(context.Background(), url); err == nil {
		t.Fatal("expected an error for an unreachable endpoint")
	}
}
