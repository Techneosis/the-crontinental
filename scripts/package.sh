#!/usr/bin/env bash
# Build and package every release target: scripts/package.sh <version> <outdir>
#
# Each archive, crontinental-<os>-<arch>.tar.gz, unpacks to a directory that
# install.sh recognises as a release (a VERSION file and build/crontinental),
# so the installer inside it installs that binary rather than building one.
# SHA256SUMS covers every archive; VERSION names the release, for
# `crontinental upgrade`. The release workflow runs this; so can you.
set -euo pipefail

version=${1:?usage: scripts/package.sh <version> <outdir>}
out=${2:?usage: scripts/package.sh <version> <outdir>}
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
mkdir -p "$out"
out=$(cd "$out" && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
  os=${target%/*} arch=${target#*/}
  name=crontinental-$os-$arch
  dir=$work/$name
  mkdir -p "$dir/build"
  (cd "$root" && CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
    -ldflags "-s -w -X main.version=$version" -o "$dir/build/crontinental" ./cmd/crontinental)
  cp -R "$root/completions" "$root/systemd" "$root/launchd" "$root/agents" "$root/notify" "$dir/"
  mkdir -p "$dir/docs"
  cp "$root/install.sh" "$root/LICENSE" "$root/README.md" "$dir/"
  cp "$root/docs/design.md" "$dir/docs/"
  echo "$version" >"$dir/VERSION"
  tar -C "$work" -czf "$out/$name.tar.gz" "$name"
  echo "packaged $name.tar.gz"
done
(cd "$out" && { command -v sha256sum >/dev/null && sha256sum crontinental-*.tar.gz || shasum -a 256 crontinental-*.tar.gz; } >SHA256SUMS)
echo "wrote $out/SHA256SUMS"
# What `crontinental upgrade` reads, from <releases>/latest/download/VERSION.
echo "$version" >"$out/VERSION"
echo "wrote $out/VERSION"
