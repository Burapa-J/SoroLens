package sorotrailsource

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sorotrail/sorolens/internal/source"
)

// Tuning for the backwards scan. SoroTrail returns events ascending, so to
// show the newest first SoroLens reads a window of ledgers ending at the tip,
// then walks the window backwards. These bounds keep one page view to a
// predictable, small number of upstream requests.
const (
	// initialWindow is the first ledger span tried. At roughly 5s per ledger
	// this is about seven hours of chain.
	initialWindow int64 = 5000
	// minWindow is the narrowest span the scanner will shrink to before it
	// accepts paging through a dense window.
	minWindow int64 = 1
	// maxWindows bounds how many windows one request may walk before giving
	// up, so a sparsely populated indexer cannot spin forever.
	maxWindows = 24
	// maxPagesPerWindow bounds paging within a single window. Exceeding it
	// marks the window dense, which makes the scanner narrow it and retry.
	maxPagesPerWindow = 8
)

// scanResult carries what a backwards scan collected.
type scanResult struct {
	// events are newest first.
	events []source.Event
	// exhausted is true when the scan reached the lower ledger bound, meaning
	// no older events exist within the requested range.
	exhausted bool
}

// scanBackwards collects at least want events older than beforeID, newest
// first, by walking ledger windows down from toLedger.
//
// The window adapts: a window that returns too many pages is narrowed and
// retried, and a window that comes back sparse is widened. In practice a page
// view costs two to four upstream requests.
func (s *Source) scanBackwards(ctx context.Context, q upstreamQuery, beforeID string, want int, toLedger, fromLedger int64) (scanResult, error) {
	if want <= 0 {
		want = source.DefaultLimit
	}
	if fromLedger < 1 {
		fromLedger = 1
	}

	var (
		collected []source.Event
		window    = initialWindow
		winEnd    = toLedger
	)

	for i := 0; i < maxWindows; i++ {
		if err := ctx.Err(); err != nil {
			return scanResult{}, err
		}
		if winEnd < fromLedger {
			return scanResult{events: sortDesc(collected), exhausted: true}, nil
		}

		winStart := winEnd - window + 1
		if winStart < fromLedger {
			winStart = fromLedger
		}

		events, dense, err := s.fetchWindow(ctx, q, winStart, winEnd, beforeID)
		if err != nil {
			return scanResult{}, err
		}

		// A dense window means the scan would have to page a long way to reach
		// its newest events. Narrow it and try again against the same tip
		// instead of paying for those pages.
		if dense && window > minWindow {
			window = max(window/4, minWindow)
			continue
		}

		collected = append(collected, events...)
		if len(collected) >= want {
			return scanResult{events: sortDesc(collected)}, nil
		}
		if winStart <= fromLedger {
			return scanResult{events: sortDesc(collected), exhausted: true}, nil
		}

		// The window was sparse, so reach further back next time.
		window = min(window*4, initialWindow*64)
		winEnd = winStart - 1
	}

	// Ran out of window budget with results still possibly older. Report what
	// was found; the cursor lets the caller continue from here.
	return scanResult{events: sortDesc(collected)}, nil
}

// fetchWindow reads every event in [fromLedger, toLedger] that is older than
// beforeID. It reports dense when it hit the page cap without draining the
// window, which tells the caller to narrow the range.
func (s *Source) fetchWindow(ctx context.Context, q upstreamQuery, fromLedger, toLedger int64, beforeID string) ([]source.Event, bool, error) {
	var (
		out    []source.Event
		cursor string
	)

	for page := 0; page < maxPagesPerWindow; page++ {
		req := q
		req.FromLedger = fromLedger
		req.ToLedger = toLedger
		req.Cursor = cursor
		req.Limit = maxUpstreamLimit

		res, err := s.api.Events(ctx, req)
		if err != nil {
			return nil, false, err
		}

		for _, e := range res.Events {
			// TOIDs are zero-padded, so a lexicographic comparison is a
			// chronological one.
			if beforeID != "" && e.ID >= beforeID {
				continue
			}
			out = append(out, e)
		}

		if res.Cursor == "" || len(res.Events) == 0 {
			return out, false, nil
		}
		cursor = res.Cursor
	}

	return out, true, nil
}

// sortDesc orders events newest first and drops duplicates, which overlapping
// windows can otherwise produce.
func sortDesc(events []source.Event) []source.Event {
	sort.Slice(events, func(i, j int) bool { return events[i].ID > events[j].ID })

	out := events[:0]
	var last string
	for _, e := range events {
		if e.ID == last {
			continue
		}
		out = append(out, e)
		last = e.ID
	}
	return out
}

// Cursors for upstream mode encode the exclusive upper bound of the next page
// plus the ledger to resume scanning from, so continuing a page never rescans
// from the tip. They are opaque to callers.
func encodeCursor(beforeID string, resumeLedger int64) string {
	return base64.RawURLEncoding.EncodeToString(
		[]byte(fmt.Sprintf("%s|%d", beforeID, resumeLedger)))
}

func decodeCursor(cursor string) (beforeID string, resumeLedger int64, err error) {
	raw, decErr := base64.RawURLEncoding.DecodeString(cursor)
	if decErr != nil {
		return "", 0, source.ErrInvalidCursor
	}
	id, ledgerStr, ok := strings.Cut(string(raw), "|")
	if !ok || ledgerStr == "" {
		return "", 0, source.ErrInvalidCursor
	}
	ledger, parseErr := strconv.ParseInt(ledgerStr, 10, 64)
	if parseErr != nil || ledger < 0 {
		return "", 0, source.ErrInvalidCursor
	}
	return id, ledger, nil
}

// decodeContractCursor decodes an upstream contract-list cursor, which carries
// the last contract ID and its LastLedger sort key ("<contract_id>|<last_ledger>").
// Rejecting empty IDs, TOID-shaped event IDs, and non-positive ledgers ensures
// that an event cursor or a cursor pasted from standalone mode (which encodes
// "<last_ledger>|<contract_id>") fails cleanly with source.ErrInvalidCursor.
func decodeContractCursor(cursor string) (afterID string, afterLedger int64, err error) {
	id, ledger, err := decodeCursor(cursor)
	if err != nil || id == "" || ledger <= 0 || strings.Contains(id, "-") {
		return "", 0, source.ErrInvalidCursor
	}
	return id, ledger, nil
}
