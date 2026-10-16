# Substrate v0.5 deployment

Install the pinned control plane with `bash test/harness.sh up-substrate`.
The local test deployment is `test/proxy-substrate.yaml`; the browser template
and worker pool here use the same v0.5 schemas.

`WorkerPool` remains a Kubernetes resource. `ActorTemplate`, `Actor`, and actor
`EgressPolicy` are substrate API resources, **not Kubernetes CRDs**:

```sh
kubectl create namespace agents
kubectl label namespace agents ate.dev/api-access=true
kubectl-ate create atespace agents
# Substitute NAMESPACE, ATEOM_IMAGE and the browser digest first:
kubectl apply -f rendered-workerpool.yaml
kubectl-ate create actor-template -f rendered-actortemplate.yaml
```

Build `kubectl-ate` and the worker image from substrate **v0.5.0**. Templates are
immutable: use a new template name for a changed image, runtime or process
flags. Existing actors using a different template are rejected by the proxy;
migrate or delete those actors explicitly.

Configure the proxy with `BACKEND=substrate`,
`SUBSTRATE_ACTOR_TEMPLATE=agents/playwright`,
`SUBSTRATE_API_ENDPOINT=api.ate-system.svc.cluster.local:443`, and
`SUBSTRATE_ROUTER_ADDR=atenet-router.ate-system.svc.cluster.local:80`.
`SUBSTRATE_FORCE_BOOT=true` is rejected: v0.5 removed that RPC option.

Project a ServiceAccount token with **audience `api.ate-system.svc`**, mount it
read-only, and set `SUBSTRATE_TOKEN_FILE` to the mounted file. The default
Kubernetes API token has the wrong audience. See `test/proxy-substrate.yaml`
for the exact volume and environment configuration. Token contents are reread
for each RPC to support rotation. The current adapter encrypts API traffic but
does not verify its self-signed server certificate; provision verified TLS
before treating this example as a production trust configuration.

The namespace's API-access label is a **NetworkPolicy allowance**, not RBAC.
Upstream v0.5 authentication does not yet provide per-resource authorization.
Only trusted proxies should receive API credentials.

The proxy derives `ate-target-actor: agents/pw-<id>` from the identified
caller and overwrites any client-supplied value on HTTP and WebSocket paths.
Actor services listen on **port 80**; actor selection is no longer encoded in
the HTTP Host header.

Actor egress, including DNS, is denied without an EgressPolicy. Either manage
policies with `kubectl-ate create egress-policy ACTOR -a agents -f FILE`, or
explicitly configure `SUBSTRATE_ALLOWED_HTTP_HOSTS=fixture.agents.svc.cluster.local`.
That optional setting manages each actor's `default` policy for **cleartext HTTP
on port 80 only**. An empty setting leaves policies untouched; it does not
allow all egress. A conflicting policy causes an explicit error rather than
being overwritten. HTTPS and TLS passthrough require operator-managed policies
and an unset `SUBSTRATE_ALLOWED_HTTP_HOSTS`.

For a complete local browser run:

```sh
SUBSTRATE_FLAVOUR=playwright bash test/harness.sh test substrate
```

The kind setup raises `fs.inotify.max_user_instances` to at least 8192 on its
nodes when needed by Envoy. On Docker this sysctl affects the shared Linux VM.
