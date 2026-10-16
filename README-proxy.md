# playwright-proxy

A per-namespace Kubernetes proxy for Playwright's native WebSocket protocol
and MCP over HTTP. Agent pods labelled `playwright-id` get a dedicated sandbox;
idle sessions are reaped only when no connections remain open.

Current release pins are in [`test/versions.env`](test/versions.env):
agent-sandbox v1.0.7, isola v0.7.0, KARS v0.1.26, substrate v0.5.0.
See the [release update and kind results](docs/blog/sandbox-updates-2026-10.md).
OpenShell remains supported through SandboxClaim but was not part of this update.

## Identity and routing

Set a stable label on each calling agent pod:

```yaml
metadata:
  labels:
    playwright-id: my-agent
```

The proxy maps the TCP peer's source IP to that pod's label using an informer
and bounded API fallback. Unlabelled callers receive 403. A session manager
singleflights provisioning per ID, then proxies HTTP or upgrades and splices
WebSocket connections. Requests from a rescheduled pod with the same label
reuse the same sandbox.

| `BACKEND` | Provisioned resource | Data path |
|---|---|---|
| `sandboxclaim` | SandboxClaim referencing a SandboxTemplate/WarmPool | Bound sandbox pod; agent-sandbox and OpenShell |
| `isola` | `sandbox.isola.run/v1alpha1` Sandbox | gVisor pod IP in the shared sandbox namespace |
| `karssandbox` | `kars.azure.com/v1alpha1` KarsSandbox | Ready BYO pod IP in its dedicated namespace |
| `substrate` | Atespace-scoped Actor through the Control gRPC API | atenet-router with trusted `ate-target-actor: atespace/name` |

Substrate's namespace-like API scope is called an **atespace**. Its Actors and
ActorTemplates are not Kubernetes CRDs. Caller-supplied actor routing headers
are stripped or overwritten; they never select another actor.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `POD_NAMESPACE` | required | Proxy/client namespace, normally from the downward API |
| `BACKEND` | `sandboxclaim` | One of the four adapter names above |
| `LISTEN_ADDR` / `METRICS_ADDR` | `:9000` / `:9090` | Data plane / management |
| `PLAYWRIGHT_LABEL_KEY` | `playwright-id` | Caller identity label |
| `WARMPOOL_NAME` | required for SandboxClaim | SandboxWarmPool name |
| `SANDBOX_TEMPLATE_NAME` | `$WARMPOOL_NAME` | SandboxTemplate name |
| `SANDBOX_PORT` | `9222` | Browser port for pod-based adapters |
| `SANDBOX_MCP_PORT` | same as browser port | Optional separate HTTP/MCP port for SandboxClaim |
| `ISOLA_NAMESPACE` | `isola-sandboxes` | Shared isola sandbox namespace |
| `ISOLA_SANDBOX_IMAGE` | required for isola | Browser image |
| `ISOLA_ALLOWED_EGRESS_CIDRS` | unset | Upstream pass-through; v0.7 ignores it with internet egress enabled; not a private-network exception |
| `KARS_INFERENCE_REF` | required for KARS | In-namespace InferencePolicy |
| `KARS_SANDBOX_IMAGE` | required for KARS | BYO browser image |
| `SUBSTRATE_API_ENDPOINT` | `api.ate-system.svc.cluster.local:443` | Control gRPC endpoint |
| `SUBSTRATE_ROUTER_ADDR` | `atenet-router.ate-system.svc.cluster.local:80` | Data-plane router |
| `SUBSTRATE_ACTOR_TEMPLATE` | required for substrate | `atespace/name` |
| `SUBSTRATE_TOKEN_FILE` | standard ServiceAccount token path | Rotating bearer-token file; explicitly project the correct audience |
| `SUBSTRATE_ALLOWED_HTTP_HOSTS` | unset | Opt-in managed actor egress policy: comma-separated hostnames, HTTP port 80 only |
| `SUBSTRATE_FORCE_BOOT` | false | Legacy setting; true is rejected by the v0.5 adapter |
| `IDLE_TTL` / `IDLE_CHECK_INTERVAL` | `10m` / `30s` | Soft idle reap / scan cadence |
| `ENSURE_TIMEOUT` | `30s` | Provisioning budget; kind examples increase this |

`/healthz`, `/readyz`, and `/metrics` are on the management listener.

## Deploy

Install the appropriate controller before deploying its examples under
[`deploy/examples/`](deploy/examples/). The base proxy manifests are
[`deploy/rbac.yaml`](deploy/rbac.yaml) and [`deploy/proxy.yaml`](deploy/proxy.yaml).
For agent-sandbox, install both release assets:

```sh
source test/versions.env
kubectl apply --server-side -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$AGENT_SANDBOX_VERSION/sandbox.yaml"
kubectl apply --server-side -f "https://github.com/kubernetes-sigs/agent-sandbox/releases/download/$AGENT_SANDBOX_VERSION/extensions.yaml"
```

Isola requires the companion proxy ingress policy. Cluster-internal browser
targets also need an explicit destination-scoped egress policy; see
[`test/isola-allow-test-target.yaml`](test/isola-allow-test-target.yaml).

KARS requires its additional RBAC, an InferencePolicy selecting managed
sandboxes, and an in-namespace `kars-default` ToolPolicy with default governance.
The adapter creates a browser ingress policy restricted to proxy pods in the
configured namespace; this requires cluster-scoped NetworkPolicy create/get
permission because sandbox namespaces are dynamic. It does not modify KARS'
egress guard or default policy. Private browser destinations remain blocked.

For substrate's new schemas, projected-token audience, API network access,
snapshot configuration, and explicit egress, use the
[v0.5 deployment guide](deploy/examples/substrate/README.md).

## Local kind tests

```sh
bash test/harness.sh up
bash test/harness.sh e2e
bash test/harness.sh up-isola
bash test/harness.sh test isola
bash test/harness.sh up-kars
bash test/harness.sh test kars # private fixture fetch currently fails under KARS egress enforcement
# Narrower browser rendering test, without claiming cross-pod egress:
KARS_TARGET_URL=http://127.0.0.1:9222/ KARS_EXPECT_TEXT=Running bash test/harness.sh test kars
bash test/harness.sh up-substrate
SUBSTRATE_FLAVOUR=playwright bash test/harness.sh test substrate
```

The first three controllers share `playwright-proxy`; substrate uses `kind`
and its local registry. Source caches use pinned tags, not mutable sibling
checkouts. `KARS_REPO`, `SUBSTRATE_REPO`, and `KUBECTL_ATE` override those paths.
`up-isola` installs checksum-verified runsc and its containerd shim, not just a
RuntimeClass. The common browser workload stays pinned to Playwright 1.53.0,
MCP 0.0.29, and Node 22 to distinguish backend changes from workload upgrades.

`test/bench.sh BACKEND` measures cold/warm/restore scenarios and exits nonzero
on failed measurements. For agent-sandbox, isola and KARS, "restore" is
delete/recreate, not checkpoint restoration. Substrate uses a real suspend
and automatic router resume, with no force-boot override.

## Caveats

Source-IP identity requires in-cluster traffic that preserves pod addresses.
Ingress/LB hops, shared `hostNetwork` addresses, and an untrusted caller's
ability to assign another agent's label undermine this identity model.
Use admission controls and stronger authenticated identity where appropriate.
Labels must meet Kubernetes constraints; hash longer identifiers.

## Build

```sh
go build ./...
go test ./...
docker build -t playwright-proxy:dev .
```
