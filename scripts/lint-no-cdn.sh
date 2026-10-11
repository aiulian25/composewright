#!/usr/bin/env bash
# Nothing in the browser assets may reach the network: no CDN scripts or
# stylesheets, no web fonts, no analytics, no absolute URLs.
# SVG and XML namespace identifiers are names, not requests, and are allowed.
set -euo pipefail
cd "$(dirname "$0")/.."

roots=()
for root in web/static web/templates internal/web; do
  [[ -d "$root" ]] && roots+=("$root")
done
[[ ${#roots[@]} -eq 0 ]] && exit 0

pattern='https?://|(?<![:\w])//[a-z0-9.-]+\.[a-z]{2,}|@font-face|@import|\burl\(\s*["'"'"']?(?:https?:|//)|googleapis|gstatic|jsdelivr|unpkg|cdnjs|googletagmanager|google-analytics|plausible|matomo|sentry'
namespaces='https?://www\.w3\.org/'

findings=$(grep -rnP --include='*.html' --include='*.css' --include='*.js' --include='*.svg' --include='*.json' \
  "$pattern" "${roots[@]}" | grep -vP "$namespaces" || true)

if [[ -n "$findings" ]]; then
  echo "network references in browser assets:" >&2
  echo "$findings" >&2
  exit 1
fi
