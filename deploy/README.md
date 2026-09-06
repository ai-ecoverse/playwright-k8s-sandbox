# Playwright Proxy Deployment

This directory contains Kubernetes manifests for deploying the Playwright proxy.

## Prerequisites

- Kubernetes cluster (1.24+)
- `kubectl` configured with cluster access
- GitHub Container Registry access (for CI/CD)

## Quick Start

### 1. Deploy to a namespace

Replace `NAMESPACE` with your target namespace (e.g., `playwright-sandbox`):

```bash
# Apply RBAC first
sed 's/NAMESPACE/playwright-sandbox/g' rbac.yaml | kubectl apply -f -

# Deploy the proxy
sed 's/NAMESPACE/playwright-sandbox/g' proxy.yaml | kubectl apply -f -

# (Optional) Add PodDisruptionBudget for production
sed 's/NAMESPACE/playwright-sandbox/g' pdb.yaml | kubectl apply -f -

# (Optional) Add NetworkPolicy for network segmentation
sed 's/NAMESPACE/playwright-sandbox/g' networkpolicy.yaml | kubectl apply -f -
```

### 2. Verify deployment

```bash
kubectl -n playwright-sandbox get pods -l app.kubernetes.io/name=playwright-proxy
kubectl -n playwright-sandbox logs -l app.kubernetes.io/name=playwright-proxy
```

### 3. Test the proxy

```bash
kubectl -n playwright-sandbox port-forward svc/playwright-proxy 9000:9000

# In another terminal, test the connection
curl http://localhost:9000/healthz
```

## Configuration

The proxy is configured via environment variables in `proxy.yaml`:

| Variable | Default | Description |
|----------|---------|-------------|
| `BACKEND` | `sandboxclaim` | Backend type: `sandboxclaim` or `substrate` |
| `WARMPOOL_NAME` | `playwright` | SandboxWarmPool name (agent-sandbox) |
| `SANDBOX_TEMPLATE_NAME` | (from WARMPOOL_NAME) | Override sandbox template |
| `SANDBOX_PORT` | `9222` | Port exposed by sandbox containers |
| `IDLE_TTL` | `10m` | How long to keep idle sandboxes alive |
| `IDLE_CHECK_INTERVAL` | `30s` | How often to check for idle sandboxes |
| `ENSURE_TIMEOUT` | `30s` | Timeout for sandbox creation |

For substrate backend, also configure:
- `SUBSTRATE_ROUTER_ADDR`: Substrate router address
- `SUBSTRATE_ACTOR_TEMPLATE`: Actor template name

## Security Features

The deployment includes several security best practices:

### Dockerfile
- Multi-stage build with minimal runtime image (distroless)
- Non-root user (UID 65532)
- Static binary compilation (no dynamic dependencies)
- Build-time dependency verification
- Multi-architecture support (amd64/arm64)

### Pod Security
- Read-only root filesystem
- No privilege escalation
- All capabilities dropped
- Runs as non-root user (65532)
- Seccomp profile (RuntimeDefault)
- Resource limits enforced

### RBAC
- Minimal permissions (namespace-scoped Role)
- Service account with least privilege
- Only required API access granted

### Network Security (optional)
- NetworkPolicy restricts ingress/egress
- DNS and API server access allowed
- Pod-to-pod communication controlled

### High Availability (optional)
- PodDisruptionBudget ensures availability during disruptions
- RollingUpdate strategy (zero-downtime deployments)

## CI/CD Setup

### GitHub Actions

The repository includes a GitHub Actions workflow that automatically builds and pushes images to GitHub Container Registry (ghcr.io).

#### Required Secrets

**No additional secrets required!** The workflow uses `GITHUB_TOKEN` which is automatically available with permissions to push to GitHub Container Registry.

#### First-time Setup

After your first successful build, make the package public (optional):
1. Go to: `https://github.com/users/YOUR_USERNAME/packages/container/playwright-k8s-sandbox`
2. Click "Package settings"
3. Scroll to "Danger Zone" → "Change visibility" → "Public"

#### Trigger Conditions

The workflow runs on:
- Push to `main` branch (with changes to source code)
- Pull requests to `main` branch
- Release publication
- Manual trigger (workflow_dispatch)

#### Image Tags

Images are tagged as:
- `latest` - Latest build from main branch
- `main-<sha>` - Commit-based tags
- `pr-<number>` - Pull request builds
- `v1.2.3`, `v1.2`, `v1` - Semantic version tags (on releases)

### Manual Build

To build and push manually:

```bash
# Build the image
docker build -t ghcr.io/carlossg/playwright-k8s-sandbox:latest .

# Log in to GitHub Container Registry
echo $GITHUB_TOKEN | docker login ghcr.io -u USERNAME --password-stdin

# Push the image
docker push ghcr.io/carlossg/playwright-k8s-sandbox:latest
```

## Monitoring

The proxy exposes metrics and health endpoints on the mgmt port 9090:

- `/metrics` - Prometheus metrics (see below)
- `/healthz` - Liveness check
- `/readyz`  - Readiness check

### Scraping

Two options:

1. **Pod annotations** (plain Prometheus with `kubernetes-pods` relabeling),
   defined in `proxy.yaml`. The Deployment pod template already carries:

   ```yaml
   annotations:
     prometheus.io/scrape: "true"
     prometheus.io/port: "9090"
     prometheus.io/path: "/metrics"
   ```

2. **Prometheus Operator** — apply `servicemonitor.yaml` (targets the `mgmt`
   Service port) and `prometheusrule.yaml` (alerts). Adjust the `release` label
   to match your operator's selector.

### Exported metrics

All series are prefixed `playwright_`. Standard `go_*` / `process_*` runtime
metrics are also exported.

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `playwright_sessions_created_total` | counter | `backend`, `outcome` | Sessions created (when / how many). |
| `playwright_sessions_active` | gauge | — | Sessions currently running. |
| `playwright_sessions_reaped_total` | counter | `reason` | Sessions torn down. |
| `playwright_session_ensure_duration_seconds` | histogram | `backend`, `outcome` | Provisioning / cold-start time. |
| `playwright_session_ensure_failures_total` | counter | `backend` | Provisioning failures. |
| `playwright_session_lifetime_seconds` | histogram | `backend` | Creation → teardown. |
| `playwright_session_idle_seconds` | histogram | — | Idle time at reap. |
| `playwright_active_connections` | gauge | — | Open proxied connections. |
| `playwright_connection_duration_seconds` | histogram | `protocol` | Connection duration = actual usage time. |
| `playwright_requests_total` | counter | `protocol`, `code` | Proxied requests. |
| `playwright_request_duration_seconds` | histogram | `protocol` | HTTP/MCP request latency. |
| `playwright_bytes_transferred_total` | counter | `direction` | Bytes proxied (usage volume). |
| `playwright_lookups_total` | counter | `result` | IP→id lookups (cache_hit / api_fallback / miss). |
| `playwright_unknown_client_total` | counter | — | Rejected unlabelled clients (403s). |
| `playwright_registered_pods` | gauge | — | Client pods in the identify cache. |
| `playwright_backend_dial_failures_total` | counter | — | WS dial failures to sandbox. |
| `playwright_proxy_errors_total` | counter | `kind` | Proxy-layer errors. |
| `playwright_backend_delete_failures_total` | counter | — | Reap delete failures (sandbox leaks). |
| `playwright_build_info` | gauge | `version`, `commit`, `backend` | Build metadata (always 1). |

Useful queries:

```promql
# Sessions created per hour by outcome
sum by (outcome) (increase(playwright_sessions_created_total[1h]))

# Actual usage time (connection-seconds) over the last hour
sum(increase(playwright_connection_duration_seconds_sum[1h]))

# p95 cold-start latency
histogram_quantile(0.95, sum by (le) (rate(playwright_session_ensure_duration_seconds_bucket[10m])))
```

### Alerts

`prometheusrule.yaml` ships starting-point alerts for the error conditions worth
watching: proxy down, provisioning failures / high ratio, slow cold starts,
backend dial failures, upstream & 5xx errors, unknown-client rejections, high
lookup-miss ratio, and sandbox-reap (leak) failures. Tune the thresholds to your
traffic.

### Grafana dashboard

`grafana-dashboard.json` is an importable dashboard (Dashboards → New → Import →
Upload JSON) covering the full metric set:

- **Overview** — sessions running, active connections, creation & ensure-failure
  rates, registered client pods, and proxy up/down.
- **Sandbox lifecycle** — creation/reap rates, provisioning (Ensure) latency
  p50/p95/p99, ensure failures, and session lifetime / idle-at-reap.
- **Usage** — active connections, connection-duration p95, usage time
  (connection-seconds per minute), request rate by protocol & code, bytes
  transferred, and request latency p95.
- **Routing & errors** — identify lookups by result, unknown-client rejections,
  proxy/backend/reap errors, and the HTTP 5xx ratio.

It uses a `datasource` template variable (pick your Prometheus source on import)
and a `backend` variable to filter by backend. No manual UID editing needed.

## Troubleshooting

### Proxy pod not starting

```bash
kubectl -n playwright-sandbox describe pod -l app.kubernetes.io/name=playwright-proxy
kubectl -n playwright-sandbox logs -l app.kubernetes.io/name=playwright-proxy
```

### RBAC permission errors

Verify the service account has proper permissions:

```bash
kubectl -n playwright-sandbox get role playwright-proxy -o yaml
kubectl -n playwright-sandbox get rolebinding playwright-proxy -o yaml
```

### Connection issues

Check if the service is properly configured:

```bash
kubectl -n playwright-sandbox get svc playwright-proxy
kubectl -n playwright-sandbox get endpoints playwright-proxy
```

### NetworkPolicy blocking traffic

If you applied the NetworkPolicy and traffic is blocked:

```bash
# Temporarily remove to test
kubectl -n playwright-sandbox delete networkpolicy playwright-proxy

# Review and adjust the policy based on your pod labels
kubectl -n playwright-sandbox get pods --show-labels
```

## Production Considerations

1. **Multi-replica deployment**: Increase `replicas` in `proxy.yaml` for HA
2. **Resource tuning**: Adjust CPU/memory based on load
3. **Monitoring**: Add metrics collection and alerting
4. **Backup**: Ensure RBAC and config are version-controlled
5. **Image updates**: Pin specific image tags instead of `latest`
6. **Network policies**: Customize based on your cluster's network model
7. **Pod security standards**: Apply appropriate PSS labels to namespace

## License

See [LICENSE](../LICENSE) file in the repository root.
