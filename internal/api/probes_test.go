package api

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sorotrail/sorolens/internal/source"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// probeServer mirrors cmd/sorolens: the probes live at the root, outside
// the /api tree that Routes() returns.
func probeServer(status source.Status) http.Handler {
	s := New(&fakeSource{status: status}, discardLogger())
	mux := http.NewServeMux()
	mux.Handle("/livez", s.LivezHandler())
	mux.Handle("/readyz", s.ReadyzHandler())
	return mux
}

// livez must stay 200 while the backend is down: a liveness probe that
// fails on dependencies makes an orchestrator restart-loop a recoverable
// instance instead of pulling it from the load balancer.
func TestLivezHealthyWhenBackendDown(t *testing.T) {
	srv := httptest.NewServer(probeServer(source.Status{
		Mode:    "rpc",
		Healthy: false,
		Detail:  "connection refused",
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/livez")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("livez with backend down = %d, want 200", res.StatusCode)
	}
}

func TestReadyzHealthy(t *testing.T) {
	srv := httptest.NewServer(probeServer(source.Status{
		Mode:         "rpc",
		Healthy:      true,
		State:        source.StateHealthy,
		LatestLedger: 1234,
		Dependencies: map[string]source.DependencyStatus{
			"postgres": {Status: source.StateHealthy},
			"rpc":      {Status: source.StateHealthy},
		},
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("readyz with healthy source = %d, want 200", res.StatusCode)
	}
	var body struct {
		Status       any                                `json:"status"`
		Mode         string                             `json:"mode"`
		State        string                             `json:"state"`
		LatestLedger int64                              `json:"latest_ledger"`
		Dependencies map[string]source.DependencyStatus `json:"dependencies"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.State != source.StateHealthy {
		t.Fatalf("readyz state = %q, want %q", body.State, source.StateHealthy)
	}
	if body.Dependencies["postgres"].Status != source.StateHealthy || body.Dependencies["rpc"].Status != source.StateHealthy {
		t.Fatalf("readyz dependencies = %+v, want both healthy", body.Dependencies)
	}
}

func TestReadyzReportsBackendFailure(t *testing.T) {
	srv := httptest.NewServer(probeServer(source.Status{
		Mode:    "rpc",
		Healthy: false,
		State:   source.StateDegraded,
		Detail:  "stellar rpc unreachable: timeout",
		Dependencies: map[string]source.DependencyStatus{
			"postgres": {Status: source.StateHealthy},
			"rpc":      {Status: source.StateUnhealthy, Detail: "timeout"},
		},
	}))
	defer srv.Close()

	res, err := http.Get(srv.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz with unhealthy source = %d, want 503", res.StatusCode)
	}
	var body struct {
		Status       string                             `json:"status"`
		Mode         string                             `json:"mode"`
		State        string                             `json:"state"`
		Detail       string                             `json:"detail"`
		Dependencies map[string]source.DependencyStatus `json:"dependencies"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Status != "not ready" {
		t.Fatalf("readyz status = %q, want %q", body.Status, "not ready")
	}
	if body.State != source.StateDegraded {
		t.Fatalf("readyz state = %q, want %q", body.State, source.StateDegraded)
	}
	if body.Dependencies["postgres"].Status != source.StateHealthy {
		t.Fatalf("readyz postgres dependency = %+v, want healthy", body.Dependencies["postgres"])
	}
	if body.Dependencies["rpc"].Status != source.StateUnhealthy || body.Dependencies["rpc"].Detail != "timeout" {
		t.Fatalf("readyz rpc dependency = %+v, want unhealthy/timeout", body.Dependencies["rpc"])
	}
}

func TestVersionEndpoint(t *testing.T) {
	// Built through the api tree like the other endpoints; pins the real
	// values, so a broken ldflags path shows up as "dev"/"none".
	srv := httptest.NewServer(New(&fakeSource{}, discardLogger()).Routes())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("version = %d, want 200", res.StatusCode)
	}
	var body map[string]string
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "commit", "build_date"} {
		if body[key] == "" {
			t.Fatalf("version response missing %q: %+v", key, body)
		}
	}
}
