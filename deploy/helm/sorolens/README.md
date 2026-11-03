# SoroLens Helm Chart

Deploys [SoroLens](https://github.com/sorotrail/SoroLens) — a lightweight, self-hostable Stellar/Soroban contract-event explorer and inspector — to Kubernetes in either **standalone (`rpc`)** or **upstream (`sorotrail`)** operating mode.

## Prerequisites

- Kubernetes 1.25+
- Helm 3.10+
- **Standalone (`rpc`) mode**: An external PostgreSQL 15+ instance (see [Database](#database)).
- **Upstream (`sorotrail`) mode**: A reachable [SoroTrail](https://github.com/sorotrail/sorotrail) HTTP API instance (no database required).

## Operating Modes

### 1. Standalone mode (`config.sourceMode=rpc`, default)

SoroLens polls a Stellar Soroban JSON-RPC endpoint directly and stores indexed events in PostgreSQL.

The chart **does not bundle PostgreSQL**. Provision Postgres separately (for example with [CloudNativePG](https://cloudnative-pg.io), [Zalando Postgres Operator](https://github.com/zalando/postgres-operator), RDS, or Cloud SQL) and pass the connection string via `database.url` or an existing Secret (`database.existingSecret`).

```bash
# Install with a chart-managed Secret:
helm install sorolens ./deploy/helm/sorolens \
  --set database.url="postgres://user:pass@host:5432/sorolens?sslmode=require" \
  --set config.network="testnet"

# Or install referencing a pre-existing Secret (must contain key DATABASE_URL):
helm install sorolens ./deploy/helm/sorolens \
  --set database.existingSecret="my-sorolens-db-secret"
```

### 2. Upstream mode (`config.sourceMode=sorotrail`)

SoroLens acts as a stateless web UI and JSON API layer over an existing SoroTrail deployment. In this mode:
- No `Secret` is created or required.
- `DATABASE_URL` and standalone RPC settings (`NETWORK`, `RPC_URL`, `POLL_INTERVAL`, `RETENTION_LEDGERS`, etc.) are omitted from the rendered manifests.

```bash
helm install sorolens ./deploy/helm/sorolens \
  --set config.sourceMode="sorotrail" \
  --set config.sorotrailUrl="http://sorotrail.default.svc.cluster.local:8080"
```

## Upgrading

```bash
helm upgrade sorolens ./deploy/helm/sorolens --reuse-values
```

## Values

| Key | Default | Description |
|-----|---------|-------------|
| `replicaCount` | `1` | Number of pod replicas. |
| `image.repository` | `ghcr.io/sorotrail/sorolens` | Container image repository. |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy (`Always`, `IfNotPresent`, `Never`). |
| `image.tag` | `""` (uses `appVersion`) | Image tag override. |
| `imagePullSecrets` | `[]` | Secrets for pulling images from private registries. |
| `nameOverride` | `""` | Override the chart name. |
| `fullnameOverride` | `""` | Override the full release name. |
| `serviceAccount.create` | `true` | Create a dedicated ServiceAccount (`automountServiceAccountToken: false`). |
| `serviceAccount.annotations` | `{}` | Annotations for the ServiceAccount. |
| `serviceAccount.name` | `""` | Use an existing ServiceAccount name. |
| `podAnnotations` | `{}` | Extra annotations applied to the pod template. |
| `podSecurityContext.runAsNonRoot` | `true` | Run the container as a non-root user. |
| `podSecurityContext.runAsUser` | `10001` | UID the container runs as (matches `Dockerfile`). |
| `podSecurityContext.runAsGroup` | `10001` | GID the container runs as. |
| `podSecurityContext.fsGroup` | `10001` | FSGroup for mounted volumes. |
| `securityContext.allowPrivilegeEscalation` | `false` | Disallow privilege escalation. |
| `securityContext.readOnlyRootFilesystem` | `true` | Mount the container root filesystem read-only. |
| `securityContext.capabilities.drop` | `[ALL]` | Drop all Linux capabilities. |
| `service.type` | `ClusterIP` | Kubernetes Service type. |
| `service.port` | `80` | Service port. |
| `service.targetPort` | `8080` | Container HTTP port. |
| `resources` | `{}` | CPU/memory resource requests and limits. |
| `nodeSelector` | `{}` | Node selector for pod scheduling. |
| `tolerations` | `[]` | Tolerations for pod scheduling. |
| `affinity` | `{}` | Affinity rules for pod scheduling. |
| `pdb.enabled` | `false` | Create a `PodDisruptionBudget`. |
| `pdb.minAvailable` | `1` | Minimum pods that must remain available during voluntary disruptions. |
| `pdb.maxUnavailable` | _(unset)_ | Maximum pods that may be unavailable during voluntary disruptions. |
| `config.sourceMode` | `"rpc"` | Operating mode: `"rpc"` (standalone) or `"sorotrail"` (upstream). |
| `config.sorotrailUrl` | `""` | Upstream SoroTrail base URL (`SOROTRAIL_URL`). Required when `config.sourceMode=sorotrail`. |
| `config.network` | `"testnet"` | Stellar network preset (`NETWORK`): `testnet`, `mainnet`, `futurenet`, or `custom` (`rpc` mode only). |
| `config.rpcUrl` | `"https://soroban-testnet.stellar.org"` | Soroban JSON-RPC endpoint (`RPC_URL`, `rpc` mode only). |
| `config.networkPassphrase` | `""` | Stellar network passphrase (`NETWORK_PASSPHRASE`, required only when `config.network=custom`). |
| `config.pollInterval` | `"5s"` | RPC polling interval (`POLL_INTERVAL`, `rpc` mode only). |
| `config.httpAddr` | `":8080"` | HTTP listen address (`HTTP_ADDR`). Must match `service.targetPort`. |
| `config.watchedContracts` | `""` | Comma-separated contract IDs (`C...`) to index (`WATCHED_CONTRACTS`, `rpc` mode only). |
| `config.startLedger` | `""` | Cold-start ledger sequence (`START_LEDGER`, `rpc` mode only). |
| `config.retentionLedgers` | `"17280"` | Cold-start reach-back in ledgers (`RETENTION_LEDGERS`, `rpc` mode only). |
| `config.logLevel` | `"info"` | Log level (`LOG_LEVEL`): `debug`, `info`, `warn`, `error`. |
| `config.corsAllowedOrigins` | `""` | Comma-separated CORS origins for `/api/*` (`CORS_ALLOWED_ORIGINS`). |
| `database.existingSecret` | `""` | Existing Secret name containing `DATABASE_URL` (`rpc` mode only). |
| `database.existingSecretKey` | `"DATABASE_URL"` | Key within `database.existingSecret` (`rpc` mode only). |
| `database.url` | `""` | Inline Postgres connection string stored in a chart-managed Secret (`rpc` mode only). |
| `serviceMonitor.enabled` | `false` | Create a Prometheus `ServiceMonitor` resource. |
| `serviceMonitor.namespace` | `""` | Namespace for the `ServiceMonitor` (defaults to release namespace). |
| `serviceMonitor.additionalLabels` | `{}` | Extra labels for the `ServiceMonitor`. |
| `serviceMonitor.interval` | `"30s"` | Prometheus scrape interval. |
| `serviceMonitor.scrapeTimeout` | `"10s"` | Prometheus scrape timeout. |
| `serviceMonitor.path` | `"/metrics"` | Prometheus metrics HTTP path. |

## Health Probes

The chart configures three probes against SoroLens's built-in health endpoints:

| Probe | Endpoint | Purpose |
|-------|----------|---------|
| **Startup** | `/livez` | Gives the process up to 30s to connect to Postgres, apply migrations (in standalone mode), and bind the HTTP listener. |
| **Liveness** | `/livez` | Restarts the container if the HTTP listener deadlocks. Matches the `Dockerfile` `HEALTHCHECK`. |
| **Readiness** | `/readyz` | Removes the pod from the Service when Postgres/RPC (standalone) or the upstream SoroTrail API (upstream mode) is unreachable. |
