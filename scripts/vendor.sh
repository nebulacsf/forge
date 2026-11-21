#!/usr/bin/env bash
# Downloads the third-party frontend assets into web/vendor so the UI can load
# without any network access. Run this only when bumping a version below; the
# generated files are committed so `go build` stays hermetic.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VENDOR="$ROOT/web/vendor"

BOOTSTRAP_VERSION="5.3.3"
BOOTSTRAP_ICONS_VERSION="1.11.3"
MARKED_VERSION="12.0.2"
HIGHLIGHT_VERSION="11.10.0"

download() {
  local url="$1" dest="$2"
  mkdir -p "$(dirname "$dest")"
  echo "  $url"
  curl -fsSL --retry 3 --retry-delay 1 "$url" -o "$dest"
  # Drop source-map comments so a debugger never requests a missing .map file.
  # Only whole-line comments are removed; inline trailing comments are kept.
  # The temp file keeps this portable across GNU and BSD sed.
  local tmp
  tmp="$(mktemp)"
  sed '/^\/\/# sourceMappingURL=/d; /^\/\*# sourceMappingURL=.*\*\/$/d' "$dest" >"$tmp"
  mv "$tmp" "$dest"
  chmod 0644 "$dest"
  if [ ! -s "$dest" ]; then
    echo "error: $dest is empty" >&2
    exit 1
  fi
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$@"
  else
    shasum -a 256 "$@"
  fi
}

echo "Vendoring frontend assets into web/vendor"

download \
  "https://cdn.jsdelivr.net/npm/bootstrap@${BOOTSTRAP_VERSION}/dist/css/bootstrap.min.css" \
  "$VENDOR/bootstrap/bootstrap.min.css"
download \
  "https://cdn.jsdelivr.net/npm/bootstrap@${BOOTSTRAP_VERSION}/dist/js/bootstrap.bundle.min.js" \
  "$VENDOR/bootstrap/bootstrap.bundle.min.js"

download \
  "https://cdn.jsdelivr.net/npm/bootstrap-icons@${BOOTSTRAP_ICONS_VERSION}/font/bootstrap-icons.min.css" \
  "$VENDOR/bootstrap-icons/bootstrap-icons.min.css"
download \
  "https://cdn.jsdelivr.net/npm/bootstrap-icons@${BOOTSTRAP_ICONS_VERSION}/font/fonts/bootstrap-icons.woff2" \
  "$VENDOR/bootstrap-icons/fonts/bootstrap-icons.woff2"
download \
  "https://cdn.jsdelivr.net/npm/bootstrap-icons@${BOOTSTRAP_ICONS_VERSION}/font/fonts/bootstrap-icons.woff" \
  "$VENDOR/bootstrap-icons/fonts/bootstrap-icons.woff"

download \
  "https://cdn.jsdelivr.net/npm/marked@${MARKED_VERSION}/marked.min.js" \
  "$VENDOR/marked/marked.min.js"

download \
  "https://cdn.jsdelivr.net/npm/@highlightjs/cdn-assets@${HIGHLIGHT_VERSION}/highlight.min.js" \
  "$VENDOR/highlight/highlight.min.js"
download \
  "https://cdn.jsdelivr.net/npm/@highlightjs/cdn-assets@${HIGHLIGHT_VERSION}/styles/github-dark.min.css" \
  "$VENDOR/highlight/github-dark.min.css"
download \
  "https://cdn.jsdelivr.net/npm/@highlightjs/cdn-assets@${HIGHLIGHT_VERSION}/styles/github.min.css" \
  "$VENDOR/highlight/github.min.css"

echo "Writing checksums"
(
  cd "$VENDOR"
  : >SHA256SUMS
  find . -type f ! -name SHA256SUMS ! -name LICENSES.md | LC_ALL=C sort | while IFS= read -r file; do
    sha256 "$file" >>SHA256SUMS
  done
)

echo "Done. Versions: bootstrap=$BOOTSTRAP_VERSION bootstrap-icons=$BOOTSTRAP_ICONS_VERSION marked=$MARKED_VERSION highlight.js=$HIGHLIGHT_VERSION"
