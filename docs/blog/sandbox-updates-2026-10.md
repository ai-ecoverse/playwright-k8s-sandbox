# What changed from the sandbox integrations already in this repository

*October 16, 2026*

The [original Playwright sandbox comparison](playwright-k8s-sandbox-proxy.md)
looked at a useful abstraction: identify a calling agent pod, give it its own
browser sandbox, and hide provisioning behind one HTTP/WebSocket endpoint.
This follow-up compares that repository's existing integrations with the
updated setup. The baseline is the
[pre-upgrade repository at `1705fa2`](https://github.com/carlossg/playwright-k8s-sandbox/tree/1705fa2),
including its installation choices, API bindings, workarounds, and tests.
It is **not** a comparison of each latest upstream release with its
immediately preceding release.

## Which versions are we actually comparing?

| Implementation | Before, in this repository's setup | After |
|---|---|---|
| agent-sandbox | Installer explicitly pinned **v0.5.4** | **[v1.0.7](https://github.com/kubernetes-sigs/agent-sandbox/releases/tag/v1.0.7)** |
| isola | OCI Helm chart with **no version pin** | **[v0.7.0](https://github.com/isola-run/isola/releases/tag/v0.7.0)**; chart **0.7.0** |
| Azure KARS | Local source build, no release pin; starting checkout **`4c14dbe`** from June 3, 2026 | Published images and chart source from **[v0.1.26](https://github.com/Azure/kars/releases/tag/v0.1.26)** |
| substrate | Local source, no release pin, plus the repo's legacy vendored API; starting checkout **`aa9b7b82`** from August 5, 2026 | Source and vendored API from **[v0.5.0](https://github.com/agent-substrate/substrate/releases/tag/v0.5.0)** |

The
[old harness](https://github.com/carlossg/playwright-k8s-sandbox/blob/1705fa2/test/harness.sh)
only recorded a release version for agent-sandbox. The KARS and substrate
commits above identify the local source checkouts recorded at the start of
this work, not version pins committed in the repo or proof of a previously
deployed release. In particular, the substrate adapter's old vendored API
must be compared directly with its replacement; a local checkout could already
have moved ahead of those bindings.

For isola, there was no existing kind cluster or Helm release history to
recover an installed chart version from. Its local checkout was `58f72e2e`,
but the installer fetched the OCI chart independently, so that commit does
not identify the chart it would have installed. The old chart version
therefore remains unknown.

The updated harness removes that ambiguity. It records all four releases in
[`test/versions.env`](../../test/versions.env), selects matching
charts/manifests, and uses release-tagged source checkouts where a build is
needed.

## The changes that matter relative to the old setup

| Area | Before | After |
|---|---|---|
| agent-sandbox | v0.5.4 controller, SandboxClaims and a warm pool | v1.0.7 controller with the same proxy integration, checked with real browser and MCP clients |
| isola in kind | RuntimeClass object only; runsc installation left to the operator | Checksum-verified runtime and shim installed, policies corrected, real gVisor browser fetches passing |
| KARS deployment and routing | Local Rust build; browser traffic sent to the generated Service; pod phase treated as readiness | Published release images; actual PodReady check; proxy-only browser ingress and direct pod endpoint |
| substrate integration | Legacy API bindings, Host-based routing, Kubernetes-style template manifests, force-boot workaround | v0.5 API resources, trusted actor-routing header, projected API token, explicit egress, tested memory-resume path |

The important outcome is not four green controller Deployments. It is what a
real Chromium client could do after the upgrade, with network enforcement left
in place. Agent-sandbox, isola, and substrate passed the cross-pod browser-fetch
test. KARS passed WebSocket-driven browser rendering against a loopback target,
but **the original cluster-internal fetch still fails** under its enforced
egress boundary. Those are different results, not interchangeable successes.

## agent-sandbox: v0.5.4 to v1.0.7, without changing the proxy contract

The repository already used SandboxClaims and a SandboxWarmPool with
v0.5.4. Those are not new capabilities introduced by this update. The
[upstream comparison spans v0.5.4 through v1.0.7](https://github.com/kubernetes-sigs/agent-sandbox/compare/v0.5.4...v1.0.7),
not just the last patch release.

For this application, the change is the controller version behind the
existing contract: identify the client, create or reuse its claim, wait for
the bound sandbox, and proxy the browser connection. The SandboxClaim adapter
did not need an API rewrite. The runtime remains pod-based and the
`restore` benchmark still deletes and recreates the claim; this update does
not add checkpoint/restore to that backend.

The rerun checked that contract against v1.0.7: two labelled clients obtained
distinct sandboxes and drove Chromium over native Playwright WebSockets.
A third client completed an MCP-over-HTTP session, listed 25 tools, and
navigated through the separate MCP port.

This proxy does not use the upstream SDKs, `sandboxd`, or `sandbox-router`.
Their release notes are not evidence of new functionality enabled in this
repository.

## isola: from an incomplete local runtime setup to a verified one

The old harness installed an unversioned chart and created a `gvisor`
RuntimeClass, but explicitly left installation of runsc and its containerd
shim out of scope. It warned that stock kind nodes could not start those
sandbox pods. The upgrade now pins chart 0.7.0, installs checksum-verified
`runsc` and its shim, and configures containerd before creating the
RuntimeClass. The tested sandbox pods actually selected that runtime.

Two old networking assumptions also needed correction. The proxy ingress
policy selected `app: playwright-proxy`, while the Deployment actually used
`app.kubernetes.io/name: playwright-proxy`. The old fixture configuration
supplied broad private CIDRs, but the tested operator's internet-egress mode
skips custom CIDRs and excludes private addresses. That configuration did not
open the fixture.

The replacement is a narrowly scoped NetworkPolicy allowing sandbox pods to
reach only the labelled fixture in the test namespace on port 80. The target
also uses its namespace-qualified name,
`test-target.pw-isola.svc.cluster.local`, because the browser lives in
`isola-sandboxes`. Shell lookups now qualify the isola CR's API group so they
cannot accidentally select the co-installed agent-sandbox resource.

The improvement relative to this repo is a reproducible, passing gVisor
browser path, not the invention of gVisor support in isola. Since the old
chart version is unknown, this post does not present v0.7's bandwidth controls
or other changelog items as features newly acquired by this installation.

## KARS: local source and Service assumptions to release images and pod readiness

The old harness built controller and inference-router images from a local
checkout, whose recorded starting commit was `4c14dbe`. It did not select a
KARS release. The default path now pulls the published multi-architecture
v0.1.26 images and uses matching chart source; building Rust remains opt-in.
It also selects the public, versioned OpenClaw image used by the guard init
container instead of inheriting a private-registry `latest` reference.

The old example InferencePolicy lacked an `appliesTo` selector, and the
test namespace lacked the `kars-default` ToolPolicy required by the tested
governance configuration. Both are now supplied. These are differences from
the repo's previous setup, not claims about which individual upstream release
first introduced the requirements. Unused mesh federation is disabled for
the local test; governance and the egress guard remain enabled.

Browser routing also changed. Previously, the adapter accepted a pod in
phase `Running`, then returned the generated Service's DNS name on port 9222.
The tested release's Service only exposes inference on 8443. The adapter now
waits for the actual PodReady condition, creates a browser ingress policy
restricted to the proxy, and returns the ready pod's IP. The companion policy
requires cluster-scoped NetworkPolicy create/get permissions because the
sandbox namespaces are created dynamically.

The original article left KARS benchmark results pending. This rerun supplies
an actual result rather than assuming compatibility: Chromium rendered a
loopback page, but the original private ClusterIP fixture timed out in all
three lifecycle scenarios. This is not evidence that a previously passing
KARS fixture test regressed.

The v0.1.26 forward proxy rejects DNS results resolving to private IPs
unconditionally. A domain-allowlist entry or another Kubernetes NetworkPolicy
cannot override that check. We did not disable enforcement or relabel a
loopback fetch as proof of cluster-wide egress. The namespace-per-sandbox and
delete/recreate lifecycle were already part of this repo's KARS integration;
neither is a new capability from this update.

This run did not verify image signatures, Azure inference, federation, or
confidential isolation. The local inference endpoint is deliberately a
nonfunctional placeholder.

## substrate: legacy bindings and forced boot to the v0.5 API and memory resume

This is the largest integration change. The old repo combined local source
with vendored bindings using string actor IDs, `Actor_STATUS_*` values, and
`ResumeActorRequest.Boot`. Its templates were Kubernetes-style manifests with
embedded runsc URLs, `workerPoolRef`, and `snapshotsConfig`; the WorkerPool used
`ateomImage`. The proxy selected actors using a fabricated HTTP Host header.

The migration replaces those repo-level contracts with the pinned v0.5.0
equivalents:

| Contract | Previous repo | Updated setup |
|---|---|---|
| Actor identity and state | String actor IDs and `Actor_STATUS_*` | Atespace-scoped ObjectRefs and `ACTOR_STATE_*` |
| ActorTemplate installation | Kubernetes `apiVersion/kind/spec` wrapper | Resource created through the substrate API |
| Runtime selection | Per-template runsc URLs, including the May 19 nightly | Named SandboxConfig and SandboxClass; tested release uses gVisor 20260907.0 |
| WorkerPool | `ateomImage`, template `workerPoolRef` | `workerImage`, `sandboxClasses`, label-based worker selection |
| Application readiness | Legacy template/container shape | Container `wakeupProbe` |
| Snapshot configuration | `snapshotsConfig.location` and a force-boot override | `snapshotConfig.preferredFidelity`; no boot override |
| Data-plane selection | `<actor>.actors.resources.substrate.ate.dev` Host | Trusted `ate-target-actor: <atespace>/<actor>` header |
| API authentication | No projected API-audience token in the proxy manifest | Rotating projected token with audience `api.ate-system.svc` |
| Fixture egress | No managed actor EgressPolicy in this adapter | Optional exact-host HTTP policy via `SUBSTRATE_ALLOWED_HTTP_HOSTS` |

The actor-routing header is derived from the identified client and overwrites
any caller-supplied value, including on retries. The new token source rereads
the mounted file for each RPC. The namespace also receives the
`ate.dev/api-access=true` network allowance, but that is not authorization:
upstream authentication still lacks per-resource RBAC. Existing self-signed
TLS handling still skips server certificate verification.

Without an actor EgressPolicy, even DNS is denied in the tested setup.
The optional hostname setting creates each actor's default policy for HTTP
port 80 only; an empty setting leaves policy management to the operator.
Conflicts produce an error instead of overwriting an operator's policy.

The old proxy manifest explicitly set `SUBSTRATE_FORCE_BOOT=true` because
application restore did not work in that environment. The old article's fast
"restore" row was therefore a boot-from-spec result, not proof of a memory
restore. The new API rejects that setting, and this run no longer needs it.

The browser configuration also drops the inherited single-process workaround:
normal multiprocess Chromium passed repeated navigation, while single-process
mode failed on a subsequent new page. The default substrate test now exercises
that real browser instead of only `whoami`; the lightweight smoke-test flavour
remains available explicitly.

The lifecycle run created a fresh actor, reused it, suspended it, observed
`ACTOR_STATE_SUSPENDED` with a completed durable
`SNAPSHOT_FIDELITY_MEMORY` snapshot, and then successfully navigated again
after router-triggered resume. No force-boot override was used. This proves a
working browser after a reported memory-fidelity restore, **not** arbitrary
application-state persistence: we did not run an in-memory marker oracle.
The proxy's idle reaper still deletes actors rather than hibernating them.

## What ran, and how to read the numbers

There were no kind clusters initially. The run created `playwright-proxy`
for agent-sandbox/isola/KARS, and `kind` for substrate's own installation.
Both run in the same macOS ARM64 Colima VM: 4 CPUs and approximately 7.7 GiB
of memory. The clusters used Kubernetes **v1.35.0** and **v1.37.0**, respectively,
with kind **v0.33.0**. These are local compatibility results, not an identical
cluster configuration for a rigorous performance comparison.
The installed kubectl client was v1.37, outside the supported minor-version
skew for the v1.35 cluster; it emitted a warning even though these commands
completed. Use a matching kubectl version for a stricter reproduction.

The browser workload was held at Playwright **1.53.0** and MCP **0.0.29**.
The test image uses pinned Node **22**; an attempted Node 26 browser-image build
stalled during the Chromium download, so it was not used for the final run.
The proxy Dockerfile now declares BuildKit's `TARGETARCH` before using it:
the final binary was checked as `GOARCH=arm64`, rather than silently falling
back to amd64 on an ARM host.

| Test surface | Result | What it establishes |
|---|---|---|
| Go unit tests, race detector, vet | PASS | Adapter lifecycle, policy handling, token rotation, trusted actor routing and existing proxy behavior |
| agent-sandbox WS + MCP | PASS | Distinct sandboxes, real Chromium fetches, tools/list and browser navigation |
| isola WS | PASS | Distinct gVisor sandbox pods and permitted cross-namespace fixture fetches |
| substrate WS | PASS | API-created actors, explicit egress, browser fetches and suspend/resume |
| KARS WS + loopback page | PASS | Distinct sandbox namespaces and actual browser rendering within the boundary |
| KARS private fixture page | FAIL | The original cross-pod fetch is not supported by the tested enforced egress configuration |

The lifecycle harness measures client-observed connect, new-page, and
navigation time, with images already present. Cold means a new sandbox
resource, not a clean registry, empty node image cache, or new cluster.
Agent-sandbox still benefits from its warm pool.

| Backend | Scenario | Connect, ms | New page, ms | Navigate, ms | Total, ms |
|---|---|---:|---:|---:|---:|
| agent-sandbox | cold | 560 | 53 | 20 | 633 |
| agent-sandbox | warm | 432 | 25 | 8 | 465 |
| agent-sandbox | recreate | 532 | 31 | 16 | 579 |
| isola | cold | 2856 | 58 | 89 | 3003 |
| isola | warm | 32 | 91 | 45 | 168 |
| isola | recreate | 3346 | 85 | 24 | 3455 |
| substrate | cold | 2065 | 300 | 83 | 2448 |
| substrate | warm | 38 | 157 | 86 | 281 |
| substrate | memory resume | 476 | 361 | 93 | 930 |

These are **one observation per scenario**, not averages or percentiles.
Some independent backend runs overlapped on the shared VM. Informer retries,
CPU contention, sandbox isolation, worker reuse, and different cluster
versions all matter. There is no defensible "the update is X times faster"
claim here. KARS gets no comparable timing row because its original target
did not pass; its successful loopback test is a different workload.
Even that narrower test showed substantial startup variability: its two
clients took 15.0 seconds and 144.8 seconds to connect. Successful rendering
is not a claim of consistently fast KARS provisioning.

The original article used a larger 6-CPU, 16-GiB VM and Kubernetes 1.33.
The old versions were not rerun on this new environment, so these numbers
are not a controlled before/after performance comparison. In particular,
the old substrate `restore` result and this run's memory-resume result measure
different lifecycle paths.

The script retains the scenario name `restore` for compatibility, but for
agent-sandbox, isola, and KARS that path is delete/recreate, **not** checkpoint
restoration. For substrate, the actor is checked as suspended before the
timed request. Restarting the proxy after suspension would be invalid:
startup reconciliation can resume actors before timing begins. The harness
now restarts **before** suspension and lets the request trigger resume.
Readiness checks use the management port instead of accidentally provisioning
an extra "probe" actor.

The [machine-readable result record](../../test/results/2026-10-16-kind.json)
includes the release pins, environment, all twelve lifecycle outcomes, and
the distinction between KARS loopback success and private-fixture failure.

## Reproducing the run

Prerequisites include Docker, kind, kubectl, Helm, Go, and `ko` for substrate.
Allow enough Docker VM capacity; running every sample sandbox simultaneously
can exhaust the four-CPU node's scheduling budget.

```sh
bash test/harness.sh up
bash test/harness.sh e2e
bash test/harness.sh up-isola
bash test/harness.sh test isola
bash test/harness.sh up-kars
bash test/harness.sh test kars # currently fails the private fixture fetch

# Explicitly narrower KARS test; does not establish external/private egress:
KARS_TARGET_URL=http://127.0.0.1:9222/ KARS_EXPECT_TEXT=Running \
  bash test/harness.sh test kars

bash test/harness.sh up-substrate
SUBSTRATE_FLAVOUR=playwright bash test/harness.sh test substrate

bash test/bench.sh agent-sandbox
bash test/bench.sh isola
bash test/bench.sh substrate
bash test/bench.sh kars # preserves failures and returns nonzero

go test -race ./...
go vet ./...
```

The measured substrate run used `SUBSTRATE_NS=pw-substrate-v050` and
`SUBSTRATE_TEMPLATE_NAME=playwright-multiprocess` to avoid reusing immutable
templates and actors created during earlier migration attempts. Use a fresh
atespace/template name when changing an existing template's image or flags.
For benchmarks, pass the same `SUBSTRATE_NS` and, if needed, `KUBECTL_ATE`.
The [substrate deployment guide](../../deploy/examples/substrate/README.md)
covers its API resources, token audience and network allowances.

The setup also raises the kind nodes' `fs.inotify.max_user_instances` to at
least 8192 when needed by Envoy; on Docker this affects the shared Linux VM.
It uses the local Docker builder for pushes to the substrate registry, avoiding
a separate build-container's unrelated `localhost`.

The broader lesson is that a sandbox upgrade changes the integration
contract, not just an image tag. Scheduling behavior, policy selectors,
API authentication, routing headers, and egress enforcement determine whether
the browser can actually do its job. Keeping those boundaries enabled, and
reporting the KARS limitation rather than bypassing it, makes this rerun more
useful than a four-row green install checklist.
