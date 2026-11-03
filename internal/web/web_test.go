package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorolens/internal/source"
	"github.com/sorotrail/sorolens/internal/source/sorotrailsource"
)

const testContract = "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC"

// fakeSource is a canned source.EventSource for rendering tests.
type fakeSource struct {
	contracts     source.ContractPage
	events        source.EventPage
	event         source.Event
	stats         source.Stats
	contractStats source.ContractStats
	status        source.Status
	err           error

	lastEventQuery    source.EventQuery
	lastContractQuery source.ContractQuery
	listContractsFunc func(context.Context, source.ContractQuery) (source.ContractPage, error)
}

func (f *fakeSource) ListContracts(ctx context.Context, q source.ContractQuery) (source.ContractPage, error) {
	f.lastContractQuery = q
	if f.listContractsFunc != nil {
		return f.listContractsFunc(ctx, q)
	}
	return f.contracts, f.err
}

func (f *fakeSource) ListEvents(_ context.Context, q source.EventQuery) (source.EventPage, error) {
	f.lastEventQuery = q
	return f.events, f.err
}

func (f *fakeSource) GetEvent(context.Context, string) (source.Event, error) {
	return f.event, f.err
}

func (f *fakeSource) ContractStats(context.Context, string) (source.ContractStats, error) {
	return f.contractStats, f.err
}

func (f *fakeSource) Stats(context.Context) (source.Stats, error) { return f.stats, f.err }
func (f *fakeSource) Status(context.Context) source.Status        { return f.status }

// sampleEvent is a realistic decoded event used across the rendering tests.
func sampleEvent() source.Event {
	return source.Event{
		ID:               "0000000100-0000000001",
		ContractID:       testContract,
		Ledger:           100,
		Type:             "contract",
		TxHash:           "b1946ac92492d2347c6235b4d2611184",
		TxIndex:          1,
		OpIndex:          2,
		InSuccessfulCall: true,
		Topics:           json.RawMessage(`[{"symbol":"transfer"},{"address":"GABC"}]`),
		Value:            json.RawMessage(`{"i128":"1000"}`),
		LedgerClosedAt:   time.Unix(1700000000, 0).UTC(),
		CreatedAt:        time.Unix(1700000100, 0).UTC(),
	}
}

func newTestServer(t *testing.T, src source.EventSource) http.Handler {
	t.Helper()
	s, err := New(src, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	return s.Routes()
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// TestTemplatesParse is the guard against a template typo shipping: templates
// are embedded and only parsed at startup, so a broken one would otherwise
// surface as a runtime failure in production rather than a failing build.
func TestTemplatesParse(t *testing.T) {
	_, err := New(&fakeSource{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
}

func TestIndexRenders(t *testing.T) {
	src := &fakeSource{
		stats: source.Stats{
			TotalEvents:   1234,
			ContractCount: 7,
			FirstLedger:   100,
			LastLedger:    900,
			TypeBreakdown: []source.TypeCount{{Type: "contract", Count: 1200}},
		},
		events: source.EventPage{Events: []source.Event{sampleEvent()}},
		status: source.Status{Mode: "rpc", Healthy: true, RetentionNote: "only while running"},
	}

	rec := get(t, newTestServer(t, src), "/")
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.Contains(t, body, "1234", "total event count should be shown")
	assert.Contains(t, body, "SoroLens")
	// The decoded event name, not the raw JSON wrapper, is what users need.
	assert.Contains(t, body, "transfer")
	// The retention caveat must be visible so an empty explorer is explicable.
	assert.Contains(t, body, "only while running")
}

func TestIndexShowsUnhealthyBanner(t *testing.T) {
	src := &fakeSource{
		status: source.Status{Mode: "rpc", Healthy: false, Detail: "database unreachable"},
		events: source.EventPage{},
	}

	rec := get(t, newTestServer(t, src), "/")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "database unreachable")
}

func TestContractsPageRenders(t *testing.T) {
	src := &fakeSource{
		contracts: source.ContractPage{
			Contracts: []source.Contract{{
				ID:           testContract,
				EventCount:   42,
				FirstLedger:  100,
				LastLedger:   200,
				LastActivity: time.Unix(1700000000, 0).UTC(),
			}},
			NextCursor: "abc",
		},
		status: source.Status{Healthy: true},
	}

	rec := get(t, newTestServer(t, src), "/contracts")
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.Contains(t, body, "42")
	assert.Contains(t, body, "/contracts/"+testContract, "each row should link to the contract")
	assert.Contains(t, body, "Next page", "a next cursor should produce a paging link")
}

func TestContractsPageEmptyState(t *testing.T) {
	src := &fakeSource{contracts: source.ContractPage{}, status: source.Status{Healthy: true}}

	rec := get(t, newTestServer(t, src), "/contracts")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "No contracts to show")
	assert.Equal(t, source.DefaultLimit, src.lastContractQuery.Limit, "default limit must come from source.NormalizeLimit")
}

func TestContractsPaginationTwoPagesAndSearchFilter(t *testing.T) {
	allContracts := []source.Contract{
		{ID: "CTOKEN00000000000000000000000000000000000000000000000001", EventCount: 10, FirstLedger: 10, LastLedger: 500},
		{ID: "CDEX0000000000000000000000000000000000000000000000000002", EventCount: 9, FirstLedger: 10, LastLedger: 400},
		{ID: "CTOKEN00000000000000000000000000000000000000000000000003", EventCount: 8, FirstLedger: 10, LastLedger: 300},
		{ID: "CTOKEN00000000000000000000000000000000000000000000000004", EventCount: 7, FirstLedger: 10, LastLedger: 200},
		{ID: "CTOKEN00000000000000000000000000000000000000000000000005", EventCount: 6, FirstLedger: 10, LastLedger: 100},
	}

	src := &fakeSource{
		status: source.Status{Healthy: true},
		listContractsFunc: func(_ context.Context, q source.ContractQuery) (source.ContractPage, error) {
			var filtered []source.Contract
			for _, c := range allContracts {
				if q.Search == "" || strings.Contains(strings.ToUpper(c.ID), strings.ToUpper(q.Search)) {
					filtered = append(filtered, c)
				}
			}
			start := 0
			if q.Cursor != "" {
				found := false
				for i, c := range filtered {
					if c.ID == q.Cursor {
						start = i + 1
						found = true
						break
					}
				}
				if !found {
					return source.ContractPage{}, source.ErrInvalidCursor
				}
			}
			limit := source.NormalizeLimit(q.Limit)
			end := min(start+limit, len(filtered))
			page := filtered[start:end]
			next := ""
			if end < len(filtered) && len(page) > 0 {
				next = page[len(page)-1].ID
			}
			return source.ContractPage{Contracts: page, NextCursor: next}, nil
		},
	}

	h := newTestServer(t, src)

	t.Run("unfiltered list pages through all results with no repeats and no gaps", func(t *testing.T) {
		var seen []string
		target := "/contracts?limit=2"

		for pageNum := 1; target != ""; pageNum++ {
			rec := get(t, h, target)
			require.Equal(t, http.StatusOK, rec.Code)
			body := rec.Body.String()

			for _, c := range allContracts {
				if strings.Contains(body, "/contracts/"+c.ID) {
					assert.NotContains(t, seen, c.ID, "contract %s repeated on page %d", c.ID, pageNum)
					seen = append(seen, c.ID)
				}
			}

			if pageNum == 1 {
				assert.Contains(t, body, "more contracts match this view")
				assert.NotContains(t, body, `rel="first"`, "page 1 should not show a first-page link")
			} else {
				assert.Contains(t, body, `rel="first"`, "continuation pages must offer a link back to the first page")
				assert.Contains(t, body, "← First page")
			}

			target = extractHrefByRel(body, "next")
			if target == "" {
				assert.Contains(t, body, "final page of results")
			}
		}

		require.Len(t, seen, len(allContracts), "every contract must be visited across pages without gaps")
		for i, c := range allContracts {
			assert.Equal(t, c.ID, seen[i])
		}
	})

	t.Run("search filter survives across pages", func(t *testing.T) {
		rec1 := get(t, h, "/contracts?search=token&limit=2")
		require.Equal(t, http.StatusOK, rec1.Code)
		body1 := rec1.Body.String()

		assert.Contains(t, body1, allContracts[0].ID)
		assert.Contains(t, body1, allContracts[2].ID)
		assert.NotContains(t, body1, allContracts[1].ID, "non-matching contract must be filtered out on page 1")

		nextURL := extractHrefByRel(body1, "next")
		require.NotEmpty(t, nextURL, "filtered page 1 should have a next page")
		assert.Contains(t, nextURL, "search=token", "next page URL must preserve the search filter")
		assert.Contains(t, nextURL, "limit=2", "next page URL must preserve the limit")

		rec2 := get(t, h, nextURL)
		require.Equal(t, http.StatusOK, rec2.Code)
		body2 := rec2.Body.String()

		assert.Equal(t, "token", src.lastContractQuery.Search, "page 2 request must still filter by search=token")
		assert.Contains(t, body2, allContracts[3].ID)
		assert.Contains(t, body2, allContracts[4].ID)
		assert.NotContains(t, body2, allContracts[1].ID, "non-matching contract must still be filtered out on page 2")
		assert.Empty(t, extractHrefByRel(body2, "next"), "filtered page 2 is the last page")

		firstURL := extractHrefByRel(body2, "first")
		assert.Equal(t, "/contracts?limit=2&search=token", firstURL, "first-page link must keep the active search filter")
	})
}

func TestContractsPageInvalidCursorRendersUsefulPage(t *testing.T) {
	firstPageContract := source.Contract{
		ID:          testContract,
		EventCount:  15,
		FirstLedger: 100,
		LastLedger:  200,
	}
	src := &fakeSource{
		status: source.Status{Healthy: true},
		listContractsFunc: func(_ context.Context, q source.ContractQuery) (source.ContractPage, error) {
			if q.Cursor != "" {
				return source.ContractPage{}, source.ErrInvalidCursor
			}
			return source.ContractPage{Contracts: []source.Contract{firstPageContract}}, nil
		},
	}

	rec := get(t, newTestServer(t, src), "/contracts?search=CDLZ&cursor=stale-or-other-mode-cursor")
	require.Equal(t, http.StatusBadRequest, rec.Code, "an invalid cursor must return 400 rather than 500")

	body := rec.Body.String()
	assert.Contains(t, body, "Invalid cursor.")
	assert.Contains(t, body, `role="alert"`)
	assert.Contains(t, body, "/contracts/"+testContract, "fallback should still render the first page of contracts")
	assert.Contains(t, body, `href="/contracts?search=CDLZ"`, "reset link should preserve the search filter")
}

func TestContractsPageUpstreamModePagingAndCrossModeCursor(t *testing.T) {
	c1 := "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4"
	c2 := "CBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBSC4"
	c3 := "CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCSC4"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/stats":
			_, _ = w.Write([]byte(`{"total_events":3,"last_ingested_ledger":300,"contract_count":3}`))
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok","checks":{"database":"ok"}}`))
		case "/events":
			_, _ = w.Write([]byte(`{"events":[` +
				`{"id":"0000000100-0000000001","contract_id":"` + c1 + `","ledger":100,"type":"contract","in_successful_call":true,"topics":[],"value":"null"},` +
				`{"id":"0000000200-0000000001","contract_id":"` + c2 + `","ledger":200,"type":"contract","in_successful_call":true,"topics":[],"value":"null"},` +
				`{"id":"0000000300-0000000001","contract_id":"` + c3 + `","ledger":300,"type":"contract","in_successful_call":true,"topics":[],"value":"null"}` +
				`],"cursor":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	src := sorotrailsource.New(upstream.URL, upstream.Client())
	h := newTestServer(t, src)

	rec1 := get(t, h, "/contracts?limit=2")
	require.Equal(t, http.StatusOK, rec1.Code)
	body1 := rec1.Body.String()
	assert.Contains(t, body1, c3)
	assert.Contains(t, body1, c2)
	assert.NotContains(t, body1, c1)

	nextURL := extractHrefByRel(body1, "next")
	require.NotEmpty(t, nextURL)

	rec2 := get(t, h, nextURL)
	require.Equal(t, http.StatusOK, rec2.Code)
	body2 := rec2.Body.String()
	assert.Contains(t, body2, c1)
	assert.NotContains(t, body2, c3)
	assert.NotContains(t, body2, c2)

	// A cursor encoded by standalone Postgres mode ("<ledger>|<contract_id>",
	// e.g. base64("300|CAAA...")) must not decode in upstream mode and must
	// render the 400 fallback page rather than a 500.
	standaloneCursor := "MzAwfGNhYWE"
	recBad := get(t, h, "/contracts?cursor="+standaloneCursor)
	require.Equal(t, http.StatusBadRequest, recBad.Code)
	assert.Contains(t, recBad.Body.String(), "Invalid cursor.")
	assert.Contains(t, recBad.Body.String(), c3, "first page of upstream contracts should still render")
}

func TestPartialContractsReturnsFragmentOnly(t *testing.T) {
	src := &fakeSource{
		contracts: source.ContractPage{
			Contracts:  []source.Contract{{ID: testContract, EventCount: 5, FirstLedger: 1, LastLedger: 2}},
			NextCursor: "opaque-next",
		},
		status: source.Status{Healthy: true},
	}
	h := newTestServer(t, src)

	rec := get(t, h, "/partials/contracts?search=CDLZ&limit=10")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.NotContains(t, body, "<html")
	assert.Contains(t, body, testContract)
	assert.Contains(t, body, `aria-label="Contracts pagination"`)

	recBadLimit := get(t, h, "/partials/contracts?limit=0")
	assert.Equal(t, http.StatusBadRequest, recBadLimit.Code)
}

func extractHrefByRel(html, rel string) string {
	needle := `rel="` + rel + `"`
	idx := strings.Index(html, needle)
	if idx < 0 {
		return ""
	}
	tagStart := strings.LastIndex(html[:idx], "<a ")
	if tagStart < 0 {
		return ""
	}
	tagEnd := strings.Index(html[tagStart:], ">")
	if tagEnd < 0 {
		return ""
	}
	tag := html[tagStart : tagStart+tagEnd]
	_, after, ok := strings.Cut(tag, `href="`)
	if !ok {
		return ""
	}
	href, _, ok := strings.Cut(after, `"`)
	if !ok {
		return ""
	}
	return strings.ReplaceAll(href, "&amp;", "&")
}

func TestContractDetailRenders(t *testing.T) {
	src := &fakeSource{
		contractStats: source.ContractStats{
			ContractID:    testContract,
			TotalEvents:   42,
			FirstLedger:   100,
			LastLedger:    200,
			TypeBreakdown: []source.TypeCount{{Type: "contract", Count: 42}},
		},
		events: source.EventPage{Events: []source.Event{sampleEvent()}},
		status: source.Status{Healthy: true},
	}

	rec := get(t, newTestServer(t, src), "/contracts/"+testContract)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.Contains(t, body, testContract)
	assert.Contains(t, body, "transfer")
	assert.Contains(t, body, "Filters", "the filter form should be present")
	assert.Contains(t, body, `id="events"`, "the htmx swap target should exist")
}

func TestContractDetailRejectsBadID(t *testing.T) {
	src := &fakeSource{status: source.Status{Healthy: true}}

	rec := get(t, newTestServer(t, src), "/contracts/not-a-contract")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "Invalid contract ID")
}

func TestContractDetailPassesFiltersThrough(t *testing.T) {
	src := &fakeSource{
		events: source.EventPage{},
		status: source.Status{Healthy: true},
	}

	rec := get(t, newTestServer(t, src), "/contracts/"+testContract+"?type=contract&from_ledger=50&to_ledger=150")
	require.Equal(t, http.StatusOK, rec.Code)

	q := src.lastEventQuery
	assert.Equal(t, testContract, q.ContractID)
	assert.Equal(t, "contract", q.Type)
	assert.Equal(t, int64(50), q.FromLedger)
	assert.Equal(t, int64(150), q.ToLedger)
}

func TestEventDetailRenders(t *testing.T) {
	src := &fakeSource{event: sampleEvent(), status: source.Status{Healthy: true}}

	rec := get(t, newTestServer(t, src), "/events/0000000100-0000000001")
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	// Everything the event detail page promises to show.
	assert.Contains(t, body, "transfer", "decoded topic")
	assert.Contains(t, body, "GABC", "decoded address topic")
	assert.Contains(t, body, "1000", "decoded value")
	assert.Contains(t, body, "b1946ac92492d2347c6235b4d2611184", "transaction hash")
	assert.Contains(t, body, testContract, "contract link")
	assert.Contains(t, body, "Raw decoded values")
}

func TestEventDetailShowsFailedCall(t *testing.T) {
	e := sampleEvent()
	e.InSuccessfulCall = false
	src := &fakeSource{event: e, status: source.Status{Healthy: true}}

	rec := get(t, newTestServer(t, src), "/events/"+e.ID)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "failed call")
}

func TestEventNotFound(t *testing.T) {
	src := &fakeSource{err: source.ErrNotFound, status: source.Status{Healthy: true}}

	rec := get(t, newTestServer(t, src), "/events/missing")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "Event not found")
}

func TestSearchRouting(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		location string
	}{
		{"contract ID goes to the contract page", testContract, "/contracts/" + testContract},
		{"lowercase contract ID is normalized", "cdlzfc3syjydzt7k67vz75hpjvieuvnixf47zg2fb2rmqqvu2hhgcysc", "/contracts/" + testContract},
		{"event TOID goes to the event page", "0000000100-0000000001", "/events/0000000100-0000000001"},
		{"plain digits are treated as an event ID", "12345", "/events/12345"},
		{"a partial ID falls back to a contract search", "CDLZ", "/contracts?search=CDLZ"},
		{"empty query lists contracts", "", "/contracts"},
	}

	h := newTestServer(t, &fakeSource{status: source.Status{Healthy: true}})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := get(t, h, "/search?q="+tt.query)
			require.Equal(t, http.StatusSeeOther, rec.Code)
			assert.Equal(t, tt.location, rec.Header().Get("Location"))
		})
	}
}

func TestPartialEventsReturnsFragmentOnly(t *testing.T) {
	// htmx swaps this in, so it must be a bare fragment: no layout, no <html>.
	src := &fakeSource{
		events: source.EventPage{Events: []source.Event{sampleEvent()}, NextCursor: "next"},
		status: source.Status{Healthy: true},
	}

	rec := get(t, newTestServer(t, src), "/partials/events?contract_id="+testContract)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.NotContains(t, body, "<html", "a partial must not include the layout")
	assert.NotContains(t, body, "<header", "a partial must not include the navigation")
	assert.Contains(t, body, "transfer")
	assert.Contains(t, body, "Load more", "a next cursor should render the load-more control")
	assert.Contains(t, body, "hx-get", "the load-more control drives htmx")
}

func TestPartialEventsRejectsBadContract(t *testing.T) {
	src := &fakeSource{status: source.Status{Healthy: true}}

	rec := get(t, newTestServer(t, src), "/partials/events?contract_id=bogus")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestPartialEventsEmptyState(t *testing.T) {
	src := &fakeSource{events: source.EventPage{}, status: source.Status{Healthy: true}}

	rec := get(t, newTestServer(t, src), "/partials/events")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "No events match these filters")
}

func TestUnknownPathRenders404(t *testing.T) {
	src := &fakeSource{status: source.Status{Healthy: true}}

	rec := get(t, newTestServer(t, src), "/no/such/page")
	require.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "Not found")
}

func TestHTMLIsEscaped(t *testing.T) {
	// Event data is attacker-controlled: a contract can emit any string it
	// likes, so it must never be rendered as markup.
	e := sampleEvent()
	e.Topics = json.RawMessage(`[{"symbol":"<script>alert(1)</script>"}]`)
	src := &fakeSource{event: e, status: source.Status{Healthy: true}}

	rec := get(t, newTestServer(t, src), "/events/"+e.ID)
	require.Equal(t, http.StatusOK, rec.Code)

	body := rec.Body.String()
	assert.NotContains(t, body, "<script>alert(1)</script>", "event data must be escaped")
	assert.Contains(t, body, "&lt;script&gt;")
}

func TestClassifySearch(t *testing.T) {
	tests := []struct {
		name  string
		query string
		want  searchKind
	}{
		{"contract ID", testContract, searchContract},
		{"event TOID", "0000000100-0000000001", searchEvent},
		{"plain digits", "12345", searchEvent},
		{"trailing dash is not an ID", "12345-", searchUnknown},
		{"double dash is not an ID", "1-2-3", searchUnknown},
		{"empty", "", searchUnknown},
		{"partial contract", "CDLZ", searchUnknown},
		{"words", "hello world", searchUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, classifySearch(tt.query))
		})
	}
}

func TestFavicon(t *testing.T) {
	rec := get(t, newTestServer(t, &fakeSource{}), "/favicon.ico")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
	assert.NotEmpty(t, rec.Body.String())
}

func TestNavHighlightsActivePage(t *testing.T) {
	src := &fakeSource{
		contracts:     source.ContractPage{Contracts: []source.Contract{{ID: testContract}}},
		contractStats: source.ContractStats{ContractID: testContract},
		status:        source.Status{Healthy: true},
	}

	tests := []struct {
		path   string
		active string // link text expected to carry class="active"
	}{
		{"/", "Overview"},
		{"/contracts", "Contracts"},
		{"/contracts/" + testContract, "Contracts"},
	}
	linkNames := []string{"Overview", "Contracts"}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			rec := get(t, newTestServer(t, src), tt.path)
			require.Equal(t, http.StatusOK, rec.Code)
			html := rec.Body.String()

			for _, name := range linkNames {
				idx := strings.Index(html, ">"+name+"</a>")
				require.GreaterOrEqual(t, idx, 0, "nav link %q not found", name)
				tagStart := strings.LastIndex(html[:idx], "<a ")
				tag := html[tagStart:idx]
				hasActive := strings.Contains(tag, `class="active"`)
				wantActive := name == tt.active
				assert.Equal(t, wantActive, hasActive, "%s: nav link %q active=%v, want %v (tag: %s)", tt.path, name, hasActive, wantActive, tag)
			}
		})
	}
}

func TestNavNotHighlightedOnEventOrErrorPages(t *testing.T) {
	src := &fakeSource{
		event:  sampleEvent(),
		status: source.Status{Healthy: true},
	}
	linkNames := []string{"Overview", "Contracts"}

	for _, path := range []string{"/events/" + sampleEvent().ID, "/no-such-page"} {
		t.Run(path, func(t *testing.T) {
			rec := get(t, newTestServer(t, src), path)
			html := rec.Body.String()
			for _, name := range linkNames {
				idx := strings.Index(html, ">"+name+"</a>")
				require.GreaterOrEqual(t, idx, 0, "nav link %q not found", name)
				tagStart := strings.LastIndex(html[:idx], "<a ")
				tag := html[tagStart:idx]
				assert.NotContains(t, tag, `class="active"`, "%s: nav link %q must not be highlighted", path, name)
			}
		})
	}
}
