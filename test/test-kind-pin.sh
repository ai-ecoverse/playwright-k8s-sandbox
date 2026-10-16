#!/usr/bin/env bash
# Offline regression tests; no Docker daemon, network or existing clusters used.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
source "$HERE/versions.env"
source "$HERE/kind-cluster.sh"
scratch="$ROOT/.backend-sources/kind-pin-test-$$"
mkdir -p "$scratch/bin"
trap 'rm -rf "$scratch"' EXIT
export KIND_TEST_SCRATCH="$scratch"
HARNESS_KUBECONFIG="$scratch/kubeconfig"
log() { printf '%s\n' "$*" >&2; }
fail() { printf '%s\n' "$*" >&2; exit 1; }
assert_contains() { grep -Fq -- "$2" "$1" || fail "Missing '$2' in $1"; }
assert_contains "$HERE/harness.sh" 'create_pinned_kind_cluster "$CLUSTER" "$HERE/kind-config.yaml"'
assert_contains "$HERE/harness.sh" 'create_substrate_kind_cluster "$SUBSTRATE_CLUSTER"'
assert_contains "$HERE/harness.sh" 'KUBECTL_CONTEXT="kind-$SUBSTRATE_CLUSTER"'

cat > "$scratch/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "$KIND_TEST_SCRATCH/kubectl-args"
if [[ " $* " == *" get nodes "* ]]; then
  printf '%s\n' "${TEST_NODE_VERSION:-v1.35.0}"
else
  cat > "$KIND_TEST_SCRATCH/registry.yaml"
fi
EOF
chmod +x "$scratch/bin/kubectl"
export PATH="$scratch/bin:$PATH"
kind() {
  printf '%s\n' "$*" >> "$scratch/kind-args"
  case "$1 ${2:-}" in
    "version ") printf 'kind %s go1.27.2 linux/arm64\n' "$KIND_BUILD_VERSION" ;;
    "build node-image") return 0 ;;
    "create cluster")
      while [ "$#" -gt 0 ]; do
        if [ "$1" = --config ]; then
          cp "$2" "$scratch/created.yaml"
          break
        fi
        shift
      done
      ;;
    "get nodes") printf '%s\n' custom-substrate-control-plane ;;
    *) fail "Unexpected kind command: $*" ;;
  esac
}
docker() {
  printf '%s\n' "$*" >> "$scratch/docker-args"
  case "$1 $2" in
    "image inspect") [ "${TEST_NODE_IMAGE_MISSING:-0}" != 1 ]; return ;;
    "container inspect") return 0 ;;
    "inspect -f") printf 'true\n' ;;
    "inspect -f="*) printf 'null\n' ;;
    "port kind-registry") printf '127.0.0.1:5001\n' ;;
    "run --rm") return 1 ;; # No KVM.
    "exec -i") cat > "$scratch/hosts.toml" ;;
  esac
}

[ "$KIND_NODE_IMAGE" = "playwright-kind-node:$KUBERNETES_VERSION" ] \
  || fail "Node image must track the exact committed Kubernetes release"
TEST_NODE_IMAGE_MISSING=1 prepare_kind_node_image
assert_contains "$scratch/kind-args" "build node-image --type release --image $KIND_NODE_IMAGE $KUBERNETES_VERSION"
export TEST_NODE_VERSION="$KUBERNETES_VERSION"
create_pinned_kind_cluster custom-primary "$HERE/kind-config.yaml"
assert_contains "$scratch/kind-args" "--name custom-primary"
assert_contains "$scratch/kind-args" "--image $KIND_NODE_IMAGE"
assert_contains "$scratch/kind-args" "--kubeconfig $HARNESS_KUBECONFIG"
[ "$(grep -c '^ *image:' "$scratch/created.yaml" || true)" = 0 ] \
  || fail "Config must not override the single --image pin"

: > "$scratch/kind-args"
ROOT="$scratch/project" create_substrate_kind_cluster custom-substrate
assert_contains "$scratch/kind-args" "--name custom-substrate"
assert_contains "$scratch/kind-args" "--image $KIND_NODE_IMAGE"
assert_contains "$scratch/kind-args" "--kubeconfig $HARNESS_KUBECONFIG"
for gate in ClusterTrustBundle ClusterTrustBundleProjection PodCertificateRequest; do
  assert_contains "$scratch/created.yaml" "$gate: true"
done
assert_contains "$scratch/created.yaml" '"certificates.k8s.io/v1beta1": "true"'
assert_contains "$scratch/created.yaml" "ipFamily: ipv4"
assert_contains "$scratch/created.yaml" "maxParallelImagePulls: 4"
assert_contains "$scratch/docker-args" "custom-substrate-control-plane"
assert_contains "$scratch/hosts.toml" '[host."http://kind-registry:5000"]'
assert_contains "$scratch/kubectl-args" "--context kind-custom-substrate apply -f -"
assert_contains "$scratch/registry.yaml" 'host: "localhost:5001"'
if grep -Eq 'delete cluster|rm -f|network rm' "$scratch/kind-args" "$scratch/docker-args"; then
  fail "Creation must never remove a shared cluster/registry/network"
fi
IP_FAMILY=dual render_substrate_kind_config 1 > "$scratch/dual.yaml"
assert_contains "$scratch/dual.yaml" "ipFamily: dual"
assert_contains "$scratch/dual.yaml" "hostPath: /dev/kvm"
IP_FAMILY=ipv6 render_substrate_kind_config 0 > "$scratch/ipv6.yaml"
assert_contains "$scratch/ipv6.yaml" "ipFamily: ipv6"
if (IP_FAMILY=invalid render_substrate_kind_config) >/dev/null 2>&1; then
  fail "Invalid IP_FAMILY accepted"
fi
unset TEST_NODE_VERSION
if (check_existing_kind_cluster old-cluster) > "$scratch/existing" 2>&1; then
  fail "Old cluster silently reused"
fi
assert_contains "$scratch/existing" "NOT recreated or upgraded"
ALLOW_EXISTING_KUBERNETES_VERSION=1 check_existing_kind_cluster old-cluster
TEST_NODE_VERSION="$KUBERNETES_VERSION" check_existing_kind_cluster latest-cluster
TEST_NODE_VERSION="$(printf '%s\n%s' "$KUBERNETES_VERSION" "$KUBERNETES_VERSION")" \
  check_existing_kind_cluster multi-node-latest
printf 'kind pin regression tests passed\n'
