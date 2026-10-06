#!/bin/bash
set -euo pipefail
photoscli_root="$(cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$photoscli_root"
mkdir -p bin
if [[ "$(uname -s)" == Darwin ]]; then
  CGO_ENABLED=1 go build -ldflags "-linkmode=external -extldflags '-Wl,-sectcreate,__TEXT,__info_plist,$photoscli_root/Info.plist'" -o bin/photoscli ./cmd/photoscli
  # A stable identity keeps the macOS Photos grant across rebuilds; an ad-hoc
  # signature changes with every build and needs a new grant. Prefer an
  # explicit identity, then a dedicated "photoscli Code Signing" certificate
  # in the user's keychain, then ad-hoc.
  photoscli_identity="${PHOTOSCLI_SIGN_IDENTITY:-}"
  if [[ -z "$photoscli_identity" ]]; then
    photoscli_identity="$(security find-identity -p codesigning 2>/dev/null | awk '/"photoscli Code Signing"/ {print $2; exit}')"
  fi
  codesign --force --sign "${photoscli_identity:--}" --identifier com.mbelinky.photoscli --timestamp=none bin/photoscli
  codesign --verify --strict bin/photoscli
else
  go build -o bin/photoscli ./cmd/photoscli
fi
