package api

import (
	"net/http"
)

// LivezHandler serves GET /livez: "is the process alive". It checks
// nothing — a process whose backend is down is still alive, and a liveness
// probe that failed on dependencies would make an orchestrator restart-loop
// a recoverable instance instead of pulling it from the load balancer.
// Wire to livenessProbe.
func (s *Server) LivezHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "alive"})
	}
}

// ReadyzHandler serves GET /readyz: "can this instance do useful work
// right now". It consults the event source's own health view — the database
// and RPC in standalone mode, the SoroTrail indexer in upstream mode — so
// readiness means the same thing regardless of backend. 200 when the source
// reports healthy, 503 otherwise, with the source's detail explaining what
// is wrong. Wire to readinessProbe.
func (s *Server) ReadyzHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		status := s.src.Status(r.Context())
		out := map[string]any{
			"status": status.Healthy,
			"mode":   status.Mode,
		}
		if status.State != "" {
			out["state"] = status.State
		}
		if status.Detail != "" {
			out["detail"] = status.Detail
		}
		if status.LatestLedger > 0 {
			out["latest_ledger"] = status.LatestLedger
		}
		if len(status.Dependencies) > 0 {
			out["dependencies"] = status.Dependencies
		}
		code := http.StatusOK
		if !status.Healthy {
			code = http.StatusServiceUnavailable
			out["status"] = "not ready"
		}
		writeJSON(w, code, out)
	}
}
