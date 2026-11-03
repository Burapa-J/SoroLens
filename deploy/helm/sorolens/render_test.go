// Package sorolens_test covers the SoroLens Helm chart's rendered output
// across both standalone ("rpc") and upstream ("sorotrail") operating modes
// (issue #57).
//
// These tests render the chart with `helm template` and compare the output
// byte-for-byte against committed golden manifests under testdata/, plus a set
// of structural invariants (probes pointing at /livez and /readyz, DATABASE_URL
// coming from a Secret in standalone mode, mode-specific variables omitted in
// the opposite mode, schema rejection of unknown/invalid values, and 1:1
// config-to-application environment variable parity).
//
// Regenerate the golden files after an intentional template change with:
//
//	go test ./deploy/helm/sorolens -run TestRenderGolden -update
package sorolens_test

import (
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "regenerate golden files instead of comparing")

func chartDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(file)
}

func helmPath(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("helm")
	if err != nil {
		if os.Getenv("SOROLENS_REQUIRE_HELM") == "1" {
			t.Fatalf("helm binary is required (SOROLENS_REQUIRE_HELM=1) but was not found on PATH: %v", err)
		}
		t.Skipf("skipping Helm chart render test: helm binary not found on PATH (%v); install helm or set PATH to run", err)
	}
	return p
}

func renderChart(t *testing.T, helm string, extraArgs ...string) string {
	t.Helper()
	args := append([]string{
		"template", "sorolens", chartDir(),
		"--namespace", "sorolens-test",
	}, extraArgs...)
	out, err := exec.Command(helm, args...).CombinedOutput()
	require.NoError(t, err, "helm template failed: %s", out)
	return string(out)
}

func renderStandaloneDefault(t *testing.T, helm string, extraArgs ...string) string {
	t.Helper()
	args := append([]string{
		"--set", "database.url=postgres://user:pass@host:5432/sorolens",
	}, extraArgs...)
	return renderChart(t, helm, args...)
}

// TestRenderGolden_DefaultValues pins the full render of the chart in
// standalone ("rpc") mode with the defaults from values.yaml.
//
// Regenerate with:
//
//	go test ./deploy/helm/sorolens -run TestRenderGolden -update
func TestRenderGolden_DefaultValues(t *testing.T) {
	helm := helmPath(t)
	got := renderStandaloneDefault(t, helm)
	golden := filepath.Join(chartDir(), "testdata", "default.golden.yaml")
	assertGolden(t, golden, got)
}

// TestRenderGolden_UpstreamMode pins the full render of the chart in upstream
// ("sorotrail") mode, verifying that neither a database Secret nor standalone
// RPC variables are emitted.
//
// Regenerate with:
//
//	go test ./deploy/helm/sorolens -run TestRenderGolden -update
func TestRenderGolden_UpstreamMode(t *testing.T) {
	helm := helmPath(t)
	got := renderChart(t, helm,
		"--set", "config.sourceMode=sorotrail",
		"--set", "config.sorotrailUrl=http://sorotrail.sorolens-test.svc.cluster.local:8080",
	)
	golden := filepath.Join(chartDir(), "testdata", "upstream.golden.yaml")
	assertGolden(t, golden, got)
}

// TestRenderGolden_RepresentativeOverrides pins a render with a representative
// override set: resource requests/limits, an existing database Secret, a
// ServiceMonitor, a PodDisruptionBudget, and a custom ServiceAccount name.
//
// Regenerate with:
//
//	go test ./deploy/helm/sorolens -run TestRenderGolden -update
func TestRenderGolden_RepresentativeOverrides(t *testing.T) {
	helm := helmPath(t)
	got := renderChart(t, helm,
		"--values", filepath.Join(chartDir(), "testdata", "overrides.yaml"),
	)
	golden := filepath.Join(chartDir(), "testdata", "overrides.golden.yaml")
	assertGolden(t, golden, got)
}

func normalizeManifest(s string) string {
	for strings.Contains(s, "\n\n---\n") {
		s = strings.ReplaceAll(s, "\n\n---\n", "\n---\n")
	}
	return strings.TrimSpace(s) + "\n"
}

func assertGolden(t *testing.T, golden, got string) {
	t.Helper()
	if *update {
		require.NoError(t, os.WriteFile(golden, []byte(normalizeManifest(got)), 0o644))
		t.Logf("updated golden file %s", golden)
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("reading golden file %s: %v (run with -update to create it)", golden, err)
	}
	assert.Equal(t, normalizeManifest(string(want)), normalizeManifest(got),
		"rendered manifests drifted from %s; if the change is intentional, regenerate with:\n\tgo test ./deploy/helm/sorolens -run TestRenderGolden -update", golden)
}

func TestRender_ManifestInvariants(t *testing.T) {
	helm := helmPath(t)

	rendered := renderStandaloneDefault(t, helm)
	deployment := manifest(t, rendered, "Deployment")
	cm := manifest(t, rendered, "ConfigMap")

	t.Run("probes hit the real endpoints", func(t *testing.T) {
		assert.Contains(t, deployment, "startupProbe:", "startup probe must exist")
		assert.Contains(t, deployment, "livenessProbe:", "liveness probe must exist")
		assert.Contains(t, deployment, "readinessProbe:", "readiness probe must exist")
		assert.Contains(t, deployment, "path: /livez", "startup and liveness probes must hit /livez")
		assert.Contains(t, deployment, "path: /readyz", "readiness probe must hit /readyz")
	})

	t.Run("standalone mode sources DATABASE_URL from a Secret and omits SOROTRAIL_URL", func(t *testing.T) {
		assert.Contains(t, deployment, "secretKeyRef:", "DATABASE_URL must come from a Secret")
		assert.Contains(t, deployment, "DATABASE_URL", "DATABASE_URL env must be declared")
		assert.NotContains(t, deployment, "postgres://user:pass@host:5432/sorolens",
			"the connection string must never be inlined into the pod spec")
		assert.Contains(t, manifest(t, rendered, "Secret"), "stringData:",
			"default standalone render must create the chart-managed Secret")
		assert.Contains(t, cm, `SOURCE_MODE: "rpc"`)
		assert.Contains(t, cm, `RPC_URL: "https://soroban-testnet.stellar.org"`)
		assert.NotContains(t, cm, "SOROTRAIL_URL",
			"standalone mode must not render SOROTRAIL_URL")
	})

	t.Run("upstream mode omits database Secret and standalone-only settings", func(t *testing.T) {
		upstream := renderChart(t, helm,
			"--set", "config.sourceMode=sorotrail",
			"--set", "config.sorotrailUrl=http://sorotrail:8080",
		)
		assert.NotContains(t, upstream, "kind: Secret",
			"upstream mode does not use Postgres and must not create a Secret")

		upDep := manifest(t, upstream, "Deployment")
		assert.NotContains(t, upDep, "DATABASE_URL",
			"upstream mode must not mount DATABASE_URL into the pod")
		assert.NotContains(t, upDep, "secretKeyRef:",
			"upstream mode must not reference a database Secret")
		assert.NotContains(t, upDep, "checksum/secret:",
			"upstream mode must not annotate the pod with a secret checksum")

		upCM := manifest(t, upstream, "ConfigMap")
		assert.Contains(t, upCM, `SOURCE_MODE: "sorotrail"`)
		assert.Contains(t, upCM, `SOROTRAIL_URL: "http://sorotrail:8080"`)
		for _, standaloneOnly := range []string{
			"NETWORK:",
			"RPC_URL:",
			"NETWORK_PASSPHRASE:",
			"POLL_INTERVAL:",
			"RETENTION_LEDGERS:",
			"WATCHED_CONTRACTS:",
			"START_LEDGER:",
		} {
			assert.NotContains(t, upCM, standaloneOnly,
				"upstream mode must not render standalone-only variable %s", standaloneOnly)
		}
	})

	t.Run("missing required mode-specific settings fail template rendering", func(t *testing.T) {
		out, err := exec.Command(helm, "template", "sorolens", chartDir()).CombinedOutput()
		require.Error(t, err, "standalone mode without database.url or existingSecret must fail")
		assert.Contains(t, string(out), "database.url is required")

		out, err = exec.Command(helm, "template", "sorolens", chartDir(),
			"--set", "config.sourceMode=sorotrail",
		).CombinedOutput()
		require.Error(t, err, "upstream mode without config.sorotrailUrl must fail")
		assert.Contains(t, string(out), "config.sorotrailUrl is required")
	})

	t.Run("schema rejects unknown and invalid values", func(t *testing.T) {
		out, err := exec.Command(helm, "lint", chartDir(),
			"--set", "database.url=postgres://user:pass@host:5432/sorolens",
			"--set", "replicaCont=2",
		).CombinedOutput()
		require.Error(t, err, "values.schema.json must reject unknown key replicaCont: %s", out)

		out, err = exec.Command(helm, "lint", chartDir(),
			"--set", "database.url=postgres://user:pass@host:5432/sorolens",
			"--set", "config.sourceMode=invalid",
		).CombinedOutput()
		require.Error(t, err, "values.schema.json must reject invalid config.sourceMode: %s", out)
	})

	t.Run("resources, existingSecret, ServiceMonitor, and PDB land when configured", func(t *testing.T) {
		withOverrides := renderChart(t, helm,
			"--values", filepath.Join(chartDir(), "testdata", "overrides.yaml"),
		)
		dep := manifest(t, withOverrides, "Deployment")
		assert.Contains(t, dep, "requests:")
		assert.Contains(t, dep, "limits:")
		assert.Contains(t, dep, "cpu:")
		assert.Contains(t, dep, "memory:")
		assert.NotContains(t, withOverrides, "kind: Secret",
			"an existingSecret must suppress the chart-managed Secret")
		assert.Contains(t, dep, "existing-db-secret")

		sm := manifest(t, withOverrides, "ServiceMonitor")
		assert.Contains(t, sm, "path: /metrics")
		assert.Contains(t, sm, "interval: 15s")

		pdb := manifest(t, withOverrides, "PodDisruptionBudget")
		assert.Contains(t, pdb, "minAvailable: 1")
	})
}

// TestRender_ConfigValuesReachThePod verifies bidirectional parity between
// the chart's config values, the ConfigMap/Deployment templates, and the
// environment variables read by internal/config.
func TestRender_ConfigValuesReachThePod(t *testing.T) {
	helmPath(t)

	template := readChartFile(t, "templates", "configmap.yaml")
	deploymentTemplate := readChartFile(t, "templates", "deployment.yaml")
	values := readChartFile(t, "values.yaml")

	configKeys := parseValuesConfigKeys(values)
	require.NotEmpty(t, configKeys, "values.yaml must declare keys under config:")

	t.Run("every config value in values.yaml is wired into the ConfigMap", func(t *testing.T) {
		for _, key := range configKeys {
			assert.Contains(t, template, ".Values.config."+key,
				"config.%s in values.yaml is never wired into templates/configmap.yaml", key)
		}
	})

	appVars := applicationEnvVars(t)
	renderedVars := configMapEnvVars(template)

	t.Run("every rendered ConfigMap variable is read by internal/config", func(t *testing.T) {
		for _, name := range renderedVars {
			assert.True(t, appVars[name],
				"templates/configmap.yaml renders %s, which internal/config does not read", name)
		}
	})

	t.Run("every internal/config environment variable is covered by the chart", func(t *testing.T) {
		covered := map[string]bool{}
		for _, name := range renderedVars {
			covered[name] = true
		}
		if strings.Contains(deploymentTemplate, "name: DATABASE_URL") {
			covered["DATABASE_URL"] = true
		}
		for name := range appVars {
			assert.True(t, covered[name],
				"internal/config reads %s, but the Helm chart never sets it", name)
		}
	})
}

var (
	envReadCallRegex = regexp.MustCompile(`\b(?:os\.Getenv|getenv)\("([A-Z][A-Z0-9_]+)"`)
	configMapEnvLine = regexp.MustCompile(`^\s+([A-Z][A-Z0-9_]+):`)
	valuesConfigLine = regexp.MustCompile(`^  ([a-z][a-zA-Z0-9]*):`)
)

func parseValuesConfigKeys(values string) []string {
	var keys []string
	inConfig := false
	for _, line := range strings.Split(values, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") {
			inConfig = strings.HasPrefix(line, "config:")
			continue
		}
		if inConfig {
			if m := valuesConfigLine.FindStringSubmatch(line); m != nil {
				keys = append(keys, m[1])
			}
		}
	}
	return keys
}

func applicationEnvVars(t *testing.T) map[string]bool {
	t.Helper()
	cfgDir := filepath.Join(chartDir(), "..", "..", "..", "internal", "config")
	entries, err := os.ReadDir(cfgDir)
	require.NoError(t, err, "reading %s", cfgDir)

	out := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(cfgDir, entry.Name()))
		require.NoError(t, err)
		for _, m := range envReadCallRegex.FindAllStringSubmatch(string(data), -1) {
			out[m[1]] = true
		}
	}
	require.NotEmpty(t, out, "internal/config must declare environment variables")
	return out
}

func configMapEnvVars(template string) []string {
	var names []string
	seen := map[string]bool{}
	for _, line := range strings.Split(template, "\n") {
		match := configMapEnvLine.FindStringSubmatch(line)
		if match == nil || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		names = append(names, match[1])
	}
	return names
}

func readChartFile(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{chartDir()}, parts...)...)
	data, err := os.ReadFile(path)
	require.NoError(t, err, "reading %s", path)
	return string(data)
}

func manifest(t *testing.T, rendered, kind string) string {
	t.Helper()
	for _, block := range strings.Split(rendered, "\n---\n") {
		if strings.Contains(block, "kind: "+kind) {
			return block
		}
	}
	t.Fatalf("rendered output contains no %s manifest", kind)
	return ""
}
