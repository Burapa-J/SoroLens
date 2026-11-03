# Deployment

## Docker Compose (single host, standalone mode)

```bash
cp .env.example .env    # set RPC_URL, DATABASE credentials
docker compose up -d
```

Put a reverse proxy (Caddy, nginx, Traefik) in front for TLS. SoroLens is read-only, so exposing it publicly is lower-risk than a service with a mutation API — but it's still worth deciding deliberately whether it should be public or internal-only.

## Deploying in upstream mode

No local Postgres is needed — just the SoroLens binary/container and network access to your SoroTrail instance's API:

```bash
SOURCE_MODE=sorotrail
SOROTRAIL_URL=https://your-sorotrail-instance.example.com
HTTP_ADDR=:8080
```

This is the lighter deployment: one process, no database, no ingestion loop of its own. If you already run SoroTrail in production, running SoroLens alongside it in upstream mode is usually the simpler choice.

## Kubernetes (Helm)

A Helm chart lives at [`deploy/helm/sorolens/`](../../deploy/helm/sorolens/README.md) and supports both operating modes:

```bash
# Standalone mode (with an external PostgreSQL database):
helm install sorolens ./deploy/helm/sorolens \
  --set database.url="postgres://user:pass@host:5432/sorolens?sslmode=require" \
  --set config.network="testnet"

# Upstream mode (reading from an existing SoroTrail HTTP API):
helm install sorolens ./deploy/helm/sorolens \
  --set config.sourceMode="sorotrail" \
  --set config.sorotrailUrl="http://sorotrail.default.svc.cluster.local:8080"
```

See [`deploy/helm/sorolens/README.md`](../../deploy/helm/sorolens/README.md) for the full values reference, existing-Secret wiring, `ServiceMonitor`, and `PodDisruptionBudget` options.

## Standalone mode operational notes

If you do run standalone mode, the same continuity consideration applies as any RPC-polling ingester: downtime beyond the RPC's retention window means a permanent gap in what SoroLens captured for that period. If continuous, gap-free history matters to you, prefer upstream mode against a SoroTrail instance that's already handling that concern.

## Sizing

**Standalone mode:** light on compute; Postgres storage scales with how many contracts you watch and how long you run. **Upstream mode:** lighter still — no local storage growth at all, since SoroLens holds nothing beyond what it needs to render the current request.

## Upgrades

1. (Standalone only) back up the database.
2. Pull the new version, run migrations if standalone, restart.
3. Verify `GET /health` reports the expected source mode and reachability.
