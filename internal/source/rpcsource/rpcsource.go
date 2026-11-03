// Package rpcsource implements source.EventSource for standalone mode, where
// SoroLens polls Stellar RPC itself and serves events back out of its own
// Postgres database.
//
// The read path here is deliberately thin: it forwards to the store, which
// does the real work. The write path — the poller that fills that store — is
// in internal/ingest.
package rpcsource

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sorotrail/sorolens/internal/rpc"
	"github.com/sorotrail/sorolens/internal/source"
	"github.com/sorotrail/sorolens/internal/store"
)

const defaultCheckTimeout = 5 * time.Second

// RetentionNote explains standalone mode's central limitation. Stellar RPC
// keeps contract events for roughly 24 hours to 7 days, so an instance can
// only ever capture what was emitted while it was running. The UI shows this
// so nobody mistakes an empty explorer for a broken one.
const RetentionNote = "Standalone mode captures events only while SoroLens is running: " +
	"Stellar RPC retains contract events for about 24 hours to 7 days, so history " +
	"from before this instance started is not available. Point SOURCE_MODE at a " +
	"SoroTrail indexer to browse deeper history."

// Source reads stored events. It satisfies source.EventSource.
type Source struct {
	store        store.Store
	rpc          rpc.Client
	checkTimeout time.Duration
}

// New returns a standalone-mode source. rpcClient is used only for status
// reporting; all reads come from the store.
func New(st store.Store, rpcClient rpc.Client) *Source {
	return &Source{
		store:        st,
		rpc:          rpcClient,
		checkTimeout: defaultCheckTimeout,
	}
}

// ListContracts returns contracts ordered by most recent activity.
func (s *Source) ListContracts(ctx context.Context, q source.ContractQuery) (source.ContractPage, error) {
	contracts, next, err := s.store.ListContracts(ctx, q)
	if err != nil {
		return source.ContractPage{}, err
	}
	return source.ContractPage{Contracts: contracts, NextCursor: next}, nil
}

// ListEvents returns events newest first.
func (s *Source) ListEvents(ctx context.Context, q source.EventQuery) (source.EventPage, error) {
	events, next, err := s.store.QueryEvents(ctx, q)
	if err != nil {
		return source.EventPage{}, err
	}
	return source.EventPage{Events: events, NextCursor: next}, nil
}

// GetEvent returns one event by TOID.
func (s *Source) GetEvent(ctx context.Context, id string) (source.Event, error) {
	return s.store.GetEvent(ctx, id)
}

// ContractStats summarizes one contract's stored events.
func (s *Source) ContractStats(ctx context.Context, contractID string) (source.ContractStats, error) {
	return s.store.ContractStats(ctx, contractID)
}

// Stats summarizes everything stored. Standalone counts are exact.
func (s *Source) Stats(ctx context.Context) (source.Stats, error) {
	return s.store.Stats(ctx)
}

// Status reports database and RPC health. It never returns an error: an
// unreachable dependency becomes an unhealthy Status so the UI can always
// render its banner. Both dependencies are checked concurrently with bounded
// timeouts so a failing or hung check never hides the other dependency's state.
func (s *Source) Status(ctx context.Context) source.Status {
	timeout := s.checkTimeout
	if timeout <= 0 {
		timeout = defaultCheckTimeout
	}

	var (
		wg           sync.WaitGroup
		pgDep        source.DependencyStatus
		pgSummary    string
		rpcDep       source.DependencyStatus
		rpcSummary   string
		latestLedger int64
	)

	wg.Add(2)
	go func() {
		defer wg.Done()
		pgCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		if err := s.store.Ping(pgCtx); err != nil {
			redacted := source.RedactSecrets(err.Error())
			pgDep = source.DependencyStatus{
				Status: source.StateUnhealthy,
				Detail: redacted,
			}
			pgSummary = fmt.Sprintf("database unreachable: %s", redacted)
			return
		}
		pgDep = source.DependencyStatus{Status: source.StateHealthy}
	}()

	go func() {
		defer wg.Done()
		rpcCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		health, err := s.rpc.GetHealth(rpcCtx)
		switch {
		case err != nil:
			redacted := source.RedactSecrets(err.Error())
			rpcDep = source.DependencyStatus{
				Status: source.StateUnhealthy,
				Detail: redacted,
			}
			rpcSummary = fmt.Sprintf("stellar rpc unreachable: %s", redacted)
		case health.Status != "healthy":
			redacted := source.RedactSecrets(health.Status)
			rpcDep = source.DependencyStatus{
				Status: source.StateUnhealthy,
				Detail: fmt.Sprintf("stellar rpc reports %q", redacted),
			}
			rpcSummary = fmt.Sprintf("stellar rpc reports %q", redacted)
		default:
			rpcDep = source.DependencyStatus{Status: source.StateHealthy}
			latestLedger = int64(health.LatestLedger)
		}
	}()
	wg.Wait()

	st := source.Status{
		Mode:          string(sourceModeRPC),
		Healthy:       true,
		State:         source.StateHealthy,
		LatestLedger:  latestLedger,
		RetentionNote: RetentionNote,
		Dependencies: map[string]source.DependencyStatus{
			"postgres": pgDep,
			"rpc":      rpcDep,
		},
	}

	pgOK := pgDep.Status == source.StateHealthy
	rpcOK := rpcDep.Status == source.StateHealthy

	switch {
	case pgOK && rpcOK:
		return st
	case pgOK && !rpcOK:
		// Stored history remains readable from Postgres while live ingestion
		// from Stellar RPC is impaired.
		st.Healthy = false
		st.State = source.StateDegraded
		st.Detail = rpcSummary
	case !pgOK && rpcOK:
		st.Healthy = false
		st.State = source.StateUnhealthy
		st.Detail = pgSummary
	default:
		st.Healthy = false
		st.State = source.StateUnhealthy
		st.Detail = pgSummary + "; " + rpcSummary
	}
	return st
}

// sourceModeRPC mirrors config.ModeRPC. It is duplicated rather than imported
// to keep this package free of a dependency on config.
const sourceModeRPC = "rpc"
