#!/usr/bin/env bash
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$HERE/versions.env"
cluster="${1:?usage: install-gvisor-kind.sh CLUSTER}"

# Based on isola v0.7.0's hack/setup.sh. Only touch this cluster's nodes.
for node in $(kind get nodes --name "$cluster"); do
  arch=$(docker exec "$node" uname -m)
  case "$arch" in
    x86_64|aarch64) ;;
    *) echo "unsupported gVisor architecture: $arch" >&2; exit 1 ;;
  esac
  url="https://storage.googleapis.com/gvisor/releases/release/$GVISOR_VERSION/$arch"
  docker exec "$node" sh -c "
    set -eu
    mkdir -p /tmp/playwright-gvisor
    cd /tmp/playwright-gvisor
    for binary in runsc containerd-shim-runsc-v1; do
      curl -fsSL '$url/'\"\$binary\" -o \"\$binary\"
      curl -fsSL '$url/'\"\$binary.sha512\" -o \"\$binary.sha512\"
      sha512sum -c \"\$binary.sha512\"
      install -m 0755 \"\$binary\" /usr/local/bin/
    done
  "
  if ! docker exec "$node" grep -q 'runtimes.runsc' /etc/containerd/config.toml; then
    docker exec -i "$node" sh -c 'cat >> /etc/containerd/config.toml' <<'EOF'

[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc]
  runtime_type = "io.containerd.runsc.v1"
  pod_annotations = ["dev.gvisor.*"]
[plugins."io.containerd.grpc.v1.cri".containerd.runtimes.runsc.options]
  TypeUrl = "io.containerd.runsc.v1.options"
  ConfigPath = "/etc/containerd/runsc.toml"
EOF
  fi
  docker exec -i "$node" sh -c 'cat > /etc/containerd/runsc.toml' <<'EOF'
[runsc_config]
  platform = "systrap"
  allow-rootfs-tar-annotation = "true"
  systemd-cgroup = "true"
EOF
  docker exec "$node" systemctl restart containerd
  docker exec "$node" runsc --version
done
kubectl --context "kind-$cluster" wait --for=condition=Ready nodes --all --timeout=120s
