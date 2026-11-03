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

## Standalone mode operational notes

If you do run standalone mode, the same continuity consideration applies as any RPC-polling ingester: downtime beyond the RPC's retention window means a permanent gap in what SoroLens captured for that period. If continuous, gap-free history matters to you, prefer upstream mode against a SoroTrail instance that's already handling that concern.

## Sizing

**Standalone mode:** light on compute; Postgres storage scales with how many contracts you watch and how long you run. **Upstream mode:** lighter still — no local storage growth at all, since SoroLens holds nothing beyond what it needs to render the current request.

## Upgrades

1. (Standalone only) back up the database.
2. Pull the new version, run migrations if standalone, restart.
3. Verify `GET /health` reports the expected source mode and reachability.

## Health & Readiness Probes

SoroLens exposes three health endpoints at the root path:

- `GET /livez` — process liveness probe (`200 OK`, `{"status": "alive"}`). Checks no external dependencies so a transient Postgres or Stellar RPC outage never causes an orchestrator restart loop.
- `GET /readyz` — readiness probe (`200 OK` when `healthy` is `true`, `503 Service Unavailable` otherwise). Reports `mode`, `state`, `detail`, `latest_ledger`, and per-dependency status in `dependencies`.
- `GET /health` — full source health report (`200 OK` when `healthy` is `true`, `503 Service Unavailable` otherwise). Serializes `source.Status` including `state`, `dependencies`, `detail`, `latest_ledger`, and `retention_note`.

Example `GET /health` response in standalone mode (`SOURCE_MODE=rpc`) when Stellar RPC is unreachable while Postgres remains up:

```json
{
  "mode": "rpc",
  "healthy": false,
  "state": "degraded",
  "detail": "stellar rpc unreachable: context deadline exceeded",
  "retention_note": "Standalone mode captures events only while SoroLens is running...",
  "dependencies": {
    "postgres": {
      "status": "healthy"
    },
    "rpc": {
      "status": "unhealthy",
      "detail": "context deadline exceeded"
    }
  }
}
```

### Alerting on `state` and `dependencies`

- `state: "healthy"` — all dependencies (`postgres` and `rpc` in standalone mode, `sorotrail` in upstream mode) are reachable and healthy.
- `state: "degraded"` — standalone mode can still serve stored event history from `postgres`, but live ingestion from `rpc` is failing or syncing. Page or warn on `dependencies.rpc` without treating stored-history reads as down.
- `state: "unhealthy"` — the primary data store (`postgres` in standalone mode or `sorotrail` in upstream mode) is unreachable. Any connection-string credentials in error details are automatically redacted before serialization.
