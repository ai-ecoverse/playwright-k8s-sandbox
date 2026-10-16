#!/usr/bin/env bash
# Sourced by harness.sh and the offline regression test.

prepare_kind_node_image() {
  if docker image inspect "$KIND_NODE_IMAGE" >/dev/null 2>&1; then
    return
  fi
  local builder_version
  builder_version=$(kind version) || fail "Cannot determine the kind builder version"
  case "$builder_version" in
    "kind $KIND_BUILD_VERSION "*) ;;
    *) fail "Building $KIND_NODE_IMAGE requires kind $KIND_BUILD_VERSION (got '$builder_version')" ;;
  esac
  log "building $KIND_NODE_IMAGE from Kubernetes release $KUBERNETES_VERSION"
  kind build node-image --type release --image "$KIND_NODE_IMAGE" "$KUBERNETES_VERSION" \
    || fail "Could not build the pinned Kubernetes $KUBERNETES_VERSION node image; no older version was substituted"
}

check_existing_kind_cluster() {
  local cluster=$1 versions version mismatch=0
  versions=$(command kubectl --context "kind-$cluster" get nodes \
    -o jsonpath='{range .items[*]}{.status.nodeInfo.kubeletVersion}{"\n"}{end}') \
    || fail "Cannot verify Kubernetes version in existing cluster '$cluster'; no workloads changed"
  [ -n "$versions" ] || mismatch=1
  while IFS= read -r version; do
    [ "$version" = "$KUBERNETES_VERSION" ] || mismatch=1
  done <<< "$versions"
  if [ "$mismatch" = 1 ]; then
    if [ "${ALLOW_EXISTING_KUBERNETES_VERSION:-0}" != 1 ]; then
      fail "Existing cluster '$cluster' runs [${versions:-unknown}], expected latest stable $KUBERNETES_VERSION. It was NOT recreated or upgraded. Use a new CLUSTER/SUBSTRATE_CLUSTER name, or ALLOW_EXISTING_KUBERNETES_VERSION=1 to explicitly reuse historical Kubernetes (not a latest-version test)."
    fi
    log "WARNING: explicitly reusing '$cluster' on [$versions], NOT verified latest stable $KUBERNETES_VERSION"
  fi
}

render_substrate_kind_config() {
  local ip_family=${IP_FAMILY:-ipv4} has_kvm=${1:-0}
  case "$ip_family" in
    ipv4|ipv6|dual) ;;
    *) fail "IP_FAMILY must be ipv4, ipv6 or dual (got '$ip_family')" ;;
  esac
  cat <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
EOF
  if [ "$has_kvm" = 1 ]; then
    cat <<'EOF'
  extraMounts:
  - hostPath: /dev/kvm
    containerPath: /dev/kvm
EOF
  fi
  cat <<EOF
featureGates:
  ClusterTrustBundle: true
  ClusterTrustBundleProjection: true
  PodCertificateRequest: true
runtimeConfig:
  "certificates.k8s.io/v1beta1": "true"
networking:
  ipFamily: $ip_family
kubeadmConfigPatches:
- |
  kind: KubeletConfiguration
  serializeImagePulls: false
  maxParallelImagePulls: 4
EOF
}

create_pinned_kind_cluster() {
  local cluster=$1 config=$2
  prepare_kind_node_image
  mkdir -p "$ROOT/.backend-sources"
  # Do not let kind switch the user's global current-context.
  kind create cluster --name "$cluster" --config "$config" \
    --image "$KIND_NODE_IMAGE" --kubeconfig "$HARNESS_KUBECONFIG" --wait 120s
  check_existing_kind_cluster "$cluster"
}

create_substrate_kind_cluster() {
  local cluster=$1 reg_name=kind-registry reg_port=${KIND_REGISTRY_PORT:-5001}
  local has_kvm=0 node config
  prepare_kind_node_image
  # Upstream v0.5.0 has no image/config override and unconditionally deletes the
  # cluster. Generate its required configuration locally instead of modifying
  # an upstream checkout (including its possibly dirty bin/kind-config.yaml).
  case "${IP_FAMILY:-ipv4}" in
    ipv4|ipv6|dual) ;;
    *) fail "IP_FAMILY must be ipv4, ipv6 or dual" ;;
  esac
  if [ "${IP_FAMILY:-ipv4}" != ipv4 ] &&
      [ "$(docker network inspect kind --format '{{.EnableIPv6}}' 2>/dev/null || true)" = false ]; then
    fail "Existing shared kind network lacks IPv6; refusing to delete it. Use ipv4 or a separate Docker environment."
  fi
  if docker container inspect "$reg_name" >/dev/null 2>&1; then
    [ "$(docker inspect -f '{{.State.Running}}' "$reg_name")" = true ] \
      || fail "Existing $reg_name is stopped; start it explicitly (it was not replaced)"
    docker port "$reg_name" 5000/tcp | grep -q ":${reg_port}$" \
      || fail "Existing $reg_name does not publish port $reg_port; refusing to replace a shared registry"
  else
    docker run -d --restart=always --label created-by=agent-substrate \
      -p "127.0.0.1:$reg_port:5000" -p "[::1]:$reg_port:5000" \
      --network bridge --name "$reg_name" registry:3
  fi
  if docker run --rm --device /dev/kvm busybox true >/dev/null 2>&1; then
    has_kvm=1
  fi
  mkdir -p "$ROOT/.backend-sources"
  config="$ROOT/.backend-sources/substrate-kind-$cluster.yaml"
  render_substrate_kind_config "$has_kvm" > "$config"
  create_pinned_kind_cluster "$cluster" "$config"
  if [ "${IP_FAMILY:-ipv4}" != ipv4 ]; then
    [ "$(docker network inspect kind --format '{{.EnableIPv6}}')" = true ] \
      || fail "Docker kind network lacks IPv6; enable it explicitly (cluster retained)"
  fi
  if [ "${IP_FAMILY:-ipv4}" = ipv6 ]; then
    local server
    server=$(command kubectl --kubeconfig "$HARNESS_KUBECONFIG" config view \
      -o jsonpath="{.clusters[?(@.name==\"kind-$cluster\")].cluster.server}")
    if [[ "$server" == "https://[::1]:"* ]]; then
      command kubectl --kubeconfig "$HARNESS_KUBECONFIG" config set-cluster "kind-$cluster" \
        --server="https://localhost:${server##*:}" >/dev/null
    fi
  fi
  for node in $(kind get nodes --name "$cluster"); do
    docker exec "$node" sysctl net.ipv4.conf.all.proxy_arp=1
    docker exec "$node" sysctl -e net.ipv6.conf.all.proxy_ndp=1
    if [ "$has_kvm" = 1 ]; then
      docker exec "$node" chmod 666 /dev/kvm
    fi
    docker exec "$node" mkdir -p "/etc/containerd/certs.d/localhost:$reg_port"
    printf '[host."http://%s:5000"]\n' "$reg_name" |
      docker exec -i "$node" cp /dev/stdin "/etc/containerd/certs.d/localhost:$reg_port/hosts.toml"
  done
  if [ "$(docker inspect -f='{{json .NetworkSettings.Networks.kind}}' "$reg_name")" = null ]; then
    docker network connect kind "$reg_name"
  fi
  cat <<EOF | command kubectl --context "kind-$cluster" apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:$reg_port"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF
}
