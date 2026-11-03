package rpcsource

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sorotrail/sorolens/internal/rpc"
	"github.com/sorotrail/sorolens/internal/source"
	"github.com/sorotrail/sorolens/internal/store"
)

// stubStore is a canned store.Store.
type stubStore struct {
	contracts     []source.Contract
	contractNext  string
	events        []source.Event
	eventsNext    string
	event         source.Event
	stats         source.Stats
	contractStats source.ContractStats

	err     error
	pingErr error

	lastEventQuery    source.EventQuery
	lastContractQuery source.ContractQuery
}

func (s *stubStore) UpsertEvents(context.Context, []source.Event) (int64, error) { return 0, nil }

func (s *stubStore) GetEvent(context.Context, string) (source.Event, error) {
	return s.event, s.err
}

func (s *stubStore) QueryEvents(_ context.Context, q source.EventQuery) ([]source.Event, string, error) {
	s.lastEventQuery = q
	return s.events, s.eventsNext, s.err
}

func (s *stubStore) ListContracts(_ context.Context, q source.ContractQuery) ([]source.Contract, string, error) {
	s.lastContractQuery = q
	return s.contracts, s.contractNext, s.err
}

func (s *stubStore) ContractStats(context.Context, string) (source.ContractStats, error) {
	return s.contractStats, s.err
}

func (s *stubStore) Stats(context.Context) (source.Stats, error) { return s.stats, s.err }

func (s *stubStore) GetIngestState(context.Context) (store.IngestState, error) {
	return store.IngestState{}, nil
}
func (s *stubStore) SaveIngestState(context.Context, store.IngestState) error { return nil }
func (s *stubStore) Ping(ctx context.Context) error {
	if s.pingErr != nil {
		return s.pingErr
	}
	return ctx.Err()
}
func (s *stubStore) Close() {}

// stubRPC is a canned rpc.Client used only for status reporting here.
type stubRPC struct {
	health  rpc.Health
	err     error
	blockCh <-chan struct{}
}

func (s *stubRPC) GetEvents(context.Context, rpc.GetEventsRequest) (rpc.GetEventsResult, error) {
	return rpc.GetEventsResult{}, nil
}
func (s *stubRPC) GetLatestLedger(context.Context) (rpc.LatestLedger, error) {
	return rpc.LatestLedger{}, nil
}
func (s *stubRPC) GetHealth(ctx context.Context) (rpc.Health, error) {
	if s.blockCh != nil {
		select {
		case <-s.blockCh:
		case <-ctx.Done():
			return rpc.Health{}, ctx.Err()
		}
	}
	return s.health, s.err
}

func healthyRPC() *stubRPC { return &stubRPC{health: rpc.Health{Status: "healthy", LatestLedger: 900}} }

func TestListEventsForwardsQueryAndCursor(t *testing.T) {
	st := &stubStore{
		events:     []source.Event{{ID: "e1"}, {ID: "e2"}},
		eventsNext: "cursor-2",
	}
	src := New(st, healthyRPC())

	q := source.EventQuery{ContractID: "CABC", Type: "contract", FromLedger: 10, Limit: 25}
	page, err := src.ListEvents(context.Background(), q)
	require.NoError(t, err)

	assert.Len(t, page.Events, 2)
	assert.Equal(t, "cursor-2", page.NextCursor)
	// The query must reach the store unchanged.
	assert.Equal(t, q, st.lastEventQuery)
}

func TestListContractsForwardsQuery(t *testing.T) {
	st := &stubStore{
		contracts:    []source.Contract{{ID: "CABC", EventCount: 3}},
		contractNext: "next",
	}
	src := New(st, healthyRPC())

	page, err := src.ListContracts(context.Background(), source.ContractQuery{Search: "CA", Limit: 10})
	require.NoError(t, err)

	require.Len(t, page.Contracts, 1)
	assert.Equal(t, "next", page.NextCursor)
	assert.Equal(t, "CA", st.lastContractQuery.Search)
}

func TestStatsAreExact(t *testing.T) {
	st := &stubStore{stats: source.Stats{TotalEvents: 10, ContractCount: 2}}
	src := New(st, healthyRPC())

	got, err := src.Stats(context.Background())
	require.NoError(t, err)

	assert.Equal(t, int64(10), got.TotalEvents)
	assert.False(t, got.Approximate, "standalone mode counts exactly")
}

func TestErrorsPropagate(t *testing.T) {
	st := &stubStore{err: errors.New("database exploded")}
	src := New(st, healthyRPC())
	ctx := context.Background()

	_, err := src.ListEvents(ctx, source.EventQuery{})
	assert.Error(t, err)

	_, err = src.ListContracts(ctx, source.ContractQuery{})
	assert.Error(t, err)

	_, err = src.Stats(ctx)
	assert.Error(t, err)
}

func TestGetEventNotFoundPassesThrough(t *testing.T) {
	st := &stubStore{err: source.ErrNotFound}
	src := New(st, healthyRPC())

	_, err := src.GetEvent(context.Background(), "missing")
	assert.ErrorIs(t, err, source.ErrNotFound)
}

func TestStatus(t *testing.T) {
	t.Run("healthy reports the retention caveat and both dependencies healthy", func(t *testing.T) {
		src := New(&stubStore{}, healthyRPC())
		st := src.Status(context.Background())

		assert.Equal(t, "rpc", st.Mode)
		assert.True(t, st.Healthy)
		assert.Equal(t, source.StateHealthy, st.State)
		assert.Equal(t, int64(900), st.LatestLedger)
		assert.Contains(t, st.RetentionNote, "only while SoroLens is running",
			"the capture-window caveat must always be surfaced")
		require.Len(t, st.Dependencies, 2)
		assert.Equal(t, source.DependencyStatus{Status: source.StateHealthy}, st.Dependencies["postgres"])
		assert.Equal(t, source.DependencyStatus{Status: source.StateHealthy}, st.Dependencies["rpc"])
	})

	t.Run("a database failure is unhealthy and still reports rpc status", func(t *testing.T) {
		src := New(&stubStore{pingErr: errors.New("connection refused")}, healthyRPC())
		st := src.Status(context.Background())

		assert.False(t, st.Healthy)
		assert.Equal(t, source.StateUnhealthy, st.State)
		assert.Contains(t, st.Detail, "database unreachable")
		assert.Equal(t, int64(900), st.LatestLedger)
		require.Len(t, st.Dependencies, 2)
		assert.Equal(t, source.DependencyStatus{
			Status: source.StateUnhealthy,
			Detail: "connection refused",
		}, st.Dependencies["postgres"])
		assert.Equal(t, source.DependencyStatus{Status: source.StateHealthy}, st.Dependencies["rpc"])
	})

	t.Run("an unreachable node with healthy postgres is degraded", func(t *testing.T) {
		src := New(&stubStore{}, &stubRPC{err: errors.New("timeout")})
		st := src.Status(context.Background())

		assert.False(t, st.Healthy)
		assert.Equal(t, source.StateDegraded, st.State)
		assert.Contains(t, st.Detail, "stellar rpc unreachable")
		require.Len(t, st.Dependencies, 2)
		assert.Equal(t, source.DependencyStatus{Status: source.StateHealthy}, st.Dependencies["postgres"])
		assert.Equal(t, source.DependencyStatus{
			Status: source.StateUnhealthy,
			Detail: "timeout",
		}, st.Dependencies["rpc"])
	})

	t.Run("a syncing node with healthy postgres is degraded", func(t *testing.T) {
		src := New(&stubStore{}, &stubRPC{health: rpc.Health{Status: "syncing"}})
		st := src.Status(context.Background())

		assert.False(t, st.Healthy)
		assert.Equal(t, source.StateDegraded, st.State)
		assert.Contains(t, st.Detail, "syncing")
		require.Len(t, st.Dependencies, 2)
		assert.Equal(t, source.DependencyStatus{Status: source.StateHealthy}, st.Dependencies["postgres"])
		assert.Equal(t, source.StateUnhealthy, st.Dependencies["rpc"].Status)
		assert.Contains(t, st.Dependencies["rpc"].Detail, "syncing")
	})

	t.Run("both dependencies failing reports both in dependencies and detail", func(t *testing.T) {
		src := New(
			&stubStore{pingErr: errors.New("connection refused")},
			&stubRPC{err: errors.New("rpc dial timeout")},
		)
		st := src.Status(context.Background())

		assert.False(t, st.Healthy)
		assert.Equal(t, source.StateUnhealthy, st.State)
		assert.Contains(t, st.Detail, "database unreachable: connection refused")
		assert.Contains(t, st.Detail, "stellar rpc unreachable: rpc dial timeout")
		require.Len(t, st.Dependencies, 2)
		assert.Equal(t, source.DependencyStatus{
			Status: source.StateUnhealthy,
			Detail: "connection refused",
		}, st.Dependencies["postgres"])
		assert.Equal(t, source.DependencyStatus{
			Status: source.StateUnhealthy,
			Detail: "rpc dial timeout",
		}, st.Dependencies["rpc"])
	})

	t.Run("DSN credentials and passwords are redacted from status details", func(t *testing.T) {
		src := New(
			&stubStore{pingErr: errors.New("dial postgres://sorolens:s3cr3tPass!@db.internal:5432/sorolens?sslmode=disable: connection refused")},
			&stubRPC{err: errors.New("Get \"https://api:topSecretToken@rpc.example.com\": password=myKvSecret")},
		)
		st := src.Status(context.Background())

		assert.False(t, st.Healthy)
		assert.Equal(t, source.StateUnhealthy, st.State)
		assert.NotContains(t, st.Detail, "s3cr3tPass!")
		assert.NotContains(t, st.Detail, "topSecretToken")
		assert.NotContains(t, st.Detail, "myKvSecret")
		assert.NotContains(t, st.Dependencies["postgres"].Detail, "s3cr3tPass!")
		assert.Contains(t, st.Dependencies["postgres"].Detail, "postgres://sorolens:redacted@db.internal:5432")
		assert.NotContains(t, st.Dependencies["rpc"].Detail, "topSecretToken")
		assert.NotContains(t, st.Dependencies["rpc"].Detail, "myKvSecret")
		assert.Contains(t, st.Dependencies["rpc"].Detail, "https://api:redacted@rpc.example.com")
		assert.Contains(t, st.Dependencies["rpc"].Detail, "password=redacted")
	})

	t.Run("a hung check times out within the per-check bound without hiding the other dependency", func(t *testing.T) {
		blockCh := make(chan struct{})
		defer close(blockCh)

		src := New(&stubStore{}, &stubRPC{blockCh: blockCh})
		src.checkTimeout = 25 * time.Millisecond

		st := src.Status(context.Background())
		assert.False(t, st.Healthy)
		assert.Equal(t, source.StateDegraded, st.State)
		assert.Equal(t, source.DependencyStatus{Status: source.StateHealthy}, st.Dependencies["postgres"])
		assert.Equal(t, source.StateUnhealthy, st.Dependencies["rpc"].Status)
		assert.Contains(t, st.Dependencies["rpc"].Detail, "context deadline exceeded")
	})
}

// TestSourceSatisfiesInterface fails at compile time if the standalone source
// ever drifts from source.EventSource.
func TestSourceSatisfiesInterface(t *testing.T) {
	var _ source.EventSource = New(&stubStore{}, healthyRPC())
}
