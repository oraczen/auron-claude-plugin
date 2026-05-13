#!/usr/bin/env bash
# Cross-compile auron-auth for macOS, Linux, and Windows.
# Outputs binaries into skills/auth/bin/ so the auth skill can invoke them.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VERSION="$(git -C "${ROOT}" describe --tags --always --dirty 2>/dev/null || echo dev)"

# name : pkg : out_dir
BINARIES=(
  "auron-auth:./cmd/auron-auth:${ROOT}/skills/auth/bin"
  "auron-api:./cmd/auron-api:${ROOT}/skills/api/bin"
)

TARGETS=(
  "darwin/arm64"
  "darwin/amd64"
  "linux/amd64"
  "linux/arm64"
  "windows/amd64"
)

# Allow `build.sh host` to only build for the current platform.
if [[ "${1:-}" == "host" ]]; then
  TARGETS=("$(go env GOOS)/$(go env GOARCH)")
fi



host_goos="$(go env GOOS)"
host_goarch="$(go env GOARCH)"
host_ext=""
[[ "${host_goos}" == "windows" ]] && host_ext=".exe"

for entry in "${BINARIES[@]}"; do
  IFS=':' read -r name pkg out_dir <<< "${entry}"
  mkdir -p "${out_dir}"
  echo "Building ${name} ${VERSION} → ${out_dir}"

  for target in "${TARGETS[@]}"; do
    goos="${target%/*}"
    goarch="${target#*/}"
    ext=""
    [[ "${goos}" == "windows" ]] && ext=".exe"
    out="${out_dir}/${name}-${goos}-${goarch}${ext}"

    echo "  → ${goos}/${goarch}"
    GOOS="${goos}" GOARCH="${goarch}" CGO_ENABLED=0 \
      go build \
        -trimpath \
        -ldflags="-s -w -X main.version=${VERSION}" \
        -o "${out}" \
        "${pkg}"
  done

  host_bin="${name}-${host_goos}-${host_goarch}${host_ext}"
  if [[ -f "${out_dir}/${host_bin}" ]]; then
    ln -sf "${host_bin}" "${out_dir}/${name}${host_ext}"
  fi
done

echo "Done."
