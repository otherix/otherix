#!/usr/bin/env bash
# render-demo-manifest.sh — render dev/manifests/demo-vm.yaml from the template
# for THIS host's architecture. The dev node runs on the host (Linux native) or
# in a same-arch Lima VM (macOS), so the demo VM arch equals the host arch and
# boots under KVM instead of slow cross-arch TCG. The rendered file is gitignored.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
TMPL="${REPO_ROOT}/dev/manifests/demo-vm.yaml.tmpl"
OUT="${REPO_ROOT}/dev/manifests/demo-vm.yaml"

case "$(uname -m)" in
    x86_64|amd64)  arch=amd64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "unsupported host arch: $(uname -m)" >&2; exit 1 ;;
esac

url="https://cloud-images.ubuntu.com/minimal/releases/noble/release/ubuntu-24.04-minimal-cloudimg-${arch}.img"

sed -e "s|__ARCH__|${arch}|g" -e "s|__IMAGE_URL__|${url}|g" "${TMPL}" > "${OUT}"
echo ">> rendered ${OUT} (arch ${arch})"
